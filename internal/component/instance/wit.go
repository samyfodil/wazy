package instance

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/samyfodil/wazy/internal/component/binary"
	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// This file prints a component's own type -- what it imports and exports,
// as the type check computes it -- as WIT, the way `wasm-tools component wit`
// does: a world "root" in a package "root:component", and each interface it
// names in a package block of its own package.
//
// The type check keeps types structural, so the names come from where the
// component's instance types export them: a value type or resource an
// instance exports is named there, and every use of the same type is the
// same *ctVal or *ctResource. An interface that exports a type an earlier
// one named -- the same one, or for an export, whose types a shim component
// declares anew, an equal one under the same name -- uses it from there.

// ComponentWIT returns the WIT of the component in componentBytes.
func ComponentWIT(componentBytes []byte) (string, error) {
	comp, err := binary.Decode(bytes.NewReader(componentBytes))
	if err != nil {
		return "", fmt.Errorf("wit: decode component: %w", err)
	}
	env := newCTEnv(comp, nil)
	defer env.release()
	self, err := env.selfType()
	if err != nil {
		return "", err
	}
	p := &witPrinter{
		valOwner: map[*ctVal]witName{},
		resOwner: map[*ctResource]witName{},
		byName:   map[string][]witName{},
	}
	world := p.newScope("", "")
	var imports, exports []string
	importAttrs := make(map[string]*binary.NameAttrs, len(comp.Imports))
	for _, im := range comp.Imports {
		importAttrs[im.Name] = im.Attrs
	}
	exportAttrs := make(map[string]*binary.NameAttrs, len(comp.Exports))
	for _, ex := range comp.Exports {
		exportAttrs[ex.Name] = ex.Attrs
	}
	for _, side := range []struct {
		list  []ctNamed
		attrs map[string]*binary.NameAttrs
		out   *[]string
		kw    string
	}{{self.imports, importAttrs, &imports, "import"}, {self.exports, exportAttrs, &exports, "export"}} {
		for _, n := range side.list {
			line, err := p.item(world, side.kw, n, side.attrs[n.name])
			if err != nil {
				return "", fmt.Errorf("wit: %s %q: %w", side.kw, n.name, err)
			}
			if line != "" {
				*side.out = append(*side.out, line)
			}
		}
	}

	var b strings.Builder
	b.WriteString("package root:component;\n\nworld root {\n")
	p.writeUses(&b, world, "  ")
	p.writeDefs(&b, world, "  ")
	for _, group := range [][]string{imports, exports} {
		for _, line := range group {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("}\n")
	for _, pkg := range p.pkgs {
		fmt.Fprintf(&b, "package %s {\n", pkg)
		for _, s := range p.ifaces {
			if s.pkg != pkg {
				continue
			}
			fmt.Fprintf(&b, "  interface %s {\n", witID(s.short))
			p.writeBody(&b, s, "    ")
			b.WriteString("  }\n")
		}
		b.WriteString("}\n")
	}
	return b.String(), nil
}

type witPrinter struct {
	ifaces []*witScope
	pkgs   []string

	// valOwner and resOwner are where each named type is defined; byName
	// is every named type by its name, in the order they were met.
	valOwner map[*ctVal]witName
	resOwner map[*ctResource]witName
	byName   map[string][]witName
}

// witName is a type named in a scope.
type witName struct {
	scope *witScope
	name  string
	val   *ctVal
	res   *ctResource
}

// use is the use of n by another scope, which names it local there: by that
// name, when the defining scope declares it as an alias of n.
func (n witName) use(local string) witUse {
	if n.scope.aliases[local] {
		return witUse{n.scope, local, local}
	}
	return witUse{n.scope, n.name, local}
}

// witScope is an interface, or the world: its names, the types it defines
// and uses, and its funcs, printed.
type witScope struct {
	name, pkg, short string

	vals    map[*ctVal]string
	res     map[*ctResource]string
	aliases map[string]bool // the names that are another name for one of the scope's types
	uses    []witUse
	defs    []string // printed definitions, in order
	funcs   []string // printed freestanding funcs, in order
	// resFuncs are each resource's printed funcs, by its name.
	resFuncs map[string][]string
	resDefs  map[string]int // a resource's index in defs
}

type witUse struct {
	from        *witScope
	name, local string
}

func (p *witPrinter) newScope(name, pkg string) *witScope {
	s := &witScope{
		name: name, pkg: pkg,
		vals: map[*ctVal]string{}, res: map[*ctResource]string{}, aliases: map[string]bool{},
		resFuncs: map[string][]string{}, resDefs: map[string]int{},
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		s.short, _, _ = strings.Cut(name[i+1:], "@")
	}
	return s
}

// item reads one import or export of the component, adding what it names
// to the world, and returns the world's line for it.
func (p *witPrinter) item(world *witScope, kw string, n ctNamed, attrs *binary.NameAttrs) (string, error) {
	switch n.ent.sort {
	case ctSortInstance:
		iface := witInterfaceOf(n.name, attrs)
		pkg, qualified := witPackageOf(iface)
		line := fmt.Sprintf("%s %s;", kw, iface)
		if attrs != nil && attrs.HasImplements && qualified {
			line = fmt.Sprintf("%s %s: %s;", kw, witID(n.name), iface)
		}
		if i := slices.IndexFunc(p.ifaces, func(s *witScope) bool { return s.name == iface }); i >= 0 {
			p.same(p.ifaces[i], n.ent.inst)
			return line, nil
		}
		s := p.newScope(iface, pkg)
		if err := p.instance(s, n.ent.inst); err != nil {
			return "", err
		}
		if !qualified {
			var b strings.Builder
			p.writeBody(&b, s, "    ")
			return fmt.Sprintf("%s %s: interface {\n%s  }", kw, witID(n.name), b.String()), nil
		}
		p.ifaces = append(p.ifaces, s)
		if !slices.Contains(p.pkgs, pkg) {
			p.pkgs = append(p.pkgs, pkg)
		}
		return line, nil
	case ctSortFunc:
		sig, err := p.funcSig(world, n.ent.fn, 0)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s: %s;", kw, witID(n.name), sig), nil
	case ctSortType:
		return "", p.typeExport(world, n.name, n.ent.ty)
	case ctSortComponent, ctSortCoreModule:
		// WIT has no word for these, and wasm-tools leaves them out too: a
		// host supplies an imported component with WithComponentImport.
		return "", nil
	}
	return "", fmt.Errorf("a %s is not something WIT can describe", ctSortDesc(n.ent.sort))
}

// witInterfaceOf is the interface an instance imported or exported under
// name is, as WIT spells it: the one it `implements`, or else its name, with
// its `version-suffix` put back.
func witInterfaceOf(name string, attrs *binary.NameAttrs) string {
	if attrs == nil {
		return name
	}
	if attrs.HasImplements {
		name = attrs.Implements
	}
	if strings.IndexByte(name, '@') >= 0 {
		name += attrs.VersionSuffix
	}
	return name
}

// same reads another instance of the interface s describes: its types are
// s's, under the names they are exported by.
func (p *witPrinter) same(s *witScope, in *ctInstance) {
	for _, e := range in.exports {
		if e.ent.sort != ctSortType {
			continue
		}
		switch t := e.ent.ty; {
		case t.res != nil:
			if _, ok := p.resOwner[t.res]; !ok {
				p.resOwner[t.res] = witName{scope: s, name: e.name, res: t.res}
			}
		case t.val != nil && t.val.kind != ctvPrim:
			if _, ok := p.valOwner[t.val]; !ok {
				p.valOwner[t.val] = witName{scope: s, name: e.name, val: t.val}
			}
		}
	}
}

// instance reads an instance type into s: its types first, then its funcs.
func (p *witPrinter) instance(s *witScope, in *ctInstance) error {
	for _, e := range in.exports {
		if e.ent.sort == ctSortType {
			if err := p.typeExport(s, e.name, e.ent.ty); err != nil {
				return fmt.Errorf("type %s: %w", e.name, err)
			}
		}
	}
	for _, e := range in.exports {
		if e.ent.sort != ctSortFunc {
			if e.ent.sort != ctSortType {
				return fmt.Errorf("%s: a %s is not something WIT can describe", e.name, ctSortDesc(e.ent.sort))
			}
			continue
		}
		if err := p.addFunc(s, e.name, e.ent.fn); err != nil {
			return fmt.Errorf("%s: %w", e.name, err)
		}
	}
	return nil
}

// typeExport names the type t exports as name in s: a use of the scope
// that defines it, or a definition of s's own.
func (p *witPrinter) typeExport(s *witScope, name string, t ctType) error {
	switch {
	case t.res != nil:
		if owner, ok := p.resOwner[t.res]; ok {
			if owner.scope == s { // another name for one of s's own
				s.aliases[name] = true
				s.defs = append(s.defs, "type "+witID(name)+" = "+witID(owner.name)+";")
				return nil
			}
			if _, named := s.res[t.res]; !named {
				s.res[t.res] = name
			}
			s.uses = append(s.uses, owner.use(name))
			return nil
		}
		s.res[t.res] = name
		p.resOwner[t.res] = witName{scope: s, name: name, res: t.res}
		s.resDefs[name] = len(s.defs)
		s.defs = append(s.defs, "") // the resource, printed once its funcs are known
		return nil
	case t.val == nil:
		return fmt.Errorf("a %s type is not something WIT can describe", t.desc())
	}
	v := t.val
	if v.kind != ctvPrim { // a primitive's *ctVal is shared by every alias of it
		if owner, ok := p.valOwner[v]; ok {
			if owner.scope == s {
				s.aliases[name] = true
				s.defs = append(s.defs, "type "+witID(name)+" = "+witID(owner.name)+";")
				return nil
			}
			if _, named := s.vals[v]; !named {
				s.vals[v] = name
			}
			s.uses = append(s.uses, owner.use(name))
			return nil
		}
		s.vals[v] = name
		for _, owner := range p.byName[name] {
			if owner.val != nil && owner.scope != s && owner.scope.short != "" && ctValSub(v, owner.val) == nil {
				s.uses = append(s.uses, witUse{owner.scope, owner.name, name})
				return nil
			}
		}
		owner := witName{scope: s, name: name, val: v}
		p.valOwner[v] = owner
		p.byName[name] = append(p.byName[name], owner)
	} else {
		s.vals[v] = name
	}
	def, err := p.typeDef(s, name, v)
	if err != nil {
		return err
	}
	s.defs = append(s.defs, def)
	return nil
}

// typeDef prints the definition of v, named name in s.
func (p *witPrinter) typeDef(s *witScope, name string, v *ctVal) (string, error) {
	var b strings.Builder
	id := witID(name)
	switch v.kind {
	case ctvRecord:
		fmt.Fprintf(&b, "record %s {\n", id)
		for i, l := range v.labels {
			t, err := p.val(s, v.elems[i])
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "  %s: %s,\n", witID(l), t)
		}
		b.WriteString("}")
	case ctvVariant:
		fmt.Fprintf(&b, "variant %s {\n", id)
		for i, l := range v.labels {
			if v.elems[i] == nil {
				fmt.Fprintf(&b, "  %s,\n", witID(l))
				continue
			}
			t, err := p.val(s, v.elems[i])
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "  %s(%s),\n", witID(l), t)
		}
		b.WriteString("}")
	case ctvEnum, ctvFlags:
		kw := "enum"
		if v.kind == ctvFlags {
			kw = "flags"
		}
		fmt.Fprintf(&b, "%s %s {\n", kw, id)
		for _, l := range v.labels {
			fmt.Fprintf(&b, "  %s,\n", witID(l))
		}
		b.WriteString("}")
	default:
		t, err := p.anon(s, v)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "type %s = %s;", id, t)
	}
	return b.String(), nil
}

