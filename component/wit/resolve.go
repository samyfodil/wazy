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

// resolver holds every package being resolved together: the root, its
// nested packages and its deps, any of which may name any other. check has
// already validated each package on its own; what is left is what depends
// on the other packages and on which feature gates are enabled, as
// wit-parser's Resolve leaves it.
type resolver struct {
	pkgs   []*pkgScope // each after the packages it names
	byName map[string]*pkgScope
	scopes []*scope // every interface and world scope, for validation
	funcs  []*Func  // every func, for validation
	cfg    config
	// offTypes are the other packages' types a package's uses take whose
	// first use's gate leaves them out: wit-parser gates such a type by its
	// first use, so through any use it is excluded.
	offTypes map[offType]bool
}

type offType struct {
	from        *pkgScope
	iface, name string
}

type pkgScope struct {
	name   string
	index  int // in resolver.pkgs
	raw    *rawPackage
	ifaces map[string]*scope // by short name
	worlds map[string]*worldState
	order  []*worldState
	pub    *Package
	// The interfaces and worlds a gate left out, so that a reference to
	// one fails as wit-parser fails it.
	offIfaces, offWorlds map[string]bool
}

// fileScope is one file of a package, whose top-level uses name items for
// that file alone.
type fileScope struct {
	pkg  *pkgScope
	uses map[string]string // local name -> path
}

// scope is the namespace one interface or world declares its types in.
type scope struct {
	res   *resolver
	file  *fileScope
	iface string // the interface's qualified name; "" for a world
	where string // for error messages
	types map[string]*ast.TypeDef
	order []string // type names in declaration order
	uses  map[string]useRef
	used  []string        // use aliases in declaration order
	off   map[string]bool // types and use aliases a gate left out
	pub   *Interface      // nil for a world
}

// useRef is one name a `use` brings into a scope: the interface it comes
// from, and its name there.
type useRef struct{ path, name string }

type worldState struct {
	file *fileScope
	ast  *ast.World
	pub  *World
	done bool
	// types are the world's types, as items of its own.
	types []worldType
	// keys are the names the world imports and exports items by -- its
	// types too, which are imports -- in the order wit-parser keeps them:
	// its own, then what each include brings in.
	keys [2][]string
}

