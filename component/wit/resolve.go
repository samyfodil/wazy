package wit

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// resolver holds every package being resolved together: the root and its
// deps, any of which may name any other.
type resolver struct {
	pkgs   []*pkgScope // the root first
	byName map[string]*pkgScope
	scopes []*scope // every interface and world scope, for validation
	funcs  []*Func  // every func, for validation
	cfg    config
}

type pkgScope struct {
	name   string
	raw    *rawPackage
	ifaces map[string]*scope // by short name
	worlds map[string]*worldState
	order  []*worldState
	uses   map[string]string // package-level use: local name -> interface path
	pub    *Package
	// The interfaces, worlds and package-level uses a gate left out, so that
	// a reference to one fails as wit-parser fails it rather than as unknown.
	offIfaces, offWorlds, offUses map[string]bool
}

// scope is the namespace one interface or world declares its types in.
type scope struct {
	res   *resolver
	pkg   *pkgScope
	iface string // the interface's qualified name; "" for a world
	where string // for error messages
	types map[string]*ast.TypeDef
	order []string // type names in declaration order
	uses  map[string]useRef
	used  []string // use aliases in declaration order
	names map[string]bool
	off   map[string]bool // types and use aliases a gate left out
	pub   *Interface      // nil for a world
}

// useRef is one name a `use` brings into a scope: the interface it comes
// from, and its name there.
type useRef struct{ path, name string }

type worldState struct {
	pkg   *pkgScope
	ast   *ast.World
	pub   *World
	state int // 0 unresolved, 1 resolving, 2 resolved
	// types are the world's types, as items of its own.
	types []worldType
	// named is every interface the world itself imports or exports by name,
	// keyed by whether it exports it: one world may not name an interface
	// twice, though what it includes may bring one in again.
	named map[namedKey]bool
}

type namedKey struct {
	iface  *Interface
	export bool
}

func resolve(root *rawPackage, deps []*rawPackage, cfg config) (*Package, error) {
	r := &resolver{byName: map[string]*pkgScope{}, cfg: cfg}
	for _, raw := range append([]*rawPackage{root}, deps...) {
		if r.byName[raw.name] != nil {
			return nil, fmt.Errorf("wit: package %s is defined more than once", raw.name)
		}
		p := &pkgScope{
			name:   raw.name,
			raw:    raw,
			ifaces: map[string]*scope{},
			worlds: map[string]*worldState{},
			uses:   map[string]string{},
			pub:    &Package{Name: raw.name, short: map[string]*Interface{}},

			offIfaces: map[string]bool{},
			offWorlds: map[string]bool{},
			offUses:   map[string]bool{},
		}
		r.byName[raw.name] = p
		r.pkgs = append(r.pkgs, p)
	}
	// Every package's interfaces first, so a world or a use in any package
	// can name an interface in any other.
	for _, p := range r.pkgs {
		if err := r.declarePackage(p); err != nil {
			return nil, err
		}
	}
	for _, p := range r.pkgs {
		for _, w := range p.order {
			if err := r.world(w); err != nil {
				return nil, err
			}
		}
	}
	// Build every type and func once, so a bad name fails here rather than
	// at the first Signature that happens to reach it.
	for _, s := range r.scopes {
		for _, alias := range s.used {
			if _, _, err := s.lookupType(alias); err != nil {
				return nil, fmt.Errorf("wit: %w", err)
			}
		}
		for _, name := range s.order {
			if _, err := newBuilder(nil, true).named(s, name); err != nil {
				return nil, fmt.Errorf("wit: %w", err)
			}
		}
	}
	for _, f := range r.funcs {
		if _, err := newBuilder(nil, true).funcDesc(f); err != nil {
			return nil, fmt.Errorf("wit: %s: %w", f.where(), err)
		}
	}
	r.pkgs[0].pub.Deps = sortedDeps(r.pkgs[1:])
	return r.pkgs[0].pub, nil
}

