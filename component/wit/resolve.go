package wit

import (
	"fmt"
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
}

type pkgScope struct {
	name   string
	raw    *rawPackage
	ifaces map[string]*scope // by short name
	worlds map[string]*worldState
	order  []*worldState
	uses   map[string]string // package-level use: local name -> interface path
	pub    *Package
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
	pub   *Interface // nil for a world
}

// useRef is one name a `use` brings into a scope: the interface it comes
// from, and its name there.
type useRef struct{ path, name string }

type worldState struct {
	pkg   *pkgScope
	ast   *ast.World
	pub   *World
	state int // 0 unresolved, 1 resolving, 2 resolved
	// named is every interface the world itself imports or exports by name,
	// keyed by whether it exports it: one world may not name an interface
	// twice, though what it includes may bring one in again.
	named map[namedKey]bool
}

type namedKey struct {
	iface  *Interface
	export bool
}

func resolve(root *rawPackage, deps []*rawPackage) (*Package, error) {
	r := &resolver{byName: map[string]*pkgScope{}}
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
			if p.ifaces[it.Name] != nil {
				return fmt.Errorf("wit: package %s: interface %s is defined more than once", p.name, it.Name)
			}
			s, err := r.newInterface(p, qualify(p.name, it.Name), it.Items)
			if err != nil {
				return err
			}
			p.ifaces[it.Name] = s
			p.pub.short[it.Name] = s.pub
			p.pub.Interfaces = append(p.pub.Interfaces, s.pub)
		case *ast.World:
			if p.worlds[it.Name] != nil {
				return fmt.Errorf("wit: package %s: world %s is defined more than once", p.name, it.Name)
			}
			w := &worldState{pkg: p, ast: it, pub: &World{Name: it.Name}}
			p.worlds[it.Name] = w
			p.order = append(p.order, w)
			p.pub.Worlds = append(p.pub.Worlds, w.pub)
		case *ast.Use:
			// "use a:b/c;" names interface c locally; "use a:b/c as d;" names
			// it d. The parser records the second as Names[d] = path.
			if len(it.Names) == 0 {
				p.uses[lastSegment(it.Path)] = it.Path
			}
			for alias, target := range it.Names {
				if target != it.Path {
					return fmt.Errorf("wit: package %s: use %s.{...} names types, which only an interface or world can", p.name, it.Path)
				}
				p.uses[alias] = it.Path
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
			f, err := s.newFunc(it.Name, Freestanding, "", it.Func)
			if err != nil {
				return nil, err
			}
			s.pub.Funcs = append(s.pub.Funcs, f)
		}
	}
	return s, nil
}

func (s *scope) declare(name string) error {
	if s.names[name] {
		return fmt.Errorf("wit: %s: %s is defined more than once", s.where, name)
	}
	s.names[name] = true
	return nil
}

// declareType adds a type to the scope, returning the funcs a resource
// declares (non-nil, possibly empty, for a resource).
func (s *scope) declareType(td *ast.TypeDef) ([]*Func, error) {
	if err := s.declare(td.Name); err != nil {
		return nil, err
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
		f, err := s.newFunc(name, kind, td.Name, m.Func)
		if err != nil {
			return nil, err
		}
		funcs = append(funcs, f)
	}
	return funcs, nil
}

func (s *scope) declareUse(u *ast.Use) error {
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
	for _, item := range w.ast.Items {
		var err error
		switch it := item.(type) {
		case *ast.Import:
			err = r.worldItem(w, s, &w.pub.Imports, it.Name, importParts(it.Type))
		case *ast.Export:
			err = r.worldItem(w, s, &w.pub.Exports, it.Name, exportParts(it.Type))
		case *ast.Include:
			err = r.include(w, it)
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

func (r *resolver) worldItem(w *worldState, s *scope, list *[]WorldItem, name string, parts itemParts) error {
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
	var target *worldState
	if !strings.Contains(inc.Path, ":") {
		target = w.pkg.worlds[inc.Path]
	} else {
		pkgName, name, err := splitPath(inc.Path)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		p, err := r.pkg(pkgName)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		target = p.worlds[name]
	}
	if target == nil {
		return fmt.Errorf("wit: world %s: unknown world %s", w.ast.Name, inc.Path)
	}
	if err := r.world(target); err != nil {
		return err
	}
	for _, from := range []struct {
		src []WorldItem
		dst *[]WorldItem
	}{{target.pub.Imports, &w.pub.Imports}, {target.pub.Exports, &w.pub.Exports}} {
		for _, item := range from.src {
			if alias, ok := inc.Renames[item.Name]; ok {
				if item.Func == nil {
					return fmt.Errorf("wit: world %s: include %s: renaming interface %s is not supported", w.ast.Name, inc.Path, item.Name)
				}
				f := *item.Func
				f.Name = alias
				item = WorldItem{Name: alias, Func: &f}
			}
			if err := w.add(from.dst, item); err != nil {
				return err
			}
		}
	}
	return nil
}

// iface finds the interface a use or a world names by path, relative to
// package p: a bare name is an interface of p, or one a package-level use
// brought in; "ns:pkg/name@ver" is an interface of that package.
func (r *resolver) iface(p *pkgScope, path string) (*scope, error) {
	if !strings.Contains(path, ":") {
		if s := p.ifaces[path]; s != nil {
			return s, nil
		}
		target, ok := p.uses[path]
		if !ok {
			return nil, fmt.Errorf("unknown interface %s", path)
		}
		path = target
		if !strings.Contains(path, ":") {
			if s := p.ifaces[path]; s != nil {
				return s, nil
			}
			return nil, fmt.Errorf("unknown interface %s", path)
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
	if s := pkg.ifaces[name]; s != nil {
		return s, nil
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
		if !ok {
			return nil, nil, fmt.Errorf("%s: unknown type %s", s.where, name)
		}
		k := key{s, name}
		if seen[k] {
			return nil, nil, fmt.Errorf("%s: use of %s is circular", s.where, name)
		}
		seen[k] = true
		t, err := s.res.iface(s.pkg, u.path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: use %s: %w", s.where, u.path, err)
		}
		s, name = t, u.name
	}
}
