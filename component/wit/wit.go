// Package wit reads WIT -- the WebAssembly Interface Type language -- into
// the descriptors component.WithImportCustom takes, so a host can register an
// interface from its .wit file instead of building each signature by hand.
//
// It needs no wazy.Runtime: parsing is pure and touches no engine, so a code
// generator or a linter can use it on its own.
//
//	pkg, err := wit.ParseDir("wit") // wit/*.wit plus wit/deps/
//	if err != nil {
//		return err
//	}
//	w := pkg.World("app")
//	for _, imp := range w.Imports {
//		fmt.Println("import", imp.Name)
//	}
//	for _, exp := range w.Exports {
//		fmt.Println("export", exp.Name)
//	}
//
// and to register an import from it:
//
//	host := pkg.Interface("acme:api/host@1.0.0")
//	sig, err := host.Func("lookup").Signature(nil)
//	if err != nil {
//		return err
//	}
//	opt := component.WithImportCustom(host.Name, "lookup", fn, sig.Desc, sig.Types.Resolver())
//
// # Resources
//
// own<R> and borrow<R> name a resource by the tag the host mints its handles
// under, which is the host's choice. Signature takes those tags keyed the way
// component.WithResourceTag is -- the interface that declares the resource,
// and the resource's name -- so a descriptor and the tag registration agree:
//
//	tags := wit.Tags{{Interface: host.Name, Name: "counter"}: counterTag}
//	sig, err := host.Func("[method]counter.bump").Signature(tags)
//	opt := component.WithResourceTag(host.Name, "counter", counterTag)
//
// # Names
//
// A Signature carries every name a code generator needs alongside the types:
// FuncDesc params keep their WIT names, the descriptors in the TypeTable keep
// record field, variant, enum and flags case names, and Signature.TypeNames
// names each table slot that holds a named WIT type. A resource's
// constructor, methods and static funcs are grouped under Interface.Resources.
//
// # Feature gates
//
// Gates apply as wit-parser applies them. An item marked @since or
// @deprecated is always read; one marked @unstable(feature = x) is read only
// when x is enabled with WithFeatures, or every feature is with
// WithAllFeatures. By default none is, so a parse sees what a toolchain
// targeting the stable API sees:
//
//	pkg, err := wit.ParseDir("wit", wit.WithFeatures("cli-exit-with-code"))
//
// An item left out is gone everywhere: from its interface or world, from its
// resource, from a use that named it and from an include that would have
// brought it in. Anything still read that refers to it is an error, as is a
// type kept by its own gate inside an interface or world that is left out.
// An @since gate needs a package version no earlier than its own.
//
// One difference from wit-parser 1.262.0: there, a type or use inside a world
// that its gate leaves out is an error ("found a reference to a type which is
// excluded"), because its world pass maps the type before checking its gate.
// Here it is left out like any other item.
package wit

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/samyfodil/wazy/component"
	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// Package is one WIT package with every name in it resolved.
type Package struct {
	// Name is the package name as declared, e.g. "acme:api@1.0.0". It is empty
	// for a source with no package declaration.
	Name string
	// Interfaces are the package's interfaces in declaration order.
	Interfaces []*Interface
	// Worlds are the package's worlds in declaration order.
	Worlds []*World
	// Deps are the packages read from the deps/ directory, sorted by name.
	// Parse and ParseFile read no deps.
	Deps []*Package

	short map[string]*Interface
}

// Interface returns the interface called name: either its short name within
// this package ("host") or its qualified name in this package or any of its
// Deps ("acme:api/host@1.0.0"). It returns nil when there is none.
func (p *Package) Interface(name string) *Interface {
	if i := p.short[name]; i != nil {
		return i
	}
	for _, pkg := range append([]*Package{p}, p.Deps...) {
		for _, i := range pkg.Interfaces {
			if i.Name == name {
				return i
			}
		}
	}
	return nil
}

// World returns the world called name in this package, or nil.
func (p *Package) World(name string) *World {
	for _, w := range p.Worlds {
		if w.Name == name {
			return w
		}
	}
	return nil
}