func (r *resolver) declarePackage(p *pkgScope) error {
	for _, item := range p.raw.items {
		switch it := item.(type) {
		case *ast.Interface:
			if p.ifaces[it.Name] != nil || p.offIfaces[it.Name] {
				return fmt.Errorf("wit: package %s: interface %s is defined more than once", p.name, it.Name)
			}
			name := qualify(p.name, it.Name)
			on, err := r.enabled(p, it.Gate)
			if err != nil {
				return fmt.Errorf("wit: interface %s: %w", name, err)
			}
			if !on {
				p.offIfaces[it.Name] = true
				for _, item := range it.Items {
					if err := r.offItem(p, "interface "+name, "interface", item); err != nil {
						return err
					}
				}
				continue
			}
			s, err := r.newInterface(p, name, it.Items)
			if err != nil {
				return err
			}
			p.ifaces[it.Name] = s
			p.pub.short[it.Name] = s.pub
			p.pub.Interfaces = append(p.pub.Interfaces, s.pub)
		case *ast.World:
			if p.worlds[it.Name] != nil || p.offWorlds[it.Name] {
				return fmt.Errorf("wit: package %s: world %s is defined more than once", p.name, it.Name)
			}
			on, err := r.enabled(p, it.Gate)
			if err != nil {
				return fmt.Errorf("wit: world %s: %w", it.Name, err)
			}
			if !on {
				p.offWorlds[it.Name] = true
				for _, item := range it.Items {
					if err := r.offItem(p, "world "+it.Name, "world", item); err != nil {
						return err
					}
				}
				continue
			}
			w := &worldState{pkg: p, ast: it, pub: &World{Name: it.Name}}
			p.worlds[it.Name] = w
			p.order = append(p.order, w)
			p.pub.Worlds = append(p.pub.Worlds, w.pub)
		case *ast.Use:
			// "use a:b/c;" names interface c locally; "use a:b/c as d;" names
			// it d. The parser records the second as Names[d] = path.
			on, err := r.enabled(p, it.Gate)
			if err != nil {
				return fmt.Errorf("wit: package %s: use %s: %w", p.name, it.Path, err)
			}
			var aliases []string
			if len(it.Names) == 0 {
				aliases = append(aliases, lastSegment(it.Path))
			}
			for alias, target := range it.Names {
				if target != it.Path {
					return fmt.Errorf("wit: package %s: use %s.{...} names types, which only an interface or world can", p.name, it.Path)
				}
				aliases = append(aliases, alias)
			}
			for _, alias := range aliases {
				if on {
					p.uses[alias] = it.Path
				} else {
					p.offUses[alias] = true
				}
			}
		case *ast.TypeDef:
			return fmt.Errorf("wit: package %s: type %s is outside any interface or world", p.name, it.Name)
		}
	}
	return nil
}

func newScope(r *resolver, p *pkgScope, iface, where string) *scope {
	s := &scope{
		res:   r,
		pkg:   p,
		iface: iface,
		where: where,
		types: map[string]*ast.TypeDef{},
		uses:  map[string]useRef{},
		names: map[string]bool{},
		off:   map[string]bool{},
	}
	r.scopes = append(r.scopes, s)
	return s
}

// newInterface declares an interface's types, uses and funcs.
func (r *resolver) newInterface(p *pkgScope, name string, items []ast.InterfaceItem) (*scope, error) {
	s := newScope(r, p, name, "interface "+name)
	s.pub = &Interface{Name: name, scope: s}
	for _, item := range items {
		switch it := item.(type) {
		case *ast.TypeDef:
			funcs, err := s.declareType(it)
			if err != nil {
				return nil, err
			}
			if funcs != nil {
				s.pub.Resources = append(s.pub.Resources, &Resource{Interface: name, Name: it.Name, Funcs: funcs})
			}
			s.pub.Funcs = append(s.pub.Funcs, funcs...)
		case *ast.Use:
			if err := s.declareUse(it); err != nil {
				return nil, err
			}
		case *ast.InterfaceFunc:
			f, err := s.newGatedFunc(it.Name, it.Gate, Freestanding, "", it.Func)
			if err != nil {
				return nil, err
			}
			if f != nil {
				s.pub.Funcs = append(s.pub.Funcs, f)
			}
		}
	}
	return s, nil
}