// val prints a reference to v from s: its name in s, or the type written
// out.
func (p *witPrinter) val(s *witScope, v *ctVal) (string, error) {
	if n, ok := s.vals[v]; ok {
		return witID(n), nil
	}
	return p.anon(s, v)
}

// anon prints v written out, its parts by reference.
func (p *witPrinter) anon(s *witScope, v *ctVal) (string, error) {
	elems := func(sep string) (string, error) {
		parts := make([]string, len(v.elems))
		for i, e := range v.elems {
			t, err := p.val(s, e)
			if err != nil {
				return "", err
			}
			parts[i] = t
		}
		return strings.Join(parts, sep), nil
	}
	switch v.kind {
	case ctvPrim:
		return v.prim, nil
	case ctvList, ctvOption, ctvTuple, ctvMap:
		inner, err := elems(", ")
		if err != nil {
			return "", err
		}
		return map[ctValKind]string{ctvList: "list", ctvOption: "option", ctvTuple: "tuple", ctvMap: "map"}[v.kind] + "<" + inner + ">", nil
	case ctvFixedList:
		inner, err := elems("")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("list<%s, %d>", inner, v.length), nil
	case ctvStream, ctvFuture:
		kw := "stream"
		if v.kind == ctvFuture {
			kw = "future"
		}
		if v.elems[0] == nil {
			return kw, nil
		}
		inner, err := p.val(s, v.elems[0])
		return kw + "<" + inner + ">", err
	case ctvResult:
		ok, errT := "_", ""
		var err error
		if v.elems[0] != nil {
			if ok, err = p.val(s, v.elems[0]); err != nil {
				return "", err
			}
		}
		if v.elems[1] != nil {
			if errT, err = p.val(s, v.elems[1]); err != nil {
				return "", err
			}
		}
		switch {
		case errT != "":
			return "result<" + ok + ", " + errT + ">", nil
		case ok != "_":
			return "result<" + ok + ">", nil
		}
		return "result", nil
	case ctvOwn, ctvBorrow:
		name, err := p.resName(s, v.res)
		if err != nil || v.kind == ctvOwn {
			return name, err
		}
		return "borrow<" + name + ">", nil
	}
	if owner, ok := p.valOwner[v]; ok {
		// A named type of another scope, which s did not export: use it.
		s.uses = append(s.uses, witUse{owner.scope, owner.name, owner.name})
		s.vals[v] = owner.name
		return witID(owner.name), nil
	}
	return "", fmt.Errorf("a %s that no interface names", v.kind.desc())
}

