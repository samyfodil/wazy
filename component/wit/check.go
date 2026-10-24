package wit

import (
	"fmt"
	"slices"
	"strings"

	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// check validates one package -- all its files -- as wit-parser's AST
// resolver (crates/wit-parser/src/ast/resolve.rs) does, before any feature
// gate applies: names unique and defined, no cycles, handles of resources,
// well-formed types, funcs and worlds. Errors carry wit-parser's text.
// What depends on the other packages, or on which gates are enabled, is
// left to resolve.
func check(p *rawPackage) error {
	c := &checker{p: p, items: map[string]*citem{}, foreign: map[string]map[string]*citem{}}
	c.populateForeignDeps()
	ifaces, worlds, err := c.populateItems()
	if err != nil {
		return err
	}
	if err := c.populateForeignTypes(); err != nil {
		return err
	}
	for _, it := range ifaces {
		c.file = it.file
		if err := c.resolveInterface(it, it.iface.Items, it.iface.Attrs); err != nil {
			return err
		}
	}
	for _, it := range worlds {
		c.file = it.file
		if err := c.resolveWorld(it.world); err != nil {
			return err
		}
	}
	return nil
}

func errAt(pos ast.Pos, format string, args ...any) error {
	return fmt.Errorf("wit: %s: %s", pos, fmt.Sprintf(format, args...))
}

func notFound(kind, name string, pos ast.Pos) error {
	return errAt(pos, "%s `%s` does not exist", kind, name)
}

type checker struct {
	p       *rawPackage
	items   map[string]*citem            // the package's interfaces and worlds
	fileNS  []map[string]*citem          // each file's names, its top-level uses included
	foreign map[string]map[string]*citem // other packages' items, by package and name
	file    int                          // the file being checked
	lookup  *tscope                      // the types of the scope being checked
}

// citem is an interface or a world a path can name.
type citem struct {
	isWorld bool
	name    string
	foreign bool
	pkg     string // a foreign item's package
	file    int
	iface   *ast.Interface
	world   *ast.World
	// types are an interface's names once checked -- for a foreign one,
	// those its users name.
	types *tscope
}

// ctype is a type as far as checking needs to know it. Each definition is
// one ctype, so pointer equality is wit-parser's TypeId equality.
type ctype struct {
	kind   string // "prim", "alias", "resource", "unknown", "result", "own", "borrow", ...
	target *ctype // alias, own, borrow
	ok     *ctype // result
}

// tscope is a scope's names that can be types, in definition order: types
// and uses, and in an interface its funcs, which only make a better error.
type tscope struct {
	m     map[string]tentry
	order []string
}

type tentry struct {
	t   *ctype // nil for a func
	pos ast.Pos
}

func newTScope() *tscope { return &tscope{m: map[string]tentry{}} }

func (s *tscope) define(name string, pos ast.Pos, t *ctype) error {
	if _, ok := s.m[name]; ok {
		return errAt(pos, "name `%s` is defined more than once", name)
	}
	s.add(name, pos, t)
	return nil
}

// add defines a name the scope does not have yet.
func (s *tscope) add(name string, pos ast.Pos, t *ctype) {
	s.m[name] = tentry{t, pos}
	s.order = append(s.order, name)
}

// pathVisit is one path a declaration list names, as wit-parser's
// for_each_path visits them.
type pathVisit struct {
	owner   string // the interface whose use, or world whose include, it is
	attrs   ast.Attrs
	path    string
	pos     ast.Pos
	names   []ast.UseName
	isUse   bool // a use, which names types
	isWorld bool // an include, which names a world
}

// paths lists the paths one file names: per world, its uses and includes,
// then its imports' and exports' paths and inline interfaces' uses; per
// interface, its uses; and the top-level uses.
func paths(f *ast.Package) []pathVisit {
	var out []pathVisit
	uses := func(owner string, items []ast.InterfaceItem) {
		for _, it := range items {
			if u, ok := it.(*ast.Use); ok {
				out = append(out, pathVisit{owner: owner, attrs: u.Attrs, path: u.Path, pos: u.PathPos, names: u.Names, isUse: true})
			}
		}
	}
	for _, item := range f.Items {
		switch it := item.(type) {
		case *ast.World:
			var imports, exports []ast.Extern
			for _, wi := range it.Items {
				switch w := wi.(type) {
				case *ast.Use:
					out = append(out, pathVisit{attrs: w.Attrs, path: w.Path, pos: w.PathPos, names: w.Names, isUse: true})
				case *ast.Include:
					out = append(out, pathVisit{owner: it.Name, attrs: w.Attrs, path: w.Path, pos: w.PathPos, isWorld: true})
				case *ast.Import:
					imports = append(imports, w.Extern)
				case *ast.Export:
					exports = append(exports, w.Extern)
				}
			}
			for _, ext := range append(imports, exports...) {
				switch k := ext.Kind.(type) {
				case *ast.ExternInterface:
					uses("", k.Items)
				case *ast.ExternPath:
					out = append(out, pathVisit{attrs: ext.Attrs, path: ext.Name, pos: ext.Pos})
				case *ast.ExternNamedPath:
					out = append(out, pathVisit{attrs: ext.Attrs, path: k.Path, pos: k.PathPos})
				}
			}
		case *ast.Interface:
			uses(it.Name, it.Items)
		case *ast.ToplevelUse:
			out = append(out, pathVisit{attrs: it.Attrs, path: it.Path, pos: it.PathPos})
		}
	}
	return out
}

// splitForeign splits "ns:pkg/name@ver" into "ns:pkg@ver" and "name"; ok is
// false for a bare name.
func splitForeign(path string) (pkg, name string, ok bool) {
	ns, rest, ok := strings.Cut(path, "/")
	if !ok {
		return "", "", false
	}
	name, ver, hasVer := strings.Cut(rest, "@")
	if hasVer {
		return ns + "@" + ver, name, true
	}
	return ns, name, true
}

// populateForeignDeps notes every item of another package a path names, as
// a world when an include names it first and an interface otherwise.
func (c *checker) populateForeignDeps() {
	for _, f := range c.p.files {
		for _, v := range paths(f) {
			pkg, name, ok := splitForeign(v.path)
			if !ok {
				continue
			}
			if c.foreign[pkg] == nil {
				c.foreign[pkg] = map[string]*citem{}
				c.p.deps = append(c.p.deps, dep{pkg, v.pos})
			}
			it := c.foreign[pkg][name]
			if it == nil {
				it = &citem{isWorld: v.isWorld, name: name, foreign: true, pkg: pkg, types: newTScope()}
				c.foreign[pkg][name] = it
				c.p.foreign = append(c.p.foreign, &foreignItem{pkg: pkg, name: name, world: v.isWorld})
			}
			fi := c.p.foreign[slices.IndexFunc(c.p.foreign, func(f *foreignItem) bool { return f.pkg == pkg && f.name == name })]
			fi.gates = append(fi.gates, v.attrs.Gate)
		}
	}
}

// foreignItem is an interface or world of another package that a package
// names, with the gate of each place that names it.
type foreignItem struct {
	pkg, name string
	world     bool
	gates     []ast.Gate
}

// foreignType is a type a package's uses take from another package, with
// the gate of the first use that names it, which is the one wit-parser
// keeps.
type foreignType struct {
	pkg, iface, name string
	gate             ast.Gate
	pos              ast.Pos
}

// nsEntry is what a file-level name stands for: one of the package's items,
// by name, or another package's.
type nsEntry struct {
	local   string
	pos     ast.Pos
	foreign bool
}

// populateItems names the package's interfaces and worlds, checks each
// file's top-level names, and orders the interfaces and worlds so that each
// comes after those its uses and includes name.
func (c *checker) populateItems() (ifaces, worlds []*citem, err error) {
	var keys []string
	deps := map[string][]dep{}
	for i, f := range c.p.files {
		for _, item := range f.Items {
			var it *citem
			var pos ast.Pos
			switch x := item.(type) {
			case *ast.Interface:
				it, pos = &citem{name: x.Name, iface: x, file: i}, x.Pos
			case *ast.World:
				it, pos = &citem{isWorld: true, name: x.Name, world: x, file: i}, x.Pos
			default:
				continue
			}
			if c.items[it.name] != nil {
				return nil, nil, errAt(pos, "duplicate item named `%s`", it.name)
			}
			c.items[it.name] = it
			keys = append(keys, it.name)
		}
	}
	for _, f := range c.p.files {
		ns := map[string]nsEntry{}
		for _, item := range f.Items {
			var name string
			var pos ast.Pos
			var e nsEntry
			switch x := item.(type) {
			case *ast.ToplevelUse:
				name, pos = x.Local()
				_, _, e.foreign = splitForeign(x.Path)
				e.local, e.pos = x.Path, x.PathPos
			case *ast.Interface:
				name, pos = x.Name, x.Pos
				e.local, e.pos = name, pos
			case *ast.World:
				name, pos = x.Name, x.Pos
				e.local, e.pos = name, pos
			}
			if _, ok := ns[name]; ok {
				return nil, nil, errAt(pos, "duplicate name `%s` in this file", name)
			}
			ns[name] = e
		}
		for _, v := range paths(f) {
			if v.owner == "" || strings.Contains(v.path, ":") {
				continue
			}
			e, ok := ns[v.path]
			switch {
			case ok && e.foreign:
				continue
			case ok:
				deps[v.owner] = append(deps[v.owner], dep{e.local, e.pos})
			case c.items[v.path] != nil:
				deps[v.owner] = append(deps[v.owner], dep{v.path, v.pos})
			default:
				return nil, nil, notFound("interface or world", v.path, v.pos)
			}
		}
	}
	order, err := toposort("interface or world", keys, deps)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range order {
		if it := c.items[name]; it.isWorld {
			worlds = append(worlds, it)
		} else {
			ifaces = append(ifaces, it)
		}
	}
	for _, f := range c.p.files {
		ns := map[string]*citem{}
		for _, item := range f.Items {
			switch x := item.(type) {
			case *ast.ToplevelUse:
				if len(x.Gate) > 0 || len(x.ExternalID) > 0 {
					return nil, nil, errAt(x.Pos, "attributes not allowed on top-level use")
				}
				name, _ := x.Local()
				it := c.items[x.Path]
				if pkg, n, ok := splitForeign(x.Path); ok {
					it = c.foreign[pkg][n]
				} else if it == nil {
					return nil, nil, notFound("interface or world", x.Path, x.PathPos)
				}
				ns[name] = it
			case *ast.Interface:
				ns[x.Name] = c.items[x.Name]
			case *ast.World:
				ns[x.Name] = c.items[x.Name]
			}
		}
		c.fileNS = append(c.fileNS, ns)
	}
	return ifaces, worlds, nil
}

// resolvePath finds the item a path names in the current file.
func (c *checker) resolvePath(path string, pos ast.Pos) (*citem, error) {
	if it := c.item(path); it != nil {
		return it, nil
	}
	return nil, notFound("interface or world", path, pos)
}

// item is the item a path names in the current file, or nil.
func (c *checker) item(path string) *citem {
	if pkg, name, ok := splitForeign(path); ok {
		return c.foreign[pkg][name]
	}
	if it := c.fileNS[c.file][path]; it != nil {
		return it
	}
	return c.items[path]
}

func (c *checker) ifaceAt(path string, pos ast.Pos) (*citem, error) {
	it, err := c.resolvePath(path, pos)
	if err == nil && it.isWorld {
		err = errAt(pos, "name `%s` is defined as a world, not an interface", ast.LastSegment(path))
	}
	return it, err
}

// populateForeignTypes notes the types each use takes from another package.
func (c *checker) populateForeignTypes() error {
	for i, f := range c.p.files {
		c.file = i
		for _, v := range paths(f) {
			if !v.isUse {
				continue
			}
			if err := checkAttrs(v.attrs); err != nil {
				return err
			}
			it, err := c.ifaceAt(v.path, v.pos)
			if err != nil {
				return err
			}
			if !it.foreign {
				continue
			}
			for _, n := range v.names {
				if _, ok := it.types.m[n.Name]; !ok {
					it.types.add(n.Name, n.Pos, &ctype{kind: "unknown"})
					c.p.foreignTypes = append(c.p.foreignTypes, foreignType{pkg: it.pkg, iface: it.name, name: n.Name, gate: v.attrs.Gate, pos: n.Pos})
				}
			}
		}
	}
	return nil
}

// checkStability checks the gate attributes as wit-parser's stability()
// does: each at most once, @since and @unstable not both, and @deprecated
// never alone.
func checkStability(a ast.Attrs) error {
	var since, deprecated, unstable *ast.GateItem
	for i := range a.Gate {
		g := &a.Gate[i]
		seen := map[string]**ast.GateItem{"since": &since, "deprecated": &deprecated, "unstable": &unstable}[g.Kind]
		if *seen != nil {
			return errAt(g.Pos, "cannot specify @%s twice", g.Kind)
		}
		*seen = g
	}
	switch {
	case since != nil && unstable != nil:
		return errAt(since.Pos, "cannot specify both @since and @unstable")
	case deprecated != nil && since == nil && unstable == nil:
		return errAt(deprecated.Pos, "cannot specify both @deprecated without @since or @unstable")
	}
	return nil
}

// checkAttrs is checkStability, then at most one @external-id.
func checkAttrs(a ast.Attrs) error {
	if err := checkStability(a); err != nil {
		return err
	}
	if len(a.ExternalID) > 1 {
		return errAt(a.ExternalID[1], "cannot specify @external-id twice")
	}
	return nil
}

// resolveInterface checks an interface -- or an interface written inline in
// a world -- and keeps its names for the uses that name it.
func (c *checker) resolveInterface(it *citem, items []ast.InterfaceItem, attrs ast.Attrs) error {
	if err := checkStability(attrs); err != nil {
		return err
	}
	var typeItems []any
	for _, item := range items {
		switch item.(type) {
		case *ast.Use, *ast.TypeDef:
			typeItems = append(typeItems, item)
		}
	}
	if err := c.resolveTypes(typeItems); err != nil {
		return err
	}
	names := newNameScope()
	for _, name := range c.lookup.order {
		if err := names.define(name, c.lookup.m[name].pos); err != nil {
			return err
		}
	}
	for _, item := range items {
		switch x := item.(type) {
		case *ast.InterfaceFunc:
			if _, ok := c.lookup.m[x.Name]; !ok {
				c.lookup.add(x.Name, x.Pos, nil)
			}
			name := freestandingName(x.Name, x.Func.Accessor)
			if err := c.resolveFunc(x.Attrs, x.Func, funcKind{}, nil); err != nil {
				return err
			}
			if err := names.define(name, x.Pos); err != nil {
				return err
			}
		case *ast.TypeDef:
			if err := c.resolveResourceFuncs(x, names.define); err != nil {
				return err
			}
		}
	}
	if err := names.checkSetters(); err != nil {
		return err
	}
	if it != nil {
		it.types = c.lookup
	}
	return nil
}

// resolveResourceFuncs checks a resource's funcs, defining each name.
func (c *checker) resolveResourceFuncs(td *ast.TypeDef, define func(string, ast.Pos) error) error {
	res, ok := td.Type.(*ast.Resource)
	if !ok {
		return nil
	}
	self := c.lookup.m[td.Name].t
	for _, m := range res.Methods {
		kind := funcKind{method: !m.IsStatic && !m.IsConstructor, ctor: m.IsConstructor}
		if err := c.resolveFunc(m.Attrs, m.Func, kind, self); err != nil {
			return err
		}
		if err := define(resourceFuncName(td.Name, m), m.Pos); err != nil {
			return err
		}
	}
	return nil
}

// resourceFuncName is a resource func's canonical name: "[constructor]r",
// "[method]r.m", "[static]r.m", with [get] or [set] for an accessor.
func resourceFuncName(resource string, m ast.ResourceMethod) string {
	if m.IsConstructor {
		return "[constructor]" + resource
	}
	prefix := "[method]"
	if m.IsStatic {
		prefix = "[static]"
	}
	return prefix + accessorPrefix(m.Func.Accessor) + resource + "." + m.Name
}

// freestandingName is a freestanding func's name: its own, or "[get]x" or
// "[set]x" for an accessor.
func freestandingName(name string, acc ast.Accessor) string {
	return accessorPrefix(acc) + name
}

func accessorPrefix(acc ast.Accessor) string {
	return [...]string{"", "[get]", "[set]"}[acc]
}

// resolveTypes defines a scope's uses and type definitions, in an order
// where each type comes after those it names.
func (c *checker) resolveTypes(items []any) error {
	c.lookup = newTScope()
	for _, item := range items {
		if u, ok := item.(*ast.Use); ok {
			if err := c.resolveUse(u); err != nil {
				return err
			}
		}
	}
	var keys []string
	deps := map[string][]dep{}
	defs := map[string]*ast.TypeDef{}
	add := func(name string, td *ast.TypeDef, d []dep) {
		if _, ok := deps[name]; !ok {
			keys = append(keys, name)
		}
		defs[name], deps[name] = td, d
	}
	for _, item := range items {
		switch x := item.(type) {
		case *ast.TypeDef:
			if _, ok := defs[x.Name]; ok {
				return errAt(x.Pos, "name `%s` is defined more than once", x.Name)
			}
			add(x.Name, x, typeDeps(x.Type))
		case *ast.Use:
			for _, n := range x.Names {
				name, _ := n.Local()
				// A use after a type of the same name takes its place, as
				// in wit-parser, which drops the type unchecked.
				add(name, nil, nil)
			}
		}
	}
	order, err := toposort("type", keys, deps)
	if err != nil {
		return err
	}
	for _, name := range order {
		td := defs[name]
		if td == nil {
			continue
		}
		if err := checkAttrs(td.Attrs); err != nil {
			return err
		}
		t, err := c.typeDef(td)
		if err != nil {
			return err
		}
		// Unique: the defs are, and a use of the same name has replaced
		// a def before it or fails one after it.
		c.lookup.add(name, td.Pos, t)
	}
	return nil
}

// resolveUse defines the names a use brings in. Its path and attributes
// were checked with the package's other uses, in populateForeignTypes.
func (c *checker) resolveUse(u *ast.Use) error {
	it := c.item(u.Path)
	for _, n := range u.Names {
		e, ok := it.types.m[n.Name]
		switch {
		case !ok:
			return notFound("name", n.Name, n.Pos)
		case e.t == nil:
			return errAt(n.Pos, "cannot import function `%s`", n.Name)
		}
		name, pos := n.Local()
		if err := c.lookup.define(name, pos, &ctype{kind: "alias", target: e.t}); err != nil {
			return err
		}
	}
	return nil
}

// dep is a name a type or item refers to, and where.
type dep struct {
	name string
	pos  ast.Pos
}

// typeDeps lists the names a type definition refers to.
func typeDeps(body ast.TypeDefBody) []dep {
	var out []dep
	var walk func(t *ast.TypeRef)
	walk = func(t *ast.TypeRef) {
		if t == nil {
			return
		}
		switch t.Kind {
		case "named", "own", "borrow":
			out = append(out, dep{t.Name, t.Pos})
		}
		walk(t.Inner)
		walk(t.Inner2)
		for _, e := range t.Tuple {
			walk(e)
		}
	}
	switch b := body.(type) {
	case *ast.TypeAlias:
		walk(&b.Target)
	case *ast.Record:
		for i := range b.Fields {
			walk(&b.Fields[i].Type)
		}
	case *ast.Variant:
		for _, cs := range b.Cases {
			walk(cs.Type)
		}
	}
	return out
}

// toposort orders keys so each comes after its deps, as wit-parser's
// toposort does, failing on a dep that is not a key or on a cycle.
func toposort(kind string, keys []string, deps map[string][]dep) ([]string, error) {
	index := map[string]int{}
	for i, k := range keys {
		index[k] = i
	}
	remaining := make([]int, len(keys))
	reverse := make([][]int, len(keys))
	for i, k := range keys {
		remaining[i] = len(deps[k])
		for _, d := range deps[k] {
			j, ok := index[d.name]
			if !ok {
				hint := ""
				if to, ok := map[string]string{"float32": "f32", "float64": "f64"}[d.name]; ok && kind == "type" {
					hint = fmt.Sprintf("\nthe `%s` type has been renamed to `%s` and is no longer accepted, but the `WIT_REQUIRE_F32_F64=0` environment variable can be used to temporarily disable this error", d.name, to)
				}
				return nil, errAt(d.pos, "%s `%s` does not exist%s", kind, d.name, hint)
			}
			reverse[j] = append(reverse[j], i)
		}
	}
	var order []string
	ready := map[int]bool{}
	for i := range keys {
		if remaining[i] == 0 {
			ready[i] = true
		}
	}
	for len(ready) > 0 {
		next := len(keys)
		for i := range ready {
			next = min(next, i)
		}
		delete(ready, next)
		order = append(order, keys[next])
		for _, i := range reverse[next] {
			if remaining[i]--; remaining[i] == 0 {
				ready[i] = true
			}
		}
	}
	if len(order) == len(keys) {
		return order, nil
	}
	// A cycle: the first key left over has a dep left over too, since a dep
	// is left over exactly when it is still waiting on deps of its own.
	k := keys[slices.IndexFunc(remaining, func(r int) bool { return r > 0 })]
	d := deps[k][slices.IndexFunc(deps[k], func(d dep) bool { return remaining[index[d.name]] > 0 })]
	return nil, errAt(d.pos, "%s `%s` depends on itself", kind, d.name)
}

// typeDef checks a type definition's body.
func (c *checker) typeDef(td *ast.TypeDef) (*ctype, error) {
	switch b := td.Type.(type) {
	case *ast.Resource:
		return &ctype{kind: "resource"}, nil
	case *ast.TypeAlias:
		k, err := c.typeKind(&b.Target)
		if err == nil && k.kind == "prim" {
			// "type t = u32;" is a type of its own, not u32 itself.
			k = &ctype{kind: "named"}
		}
		return k, err
	case *ast.Record:
		if len(b.Fields) == 0 {
			return nil, errAt(td.Pos, "empty record")
		}
		names, pos := make([]string, len(b.Fields)), make([]ast.Pos, len(b.Fields))
		for i, f := range b.Fields {
			names[i], pos[i] = f.Name, f.Pos
		}
		if err := checkUnique("field", names, pos); err != nil {
			return nil, err
		}
		for i := range b.Fields {
			if _, err := c.resolveType(&b.Fields[i].Type); err != nil {
				return nil, err
			}
		}
	case *ast.Flags:
		switch {
		case len(b.Flags) == 0:
			return nil, errAt(td.Pos, "empty flags")
		case len(b.Flags) > 32:
			return nil, errAt(b.Pos[32], "cannot have more than 32 flags")
		}
		if err := checkUnique("flag", b.Flags, b.Pos); err != nil {
			return nil, err
		}
	case *ast.Variant:
		if len(b.Cases) == 0 {
			return nil, errAt(td.Pos, "empty variant")
		}
		names, pos := make([]string, len(b.Cases)), make([]ast.Pos, len(b.Cases))
		for i, cs := range b.Cases {
			names[i], pos[i] = cs.Name, cs.Pos
		}
		if err := checkUnique("case", names, pos); err != nil {
			return nil, err
		}
		for _, cs := range b.Cases {
			if cs.Type == nil {
				continue
			}
			if _, err := c.resolveType(cs.Type); err != nil {
				return nil, err
			}
		}
	case *ast.Enum:
		if len(b.Cases) == 0 {
			return nil, errAt(td.Pos, "empty enum")
		}
		if err := checkUnique("case", b.Cases, b.Pos); err != nil {
			return nil, err
		}
	}
	return &ctype{kind: "composite"}, nil
}

var mapKeys = map[string]bool{
	"bool": true, "u8": true, "u16": true, "u32": true, "u64": true,
	"s8": true, "s16": true, "s32": true, "s64": true, "char": true, "string": true,
}

// typeKind checks a type expression, as the definition it makes.
func (c *checker) typeKind(t *ast.TypeRef) (*ctype, error) {
	switch t.Kind {
	case "named":
		target, err := c.typeName(t.Name, t.Pos)
		return &ctype{kind: "alias", target: target}, err
	case "own", "borrow":
		target, err := c.validateResource(t.Name, t.Pos)
		return &ctype{kind: t.Kind, target: target}, err
	case "tuple":
		if len(t.Tuple) == 0 {
			return nil, errAt(t.Pos, "empty tuple")
		}
		for _, e := range t.Tuple {
			if _, err := c.resolveType(e); err != nil {
				return nil, err
			}
		}
		return &ctype{kind: "tuple"}, nil
	case "result":
		r := &ctype{kind: "result"}
		var err error
		if t.Inner != nil {
			r.ok, err = c.resolveType(t.Inner)
		}
		if err == nil && t.Inner2 != nil {
			_, err = c.resolveType(t.Inner2)
		}
		return r, err
	case "map":
		key, err := c.resolveType(t.Inner)
		if err != nil {
			return nil, err
		}
		if _, err := c.resolveType(t.Inner2); err != nil {
			return nil, err
		}
		if key.kind != "prim" || !mapKeys[key.target.kind] {
			return nil, errAt(t.Pos, "invalid map key type: map keys must be bool, u8, u16, u32, u64, s8, s16, s32, s64, char, or string")
		}
		return &ctype{kind: "map"}, nil
	}
	if t.Inner != nil { // list, option, future, stream
		if _, err := c.resolveType(t.Inner); err != nil {
			return nil, err
		}
		return &ctype{kind: t.Kind}, nil
	}
	if t.Kind == "future" || t.Kind == "stream" {
		return &ctype{kind: t.Kind}, nil
	}
	// A primitive: the prim's name rides in target.
	return &ctype{kind: "prim", target: &ctype{kind: t.Kind}}, nil
}

// resolveType checks a type expression where it is used, which names a
// type it does not itself define: a name or own<T> is that type.
func (c *checker) resolveType(t *ast.TypeRef) (*ctype, error) {
	k, err := c.typeKind(t)
	if err != nil {
		return nil, err
	}
	if k.kind == "alias" || k.kind == "own" {
		return k.target, nil
	}
	return k, nil
}

func (c *checker) typeName(name string, pos ast.Pos) (*ctype, error) {
	e, ok := c.lookup.m[name]
	switch {
	case !ok:
		return nil, notFound("name", name, pos)
	case e.t == nil:
		return nil, errAt(pos, "cannot use function `%s` as a type", name)
	}
	return e.t, nil
}

// validateResource is the type a handle names, which must be a resource,
// or an alias of one. One from another package is left to resolve.
func (c *checker) validateResource(name string, pos ast.Pos) (*ctype, error) {
	id, err := c.typeName(name, pos)
	if err != nil {
		return nil, err
	}
	for cur := id; ; cur = cur.target {
		switch cur.kind {
		case "resource", "unknown":
			return id, nil
		case "alias":
			continue
		}
		return nil, errAt(pos, "type `%s` used in a handle must be a resource", name)
	}
}

// funcKind is what a func's params and result depend on.
type funcKind struct{ method, ctor bool }

// resolveFunc checks a func's attributes, params and result. self is the
// resource of a resource's func.
func (c *checker) resolveFunc(attrs ast.Attrs, f ast.Func, kind funcKind, self *ctype) error {
	if err := checkAttrs(attrs); err != nil {
		return err
	}
	var seen []string
	if kind.method {
		seen = append(seen, "self")
	}
	switch {
	case f.Accessor == ast.Getter && len(f.Params) > 0:
		return errAt(f.Params[0].Pos, "getters cannot have parameters")
	case f.Accessor == ast.Setter && len(f.Params) != 1:
		pos := f.Pos
		if len(f.Params) > 1 {
			pos = f.Params[1].Pos
		}
		return errAt(pos, "setters must have exactly one parameter")
	}
	for i := range f.Params {
		p := &f.Params[i]
		for _, s := range seen {
			if kebabCanonical(s) == kebabCanonical(p.Name) {
				return errAt(p.Pos, "param `%s` is defined more than once", p.Name)
			}
		}
		seen = append(seen, p.Name)
		if _, err := c.resolveType(&p.Type); err != nil {
			return err
		}
	}
	if f.Result == nil {
		if f.Accessor == ast.Getter {
			return errAt(f.Pos, "getters must return a value")
		}
		return nil
	}
	r, err := c.resolveType(f.Result)
	switch {
	case err != nil:
		return err
	case f.Accessor == ast.Setter && (r.kind != "result" || r.ok != nil):
		return errAt(f.Result.Pos, "if a setter return type is declared it must be a `result` with no `ok` type")
	case !kind.ctor:
		return nil
	case r.kind != "result":
		return errAt(f.Result.Pos, "if a constructor return type is declared it must be a `result`")
	case r.ok != self:
		pos := f.Result.Pos
		if f.Result.Inner != nil {
			pos = f.Result.Inner.Pos
		}
		return errAt(pos, "the `ok` type must be the resource being constructed")
	}
	return nil
}

// worldKey is what a world's import or export is keyed by: a name, or an
// interface named by path.
type worldKey struct {
	name  string
	iface *citem
}

// resolveWorld checks a world's types, includes, imports and exports.
func (c *checker) resolveWorld(w *ast.World) error {
	if err := checkStability(w.Attrs); err != nil {
		return err
	}
	var typeItems []any
	for _, item := range w.Items {
		switch item.(type) {
		case *ast.Use, *ast.TypeDef:
			typeItems = append(typeItems, item)
		}
	}
	if err := c.resolveTypes(typeItems); err != nil {
		return err
	}
	for _, item := range w.Items {
		inc, ok := item.(*ast.Include)
		if !ok {
			continue
		}
		if err := checkStability(inc.Attrs); err != nil {
			return err
		}
		// populateItems has found every include's world.
		if !c.item(inc.Path).isWorld {
			return errAt(inc.PathPos, "name `%s` is defined as an interface, not a world", ast.LastSegment(inc.Path))
		}
	}
	scopes := map[string]*nameScope{"import": newNameScope(), "export": newNameScope()}
	kinds := map[string]map[worldKey]string{"import": {}, "export": {}}
	for _, name := range c.lookup.order {
		if err := scopes["import"].define(name, c.lookup.m[name].pos); err != nil {
			return err
		}
		kinds["import"][worldKey{name: name}] = "type"
	}
	for _, item := range w.Items {
		var ext ast.Extern
		var desc string
		switch x := item.(type) {
		case *ast.Import:
			ext, desc = x.Extern, "import"
		case *ast.Export:
			ext, desc = x.Extern, "export"
		case *ast.TypeDef:
			err := c.resolveResourceFuncs(x, func(name string, pos ast.Pos) error {
				kinds["import"][worldKey{name: name}] = "func"
				return scopes["import"].define(name, pos)
			})
			if err != nil {
				return err
			}
			continue
		default:
			continue
		}
		key, kind, err := c.resolveWorldItem(ext)
		if err != nil {
			return err
		}
		if prev, ok := kinds[desc][key]; ok {
			if key.iface != nil {
				return errAt(ext.Pos, "interface cannot be %sed more than once", desc)
			}
			return errAt(ext.Pos, "%s `%s` conflicts with prior %s of same name", desc, key.name, prev)
		}
		kinds[desc][key] = kind
		if key.iface == nil {
			if err := scopes[desc].define(key.name, ext.Pos); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveWorldItem checks one import or export, returning its key and what
// kind of item it is.
func (c *checker) resolveWorldItem(ext ast.Extern) (worldKey, string, error) {
	switch k := ext.Kind.(type) {
	case *ast.ExternInterface:
		outer := c.lookup
		err := c.resolveInterface(nil, k.Items, ext.Attrs)
		c.lookup = outer
		if err == nil && len(ext.ExternalID) > 1 {
			err = errAt(ext.ExternalID[1], "cannot specify @external-id twice")
		}
		return worldKey{name: ext.Name}, "interface", err
	case *ast.ExternFunc:
		name := freestandingName(ext.Name, k.Func.Accessor)
		return worldKey{name: name}, "func", c.resolveFunc(ext.Attrs, k.Func, funcKind{}, nil)
	case *ast.ExternNamedPath:
		if err := checkAttrs(ext.Attrs); err != nil {
			return worldKey{}, "", err
		}
		_, err := c.ifaceAt(k.Path, k.PathPos)
		return worldKey{name: ext.Name}, "interface", err
	}
	if err := checkAttrs(ext.Attrs); err != nil {
		return worldKey{}, "", err
	}
	it, err := c.ifaceAt(ext.Name, ext.Pos)
	return worldKey{iface: it}, "interface", err
}