// on reports whether the item called name, behind gate g, is read in scope s.
func (s *scope) on(name string, g ast.Gate) (bool, error) {
	on, err := s.res.enabled(s.pkg, g)
	if err != nil {
		return false, fmt.Errorf("wit: %s: %s: %w", s.where, name, err)
	}
	return on, nil
}

func (s *scope) declare(name string) error {
	if s.names[name] {
		return fmt.Errorf("wit: %s: %s is defined more than once", s.where, name)
	}
	s.names[name] = true
	return nil
}

// declareType adds a type to the scope, returning the funcs a resource
// declares (non-nil, possibly empty, for a resource). A type its gate leaves
// out only takes its name, so a duplicate still fails and a reference to it
// fails as excluded; it returns no funcs.
func (s *scope) declareType(td *ast.TypeDef) ([]*Func, error) {
	on, err := s.on(td.Name, td.Gate)
	if err != nil {
		return nil, err
	}
	if err := s.declare(td.Name); err != nil {
		return nil, err
	}
	if !on {
		s.off[td.Name] = true
		return nil, s.offResource(td)
	}
	s.types[td.Name] = td
	s.order = append(s.order, td.Name)
	res, ok := td.Type.(*ast.Resource)
	if !ok {
		return nil, nil
	}
	funcs := []*Func{}
	for _, m := range res.Methods {
		kind, name := Method, "[method]"+td.Name+"."+m.Name
		switch {
		case m.IsConstructor:
			kind, name = Constructor, "[constructor]"+td.Name
		case m.IsStatic:
			kind, name = Static, "[static]"+td.Name+"."+m.Name
		}
		f, err := s.newGatedFunc(name, m.Gate, kind, td.Name, m.Func)
		if err != nil {
			return nil, err
		}
		if f != nil {
			funcs = append(funcs, f)
		}
	}
	return funcs, nil
}

// offResource checks the funcs of a resource its gate left out: each needs
// the resource, so one its own gate keeps refers to a type that is gone.
func (s *scope) offResource(td *ast.TypeDef) error {
	res, ok := td.Type.(*ast.Resource)
	if !ok {
		return nil
	}
	for _, m := range res.Methods {
		name := td.Name + "." + methodName(m)
		on, err := s.on(name, m.Gate)
		if err != nil {
			return err
		}
		if on {
			return fmt.Errorf("wit: %s: %s: %w", s.where, name, errExcluded("type"))
		}
	}
	return nil
}

func (s *scope) declareUse(u *ast.Use) error {
	on, err := s.on("use "+u.Path, u.Gate)
	if err != nil {
		return err
	}
	// Sorted so a duplicate is reported the same way every run.
	aliases := make([]string, 0, len(u.Names))
	for alias := range u.Names {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if err := s.declare(alias); err != nil {
			return err
		}
		if !on {
			s.off[alias] = true
			continue
		}
		s.uses[alias] = useRef{path: u.Path, name: u.Names[alias]}
		s.used = append(s.used, alias)
	}
	return nil
}

func (s *scope) newFunc(name string, kind FuncKind, resource string, fn ast.Func) (*Func, error) {
	if err := s.declare(name); err != nil {
		return nil, err
	}
	f := &Func{Name: name, Interface: s.iface, Resource: resource, Kind: kind, scope: s, fn: fn}
	s.res.funcs = append(s.res.funcs, f)
	return f, nil
}

// newGatedFunc is newFunc for a func behind gate g. One the gate leaves out
// only takes its name, so a duplicate still fails, and is nil.
func (s *scope) newGatedFunc(name string, g ast.Gate, kind FuncKind, resource string, fn ast.Func) (*Func, error) {
	on, err := s.on(name, g)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, s.declare(name)
	}
	return s.newFunc(name, kind, resource, fn)
}

