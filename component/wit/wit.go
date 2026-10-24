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
// # Validation
//
// A source is read as wit-parser, the Bytecode Alliance's reference WIT
// resolver, reads it, and fails where it fails, with its message: syntax
// errors (with their file:line:col), names that are not strongly unique or
// not defined, cycles among types, interfaces, worlds or packages, handles
// of what is not a resource, ill-formed records, variants, enums, flags,
// tuples and maps, constructor and accessor signatures, returned borrows,
// and the conflicts an include brings in. Every file needs a package header;
// a file may also hold nested "package ns:name { ... }" blocks, each a
// package of its own. A top-level use names an item for its own file, and a
// package is named with its exact version.
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
// An accessor ("size: get() -> u32;", "size: set(v: u32);") is a Func
// named "[get]size" or "[set]size", a resource's "[method][get]r.size" and
// so on, with its Accessor set. An import or export under a label of its
// own ("import primary: store;") is the interface it implements, copied
// under the label, as WorldItem.Implements says.
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
// A type or use of a world that its gate leaves out is an error, though, as
// in wit-parser, whose world pass maps the type before it checks the gate.
package wit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/samyfodil/wazy/component"
	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// Package is one WIT package with every name in it resolved.
type Package struct {
	// Name is the package name as declared, e.g. "acme:api@1.0.0".
	Name string
	// Interfaces are the package's interfaces in declaration order.
	Interfaces []*Interface
	// Worlds are the package's worlds in declaration order.
	Worlds []*World
	// Deps are the other packages read with it, sorted by name: those the
	// deps/ directory holds, and those written as nested
	// "package ns:name { ... }" blocks in its files or theirs.
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

// Accessor says whether a Func is a property's getter or setter.
type Accessor int

const (
	// NotAccessor is a func of its own.
	NotAccessor Accessor = iota
	// Getter is "name: get() -> T", named "[get]name" -- or for a resource,
	// a "[method][get]r.name" or "[static][get]r.name".
	Getter
	// Setter is "name: set(value: T)", named "[set]name" and so on. A world
	// or interface with a setter has the getter of the same name too.
	Setter
)

// Func is one WIT func.
type Func struct {
	// Name is the canonical import name: "lookup", "[constructor]counter",
	// "[method]counter.bump" or "[static]counter.zero", with a [get] or
	// [set] after any of those for an accessor ("[get]size",
	// "[method][set]counter.value").
	Name string
	// Interface is the qualified name of the declaring interface, or "" for a
	// func declared directly in a world.
	Interface string
	// Resource is the resource a Method, Static or Constructor belongs to.
	Resource string
	Kind     FuncKind
	Accessor Accessor

	scope *scope
	fn    ast.Func
}

// World is a WIT world: the imports and exports of one kind of component.
type World struct {
	// Name is the world's name as declared, e.g. "app".
	Name string
	// Imports and Exports are the world's own, in declaration order, then
	// what each include brings in -- renamed as its "with { a as b }" says, which
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
	// interface's qualified name, or a func's, inline interface's or
	// label's own name.
	Name string
	// Exactly one of Interface and Func is set.
	Interface *Interface
	Func      *Func
	// Implements is, for an interface under a label of its own
	// ("import primary: store;"), the qualified name of the interface it
	// implements. Its Interface is that one's copy, named by the label as an
	// inline interface is.
	Implements string
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

// Parse reads one WIT source: a package, with any nested
// "package ns:name { ... }" blocks, which become its Deps. Names resolve
// within it. filename is used only in error messages.
func Parse(filename, src string, opts ...Option) (*Package, error) {
	main, nested, err := loadSources([]source{{filename, src}})
	if err != nil {
		return nil, err
	}
	return resolve(append(nested, main), main.name, newConfig(opts))
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
	main, pkgs, err := loadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	pkgs = append(pkgs, main)
	// Stat first: on Windows, reading a file as a directory reports it as not existing.
	deps := path.Join(dir, "deps")
	var entries []fs.DirEntry
	if fi, err := fs.Stat(fsys, deps); err == nil {
		if !fi.IsDir() {
			return nil, fmt.Errorf("wit: %s is not a directory", deps)
		}
		if entries, err = fs.ReadDir(fsys, deps); err != nil {
			return nil, fmt.Errorf("wit: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("wit: %w", err)
	}
	for _, e := range entries {
		p := path.Join(dir, "deps", e.Name())
		var dep *rawPackage
		var nested []*rawPackage
		switch {
		case e.IsDir():
			dep, nested, err = loadDir(fsys, p)
		case strings.HasSuffix(e.Name(), ".wit"):
			src, rerr := fs.ReadFile(fsys, p)
			if rerr != nil {
				return nil, fmt.Errorf("wit: %w", rerr)
			}
			dep, nested, err = loadSources([]source{{p, string(src)}})
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		pkgs = append(append(pkgs, nested...), dep)
	}
	return resolve(pkgs, main.name, newConfig(opts))
}

// rawPackage is one package's parsed files, before any name is resolved.
type rawPackage struct {
	name  string
	pos   ast.Pos // where the name is declared
	files []*ast.Package
	deps  []dep // the other packages it names, as first named
	// What it names in other packages, in the order it first names them.
	foreign      []*foreignItem
	foreignTypes []foreignType
}

// source is one WIT file.
type source struct{ name, text string }

// loadDir reads every .wit file directly in dir as one package.
func loadDir(fsys fs.FS, dir string) (*rawPackage, []*rawPackage, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("wit: %w", err)
	}
	var srcs []source
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wit") {
			continue
		}
		p := path.Join(dir, e.Name())
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, nil, fmt.Errorf("wit: %w", err)
		}
		srcs = append(srcs, source{p, string(src)})
	}
	return loadSources(srcs)
}

// loadSources parses the files of one package, in name order, as
// wit-parser does: each file's nested packages are packages of their own,
// checked as the file is read, and the files that name the package must
// agree on it.
func loadSources(srcs []source) (*rawPackage, []*rawPackage, error) {
	main := &rawPackage{}
	docs := false
	var nested []*rawPackage
	for _, src := range srcs {
		f, err := ast.Parse(src.name, src.text)
		if err != nil {
			return nil, nil, fmt.Errorf("wit: %w", err)
		}
		for _, n := range f.Nested {
			if len(n.Nested) > 0 {
				return nil, nil, errAt(n.Nested[0].NamePos, "nested packages must be placed at the top-level")
			}
			p := &rawPackage{name: n.Name, pos: n.NamePos, files: []*ast.Package{n}}
			if err := check(p); err != nil {
				return nil, nil, err
			}
			nested = append(nested, p)
		}
		if f.Name != "" {
			if main.name != "" && f.Name != main.name {
				return nil, nil, errAt(f.NamePos, "package identifier `%s` does not match previous package name of `%s`", f.Name, main.name)
			}
			main.name, main.pos = f.Name, f.NamePos
			if f.Docs && docs {
				return nil, nil, errAt(f.NamePos, "found doc comments on multiple 'package' items")
			}
			docs = docs || f.Docs
		}
		main.files = append(main.files, f)
	}
	if main.name == "" {
		return nil, nil, errors.New("wit: no `package` header was found in any WIT file for this package")
	}
	return main, nested, check(main)
}