// Interface is a WIT interface: the unit a component imports or exports.
type Interface struct {
	// Name is what a component imports the interface by, and therefore the
	// iface argument of component.WithImportCustom and WithResourceTag:
	// "acme:api/host@1.0.0". An interface written inline in a world
	// ("import x: interface { ... }") is named by its import name, "x".
	Name string
	// Funcs are every func a component can import from the interface, in
	// declaration order, under its canonical import name: a freestanding
	// "lookup" as well as a resource's "[constructor]counter",
	// "[method]counter.bump" and "[static]counter.zero".
	Funcs []*Func
	// Resources are the resources the interface declares, in declaration
	// order.
	Resources []*Resource

	scope  *scope
	inline bool // written inline in a world, so named by the world
}

// Func returns the func with the canonical import name name, or nil.
func (i *Interface) Func(name string) *Func {
	for _, f := range i.Funcs {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Resource is a WIT resource and the funcs declared on it.
type Resource struct {
	// Interface is the qualified name of the declaring interface; with Name,
	// it is the key component.WithResourceTag and Tags use.
	Interface string
	Name      string
	// Funcs are the constructor, methods and static funcs in declaration
	// order. Each is also listed in the declaring Interface.Funcs.
	Funcs []*Func
}

// FuncKind says where a Func is declared.
type FuncKind int

const (
	// Freestanding is a func declared directly in an interface or world.
	Freestanding FuncKind = iota
	// Method is a resource method. Its signature starts with an implicit
	// self: borrow<R>.
	Method
	// Static is a resource's static func.
	Static
	// Constructor is a resource's constructor. Unless the WIT declares a
	// result, its signature returns own<R>.
	Constructor
)

// Func is one WIT func.
type Func struct {
	// Name is the canonical import name: "lookup", "[constructor]counter",
	// "[method]counter.bump" or "[static]counter.zero".
	Name string
	// Interface is the qualified name of the declaring interface, or "" for a
	// func declared directly in a world.
	Interface string
	// Resource is the resource a Method, Static or Constructor belongs to.
	Resource string
	Kind     FuncKind

	scope *scope
	fn    ast.Func
}

// World is a WIT world: the imports and exports of one kind of component.
type World struct {
	// Name is the world's name as declared, e.g. "app".
	Name string
	// Imports and Exports are in declaration order, with any include
	// expanded in place -- and renamed as its "with { a as b }" says, which
	// may rename a func, an interface written inline (with its funcs and
	// resources, and so the interface its resources' tags are keyed by), or
	// one of the world's types, a resource renaming its funcs too -- and
	// elaborated as wit-parser elaborates a world:
	// an interface that uses another's types brings that one in too, right
	// before itself -- as an import, or for an export that the world does
	// not export, also as an import. The interfaces the world's own uses
	// name come first.
	Imports []WorldItem
	Exports []WorldItem
}

// WorldItem is one import or export of a world: an interface or a func.
type WorldItem struct {
	// Name is the name the component imports or exports the item by: an
	// interface's qualified name, or a func's or inline interface's own name.
	Name string
	// Exactly one of Interface and Func is set.
	Interface *Interface
	Func      *Func
}

// ResourceKey identifies a resource the way component.WithResourceTag does:
// the qualified name of the interface that declares it, and its name.
type ResourceKey struct {
	Interface string
	Name      string
}

// Tags are the host's resource tags, by resource.
type Tags map[ResourceKey]uint32

// Signature is one func's descriptor, ready for component.WithImportCustom:
//
//	component.WithImportCustom(iface, f.Name, fn, sig.Desc, sig.Types.Resolver())
type Signature struct {
	Desc  component.FuncDesc
	Types *component.TypeTable
	// TypeNames maps each Types index holding a named WIT type -- a record,
	// variant, enum, flags, an alias to a composite, or a handle to a named
	// resource -- to that name. Anonymous types (list<u8>, option<T>, ...)
	// have none.
	TypeNames map[uint32]string
}

// Signature builds the func's descriptor, with every own and borrow of a
// resource carrying that resource's tag from tags. A resource reached by the
// signature that tags has no entry for is an error; a signature that reaches
// none needs no tags at all.
func (f *Func) Signature(tags Tags) (*Signature, error) {
	b := newBuilder(tags, false)
	fd, err := b.funcDesc(f)
	if err != nil {
		return nil, fmt.Errorf("wit: %s: %w", f.where(), err)
	}
	return &Signature{Desc: fd, Types: b.tbl, TypeNames: b.names}, nil
}

func (f *Func) where() string {
	if f.Interface == "" {
		return f.Name
	}
	return f.Interface + "#" + f.Name
}

// Parse reads one WIT source. Names resolve within it; a reference to
// another package is an error, since a lone source has no deps. filename is
// used only in error messages.
func Parse(filename, src string, opts ...Option) (*Package, error) {
	raw, err := parseSource(filename, src)
	if err != nil {
		return nil, err
	}
	return resolve(raw, nil, newConfig(opts))
}

// ParseFile reads the single WIT file at path, as Parse does.
func ParseFile(path string, opts ...Option) (*Package, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wit: %w", err)
	}
	return Parse(path, string(src), opts...)
}

// ParseDir reads the WIT package in dir -- every .wit file directly in it --
// together with dir/deps/, where each dependency is a directory of .wit files
// or a single .wit file. Uses resolve across all of them.
func ParseDir(dir string, opts ...Option) (*Package, error) {
	return ParseFS(os.DirFS(dir), ".", opts...)
}

// ParseFS is ParseDir over an fs.FS, such as an embed.FS.
func ParseFS(fsys fs.FS, dir string, opts ...Option) (*Package, error) {
	root, err := loadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, path.Join(dir, "deps"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("wit: %w", err)
	}
	var deps []*rawPackage
	for _, e := range entries {
		p := path.Join(dir, "deps", e.Name())
		var dep *rawPackage
		switch {
		case e.IsDir():
			dep, err = loadDir(fsys, p)
		case strings.HasSuffix(e.Name(), ".wit"):
			dep, err = loadFile(fsys, p)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if dep.name == "" {
			return nil, fmt.Errorf("wit: %s: no package declaration", p)
		}
		deps = append(deps, dep)
	}
	return resolve(root, deps, newConfig(opts))
}

// rawPackage is a package's parsed items, before any name is resolved.
type rawPackage struct {
	name  string
	items []ast.PackageItem
}

func parseSource(filename, src string) (*rawPackage, error) {
	pkg, err := ast.Parse(filename, src)
	if err != nil {
		return nil, fmt.Errorf("wit: %s: %w", filename, err)
	}
	return &rawPackage{name: pkg.Name, items: pkg.Items}, nil
}

func loadFile(fsys fs.FS, p string) (*rawPackage, error) {
	src, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, fmt.Errorf("wit: %w", err)
	}
	return parseSource(p, string(src))
}

// loadDir merges every .wit file directly in dir into one package. Files may
// omit the package declaration, but those that have one must agree.
func loadDir(fsys fs.FS, dir string) (*rawPackage, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("wit: %w", err)
	}
	var out *rawPackage
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wit") {
			continue
		}
		f, err := loadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		switch {
		case out == nil:
			out = f
			continue
		case out.name == "":
			out.name = f.name
		case f.name != "" && f.name != out.name:
			return nil, fmt.Errorf("wit: %s: files declare both package %s and %s", dir, out.name, f.name)
		}
		out.items = append(out.items, f.items...)
	}
	if out == nil {
		return nil, fmt.Errorf("wit: %s: no .wit files", dir)
	}
	if out.name == "" {
		return nil, fmt.Errorf("wit: %s: no package declaration", dir)
	}
	return out, nil
}

// sortedDeps returns the non-root packages in name order.
func sortedDeps(pkgs []*pkgScope) []*Package {
	out := make([]*Package, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.pub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