// world resolves a world's imports and exports, expanding includes.
func (r *resolver) world(w *worldState) error {
	switch w.state {
	case 2:
		return nil
	case 1:
		return fmt.Errorf("wit: world %s includes itself", w.ast.Name)
	}
	w.state = 1
	s := newScope(r, w.pkg, "", "world "+w.ast.Name)
	// Types and uses first: an import or export may name any of them.
	for _, item := range w.ast.Items {
		switch it := item.(type) {
		case *ast.TypeDef:
			funcs, err := s.declareType(it)
			if err != nil {
				return err
			}
			// A resource declared in a world is imported with the world's
			// own funcs, at the top level. newFunc has already refused a
			// duplicate name.
			for _, f := range funcs {
				w.pub.Imports = append(w.pub.Imports, WorldItem{Name: f.Name, Func: f})
			}
		case *ast.Use:
			if err := s.declareUse(it); err != nil {
				return err
			}
		}
	}
	for _, name := range append(slices.Clone(s.order), s.used...) {
		w.types = append(w.types, worldType{name, s})
	}
	for _, item := range w.ast.Items {
		var err error
		switch it := item.(type) {
		case *ast.Import:
			err = r.worldItem(w, s, &w.pub.Imports, it.Name, it.Gate, importParts(it.Type))
		case *ast.Export:
			err = r.worldItem(w, s, &w.pub.Exports, it.Name, it.Gate, exportParts(it.Type))
		case *ast.Include:
			var on bool
			if on, err = s.on("include "+it.Path, it.Gate); on {
				err = r.include(w, it)
			}
		}
		if err != nil {
			return err
		}
	}
	if err := r.elaborate(w, s); err != nil {
		return err
	}
	w.state = 2
	return nil
}

// worldFunc declares a func a world imports or exports. An import shares the
// world's namespace with its types; an export has a namespace of its own, so
// it may share an import's name, as in wit-parser. add refuses a duplicate
// within either list.
func (s *scope) worldFunc(name string, export bool, fn ast.Func) (*Func, error) {
	if !export {
		return s.newFunc(name, Freestanding, "", fn)
	}
	f := &Func{Name: name, Kind: Freestanding, scope: s, fn: fn}
	s.res.funcs = append(s.res.funcs, f)
	return f, nil
}

// elaborate adds the interfaces a world's interfaces need, as wit-parser
// elaborates a world. An interface that uses another's types needs that one
// too, and gets it right before itself: an imported interface's dependencies
// are imported, and so are those the world's own uses name, ahead of
// everything else. An exported interface's dependencies are exported when
// the world exports them and imported when not -- and none may need to be
// both.
func (r *resolver) elaborate(w *worldState, s *scope) error {
	var imports []WorldItem
	imported := map[*Interface]bool{}
	var addImport func(i *Interface) error
	addImport = func(i *Interface) error {
		if imported[i] {
			return nil
		}
		imported[i] = true
		deps, err := r.deps(i.scope)
		if err != nil {
			return err
		}
		for _, d := range deps {
			if err := addImport(d); err != nil {
				return err
			}
		}
		imports = append(imports, WorldItem{Name: i.Name, Interface: i})
		return nil
	}
	uses, err := r.deps(s)
	if err != nil {
		return err
	}
	for _, d := range uses {
		if err := addImport(d); err != nil {
			return err
		}
	}
	for _, it := range w.pub.Imports {
		if it.Interface == nil {
			imports = append(imports, it)
		} else if err := addImport(it.Interface); err != nil {
			return err
		}
	}

	declared := map[*Interface]bool{}
	for _, it := range w.pub.Exports {
		if it.Interface != nil {
			declared[it.Interface] = true
		}
	}
	var exports []WorldItem
	exported, required := map[*Interface]bool{}, map[*Interface]bool{}
	// addExport places i, as an export or else as an import, after its
	// dependencies; false means it would have to be both.
	var addExport func(i *Interface, export bool) (bool, error)
	addExport = func(i *Interface, export bool) (bool, error) {
		switch {
		case exported[i]:
			return export, nil
		case !export && required[i]:
			return true, nil
		}
		deps, err := r.deps(i.scope)
		if err != nil {
			return false, err
		}
		for _, d := range deps {
			if ok, err := addExport(d, export && declared[d]); !ok || err != nil {
				return ok, err
			}
		}
		switch {
		case export && required[i]:
			return false, nil
		case export:
			exported[i] = true
			exports = append(exports, WorldItem{Name: i.Name, Interface: i})
		default:
			required[i] = true
			if !imported[i] {
				imported[i] = true
				imports = append(imports, WorldItem{Name: i.Name, Interface: i})
			}
		}
		return true, nil
	}
	for _, it := range w.pub.Exports {
		if it.Interface == nil {
			exports = append(exports, it)
			continue
		}
		ok, err := addExport(it.Interface, true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("wit: world %s: interface `%s` transitively depends on an interface in incompatible ways", w.ast.Name, it.Name)
		}
	}
	w.pub.Imports, w.pub.Exports = imports, exports
	return nil
}