func resolve(pkgs []*rawPackage, root string, cfg config) (*Package, error) {
	order, err := sortPackages(pkgs)
	if err != nil {
		return nil, err
	}
	r := &resolver{byName: map[string]*pkgScope{}, cfg: cfg, offTypes: map[offType]bool{}}
	for i, raw := range order {
		p := &pkgScope{
			name:   raw.name,
			index:  i,
			raw:    raw,
			ifaces: map[string]*scope{},
			worlds: map[string]*worldState{},
			pub:    &Package{Name: raw.name, short: map[string]*Interface{}},

			offIfaces: map[string]bool{},
			offWorlds: map[string]bool{},
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
		if f.fn.Result != nil && hasBorrow(f.scope, f.fn.Result) {
			return nil, fmt.Errorf("wit: function `%s` returns a type which contains a `borrow<T>` which is not supported", f.Name)
		}
	}
	rp := r.byName[root]
	rp.pub.Deps = sortedDeps(r.pkgs, rp)
	return rp.pub, nil
}

// sortPackages refuses a package defined twice and orders the packages so
// that each comes after those it names, failing on a cycle, as wit-parser
// does: visiting them in name order, and each one's deps in the order it
// first names them.
func sortPackages(pkgs []*rawPackage) ([]*rawPackage, error) {
	byName := map[string]*rawPackage{}
	for _, p := range pkgs {
		if prev := byName[p.name]; prev != nil {
			return nil, fmt.Errorf("wit: package `%s` is defined in two different locations:\n  * %s\n  * %s", p.name, p.pos, prev.pos)
		}
		byName[p.name] = p
	}
	sorted := slices.Clone(pkgs)
	sort.Slice(sorted, func(i, j int) bool { return comparePackageNames(sorted[i].name, sorted[j].name) < 0 })
	var order []*rawPackage
	done := map[*rawPackage]bool{}
	visiting := map[string]bool{}
	var visit func(p *rawPackage) error
	visit = func(p *rawPackage) error {
		if done[p] {
			return nil
		}
		for _, d := range p.deps {
			if visiting[d.name] {
				return errAt(d.pos, "package `%s` creates a dependency cycle", d.name)
			}
			visiting[d.name] = true
			if q := byName[d.name]; q != nil {
				if err := visit(q); err != nil {
					return err
				}
			}
			delete(visiting, d.name)
		}
		done[p] = true
		order = append(order, p)
		return nil
	}
	for _, p := range sorted {
		if err := visit(p); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// comparePackageNames orders "ns:name@ver" names as wit-parser's
// PackageName does: by namespace, name, then version, none first.
func comparePackageNames(a, b string) int {
	an, av, aok := strings.Cut(a, "@")
	bn, bv, bok := strings.Cut(b, "@")
	ans, aname, _ := strings.Cut(an, ":")
	bns, bname, _ := strings.Cut(bn, ":")
	if c := strings.Compare(ans, bns); c != 0 {
		return c
	}
	if c := strings.Compare(aname, bname); c != 0 {
		return c
	}
	// Not both without a version: they would be the same package.
	switch {
	case aok && bok:
		return parseSemver(av).compare(parseSemver(bv))
	case aok:
		return 1
	}
	return -1
}

func (r *resolver) declarePackage(p *pkgScope) error {
	if err := r.foreignDeps(p); err != nil {
		return err
	}
	for _, file := range p.raw.files {
		f := &fileScope{pkg: p, uses: map[string]string{}}
		for _, item := range file.Items {
			switch it := item.(type) {
			case *ast.ToplevelUse:
				name, _ := it.Local()
				f.uses[name] = it.Path
			case *ast.Interface:
				name := qualify(p.name, it.Name)
				on, err := r.enabled(p, it.Gate)
				if err != nil {
					return fmt.Errorf("wit: interface %s: %w", name, err)
				}
				if !on {
					p.offIfaces[it.Name] = true
					for _, item := range it.Items {
						if err := r.offItem(f, "interface "+name, "interface", item); err != nil {
							return err
						}
					}
					continue
				}
				s, err := r.newInterface(f, name, it.Items)
				if err != nil {
					return err
				}
				p.ifaces[it.Name] = s
				p.pub.short[it.Name] = s.pub
				p.pub.Interfaces = append(p.pub.Interfaces, s.pub)
			case *ast.World:
				on, err := r.enabled(p, it.Gate)
				if err != nil {
					return fmt.Errorf("wit: world %s: %w", it.Name, err)
				}
				if !on {
					p.offWorlds[it.Name] = true
					for _, item := range it.Items {
						if err := r.offItem(f, "world "+it.Name, "world", item); err != nil {
							return err
						}
					}
					continue
				}
				w := &worldState{file: f, ast: it, pub: &World{Name: it.Name}}
				p.worlds[it.Name] = w
				p.order = append(p.order, w)
				p.pub.Worlds = append(p.pub.Worlds, w.pub)
			}
		}
	}
	return nil
}

// foreignDeps checks what package p names in the packages before it, as
// wit-parser does first thing: every package it names must exist, and so
// must every interface and world that a place whose gate is enabled names,
// and every type such a use takes.
func (r *resolver) foreignDeps(p *pkgScope) error {
	// Interfaces, then worlds.
	for _, worlds := range []bool{false, true} {
		for _, it := range p.raw.foreign {
			if it.world != worlds {
				continue
			}
			pkg, err := r.pkg(it.pkg, p)
			if err != nil {
				return fmt.Errorf("wit: %w", err)
			}
			on := false
			for _, g := range it.gates {
				if on, err = r.enabled(p, g); err != nil {
					return fmt.Errorf("wit: %s/%s: %w", it.pkg, it.name, err)
				}
				if on {
					break
				}
			}
			switch {
			case !on:
			case worlds && pkg.worlds[it.name] == nil:
				return fmt.Errorf("wit: world '%s' not found in package '%s'", it.name, it.pkg)
			case !worlds && pkg.ifaces[it.name] == nil:
				return fmt.Errorf("wit: interface '%s' not found in package '%s'", it.name, it.pkg)
			}
		}
	}
	for _, t := range p.raw.foreignTypes {
		on, err := r.enabled(p, t.gate)
		if err != nil {
			return fmt.Errorf("wit: %s/%s.%s: %w", t.pkg, t.iface, t.name, err)
		}
		if !on {
			r.offTypes[offType{p, qualify(t.pkg, t.iface), t.name}] = true
			continue
		}
		// The interface is there: this use's gate is enabled, so it was
		// looked for above.
		s := r.byName[t.pkg].ifaces[t.iface]
		if s.types[t.name] == nil && !slices.Contains(s.used, t.name) {
			return errAt(t.pos, "type `%s` not defined in interface", t.name)
		}
	}
	return nil
}

func newScope(r *resolver, f *fileScope, iface, where string) *scope {
	s := &scope{
		res:   r,
		file:  f,
		iface: iface,
		where: where,
		types: map[string]*ast.TypeDef{},
		uses:  map[string]useRef{},
		off:   map[string]bool{},
	}
	r.scopes = append(r.scopes, s)
	return s
}

// newInterface declares an interface's types, uses and funcs.
func (r *resolver) newInterface(f *fileScope, name string, items []ast.InterfaceItem) (*scope, error) {
	s := newScope(r, f, name, "interface "+name)
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
			fname := freestandingName(it.Name, it.Func.Accessor)
			fn, err := s.newGatedFunc(fname, it.Gate, Freestanding, "", it.Func)
			if err != nil {
				return nil, err
			}
			if fn != nil {
				s.pub.Funcs = append(s.pub.Funcs, fn)
			}
		}
	}
	return s, nil
}

// on reports whether the item called name, behind gate g, is read in scope s.
func (s *scope) on(name string, g ast.Gate) (bool, error) {
	on, err := s.res.enabled(s.file.pkg, g)
	if err != nil {
		return false, fmt.Errorf("wit: %s: %s: %w", s.where, name, err)
	}
	return on, nil
}

// declareType adds a type to the scope, returning the funcs a resource
// declares (non-nil, possibly empty, for a resource). A type its gate leaves
// out only takes its name, so a reference to it fails as excluded; it
// returns no funcs.
func (s *scope) declareType(td *ast.TypeDef) ([]*Func, error) {
	on, err := s.on(td.Name, td.Gate)
	if err != nil {
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
		kind := Method
		switch {
		case m.IsConstructor:
			kind = Constructor
		case m.IsStatic:
			kind = Static
		}
		f, err := s.newGatedFunc(resourceFuncName(td.Name, m), m.Gate, kind, td.Name, m.Func)
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
		name := resourceFuncName(td.Name, m)
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
	for _, n := range u.Names {
		alias, _ := n.Local()
		// A use after a type of the same name takes its place, as in
		// wit-parser.
		if s.types[alias] != nil {
			delete(s.types, alias)
			s.order = slices.DeleteFunc(s.order, func(name string) bool { return name == alias })
		}
		s.off[alias] = !on
		if on {
			s.uses[alias] = useRef{path: u.Path, name: n.Name}
			s.used = append(s.used, alias)
		}
	}
	return nil
}

func (s *scope) newFunc(name string, kind FuncKind, resource string, fn ast.Func) *Func {
	f := &Func{Name: name, Interface: s.iface, Resource: resource, Kind: kind, Accessor: Accessor(fn.Accessor), scope: s, fn: fn}
	s.res.funcs = append(s.res.funcs, f)
	return f
}

// newGatedFunc is newFunc for a func behind gate g, or nil when the gate
// leaves it out.
func (s *scope) newGatedFunc(name string, g ast.Gate, kind FuncKind, resource string, fn ast.Func) (*Func, error) {
	on, err := s.on(name, g)
	if err != nil || !on {
		return nil, err
	}
	return s.newFunc(name, kind, resource, fn), nil
}

// world resolves a world: its own types, imports and exports, then what
// each include brings in, then the interfaces they need.
func (r *resolver) world(w *worldState) error {
	if w.done {
		return nil
	}
	w.done = true
	s := newScope(r, w.file, "", "world "+w.ast.Name)
	// wit-parser refuses a type or use of a world that its gate leaves out:
	// its world pass maps the type before it checks the gate.
	excluded := func(name string) error {
		return fmt.Errorf("wit: world %s: %s: %w", w.ast.Name, name, errExcluded("type"))
	}
	for _, item := range w.ast.Items {
		switch it := item.(type) {
		case *ast.TypeDef:
			funcs, err := s.declareType(it)
			if err == nil && s.off[it.Name] {
				err = excluded(it.Name)
			}
			if err != nil {
				return err
			}
			// A resource declared in a world is imported with the world's
			// own funcs, at the top level.
			for _, f := range funcs {
				w.pub.Imports = append(w.pub.Imports, WorldItem{Name: f.Name, Func: f})
			}
		case *ast.Use:
			if err := s.declareUse(it); err != nil {
				return err
			}
			// The use's names share its gate: one is left out when all are.
			if len(it.Names) > 0 {
				if name, _ := it.Names[0].Local(); s.off[name] {
					return excluded(name)
				}
			}
		}
	}
	for _, name := range append(slices.Clone(s.order), s.used...) {
		w.types = append(w.types, worldType{name, s})
		w.keys[0] = append(w.keys[0], name)
	}
	for _, it := range w.pub.Imports {
		w.keys[0] = append(w.keys[0], it.Name)
	}
	for _, item := range w.ast.Items {
		var err error
		switch it := item.(type) {
		case *ast.Import:
			err = r.worldItem(w, s, false, it.Extern)
		case *ast.Export:
			err = r.worldItem(w, s, true, it.Extern)
		}
		if err != nil {
			return err
		}
	}
	for _, item := range w.ast.Items {
		if inc, ok := item.(*ast.Include); ok {
			on, err := s.on("include "+inc.Path, inc.Gate)
			if err == nil && on {
				err = r.include(w, inc)
			}
			if err != nil {
				return err
			}
		}
	}
	if err := w.checkNames(); err != nil {
		return err
	}
	return r.elaborate(w, s)
}

// named says whether an item is keyed by its name in the world -- a func,
// or an interface written inline or under a label -- rather than by the
// interface it is, as an interface named by its path is.
func (it WorldItem) named() bool {
	return it.Func != nil || it.Interface.inline
}

func (w *worldState) list(export bool) *[]WorldItem {
	if export {
		return &w.pub.Exports
	}
	return &w.pub.Imports
}

func listIndex(export bool) int {
	if export {
		return 1
	}
	return 0
}

// worldItem resolves one import or export of the world itself.
func (r *resolver) worldItem(w *worldState, s *scope, export bool, ext ast.Extern) error {
	on, err := s.on(ext.Name, ext.Gate)
	if err != nil {
		return err
	}
	if !on {
		return r.offWorldItem(w.file, ext)
	}
	var item WorldItem
	switch k := ext.Kind.(type) {
	case *ast.ExternFunc:
		f := &Func{Name: freestandingName(ext.Name, k.Func.Accessor), Kind: Freestanding, Accessor: Accessor(k.Func.Accessor), scope: s, fn: k.Func}
		r.funcs = append(r.funcs, f)
		item = WorldItem{Name: f.Name, Func: f}
	case *ast.ExternInterface:
		ifc, err := r.newInterface(w.file, ext.Name, k.Items)
		if err != nil {
			return err
		}
		ifc.pub.inline = true
		item = WorldItem{Name: ext.Name, Interface: ifc.pub}
	case *ast.ExternNamedPath:
		ifc, err := r.iface(w.file, k.Path)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		item = WorldItem{Name: ext.Name, Interface: r.renameIface(ifc.pub, ext.Name), Implements: ifc.pub.Name}
	default:
		ifc, err := r.iface(w.file, ext.Name)
		if err != nil {
			return fmt.Errorf("wit: world %s: %w", w.ast.Name, err)
		}
		item = WorldItem{Name: ifc.pub.Name, Interface: ifc.pub}
	}
	list := w.list(export)
	*list = append(*list, item)
	if item.named() {
		w.keys[listIndex(export)] = append(w.keys[listIndex(export)], item.Name)
	}
	return nil
}

// add adds an item an include brings in: an interface the world already
// has is merged, and any other name already there is shadowed, which
// wit-parser refuses.
func (w *worldState) add(export bool, item WorldItem) error {
	list, i := w.list(export), listIndex(export)
	if !item.named() {
		for _, have := range *list {
			if have.Interface == item.Interface {
				return nil
			}
		}
		*list = append(*list, item)
		return nil
	}
	if err := w.shadows(export, item.Name); err != nil {
		return err
	}
	*list = append(*list, item)
	w.keys[i] = append(w.keys[i], item.Name)
	return nil
}

func (w *worldState) shadows(export bool, name string) error {
	if slices.Contains(w.keys[listIndex(export)], name) {
		verb := "import"
		if export {
			verb = "export"
		}
		return fmt.Errorf("wit: world %s: %s of `%s` shadows previously %sed items", w.ast.Name, verb, name, verb)
	}
	return nil
}

// checkNames checks what includes may have broken: the names of the
// world's imports, and of its exports, must be strongly unique, and each
// setter needs a getter of the same name.
func (w *worldState) checkNames() error {
	for i, verb := range []string{"import", "export"} {
		seen := map[string]string{}
		for _, name := range w.keys[i] {
			c := canonicalName(name)
			if prev, ok := seen[c]; ok {
				return fmt.Errorf("wit: world %s: %s `%s` in world `%s` conflicts with %s `%s` (names must be strongly unique)", w.ast.Name, verb, name, w.ast.Name, verb, prev)
			}
			seen[c] = name
		}
		for _, it := range *w.list(i == 1) {
			if it.Func == nil || it.Func.Accessor != Setter {
				continue
			}
			if g := getterFor(it.Name); !slices.Contains(w.keys[i], g) {
				return fmt.Errorf("wit: world %s: %s `%s` in world `%s` has no corresponding getter `%s`", w.ast.Name, verb, it.Name, w.ast.Name, g)
			}
		}
	}
	return nil
}

// elaborate adds the interfaces a world's interfaces need, as wit-parser
// elaborates a world. An interface that uses another's types needs that one
// too, and gets it right before itself: an imported interface's dependencies
// are imported, and so are those the world's own uses name, ahead of
// everything else. An exported interface's dependencies are exported when
// the world exports them and imported when not -- and none may need to be
// both.
func (r *resolver) elaborate(w *worldState, s *scope) error {
	// An interface under a label keeps saying what it implements.
	implements := map[*Interface]string{}
	for _, it := range slices.Concat(w.pub.Imports, w.pub.Exports) {
		if it.Implements != "" {
			implements[it.Interface] = it.Implements
		}
	}
	itemFor := func(i *Interface) WorldItem {
		return WorldItem{Name: i.Name, Interface: i, Implements: implements[i]}
	}
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
		imports = append(imports, itemFor(i))
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
			exports = append(exports, itemFor(i))
		default:
			required[i] = true
			if !imported[i] {
				imported[i] = true
				imports = append(imports, itemFor(i))
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
		t, err := r.iface(s.file, u.path)
		if err != nil {
			return nil, fmt.Errorf("wit: %s: use %s: %w", s.where, u.path, errExcluded("type"))
		}
		if !seen[t.pub] {
			seen[t.pub] = true
			out = append(out, t.pub)
		}
	}
	return out, nil
}

func (r *resolver) include(w *worldState, inc *ast.Include) error {
	target, err := r.lookupWorld(w.file, inc.Path)
	if err != nil {
		return fmt.Errorf("wit: world %s: include %s: %w", w.ast.Name, inc.Path, err)
	}
	if err := r.world(target); err != nil {
		return err
	}
	rn := &renamer{r: r, to: map[string]string{}, types: map[*scope]map[string]string{}, clones: map[*scope]*scope{}}
	for _, n := range inc.Renames {
		if _, ok := rn.to[n.Name]; !ok {
			rn.to[n.Name] = n.As
		}
	}
	// A rename that names nothing the world has fails before anything is
	// brought in.
	names := slices.Concat(target.keys[0], target.keys[1])
	for _, n := range inc.Renames {
		if !slices.ContainsFunc(names, func(name string) bool { return renamesName(name, n.Name) }) {
			return fmt.Errorf("wit: world %s: include %s: no import or export kebab-name `%s`. Note that an ID does not support renaming", w.ast.Name, inc.Path, n.Name)
		}
	}
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
		if err := w.shadows(false, types[i].name); err != nil {
			return err
		}
		w.keys[0] = append(w.keys[0], types[i].name)
	}
	w.types = append(w.types, types...)
	for _, export := range []bool{false, true} {
		for _, item := range *target.list(export) {
			if err := w.add(export, rn.item(item)); err != nil {
				return err
			}
		}
	}
	return nil
}

// renamesName says whether the include rename of from applies to name, as
// wit-parser's rename decides it.
func renamesName(name, from string) bool {
	rest := name[strings.LastIndexByte(name, ']')+1:]
	key, _, _ := strings.Cut(rest, ".")
	return key == from
}

// worldType is a type a world has as an item of its own: one it declares or
// uses, or one an include brought in, with the scope that names it.
type worldType struct {
	name string
	s    *scope
}

// renamer applies one include's "with { a as b }" to what it brings in.
type renamer struct {
	r  *resolver
	to map[string]string
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
	if dotted {
		return prefix + to + "." + tail, true
	}
	return prefix + to, true
}

// item renames one item an include brings in. A func is renamed itself, or
// moved to the copy of its scope that renames its types (and its resource);
// an interface written inline in the world, or under a label, is renamed
// with everything it declares. An interface named by its path keeps it:
// the name is not the world's to give.
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
			return WorldItem{Name: name, Interface: rn.r.renameIface(item.Interface, name), Implements: item.Implements}
		}
	}
	return item
}

