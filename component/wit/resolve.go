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
	s.pub = &Interface{Name: name}
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
	w.state = 2
	return nil
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
		f, err := s.newFunc(name, Freestanding, "", *parts.fn)
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