// deps lists the interfaces whose types scope s uses, in the order its uses
// name them.
func (r *resolver) deps(s *scope) ([]*Interface, error) {
	var out []*Interface
	seen := map[*Interface]bool{}
	for _, alias := range s.used {
		u := s.uses[alias]
		t, err := r.iface(s.pkg, u.path)
		if err != nil {
			return nil, fmt.Errorf("wit: %s: use %s: %w", s.where, u.path, err)
		}
		if !seen[t.pub] {
			seen[t.pub] = true
			out = append(out, t.pub)
		}
	}
	return out, nil
}

// itemParts is what an import or export declares: a func, an inline
// interface, or (neither) an interface named by path.
type itemParts struct {
	fn     *ast.Func
	inline []ast.InterfaceItem
	isIfc  bool
}

func importParts(t ast.ImportType) itemParts {
	switch t := t.(type) {
	case *ast.ImportFunc:
		return itemParts{fn: &t.Func}
	case *ast.ImportInterface:
		return itemParts{inline: t.Items, isIfc: true}
	}
	return itemParts{}
}

func exportParts(t ast.ExportType) itemParts {
	switch t := t.(type) {
	case *ast.ExportFunc:
		return itemParts{fn: &t.Func}
	case *ast.ExportInterface:
		return itemParts{inline: t.Items, isIfc: true}
	}
	return itemParts{}
}

func (r *resolver) worldItem(w *worldState, s *scope, list *[]WorldItem, name string, g ast.Gate, parts itemParts) error {
	on, err := s.on(name, g)
	if err != nil {
		return err
	}
	if !on {
		for _, item := range parts.inline {
			if err := r.offItem(w.pkg, "interface "+name, "interface", item); err != nil {
				return err
			}
		}
		return nil
	}
	switch {
	case parts.fn != nil:
		f, err := s.worldFunc(name, list == &w.pub.Exports, *parts.fn)
		if err != nil {
			return err
		}
		return w.add(list, WorldItem{Name: name, Func: f})
	case parts.isIfc:
		ifc, err := r.newInterface(w.pkg, name, parts.inline)
		if err != nil {
			return err
		}
		ifc.pub.inline = true
		return w.add(list, WorldItem{Name: name, Interface: ifc.pub})
	default:
		ifc, err := r.iface(w.pkg, name)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		key := namedKey{ifc.pub, list == &w.pub.Exports}
		if w.named[key] {
			verb := "imported"
			if key.export {
				verb = "exported"
			}
			return fmt.Errorf("wit: world %s: %s: interface cannot be %s more than once", w.ast.Name, ifc.pub.Name, verb)
		}
		if w.named == nil {
			w.named = map[namedKey]bool{}
		}
		w.named[key] = true
		return w.add(list, WorldItem{Name: ifc.pub.Name, Interface: ifc.pub})
	}
}