// copyScope copies a scope's names under a new interface name.
func (r *resolver) copyScope(sc *scope, iface, where string) *scope {
	c := newScope(r, sc.file, iface, where)
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

// renameIface is a copy of interface i, its funcs and its resources, under
// a new name -- which is also the name its resources' tags are keyed by --
// as the world names it: inline, or under a label.
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

// iface finds the interface a use or a world names by path from file f: a
// bare name is an interface of f's package, or one a top-level use of f
// brought in; "ns:pkg/name@ver" is an interface of that package.
func (r *resolver) iface(f *fileScope, path string) (*scope, error) {
	if target, ok := f.uses[path]; ok {
		path = target
	}
	// Another package's interface is there: what an enabled reference
	// names was looked for in foreignDeps.
	if pkgName, name, foreign := splitForeign(path); foreign {
		return r.byName[pkgName].ifaces[name], nil
	}
	if s := f.pkg.ifaces[path]; s != nil {
		return s, nil
	}
	return nil, fmt.Errorf("%s: %w", path, errExcluded("interface"))
}

// lookupWorld is iface for a world an include names.
func (r *resolver) lookupWorld(f *fileScope, path string) (*worldState, error) {
	if target, ok := f.uses[path]; ok {
		path = target
	}
	if pkgName, name, foreign := splitForeign(path); foreign {
		return r.byName[pkgName].worlds[name], nil
	}
	if w := f.pkg.worlds[path]; w != nil {
		return w, nil
	}
	return nil, errExcluded("world")
}

// pkg finds a package by name, as package from names it.
func (r *resolver) pkg(name string, from *pkgScope) (*pkgScope, error) {
	if p := r.byName[name]; p != nil {
		return p, nil
	}
	// wit-parser knows the packages resolved so far, and the one at hand.
	var b strings.Builder
	fmt.Fprintf(&b, "package '%s' not found. known packages:", name)
	for _, p := range r.pkgs[:from.index+1] {
		fmt.Fprintf(&b, "\n    %s", p.name)
	}
	return nil, errors.New(b.String())
}

// qualify names interface iface of package pkg as a component imports it:
// "ns:pkg/iface@ver".
func qualify(pkg, iface string) string {
	base, ver, hasVer := strings.Cut(pkg, "@")
	if hasVer {
		return base + "/" + iface + "@" + ver
	}
	return base + "/" + iface
}

// lookupResource is lookupType through any type definitions that alias the
// resource ("type r2 = r;"), to the resource itself.
func (s *scope) lookupResource(name string) (*scope, *ast.TypeDef, error) {
	ds, td, err := s.lookupType(name)
	for err == nil {
		alias, ok := td.Type.(*ast.TypeAlias)
		if !ok || alias.Target.Kind != "named" {
			break
		}
		ds, td, err = ds.lookupType(alias.Target.Name)
	}
	return ds, td, err
}

// lookupType finds where the type called name in s is defined, following
// uses across interfaces. A name check has seen and that is not there is
// one a gate left out.
func (s *scope) lookupType(name string) (*scope, *ast.TypeDef, error) {
	for {
		if td := s.types[name]; td != nil {
			return s, td, nil
		}
		u, ok := s.uses[name]
		if !ok {
			return nil, nil, fmt.Errorf("%s: %s: %w", s.where, name, errExcluded("type"))
		}
		t, err := s.res.iface(s.file, u.path)
		if err != nil || s.res.offTypes[offType{s.file.pkg, t.iface, u.name}] {
			// What a use names in an interface a gate left out is either
			// left out too or already an error there, as an ungated type.
			return nil, nil, fmt.Errorf("%s: use %s: %w", s.where, u.path, errExcluded("type"))
		}
		s, name = t, u.name
	}
}

// hasBorrow says whether type t, written in scope s, contains a borrow
// handle, which a func may not return.
func hasBorrow(s *scope, t *ast.TypeRef) bool {
	switch t.Kind {
	case "borrow":
		return true
	case "named":
		// funcDesc has found every name the func's types use.
		ds, td, _ := s.lookupType(t.Name)
		switch b := td.Type.(type) {
		case *ast.TypeAlias:
			return hasBorrow(ds, &b.Target)
		case *ast.Record:
			return slices.ContainsFunc(b.Fields, func(f ast.RecordField) bool { return hasBorrow(ds, &f.Type) })
		case *ast.Variant:
			return slices.ContainsFunc(b.Cases, func(c ast.VariantCase) bool { return c.Type != nil && hasBorrow(ds, c.Type) })
		}
		return false
	}
	for _, inner := range append([]*ast.TypeRef{t.Inner, t.Inner2}, t.Tuple...) {
		if inner != nil && hasBorrow(s, inner) {
			return true
		}
	}
	return false
}

// sortedDeps returns every package but root, in name order.
func sortedDeps(pkgs []*pkgScope, root *pkgScope) []*Package {
	out := make([]*Package, 0, len(pkgs)-1)
	for _, p := range pkgs {
		if p != root {
			out = append(out, p.pub)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