func (p *witPrinter) resName(s *witScope, r *ctResource) (string, error) {
	if n, ok := s.res[r]; ok {
		return witID(n), nil
	}
	if owner, ok := p.resOwner[r]; ok {
		s.uses = append(s.uses, witUse{owner.scope, owner.name, owner.name})
		s.res[r] = owner.name
		return witID(owner.name), nil
	}
	return "", fmt.Errorf("a resource that no interface names")
}

// addFunc prints the func an instance exports as name into s: freestanding,
// or one of a resource's.
func (p *witPrinter) addFunc(s *witScope, name string, fn *ctFunc) error {
	if strings.Contains(name, "[get]") || strings.Contains(name, "[set]") {
		return fmt.Errorf("an accessor is not supported")
	}
	var res, line string
	switch {
	case strings.HasPrefix(name, "[constructor]"):
		res = name[len("[constructor]"):]
		sig, err := p.params(s, fn, 0)
		if err != nil {
			return err
		}
		line = "constructor" + sig
		if len(fn.results) == 1 && fn.results[0].t.kind != ctvOwn {
			r, err := p.val(s, fn.results[0].t)
			if err != nil {
				return err
			}
			line += " -> " + r
		}
	case strings.HasPrefix(name, "[method]"), strings.HasPrefix(name, "[static]"):
		kind, rest, _ := strings.Cut(name[1:], "]")
		var m string
		res, m, _ = strings.Cut(rest, ".")
		skip, prefix := 1, ""
		if kind == "static" {
			skip, prefix = 0, "static "
		}
		sig, err := p.funcSig(s, fn, skip)
		if err != nil {
			return err
		}
		line = witID(m) + ": " + prefix + sig
	default:
		sig, err := p.funcSig(s, fn, 0)
		if err != nil {
			return err
		}
		s.funcs = append(s.funcs, witID(name)+": "+sig+";")
		return nil
	}
	if _, ok := s.resDefs[res]; !ok {
		return fmt.Errorf("a func of resource %s, which the interface does not define", res)
	}
	s.resFuncs[res] = append(s.resFuncs[res], line+";")
	return nil
}