// add appends item unless the same interface is already listed under its
// name, which is how a world and a world it includes may both import one.
func (w *worldState) add(list *[]WorldItem, item WorldItem) error {
	for _, have := range *list {
		if have.Name != item.Name {
			continue
		}
		if have.Interface != nil && have.Interface == item.Interface {
			return nil
		}
		return fmt.Errorf("wit: world %s: %s is declared more than once", w.ast.Name, item.Name)
	}
	*list = append(*list, item)
	return nil
}

func (r *resolver) include(w *worldState, inc *ast.Include) error {
	p, name := w.pkg, inc.Path
	if strings.Contains(inc.Path, ":") {
		pkgName, n, err := splitPath(inc.Path)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		if p, err = r.pkg(pkgName); err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		name = n
	}
	target := p.worlds[name]
	switch {
	case target == nil && p == w.pkg && p.offWorlds[name]:
		return fmt.Errorf("wit: world %s: include %s: %w", w.ast.Name, inc.Path, errExcluded("world"))
	case target == nil:
		return fmt.Errorf("wit: world %s: unknown world %s", w.ast.Name, inc.Path)
	}
	if err := r.world(target); err != nil {
		return err
	}
	rn := &renamer{r: r, to: inc.Renames, used: map[string]bool{}, types: map[*scope]map[string]string{}, clones: map[*scope]*scope{}}
	// Types first: renaming one of a scope's types makes a copy of the
	// scope, and the funcs declared in it move to the copy.
	types := slices.Clone(target.types)
	for i, t := range types {
		if to, ok := rn.rename(t.name); ok {
			if rn.types[t.s] == nil {
				rn.types[t.s] = map[string]string{}
				rn.order = append(rn.order, t.s)
			}
			rn.types[t.s][t.name] = to
			types[i].name = to
		}
	}
	for _, sc := range rn.order {
		rn.clones[sc] = r.renameTypes(sc, rn.types[sc])
	}
	for i, t := range types {
		if c := rn.clones[t.s]; c != nil {
			types[i].s = c
		}
	}
	w.types = append(w.types, types...)
	for _, from := range []struct {
		src []WorldItem
		dst *[]WorldItem
	}{{target.pub.Imports, &w.pub.Imports}, {target.pub.Exports, &w.pub.Exports}} {
		for _, item := range from.src {
			if err := w.add(from.dst, rn.item(item)); err != nil {
				return err
			}
		}
	}
	names := make([]string, 0, len(inc.Renames))
	for name := range inc.Renames {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !rn.used[name] {
			return fmt.Errorf("wit: world %s: include %s: no import or export kebab-name `%s`. Note that an ID does not support renaming", w.ast.Name, inc.Path, name)
		}
	}
	return nil
}

// worldType is a type a world has as an item of its own: one it declares or
// uses, or one an include brought in, with the scope that names it.
type worldType struct {
	name string
	s    *scope
}

// renamer applies one include's "with { a as b }" to what it brings in.
type renamer struct {
	r    *resolver
	to   map[string]string
	used map[string]bool
	// types are the renamed types of each scope, old name to new, and
	// clones the copy of the scope that has them; order is the order the
	// scopes came up in.
	types  map[*scope]map[string]string
	clones map[*scope]*scope
	order  []*scope
}

// rename applies the renames to one world item name, as wit-parser does: the
// whole name, after any "[...]" prefix, or the resource part of a resource
// func's ("[method]r.m", with r as s, is "[method]s.m").
func (rn *renamer) rename(name string) (string, bool) {
	i := strings.LastIndexByte(name, ']') + 1
	prefix, rest := name[:i], name[i:]
	key, tail, dotted := strings.Cut(rest, ".")
	to, ok := rn.to[key]
	if !ok {
		return name, false
	}
	rn.used[key] = true
	if dotted {
		return prefix + to + "." + tail, true
	}
	return prefix + to, true
}

// item renames one item an include brings in. A func is renamed itself, or
// moved to the copy of its scope that renames its types (and its resource);
// an interface written inline in the world is renamed with everything it
// declares. An interface named by its path keeps it: the name is not the
// world's to give.
func (rn *renamer) item(item WorldItem) WorldItem {
	switch {
	case item.Func != nil:
		f := item.Func
		name, renamed := rn.rename(f.Name)
		c := rn.clones[f.scope]
		if !renamed && c == nil {
			return item
		}
		nf := *f
		nf.Name = name
		if c != nil {
			nf.scope = c
			if to, ok := rn.types[f.scope][f.Resource]; ok {
				nf.Resource = to
			}
		}
		rn.r.funcs = append(rn.r.funcs, &nf)
		return WorldItem{Name: nf.Name, Func: &nf}
	case item.Interface.inline:
		if name, ok := rn.rename(item.Name); ok {
			return WorldItem{Name: name, Interface: rn.r.renameIface(item.Interface, name)}
		}
	}
	return item
}

// copyScope copies a scope's names under a new interface name.
func (r *resolver) copyScope(sc *scope, iface, where string) *scope {
	c := newScope(r, sc.pkg, iface, where)
	maps.Copy(c.types, sc.types)
	maps.Copy(c.uses, sc.uses)
	maps.Copy(c.off, sc.off)
	c.order, c.used = slices.Clone(sc.order), slices.Clone(sc.used)
	return c
}

// renameTypes is a copy of world scope sc with the types m names renamed:
// each is there under its new name, and under its old one too, so that the
// scope's other types and funcs, which name it the old way, find it renamed.
func (r *resolver) renameTypes(sc *scope, m map[string]string) *scope {
	c := r.copyScope(sc, sc.iface, sc.where)
	for from, to := range m {
		if td := sc.types[from]; td != nil {
			renamed := *td
			renamed.Name = to
			// Every name it goes by, including one an earlier include
			// renamed it from.
			for name, have := range sc.types {
				if have == td {
					c.types[name] = &renamed
				}
			}
			c.types[to] = &renamed
			c.order[slices.Index(c.order, from)] = to
			continue
		}
		c.uses[to] = sc.uses[from]
		c.used[slices.Index(c.used, from)] = to
	}
	return c
}

// renameIface is a copy of inline interface i, its funcs and its resources,
// under a new name -- which is also the name its resources' tags are keyed
// by.
func (r *resolver) renameIface(i *Interface, name string) *Interface {
	c := r.copyScope(i.scope, name, "interface "+name)
	ni := &Interface{Name: name, scope: c, inline: true}
	c.pub = ni
	moved := map[*Func]*Func{}
	for _, f := range i.Funcs {
		nf := *f
		nf.Interface, nf.scope = name, c
		moved[f] = &nf
		r.funcs = append(r.funcs, &nf)
		ni.Funcs = append(ni.Funcs, &nf)
	}
	for _, res := range i.Resources {
		nr := &Resource{Interface: name, Name: res.Name}
		for _, f := range res.Funcs {
			nr.Funcs = append(nr.Funcs, moved[f])
		}
		ni.Resources = append(ni.Resources, nr)
	}
	return ni
}

// iface finds the interface a use or a world names by path, relative to
// package p: a bare name is an interface of p, or one a package-level use
// brought in; "ns:pkg/name@ver" is an interface of that package.
func (r *resolver) iface(p *pkgScope, path string) (*scope, error) {
	if !strings.Contains(path, ":") {
		if p.ifaces[path] == nil && !p.offIfaces[path] {
			target, ok := p.uses[path]
			switch {
			case !ok && p.offUses[path]:
				return nil, fmt.Errorf("%s: %w", path, errExcluded("interface"))
			case !ok:
				return nil, fmt.Errorf("unknown interface %s", path)
			}
			path = target
		}
		if !strings.Contains(path, ":") {
			return p.lookupIface(path, path, true)
		}
	}
	pkgName, name, err := splitPath(path)
	if err != nil {
		return nil, err
	}
	pkg, err := r.pkg(pkgName)
	if err != nil {
		return nil, err
	}
	return pkg.lookupIface(name, path, pkg == p)
}