// funcSig prints fn's type from its skip'th param on: "async func(...) -> T".
func (p *witPrinter) funcSig(s *witScope, fn *ctFunc, skip int) (string, error) {
	params, err := p.params(s, fn, skip)
	if err != nil {
		return "", err
	}
	sig := "func" + params
	if fn.async {
		sig = "async " + sig
	}
	switch len(fn.results) {
	case 0:
		return sig, nil
	case 1:
		if fn.results[0].name == "" {
			r, err := p.val(s, fn.results[0].t)
			return sig + " -> " + r, err
		}
	}
	return "", fmt.Errorf("named results are not supported")
}

func (p *witPrinter) params(s *witScope, fn *ctFunc, skip int) (string, error) {
	if skip > len(fn.params) {
		return "", fmt.Errorf("a method with no self")
	}
	parts := make([]string, 0, len(fn.params)-skip)
	for _, f := range fn.params[skip:] {
		t, err := p.val(s, f.t)
		if err != nil {
			return "", err
		}
		parts = append(parts, witID(f.name)+": "+t)
	}
	return "(" + strings.Join(parts, ", ") + ")", nil
}

// writeBody writes an interface's body: its uses, its types, its funcs.
func (p *witPrinter) writeBody(b *strings.Builder, s *witScope, indent string) {
	p.writeUses(b, s, indent)
	p.writeDefs(b, s, indent)
	for _, f := range s.funcs {
		b.WriteString(indent + f + "\n")
	}
}