// lookupIface finds interface name of p; path is how it was asked for, and
// local says whether from p itself (see excludedError).
func (p *pkgScope) lookupIface(name, path string, local bool) (*scope, error) {
	switch {
	case p.ifaces[name] != nil:
		return p.ifaces[name], nil
	case local && p.offIfaces[name]:
		return nil, fmt.Errorf("%s: %w", path, errExcluded("interface"))
	}
	return nil, fmt.Errorf("unknown interface %s", path)
}

// pkg finds a package by name. A name without a version matches the one
// package of that name, whatever its version.
func (r *resolver) pkg(name string) (*pkgScope, error) {
	if p := r.byName[name]; p != nil {
		return p, nil
	}
	if !strings.Contains(name, "@") {
		var found *pkgScope
		for _, p := range r.pkgs {
			if base, _, _ := strings.Cut(p.name, "@"); base == name {
				if found != nil {
					return nil, fmt.Errorf("package %s is ambiguous: both %s and %s are present", name, found.name, p.name)
				}
				found = p
			}
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, fmt.Errorf("package %s not found (is it in deps/?)", name)
}

// splitPath splits "ns:pkg/name@ver" into the package "ns:pkg@ver" and the
// item "name".
func splitPath(path string) (pkg, name string, err error) {
	ns, rest, ok := strings.Cut(path, "/")
	if !ok || rest == "" {
		return "", "", fmt.Errorf("malformed path %s: want ns:pkg/name", path)
	}
	name, ver, hasVer := strings.Cut(rest, "@")
	if hasVer {
		return ns + "@" + ver, name, nil
	}
	return ns, name, nil
}

// qualify names interface iface of package pkg as a component imports it:
// "ns:pkg/iface@ver".
func qualify(pkg, iface string) string {
	if pkg == "" {
		return iface
	}
	base, ver, hasVer := strings.Cut(pkg, "@")
	if hasVer {
		return base + "/" + iface + "@" + ver
	}
	return base + "/" + iface
}

// lastSegment is the interface name in "ns:pkg/name@ver".
func lastSegment(path string) string {
	name, _, _ := strings.Cut(path[strings.LastIndex(path, "/")+1:], "@")
	return name
}

// lookupResource is lookupType through any type definitions that alias the
// resource ("type r2 = r;"), to the resource itself.
func (s *scope) lookupResource(name string) (*scope, *ast.TypeDef, error) {
	ds, td, err := s.lookupType(name)
	seen := map[*ast.TypeDef]bool{}
	for err == nil {
		alias, ok := td.Type.(*ast.TypeAlias)
		if !ok || alias.Target.Kind != "named" {
			break
		}
		if seen[td] {
			return nil, nil, fmt.Errorf("%s: type %s refers to itself", ds.where, td.Name)
		}
		seen[td] = true
		ds, td, err = ds.lookupType(alias.Target.Name)
	}
	return ds, td, err
}

// lookupType finds where the type called name in s is defined, following
// uses across interfaces.
func (s *scope) lookupType(name string) (*scope, *ast.TypeDef, error) {
	type key struct {
		s    *scope
		name string
	}
	seen := map[key]bool{}
	for {
		if td := s.types[name]; td != nil {
			return s, td, nil
		}
		u, ok := s.uses[name]
		switch {
		case !ok && s.off[name]:
			return nil, nil, fmt.Errorf("%s: %s: %w", s.where, name, errExcluded("type"))
		case !ok:
			return nil, nil, fmt.Errorf("%s: unknown type %s", s.where, name)
		}
		k := key{s, name}
		if seen[k] {
			return nil, nil, fmt.Errorf("%s: use of %s is circular", s.where, name)
		}
		seen[k] = true
		t, err := s.res.iface(s.pkg, u.path)
		var ex *excludedError
		switch {
		case errors.As(err, &ex):
			// What a use names in an interface a gate left out is either
			// left out too or already an error there, as an ungated type.
			return nil, nil, fmt.Errorf("%s: use %s: %w", s.where, u.path, errExcluded("type"))
		case err != nil:
			return nil, nil, fmt.Errorf("%s: use %s: %w", s.where, u.path, err)
		}
		s, name = t, u.name
	}
}