func (p *witPrinter) writeUses(b *strings.Builder, s *witScope, indent string) {
	var from []*witScope
	byFrom := map[*witScope][]string{}
	for _, u := range s.uses {
		if byFrom[u.from] == nil {
			from = append(from, u.from)
		}
		item := witID(u.name)
		if u.local != u.name {
			item += " as " + witID(u.local)
		}
		if !slices.Contains(byFrom[u.from], item) {
			byFrom[u.from] = append(byFrom[u.from], item)
		}
	}
	for _, f := range from {
		path := witID(f.short)
		if f.pkg != s.pkg {
			path = f.name
		}
		fmt.Fprintf(b, "%suse %s.{%s};\n", indent, path, strings.Join(byFrom[f], ", "))
	}
}

func (p *witPrinter) writeDefs(b *strings.Builder, s *witScope, indent string) {
	for i, d := range s.defs {
		for name, at := range s.resDefs {
			if at != i {
				continue
			}
			fs := s.resFuncs[name]
			if len(fs) == 0 {
				d = "resource " + witID(name) + ";"
			} else {
				d = "resource " + witID(name) + " {\n  " + strings.Join(fs, "\n  ") + "\n}"
			}
		}
		b.WriteString(indent + strings.ReplaceAll(d, "\n", "\n"+indent) + "\n")
	}
}

// witPackageOf is the package of an interface's qualified name --
// "ns:pkg/iface@1.0.0" is in "ns:pkg@1.0.0" -- and whether it is qualified
// at all, rather than a world's own label.
func witPackageOf(name string) (string, bool) {
	i := strings.LastIndexByte(name, '/')
	if i < 0 || !strings.Contains(name[:i], ":") {
		return "", false
	}
	pkg := name[:i]
	if _, ver, ok := strings.Cut(name[i+1:], "@"); ok {
		pkg += "@" + ver
	}
	return pkg, true
}

// witID is name as a WIT identifier: with a '%' when it is a keyword.
func witID(name string) string {
	if ast.IsKeyword(name) {
		return "%" + name
	}
	return name
}
