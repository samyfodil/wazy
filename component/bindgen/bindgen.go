// Package bindgen generates Go bindings for a WIT world, so a host calls and
// implements a component's interfaces in Go types rather than in
// component.Values:
//
//   - a Go type for each record, variant, enum, flags and resource the world's
//     functions use;
//   - for each import, a Go interface the host implements, and the options
//     that register it;
//   - for the exports, a type wrapping the component.Instance, with a method
//     per exported function, and one making each stream and future type the
//     world passes.
//
// The generated code depends only on package component and package witgo.
// cmd/wazy-bindgen runs Generate from the command line, for go generate,
// on a WIT package or on a compiled component, whose WIT wit.FromComponent
// reads.
//
// A world's WASI imports are not bound by default: wazy's imports/wasip2
// implements them, and its options go to Instantiate next to the bindings.
//
// Bindings can share an imported interface's code rather than each holding
// a copy. GenerateTree writes each imported interface a package of its own,
// which holds nothing that depends on the world, so every world generated
// into the same tree shares it; and Options.With points an interface at such
// a package that already exists. Its Go names, its converters and its
// resource tags are the same in every world: it exports what other packages
// need of it, the host interface it is implemented by included.
package bindgen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/wit"
	"github.com/samyfodil/wazy/component/witgo"
)

// Options says what to generate.
type Options struct {
	// Package is the Go package name of the generated file, or with
	// GenerateTree of the world's package.
	Package string
	// World is the name of the world to bind, in the parsed package.
	World string
	// Skip lists prefixes of imported interface names to leave unbound, for
	// a host that implements them by other means. Nil means "wasi:".
	Skip []string
	// TagBase is the first resource tag the bindings mint host handles
	// under. Zero picks one from the world's name, well clear of the small
	// tags imports/wasip2 uses. An imported interface's own package (see
	// GenerateTree and With) has tags of its own instead, the same in every
	// world.
	TagBase uint32
	// With maps imported interfaces to the Go packages already holding their
	// bindings, which the generated code uses rather than writing its own:
	// an interface name ("wasi:http/types@0.2.0", or without the version to
	// match any) to an import path. The package is GenerateTree's for the
	// interface, from any world importing the same WIT of it.
	With map[string]string
}

// File is one file of GenerateTree's output.
type File struct {
	// Path is where it goes, relative to the tree's root, which is the
	// directory of the base import path GenerateTree is given.
	Path string
	Src  []byte
}

// Generate returns the Go source of the bindings for opts.World in pkg: one
// file, holding everything the bindings need but what opts.With maps to
// other packages.
func Generate(pkg *wit.Package, opts Options) ([]byte, error) {
	g, err := newGen(pkg, opts, "")
	if err != nil {
		return nil, err
	}
	return g.emitUnit(g.root)
}

// GenerateTree returns the bindings for opts.World in pkg as a tree of
// packages under the import path base: one for each imported interface, at
// base/<namespace>/<package>/[v<version>/]<interface> -- shared by every
// world's bindings generated into the same tree, as it holds nothing that
// depends on the world -- and one for the world, at base/<opts.Package>,
// holding the rest.
func GenerateTree(pkg *wit.Package, opts Options, base string) ([]File, error) {
	if base == "" {
		return nil, fmt.Errorf("bindgen: no base import path")
	}
	g, err := newGen(pkg, opts, base)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, u := range append(slices.Clone(g.order), g.root) {
		if u.external {
			continue
		}
		src, err := g.emitUnit(u)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: strings.TrimPrefix(u.path, base+"/") + "/bindings.go", Src: src})
	}
	return files, nil
}

func newGen(pkg *wit.Package, opts Options, base string) (*gen, error) {
	if opts.Package == "" {
		return nil, fmt.Errorf("bindgen: no Go package name")
	}
	w := pkg.World(opts.World)
	if w == nil {
		return nil, fmt.Errorf("bindgen: no world %q in package %s", opts.World, pkg.Name)
	}
	if opts.Skip == nil {
		opts.Skip = []string{"wasi:"}
	}
	if opts.TagBase == 0 {
		h := fnv.New32a()
		h.Write([]byte(pkg.Name + "/" + w.Name))
		opts.TagBase = 1<<31 | h.Sum32()&0x7fff_ff00
	}
	rootPath := ""
	if base != "" {
		rootPath = base + "/" + opts.Package
	}
	g := &gen{
		root:  newUnit(rootPath, opts.Package, nil, "Imports", "Instantiate", "Result", "Tuple"),
		opts:  opts,
		world: w,
		byKey: map[string]*typ{},
		res:   map[wit.ResourceKey]*resource{},
		units: map[string]*unit{},
		tree:  base,
	}
	g.worldGo = g.root.claim(pascal(w.Name))
	if err := g.makeUnits(); err != nil {
		return nil, err
	}
	pkgNames := map[string]bool{}
	for _, u := range g.order {
		pkgNames[u.name] = true
	}
	for _, u := range append(slices.Clone(g.order), g.root) {
		u.pkgNames = pkgNames
	}
	if err := g.collect(); err != nil {
		return nil, err
	}
	return g, nil
}

// makeUnits gives each imported interface the package Options.With maps it
// to, or with a tree its own.
func (g *gen) makeUnits() error {
	matched := map[string]bool{}
	for _, it := range g.world.Imports {
		pkgPath, qualified := witPackage(it.Name)
		if it.Interface == nil || !qualified {
			continue
		}
		var u *unit
		name := goPackageName(it.Name)
		for key, path := range g.opts.With {
			if it.Name == key || strings.HasPrefix(it.Name, key+"@") {
				matched[key] = true
				u = newUnit(path, name, it.Interface, "Host", "Options")
				u.external = true
			}
		}
		if u == nil {
			if g.tree == "" {
				continue
			}
			u = newUnit(g.tree+"/"+pkgPath, name, it.Interface, "Host", "Options")
		}
		g.units[it.Name] = u
		g.order = append(g.order, u)
	}
	for key := range g.opts.With {
		if !matched[key] {
			return fmt.Errorf("bindgen: With: world %s imports no interface %s", g.world.Name, key)
		}
	}
	return nil
}

// witPackage is where an interface's own package goes in a tree --
// "ns:pkg/iface@1.0.0" goes in ns/pkg/v1.0.0/iface -- and whether the name
// is a qualified one, which has a package, rather than a world's own label.
func witPackage(name string) (string, bool) {
	ns, rest, ok := strings.Cut(name, ":")
	if !ok {
		return "", false
	}
	pkg, iface, ok := strings.Cut(rest, "/")
	if !ok {
		return "", false
	}
	path := ns + "/" + pkg
	if iface, ver, versioned := strings.Cut(iface, "@"); versioned {
		return path + "/v" + ver + "/" + iface, true
	}
	return path + "/" + iface, true
}

// goPackageName is the Go package name of an interface's own package: its
// short name, written as a Go identifier, prefixed with its WIT package's
// when that alone is a Go keyword or predeclared name ("wasi:io/error" is
// package ioerror).
func goPackageName(name string) string {
	ident := func(s string) string { return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(s)) }
	short := ident(shortName(name))
	if !token.IsKeyword(short) && !fileIdents[short] && types.Universe.Lookup(short) == nil && short != "" {
		return short
	}
	_, rest, _ := strings.Cut(name, ":")
	pkg, _, _ := strings.Cut(rest, "/")
	return ident(pkg) + short
}

// ifaceTag is the tag of a resource of an imported interface's own package:
// the same in every world, as the package is, and well clear of the small
// tags imports/wasip2 uses.
func ifaceTag(k wit.ResourceKey) uint32 {
	h := fnv.New32a()
	h.Write([]byte(k.Interface + "#" + k.Name))
	return 1<<31 | h.Sum32()&0x7fff_ffff
}

type kind int

const (
	kPrim kind = iota
	kList
	kFixed
	kOption
	kResult
	kTuple
	kRecord
	kVariant
	kEnum
	kFlags
	kHandle
	kMap
	kStream
	kFuture
	kErrorContext
)

// typ is a WIT type, as the generator writes Go for it.
type typ struct {
	kind    kind
	prim    string
	elem    *typ   // list, fixed list, option, stream, future; map's value
	key     *typ   // map
	ok, err *typ   // result; nil for an arm WIT leaves out
	elems   []*typ // tuple
	members []member
	labels  []string // enum, flags
	n       uint32   // fixed list length
	res     *resource
	own     bool // a handle: own rather than borrow
	witName string
	goName  string
	id      string
	// unit is where a named type is defined, and its converters liftName
	// and lowerName.
	unit                *unit
	liftName, lowerName string
	caseType            string // a variant's Case type
}

type member struct {
	wit, goName string
	t           *typ // nil for a variant case without a payload
}

type resource struct {
	key                 wit.ResourceKey
	tag                 uint32
	unit                *unit
	goName              string
	tagGo               string
	liftName, lowerName string
	// guest is set for a resource of an exported interface: the component
	// implements it, and the host holds handles to it.
	guest bool
}

// fn is one function the bindings call or implement.
type fn struct {
	f       *wit.Func
	iface   string // the interface's import or export name, "" in a world
	goName  string
	params  []member
	result  *typ
	sig     *wit.Signature
	dropFor *resource // a destructor rather than a WIT func
	// onResource is set for a guest resource's method, which the bindings
	// make a method of the resource's Go type.
	onResource bool
	async      bool
	skipSelf   bool
}

// skip is how many WIT parameters the Go method leaves out: a resource
// method's self, which its receiver is.
func (b *fn) skip() int {
	if b.skipSelf {
		return 1
	}
	return 0
}

// iface is one bound interface, or the world's own functions.
type iface struct {
	name    string // as imported or exported; "" for world-level functions
	unit    *unit
	goName  string
	options string // the func returning its options
	field   string
	funcs   []*fn
	res     []*resource
}

// unit is one Go package the bindings are written in: the world's, or one of
// an imported interface's own. An external unit is one whose code another
// run of the generator wrote (Options.With): the bindings use it, and write
// nothing of it.
//
// An interface's unit holds what a host needs of the interface, and nothing
// that depends on the world importing it, so every world shares it: the
// types it defines, its resources, the Go interface its host implements and
// the options registering one. Its names are claimed in declaration order
// within it alone, and exported, which is what lets another run reference
// them without reading it.
type unit struct {
	path, name string
	iface      *wit.Interface // nil for the world's
	external   bool

	used    map[string]bool
	named   []*typ
	res     []*resource
	ins     []*iface
	anon    []*typ
	anonIdx map[*typ]int

	// deps are the units its code names, in the order it first does, under
	// the aliases it imports them by.
	deps  []*unit
	alias map[*unit]string

	// preNamed is what an interface's unit names its types, by WIT name.
	preNamed map[string]preName
	// pkgNames are the names of every unit, which its code may import
	// under them, so a parameter must not shadow them.
	pkgNames map[string]bool
}

func newUnit(path, name string, in *wit.Interface, reserved ...string) *unit {
	u := &unit{path: path, name: name, iface: in, used: map[string]bool{}, anonIdx: map[*typ]int{}, alias: map[*unit]string{}}
	for _, r := range reserved {
		u.used[r] = true
	}
	return u
}

// claim reserves a package-level Go name in u, suffixing a number on a clash.
func (u *unit) claim(name string) string {
	n := name
	for i := 2; u.used[n]; i++ {
		n = fmt.Sprintf("%s%d", name, i)
	}
	u.used[n] = true
	return n
}

// exported reports whether u's converters and tags are part of its API: an
// interface's are, for the units using it.
func (u *unit) exported() bool { return u.iface != nil }

// qualify is name, of unit o, as c's code writes it.
func (c *unit) qualify(o *unit, name string) string {
	if o == nil || o == c {
		return name
	}
	a, ok := c.alias[o]
	if !ok {
		a = o.name
		taken := func(a string) bool {
			if fileIdents[a] || c.used[a] {
				return true
			}
			for _, b := range c.alias {
				if b == a {
					return true
				}
			}
			return false
		}
		for i := 2; taken(a); i++ {
			a = fmt.Sprintf("%s%d", o.name, i)
		}
		c.alias[o] = a
		c.deps = append(c.deps, o)
	}
	return a + "." + name
}

// anonName is the number of c's converter for the anonymous type t.
func (c *unit) anonName(t *typ) string {
	n, ok := c.anonIdx[t]
	if !ok {
		c.anon = append(c.anon, t)
		n = len(c.anon)
		c.anonIdx[t] = n
	}
	return fmt.Sprintf("T%d", n)
}

// fileIdents are the identifiers every generated file may use: its imports,
// and Go's predeclared names.
var fileIdents = map[string]bool{
	"context": true, "fmt": true, "wazy": true, "component": true, "witgo": true,
	"error": true, "string": true, "bool": true, "any": true, "nil": true, "true": true, "false": true,
	"byte": true, "rune": true, "int": true, "uint32": true, "len": true, "make": true, "append": true,
}

type gen struct {
	root    *unit
	opts    Options
	world   *wit.World
	worldGo string
	byKey   map[string]*typ
	res     map[wit.ResourceKey]*resource
	imports []*iface
	exports []*iface

	// units are the imported interfaces' own units, by interface name; the
	// rest are the root's. tree is set when they are written too, rather
	// than only some, from Options.With, used.
	units map[string]*unit
	order []*unit
	tree  string
}

// unitOf is the unit that defines what interface name declares.
func (g *gen) unitOf(name string) *unit {
	if u := g.units[name]; u != nil {
		return u
	}
	return g.root
}

// unique adds name to set, suffixed with underscores until it is not
// already there, and returns it.
func unique(set map[string]bool, name string) string {
	for set[name] {
		name += "_"
	}
	set[name] = true
	return name
}

func (g *gen) skipped(name string) bool {
	for _, p := range g.opts.Skip {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (g *gen) collect() error {
	tags := wit.Tags{}
	next := g.opts.TagBase
	for i, items := range [][]wit.WorldItem{g.world.Imports, g.world.Exports} {
		for _, it := range items {
			if it.Interface == nil {
				continue
			}
			for _, r := range it.Interface.Resources {
				k := wit.ResourceKey{Interface: r.Interface, Name: r.Name}
				if _, ok := g.res[k]; ok {
					continue
				}
				res := &resource{key: k, guest: i == 1, unit: g.unitOf(r.Interface)}
				if res.unit != g.root {
					res.tag = ifaceTag(k)
				} else {
					res.tag = next
					next++
				}
				tags[k] = res.tag
				g.res[k] = res
			}
		}
	}
	// An interface's own unit names what it defines before anything else is
	// bound, in declaration order, so that the names are the same whichever
	// world, binding whatever before it, generates it.
	sets := map[*unit]*wit.TypeSet{}
	for _, u := range g.order {
		set, err := u.iface.Types(tags)
		if err != nil {
			return fmt.Errorf("bindgen: %w", err)
		}
		sets[u] = set
		u.preNamed = map[string]preName{}
		for i, ref := range set.Refs {
			if ref.TypeIndex == nil || set.TypeOwners[*ref.TypeIndex] != u.iface.Name {
				continue // an alias of a primitive, or of another interface's type
			}
			name := set.TypeNames[*ref.TypeIndex]
			if _, ok := u.preNamed[name]; ok {
				continue // an alias of one of its own
			}
			goName := u.claim(pascal(name))
			u.preNamed[name] = preName{goName: goName, lift: u.claim("Lift" + goName), lower: u.claim("Lower" + goName), decl: i}
		}
		for _, r := range u.iface.Resources {
			g.nameResource(g.res[wit.ResourceKey{Interface: r.Interface, Name: r.Name}])
		}
	}
	for _, it := range g.world.Imports {
		u := g.units[it.Name]
		switch {
		case u != nil && u.external:
			// Another package's: the world registers it, and binds nothing.
			g.imports = append(g.imports, &iface{name: it.Name, unit: u})
			continue
		case u == nil && it.Interface != nil && g.skipped(it.Name):
			continue
		}
		in, err := g.bind(it, tags, true)
		if err != nil {
			return err
		}
		if u != nil && g.skipped(it.Name) {
			// Skipped by the world, but its unit is the interface's, for
			// every world: bound all the same, and left out of Imports.
			for _, r := range in.res {
				in.funcs = append(in.funcs, &fn{dropFor: r})
			}
			in.goName, in.options = "Host", "Options"
			u.ins = append(u.ins, in)
			continue
		}
		g.imports = append(g.imports, in)
	}
	for _, it := range g.world.Exports {
		in, err := g.bind(it, tags, false)
		if err != nil {
			return err
		}
		g.exports = append(g.exports, in)
	}
	// An interface's own unit defines every type it declares, whether or not
	// a func uses it, in declaration order.
	for _, u := range g.order {
		if u.external {
			continue
		}
		set := sets[u]
		for _, ref := range set.Refs {
			if _, err := g.typeOf(ref, srcOf(set.Types, set.TypeNames, set.TypeOwners), u.iface.Name); err != nil {
				return fmt.Errorf("bindgen: %s: %w", u.iface.Name, err)
			}
		}
		slices.SortStableFunc(u.named, func(a, b *typ) int { return u.preNamed[a.witName].decl - u.preNamed[b.witName].decl })
	}
	// A resource is registered, with its destructor, by the interface that
	// declares it: by its own unit's every one, and otherwise once any bound
	// function uses it, maybe one of a later interface, as wasi:io/streams'
	// are used by every interface after it. An interface that is left with
	// nothing to register is only types.
	bound := g.imports[:0]
	for _, in := range g.imports {
		for _, r := range in.res {
			if r.goName != "" {
				in.funcs = append(in.funcs, &fn{dropFor: r})
			}
		}
		if len(in.funcs) > 0 || in.unit.external || in.unit != g.root {
			bound = append(bound, in)
		}
	}
	g.imports = bound
	g.imports = mergeWorldFuncs(g.imports, g.worldGo+"Funcs")
	g.exports = mergeWorldFuncs(g.exports, "")
	// An interface's Go type takes a package-level name, suffixed on a clash
	// with a type; its field in Imports and its accessor on the world only
	// clash with their own kind, so they keep the plain name where they can.
	fields := map[string]bool{}
	for _, in := range g.imports {
		name := in.goName
		if in.name != "" {
			name = pascal(shortName(in.name))
		}
		if in.unit == g.root {
			in.goName = g.root.claim(name)
			in.options = "options" + in.goName
		} else {
			in.goName, in.options = "Host", "Options"
			if !in.unit.external {
				in.unit.ins = append(in.unit.ins, in)
			}
		}
		in.field = unique(fields, name)
	}
	for _, in := range g.exports {
		in.unit = g.root
		if in.name != "" {
			in.field = pascal(shortName(in.name))
			in.goName = g.root.claim(in.field)
		}
	}
	return nil
}

// preName is what an interface's own unit names one of its types.
type preName struct {
	goName, lift, lower string
	decl                int
}

// mergeWorldFuncs folds the world's own functions, one iface each, into one.
func mergeWorldFuncs(ins []*iface, goName string) []*iface {
	var world *iface
	out := ins[:0]
	for _, in := range ins {
		if in.name != "" {
			out = append(out, in)
			continue
		}
		if world == nil {
			world = &iface{goName: goName, unit: in.unit}
		}
		world.funcs = append(world.funcs, in.funcs...)
	}
	if world != nil {
		out = append(out, world)
	}
	return out
}

func (g *gen) bind(it wit.WorldItem, tags wit.Tags, imported bool) (*iface, error) {
	in := &iface{unit: g.root}
	funcs := []*wit.Func{it.Func}
	if it.Interface != nil {
		in.name = it.Name
		if imported {
			in.unit = g.unitOf(it.Name)
		}
		funcs = it.Interface.Funcs
		for _, r := range it.Interface.Resources {
			res := g.res[wit.ResourceKey{Interface: r.Interface, Name: r.Name}]
			if res.guest {
				g.nameResource(res)
			}
			in.res = append(in.res, res)
		}
	}
	for _, f := range funcs {
		b, err := g.bindFunc(f, in.name, tags, imported)
		if err != nil {
			where := f.Name
			if in.name != "" {
				where = in.name + "#" + f.Name
			}
			return nil, fmt.Errorf("bindgen: %s: %w", where, err)
		}
		in.funcs = append(in.funcs, b)
	}
	return in, nil
}

func (g *gen) bindFunc(f *wit.Func, ifaceName string, tags wit.Tags, imported bool) (*fn, error) {
	sig, err := f.Signature(tags)
	if err != nil {
		return nil, err
	}
	b := &fn{f: f, iface: ifaceName, sig: sig, async: sig.Desc.Async}
	src := srcOf(sig.Types, sig.TypeNames, sig.TypeOwners)
	for _, p := range sig.Desc.Params {
		t, err := g.typeOf(p.Type, src, ifaceName)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}
		b.params = append(b.params, member{wit: p.Name, t: t})
	}
	if r := sig.Desc.Results.Unnamed; r != nil {
		if b.result, err = g.typeOf(*r, src, ifaceName); err != nil {
			return nil, fmt.Errorf("result: %w", err)
		}
	}
	return b, nil
}

func (t *typ) hasAsync() bool {
	if t == nil {
		return false
	}
	if t.kind == kStream || t.kind == kFuture || t.kind == kErrorContext {
		return true
	}
	for _, c := range []*typ{t.elem, t.key, t.ok, t.err} {
		if c.hasAsync() {
			return true
		}
	}
	for _, c := range t.elems {
		if c.hasAsync() {
			return true
		}
	}
	for _, m := range t.members {
		if m.t.hasAsync() {
			return true
		}
	}
	return false
}

func (t *typ) hasHandle() bool {
	if t == nil {
		return false
	}
	if t.kind == kHandle {
		return true
	}
	for _, c := range []*typ{t.elem, t.key, t.ok, t.err} {
		if c.hasHandle() {
			return true
		}
	}
	for _, c := range t.elems {
		if c.hasHandle() {
			return true
		}
	}
	for _, m := range t.members {
		if m.t.hasHandle() {
			return true
		}
	}
	return false
}

var goPrims = map[string]string{
	"bool": "bool", "s8": "int8", "u8": "uint8", "s16": "int16", "u16": "uint16",
	"s32": "int32", "u32": "uint32", "s64": "int64", "u64": "uint64",
	"f32": "float32", "f64": "float64", "char": "rune", "string": "string",
}

// typeSrc is where typeOf reads types: a Signature's, or a TypeSet's.
type typeSrc struct {
	resolve       component.Resolver
	names, owners map[uint32]string
}

func srcOf(tbl *component.TypeTable, names, owners map[uint32]string) typeSrc {
	return typeSrc{resolve: tbl.Resolver(), names: names, owners: owners}
}

// typeOf reads ref into a typ, interning it; ifaceName is the interface whose
// func or type it is read for.
func (g *gen) typeOf(ref component.TypeRef, src typeSrc, ifaceName string) (*typ, error) {
	if ref.Primitive == "error-context" {
		return g.intern(&typ{kind: kErrorContext, id: "error-context"}, ifaceName), nil
	}
	if ref.Primitive != "" {
		if _, ok := goPrims[ref.Primitive]; !ok {
			return nil, fmt.Errorf("type %s is not supported yet", ref.Primitive)
		}
		return g.intern(&typ{kind: kPrim, prim: ref.Primitive, id: "p:" + ref.Primitive}, ifaceName), nil
	}
	idx := *ref.TypeIndex
	d := src.resolve(idx)
	t := &typ{witName: src.names[idx]}
	if t.witName != "" {
		t.unit = g.unitOf(src.owners[idx])
	}
	sub := func(r component.TypeRef) (*typ, error) { return g.typeOf(r, src, ifaceName) }
	var err error
	switch d := d.(type) {
	case component.ListDesc:
		t.kind = kList
		t.elem, err = sub(d.Element)
	case component.FixedListDesc:
		t.kind, t.n = kFixed, d.Length
		t.elem, err = sub(d.Element)
	case component.OptionDesc:
		t.kind = kOption
		t.elem, err = sub(d.Element)
	case component.ResultDesc:
		t.kind = kResult
		if d.Ok != nil {
			if t.ok, err = sub(*d.Ok); err != nil {
				return nil, err
			}
		}
		if d.Err != nil {
			t.err, err = sub(*d.Err)
		}
	case component.TupleDesc:
		t.kind = kTuple
		if len(d.Elements) > witgo.MaxTuple && t.unit == nil {
			t.unit = g.unitOf(ifaceName)
		}
		for _, e := range d.Elements {
			et, err := sub(e)
			if err != nil {
				return nil, err
			}
			t.elems = append(t.elems, et)
		}
	case component.RecordDesc:
		t.kind = kRecord
		for _, f := range d.Fields {
			ft, err := sub(f.Type)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", f.Name, err)
			}
			t.members = append(t.members, member{wit: f.Name, goName: pascal(f.Name), t: ft})
		}
	case component.VariantDesc:
		t.kind = kVariant
		for _, c := range d.Cases {
			m := member{wit: c.Name, goName: pascal(c.Name)}
			if c.Type != nil {
				if m.t, err = sub(*c.Type); err != nil {
					return nil, fmt.Errorf("case %s: %w", c.Name, err)
				}
			}
			t.members = append(t.members, m)
		}
	case component.EnumDesc:
		t.kind, t.labels = kEnum, d.Cases
	case component.FlagsDesc:
		t.kind, t.labels = kFlags, d.Names // WIT allows at most 32
	case component.OwnDesc, component.BorrowDesc:
		tag := uint32(0)
		if o, ok := d.(component.OwnDesc); ok {
			tag, t.own = o.ResourceType, true
		} else {
			tag = d.(component.BorrowDesc).ResourceType
		}
		t.kind = kHandle
		for _, r := range g.res {
			if r.tag == tag {
				t.res = r // Signature only tags the resources collect found
			}
		}
		if t.res.unit == g.root && g.skipped(t.res.key.Interface) {
			return nil, fmt.Errorf("a handle to %s.%s, of an interface left unbound", t.res.key.Interface, t.res.key.Name)
		}
		t.witName = ""
	case component.MapDesc:
		t.kind = kMap
		if t.key, err = sub(d.Key); err == nil { // WIT keys are primitives
			t.elem, err = sub(d.Value)
		}
	case component.StreamDesc, component.FutureDesc:
		t.kind = kStream
		elem := (*component.TypeRef)(nil)
		if s, ok := d.(component.StreamDesc); ok {
			elem = s.Element
		} else {
			t.kind, elem = kFuture, d.(component.FutureDesc).Element
		}
		if elem != nil {
			if t.elem, err = sub(*elem); err != nil {
				return nil, err
			}
			if t.elem.hasHandle() || t.elem.hasAsync() {
				return nil, fmt.Errorf("a %s of %s: an element holding a resource handle, stream, future or error-context is not supported", d.Kind(), t.elem.witString())
			}
		}
		t.witName = ""
	default:
		return nil, fmt.Errorf("%s types are not supported yet", d.Kind())
	}
	if err != nil {
		return nil, err
	}
	t.id = t.key0()
	return g.intern(t, ifaceName), nil
}

// key0 is t's structural identity, from its parts' ids.
func (t *typ) key0() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d:%s:%s:%d:", t.kind, t.witName, t.prim, t.n)
	for _, c := range []*typ{t.elem, t.key, t.ok, t.err} {
		if c != nil {
			b.WriteString(c.id)
		}
		b.WriteByte(',')
	}
	for _, c := range t.elems {
		b.WriteString(c.id + ",")
	}
	for _, m := range t.members {
		b.WriteString(m.wit + "=")
		if m.t != nil {
			b.WriteString(m.t.id)
		}
		b.WriteByte(',')
	}
	b.WriteString(strings.Join(t.labels, ","))
	if t.res != nil {
		fmt.Fprintf(&b, "r%d:%v", t.res.tag, t.own)
	}
	if t.unit != nil {
		// Where it is defined is part of what it is: two interfaces' equal
		// records are two types.
		fmt.Fprintf(&b, "@%p", t.unit)
	}
	return b.String()
}

func (g *gen) intern(t *typ, ifaceName string) *typ {
	if have, ok := g.byKey[t.id]; ok {
		return have
	}
	g.byKey[t.id] = t
	switch {
	case t.kind == kHandle:
		g.nameResource(t.res)
	case t.witName != "":
		u := t.unit
		if pn, ok := u.preNamed[t.witName]; ok {
			t.goName, t.liftName, t.lowerName = pn.goName, pn.lift, pn.lower
		} else {
			name := pascal(t.witName)
			if u.used[name] && ifaceName != "" {
				name = pascal(shortName(ifaceName)) + name
			}
			t.goName = u.claim(name)
			t.liftName, t.lowerName = u.convNames(t.goName)
		}
		u.named = append(u.named, t)
	case t.kind == kTuple && len(t.elems) > witgo.MaxTuple:
		// Longer than witgo has a type for: a struct of the bindings' own,
		// each unit's that uses it.
		u := t.unit
		t.goName = u.claim(fmt.Sprintf("Tuple%d", len(t.elems)))
		t.liftName, t.lowerName = u.convNames(t.goName)
		u.named = append(u.named, t)
	case t.needsConv():
		// The world's unit numbers its anonymous types as it meets them, as it
		// always has; an interface's numbers them as its own code uses them,
		// which does not depend on the world.
		if g.unitOf(ifaceName) == g.root {
			g.root.anonName(t)
		}
	}
	return t
}

// convNames are the names of the converters of the type u calls name.
func (u *unit) convNames(name string) (lift, lower string) {
	if u.exported() {
		return u.claim("Lift" + name), u.claim("Lower" + name)
	}
	return "lift" + name, "lower" + name
}

// nameResource gives r its Go names in its unit, the first time it is used.
func (g *gen) nameResource(r *resource) {
	if r.goName != "" {
		return
	}
	u := r.unit
	r.goName = u.claim(pascal(r.key.Name))
	if u.exported() {
		r.tagGo = u.claim("Tag" + r.goName)
	} else {
		r.tagGo = u.claim("tag" + r.goName)
	}
	r.liftName, r.lowerName = u.convNames(r.goName)
	u.res = append(u.res, r)
}

// needsConv reports whether lifting or lowering t takes a function of its own.
func (t *typ) needsConv() bool {
	switch t.kind {
	case kPrim, kHandle, kStream, kFuture, kErrorContext:
		return false
	case kList:
		return !t.elem.fixedWidth()
	}
	return true
}

// fixedWidth reports whether a list of t lifts as the Go slice of t itself.
func (t *typ) fixedWidth() bool { return t.kind == kPrim && t.prim != "string" }

func (t *typ) goType(c *unit) string {
	switch t.kind {
	case kPrim:
		return goPrims[t.prim]
	case kList:
		if t.elem.kind == kPrim && t.elem.prim == "u8" {
			return "[]byte"
		}
		return "[]" + t.elem.goType(c)
	case kFixed:
		return fmt.Sprintf("[%d]%s", t.n, t.elem.goType(c))
	case kOption:
		return "*" + t.elem.goType(c)
	case kResult:
		return "witgo.Result[" + t.ok.armType(c) + ", " + t.err.armType(c) + "]"
	case kTuple:
		if t.goName != "" {
			return c.qualify(t.unit, t.goName)
		}
		parts := make([]string, len(t.elems))
		for i, e := range t.elems {
			parts[i] = e.goType(c)
		}
		return fmt.Sprintf("witgo.Tuple%d[%s]", len(t.elems), strings.Join(parts, ", "))
	case kHandle:
		return c.qualify(t.res.unit, t.res.goName)
	case kMap:
		return "map[" + t.key.goType(c) + "]" + t.elem.goType(c)
	case kStream:
		return "*witgo.StreamReader[" + t.elem.armType(c) + "]"
	case kFuture:
		return "*witgo.FutureReader[" + t.elem.armType(c) + "]"
	case kErrorContext:
		return "*component.ErrorContext"
	}
	return c.qualify(t.unit, t.goName)
}

func (t *typ) armType(c *unit) string {
	if t == nil {
		return "struct{}"
	}
	return t.goType(c)
}

// liftFn and lowerFn are t's converters, as c's code names them: a named
// type's are its unit's, an anonymous one's c's own.
func (t *typ) liftFn(c *unit) string {
	if t.goName != "" {
		return c.qualify(t.unit, t.liftName)
	}
	return "lift" + c.anonName(t)
}

func (t *typ) lowerFn(c *unit) string {
	if t.goName != "" {
		return c.qualify(t.unit, t.lowerName)
	}
	return "lower" + c.anonName(t)
}

// lift is an expression of type (t's Go type, error) reading the
// component.Value v, with the *witgo.Handles h for the handles in it. A nil h
// takes a handle as the rep the engine translated it to: it is one at the top
// level of an import's arguments.
func (t *typ) lift(c *unit, h, v string) string {
	switch t.kind {
	case kPrim:
		return "witgo." + pascal(t.prim) + "(" + v + ")"
	case kHandle:
		return fmt.Sprintf("%s(%s, %s, %v)", c.qualify(t.res.unit, t.res.liftName), h, v, t.own)
	case kList:
		if t.elem.fixedWidth() {
			return "component.ListOf[" + t.elem.goType(c) + "](" + v + ")"
		}
	case kStream:
		return "witgo.ReaderOf(" + v + ", " + t.elem.elemLift(c) + ")"
	case kFuture:
		return "witgo.FutureReaderOf(" + v + ", " + t.elem.elemLift(c) + ")"
	case kErrorContext:
		return "witgo.ErrorContextOf(" + v + ")"
	}
	return t.liftFn(c) + "(" + h + ", " + v + ")"
}

// elemLift is a func reading a stream's or future's element of type t, nil
// for none.
func (t *typ) elemLift(c *unit) string {
	switch {
	case t == nil:
		return "witgo.Unit"
	case t.kind == kPrim:
		return "witgo." + pascal(t.prim)
	case t.kind == kList && t.elem.fixedWidth():
		return "component.ListOf[" + t.elem.goType(c) + "]"
	}
	return "func(v component.Value) (" + t.goType(c) + ", error) { return " + t.lift(c, "nil", "v") + " }"
}

// elemLower is elemLift's twin, writing an element.
func (t *typ) elemLower(c *unit) string {
	if t == nil {
		return "witgo.LowerUnit"
	}
	return "func(x " + t.goType(c) + ") component.Value { return " + t.lower(c, "nil", "x") + " }"
}

// lower is an expression of type component.Value holding the Go value x; see
// lift for h.
func (t *typ) lower(c *unit, h, x string) string {
	switch t.kind {
	case kPrim:
		switch t.prim {
		case "u8", "u16":
			return "uint32(" + x + ")"
		case "s8", "s16":
			return "int32(" + x + ")"
		}
		return x
	case kHandle:
		return fmt.Sprintf("%s(%s, %s, %v)", c.qualify(t.res.unit, t.res.lowerName), h, x, !t.own)
	case kEnum, kFlags:
		return "uint32(" + x + ")"
	case kList:
		if t.elem.fixedWidth() {
			return x
		}
	case kFixed:
		if t.elem.fixedWidth() {
			return "append([]" + t.elem.goType(c) + "(nil), " + x + "[:]...)"
		}
	case kStream, kFuture:
		if strings.HasPrefix(x, "*") {
			x = "(" + x + ")"
		}
		return x + ".Raw()"
	case kErrorContext:
		return "witgo.LowerErrorContext(" + x + ")"
	}
	return t.lowerFn(c) + "(" + h + ", " + x + ")"
}

func (t *typ) witString() string {
	if t.witName != "" {
		return t.witName
	}
	switch t.kind {
	case kPrim:
		return t.prim
	case kList:
		return "list<" + t.elem.witString() + ">"
	case kFixed:
		return fmt.Sprintf("list<%s, %d>", t.elem.witString(), t.n)
	case kOption:
		return "option<" + t.elem.witString() + ">"
	case kResult:
		if t.ok == nil && t.err == nil {
			return "result"
		}
		ok := "_"
		if t.ok != nil {
			ok = t.ok.witString()
		}
		if t.err == nil {
			return "result<" + ok + ">"
		}
		return "result<" + ok + ", " + t.err.witString() + ">"
	case kTuple:
		parts := make([]string, len(t.elems))
		for i, e := range t.elems {
			parts[i] = e.witString()
		}
		return "tuple<" + strings.Join(parts, ", ") + ">"
	case kMap:
		return "map<" + t.key.witString() + ", " + t.elem.witString() + ">"
	case kStream, kFuture:
		k := "stream"
		if t.kind == kFuture {
			k = "future"
		}
		if t.elem == nil {
			return k
		}
		return k + "<" + t.elem.witString() + ">"
	case kErrorContext:
		return "error-context"
	}
	return t.res.key.Name // a handle: every other kind is named, or above
}

type writer struct{ bytes.Buffer }

func (w *writer) p(format string, args ...any) {
	fmt.Fprintf(&w.Buffer, format, args...)
	w.WriteByte('\n')
}

// emitUnit writes the source of c.
func (g *gen) emitUnit(c *unit) ([]byte, error) {
	body := &writer{}
	g.emitResources(body, c)
	for _, t := range c.named {
		g.emitType(body, c, t)
	}
	emitted := 0
	convs := func() {
		for _, t := range c.named {
			g.emitConv(body, c, t)
		}
		for ; emitted < len(c.anon); emitted++ {
			g.emitConv(body, c, c.anon[emitted])
		}
	}
	if c == g.root {
		convs()
		g.emitImports(body)
		g.emitExports(body)
	} else {
		for _, in := range c.ins {
			g.emitImport(body, c, in)
		}
		convs()
	}
	// What the code above used last may want converters of its own.
	for ; emitted < len(c.anon); emitted++ {
		g.emitConv(body, c, c.anon[emitted])
	}

	w := &writer{}
	w.p("// Code generated by wazy-bindgen. DO NOT EDIT.")
	w.p("")
	if c == g.root {
		w.p("// Package %s binds the WIT world %s.", c.name, g.world.Name)
	} else {
		w.p("// Package %s binds the WIT interface %s, for a host implementing it.", c.name, c.iface.Name)
	}
	w.p("package %s", c.name)
	w.p("")
	w.p("import (")
	for _, std := range []string{"context", "fmt"} {
		if bytes.Contains(body.Bytes(), []byte(std+".")) {
			w.p("%q", std)
		}
	}
	w.p("")
	// One group, sorted: what gofmt-style import grouping keeps as it is.
	var imports [][2]string
	if bytes.Contains(body.Bytes(), []byte("wazy.")) {
		imports = append(imports, [2]string{"", "github.com/samyfodil/wazy"})
	}
	imports = append(imports, [2]string{"", "github.com/samyfodil/wazy/component"})
	if bytes.Contains(body.Bytes(), []byte("witgo.")) {
		imports = append(imports, [2]string{"", "github.com/samyfodil/wazy/component/witgo"})
	}
	for _, d := range c.deps {
		imports = append(imports, [2]string{c.alias[d] + " ", d.path})
	}
	slices.SortFunc(imports, func(a, b [2]string) int { return strings.Compare(a[1], b[1]) })
	for _, im := range imports {
		w.p("%s%q", im[0], im[1])
	}
	w.p(")")
	w.Write(body.Bytes())
	out, err := format.Source(w.Bytes())
	if err != nil {
		return w.Bytes(), fmt.Errorf("bindgen: generated code does not parse (a bindgen bug): %w", err)
	}
	return out, nil
}

func (g *gen) emitResources(w *writer, c *unit) {
	rs := slices.Clone(c.res)
	if len(rs) == 0 {
		return
	}
	if c == g.root {
		slices.SortFunc(rs, func(a, b *resource) int { return int(int64(a.tag) - int64(b.tag)) })
	}
	w.p("// The tags the bindings mint resource handles under.")
	w.p("const (")
	for _, r := range rs {
		w.p("%s uint32 = %#x", r.tagGo, r.tag)
	}
	w.p(")")
	for _, r := range rs {
		w.p("")
		if r.guest {
			g.emitGuestResource(w, r)
			continue
		}
		w.p("// %s is the host's representation of the WIT resource %s.%s: the", r.goName, r.key.Interface, r.key.Name)
		w.p("// value the host returns from its constructor, and receives back as self.")
		w.p("type %s uint32", r.goName)
		w.p("")
		if c.exported() {
			w.p("// %s reads a %s from v: from its handle in h, or with no h its rep.", r.liftName, r.goName)
		}
		w.p("func %s(h *witgo.Handles, v component.Value, own bool) (%s, error) {", r.liftName, r.goName)
		w.p("if h == nil {")
		w.p("x, err := witgo.U32(v)")
		w.p("return %s(x), err", r.goName)
		w.p("}")
		w.p("x, err := h.Rep(%s, own, v)", r.tagGo)
		w.p("return %s(x), err", r.goName)
		w.p("}")
		w.p("")
		if c.exported() {
			w.p("// %s writes x: as a handle minted in h, or with no h its rep.", r.lowerName)
		}
		w.p("func %s(h *witgo.Handles, x %s, borrow bool) component.Value {", r.lowerName, r.goName)
		w.p("if h == nil { return uint32(x) }")
		w.p("return h.Mint(%s, uint32(x), borrow)", r.tagGo)
		w.p("}")
	}
}

func (g *gen) emitGuestResource(w *writer, r *resource) {
	w.p("// %s is a handle to the WIT resource %s.%s, which the component", r.goName, r.key.Interface, r.key.Name)
	w.p("// implements. Drop it when done with it.")
	w.p("type %s struct {", r.goName)
	w.p("inst   *component.Instance")
	w.p("handle uint32")
	w.p("}")
	w.p("")
	w.p("// Handle is the handle's index in the instance's table.")
	w.p("func (x %s) Handle() uint32 { return x.handle }", r.goName)
	w.p("")
	w.p("// Drop drops the handle, running the component's destructor for it.")
	w.p("func (x %s) Drop(ctx context.Context) error {", r.goName)
	w.p("return x.inst.DropResource(ctx, %q, %q, x.handle)", r.key.Interface, r.key.Name)
	w.p("}")
	w.p("")
	w.p("func %s(h *witgo.Handles, v component.Value, _ bool) (%s, error) {", r.liftName, r.goName)
	w.p("x, err := witgo.U32(v)")
	w.p("return %s{inst: h.Inst, handle: x}, err", r.goName)
	w.p("}")
	w.p("")
	w.p("func %s(_ *witgo.Handles, x %s, _ bool) component.Value { return x.handle }", r.lowerName, r.goName)
}

func (g *gen) emitType(w *writer, c *unit, t *typ) {
	w.p("")
	switch t.kind {
	case kTuple:
		if t.witName != "" {
			w.p("// %s is the WIT tuple %s, its elements in order.", t.goName, t.witName)
		} else {
			w.p("// %s is a WIT %s, its elements in order.", t.goName, t.witString())
		}
		w.p("type %s struct {", t.goName)
		for i, e := range t.elems {
			w.p("F%d %s", i, e.goType(c))
		}
		w.p("}")
	case kRecord:
		w.p("// %s is the WIT record %s.", t.goName, t.witName)
		w.p("type %s struct {", t.goName)
		for _, m := range t.members {
			w.p("%s %s", m.goName, m.t.goType(c))
		}
		w.p("}")
	case kEnum, kFlags:
		what, val := "enum", "%s %s = %d"
		if t.kind == kFlags {
			what, val = "flags", "%s %s = 1 << %d"
		}
		w.p("// %s is the WIT %s %s.", t.goName, what, t.witName)
		w.p("type %s uint32", t.goName)
		w.p("")
		w.p("const (")
		for i, l := range t.labels {
			w.p(val, c.claim(t.goName+pascal(l)), t.goName, i)
		}
		w.p(")")
	case kVariant:
		caseType := c.claim(t.goName + "Case")
		t.caseType = caseType
		w.p("// %s is the WIT variant %s: Case says which case it holds.", t.goName, t.witName)
		w.p("type %s struct {", t.goName)
		w.p("Case %s", caseType)
		w.p("payload any")
		w.p("}")
		w.p("")
		w.p("// %s is a case of %s.", caseType, t.goName)
		w.p("type %s uint32", caseType)
		w.p("")
		w.p("const (")
		consts := make([]string, len(t.members))
		for i, m := range t.members {
			consts[i] = c.claim(caseType + m.goName)
			w.p("%s %s = %d", consts[i], caseType, i)
		}
		w.p(")")
		for i, m := range t.members {
			ctor := c.claim(t.goName + m.goName)
			w.p("")
			if m.t == nil {
				w.p("// %s is the %s case of %s.", ctor, m.wit, t.goName)
				w.p("func %s() %s { return %s{Case: %s} }", ctor, t.goName, t.goName, consts[i])
				continue
			}
			w.p("// %s is the %s case of %s, holding v.", ctor, m.wit, t.goName)
			w.p("func %s(v %s) %s { return %s{Case: %s, payload: v} }", ctor, m.t.goType(c), t.goName, t.goName, consts[i])
			w.p("")
			w.p("// %s is the %s case's payload, and whether x holds that case.", m.goName, m.wit)
			w.p("func (x %s) %s() (%s, bool) {", t.goName, m.goName, m.t.goType(c))
			w.p("v, ok := x.payload.(%s)", m.t.goType(c))
			w.p("return v, ok && x.Case == %s", consts[i])
			w.p("}")
		}
	}
}

func (g *gen) emitConv(w *writer, c *unit, t *typ) {
	gt, lift, lower := t.goType(c), t.liftFn(c), t.lowerFn(c)
	ws := t.witString()
	w.p("")
	if c.exported() && t.goName != "" {
		w.p("// %s reads a %s from v, with h for the handles in it.", lift, t.goName)
	}
	w.p("func %s(h *witgo.Handles, v component.Value) (%s, error) {", lift, gt)
	switch t.kind {
	case kList:
		w.p("vs, err := witgo.Values(v, -1, %q)", ws)
		w.p("if err != nil { return nil, err }")
		w.p("out := make(%s, len(vs))", gt)
		w.p("for i, e := range vs {")
		w.p("if out[i], err = %s; err != nil { return nil, err }", t.elem.lift(c, "h", "e"))
		w.p("}")
		w.p("return out, nil")
	case kFixed:
		w.p("var out %s", gt)
		if t.elem.fixedWidth() {
			w.p("s, err := component.ListOf[%s](v)", t.elem.goType(c))
			w.p("if err != nil { return out, err }")
		} else {
			w.p("s, err := witgo.Values(v, -1, %q)", ws)
			w.p("if err != nil { return out, err }")
		}
		w.p("if len(s) != %d { return out, fmt.Errorf(\"%%s: %%d elements\", %q, len(s)) }", t.n, ws)
		if t.elem.fixedWidth() {
			w.p("copy(out[:], s)")
		} else {
			w.p("for i, e := range s {")
			w.p("if out[i], err = %s; err != nil { return out, err }", t.elem.lift(c, "h", "e"))
			w.p("}")
		}
		w.p("return out, nil")
	case kOption:
		w.p("if v == nil { return nil, nil }")
		if t.elem.kind == kOption {
			w.p("if s, ok := v.(component.Some); ok { v = s.Value }")
		}
		w.p("x, err := %s", t.elem.lift(c, "h", "v"))
		w.p("if err != nil { return nil, err }")
		w.p("return &x, nil")
	case kResult:
		w.p("var out %s", gt)
		w.p("r, err := witgo.ResultValue(v, %q)", ws)
		w.p("if err != nil { return out, err }")
		w.p("out.IsErr = r.IsErr")
		if t.ok != nil {
			w.p("if !r.IsErr { out.Ok, err = %s }", t.ok.lift(c, "h", "r.Payload"))
		}
		if t.err != nil {
			w.p("if r.IsErr { out.Err, err = %s }", t.err.lift(c, "h", "r.Payload"))
		}
		w.p("return out, err")
	case kTuple, kRecord:
		n := len(t.elems) + len(t.members)
		w.p("var out %s", gt)
		w.p("vs, err := witgo.Values(v, %d, %q)", n, ws)
		w.p("if err != nil { return out, err }")
		for i := 0; i < n; i++ {
			field, et := fmt.Sprintf("F%d", i), (*typ)(nil)
			if t.kind == kTuple {
				et = t.elems[i]
			} else {
				field, et = t.members[i].goName, t.members[i].t
			}
			w.p("if out.%s, err = %s; err != nil { return out, err }", field, et.lift(c, "h", fmt.Sprintf("vs[%d]", i)))
		}
		w.p("return out, nil")
	case kVariant:
		w.p("var out %s", gt)
		w.p("vv, err := witgo.Variant(v, %q)", ws)
		w.p("if err != nil { return out, err }")
		w.p("if vv.Disc >= %d { return out, fmt.Errorf(\"%%s: case %%d\", %q, vv.Disc) }", len(t.members), ws)
		w.p("out.Case = %s(vv.Disc)", t.caseType)
		w.p("switch vv.Disc {")
		for i, m := range t.members {
			if m.t == nil {
				continue
			}
			w.p("case %d:", i)
			w.p("p, err := %s", m.t.lift(c, "h", "vv.Payload"))
			w.p("if err != nil { return out, err }")
			w.p("out.payload = p")
		}
		w.p("}")
		w.p("return out, nil")
	case kEnum, kFlags:
		w.p("x, err := witgo.U32(v)")
		if t.kind == kEnum {
			w.p("if err == nil && x >= %d { err = fmt.Errorf(\"%%s: case %%d\", %q, x) }", len(t.labels), ws)
		}
		w.p("return %s(x), err", gt)
	case kMap:
		w.p("vs, err := witgo.Values(v, -1, %q)", ws)
		w.p("if err != nil { return nil, err }")
		w.p("out := make(%s, len(vs))", gt)
		w.p("for _, e := range vs {")
		w.p("kv, err := witgo.Values(e, 2, %q)", ws)
		w.p("if err != nil { return nil, err }")
		w.p("k, err := %s", t.key.lift(c, "h", "kv[0]"))
		w.p("if err != nil { return nil, err }")
		w.p("if out[k], err = %s; err != nil { return nil, err }", t.elem.lift(c, "h", "kv[1]"))
		w.p("}")
		w.p("return out, nil")
	}
	w.p("}")
	w.p("")
	if c.exported() && t.goName != "" {
		w.p("// %s writes x, with h for the handles in it.", lower)
	}
	w.p("func %s(h *witgo.Handles, x %s) component.Value {", lower, gt)
	switch t.kind {
	case kList, kFixed:
		w.p("out := make([]component.Value, len(x))")
		w.p("for i, e := range x { out[i] = %s }", t.elem.lower(c, "h", "e"))
		w.p("return out")
	case kOption:
		w.p("if x == nil { return nil }")
		if t.elem.kind == kOption {
			w.p("return component.Some{Value: %s}", t.elem.lower(c, "h", "*x"))
		} else {
			w.p("return %s", t.elem.lower(c, "h", "*x"))
		}
	case kResult:
		okv, errv := "nil", "nil"
		if t.ok != nil {
			okv = t.ok.lower(c, "h", "x.Ok")
		}
		if t.err != nil {
			errv = t.err.lower(c, "h", "x.Err")
		}
		w.p("if x.IsErr { return component.ResultValue{IsErr: true, Payload: %s} }", errv)
		w.p("return component.ResultValue{Payload: %s}", okv)
	case kTuple, kRecord:
		var parts []string
		for i, e := range t.elems {
			parts = append(parts, e.lower(c, "h", fmt.Sprintf("x.F%d", i)))
		}
		for _, m := range t.members {
			parts = append(parts, m.t.lower(c, "h", "x."+m.goName))
		}
		w.p("return []component.Value{%s}", strings.Join(parts, ", "))
	case kVariant:
		w.p("switch x.Case {")
		for i, m := range t.members {
			if m.t == nil {
				continue
			}
			w.p("case %d:", i)
			w.p("p, _ := x.payload.(%s)", m.t.goType(c))
			w.p("return component.VariantValue{Disc: %d, Payload: %s}", i, m.t.lower(c, "h", "p"))
		}
		w.p("}")
		w.p("return component.VariantValue{Disc: uint32(x.Case)}")
	case kEnum, kFlags:
		w.p("return uint32(x)")
	case kMap:
		w.p("out := make([]component.Value, 0, len(x))")
		w.p("for k, e := range x { out = append(out, []component.Value{%s, %s}) }", t.key.lower(c, "h", "k"), t.elem.lower(c, "h", "e"))
		w.p("return out")
	}
	w.p("}")
}

// ref is a Go expression building t's TypeRef in the TypeTable tbl.
func (t *typ) ref(c *unit, names map[*typ]string) string {
	if v, ok := names[t]; ok {
		return v
	}
	switch t.kind {
	case kPrim:
		return fmt.Sprintf("component.Prim(%q)", t.prim)
	case kList:
		return "tbl.List(" + t.elem.ref(c, names) + ")"
	case kFixed:
		return fmt.Sprintf("tbl.FixedList(%s, %d)", t.elem.ref(c, names), t.n)
	case kOption:
		return "tbl.Option(" + t.elem.ref(c, names) + ")"
	case kResult:
		return "tbl.Result(" + t.ok.armRef(c, names) + ", " + t.err.armRef(c, names) + ")"
	case kTuple:
		parts := make([]string, len(t.elems))
		for i, e := range t.elems {
			parts[i] = e.ref(c, names)
		}
		return "tbl.Tuple(" + strings.Join(parts, ", ") + ")"
	case kRecord:
		var parts []string
		for _, m := range t.members {
			parts = append(parts, fmt.Sprintf("%q", m.wit), m.t.ref(c, names))
		}
		return "tbl.Record(" + strings.Join(parts, ", ") + ")"
	case kVariant:
		var parts []string
		for _, m := range t.members {
			if m.t == nil {
				parts = append(parts, fmt.Sprintf("component.VariantCaseSpec{Name: %q}", m.wit))
			} else {
				parts = append(parts, fmt.Sprintf("component.VariantCaseSpec{Name: %q, Type: %s}", m.wit, m.t.ref(c, names)))
			}
		}
		return "tbl.Variant(" + strings.Join(parts, ", ") + ")"
	case kEnum, kFlags:
		parts := make([]string, len(t.labels))
		for i, l := range t.labels {
			parts[i] = fmt.Sprintf("%q", l)
		}
		what := "Enum"
		if t.kind == kFlags {
			what = "Flags"
		}
		return "tbl." + what + "(" + strings.Join(parts, ", ") + ")"
	case kHandle:
		tag := c.qualify(t.res.unit, t.res.tagGo)
		if t.own {
			return "tbl.Own(" + tag + ")"
		}
		return "tbl.Borrow(" + tag + ")"
	case kStream:
		return "tbl.Stream(" + t.elem.armRef(c, names) + ")"
	case kFuture:
		return "tbl.Future(" + t.elem.armRef(c, names) + ")"
	case kErrorContext:
		return `component.Prim("error-context")`
	}
	return "tbl.Map(" + t.key.ref(c, names) + ", " + t.elem.ref(c, names) + ")"
}

func (t *typ) armRef(c *unit, names map[*typ]string) string {
	if t == nil {
		return "component.TypeRef{}"
	}
	return t.ref(c, names)
}

// defineRefs writes a local holding the TypeRef of each named type in t, in
// tbl, before a ref to it, recording it in names.
func defineRefs(w *writer, c *unit, names map[*typ]string, t *typ) {
	if t == nil {
		return
	}
	for _, e := range []*typ{t.elem, t.key, t.ok, t.err} {
		defineRefs(w, c, names, e)
	}
	for _, e := range t.elems {
		defineRefs(w, c, names, e)
	}
	for _, m := range t.members {
		defineRefs(w, c, names, m.t)
	}
	if _, ok := names[t]; t.goName != "" && !ok {
		v := fmt.Sprintf("t%d", len(names)+1)
		w.p("%s := %s", v, t.ref(c, names))
		names[t] = v
	}
}

// methodName is the Go name of a host or export method for f.
func (b *fn) methodName() string {
	if b.dropFor != nil {
		return "Drop" + b.dropFor.goName
	}
	name := b.f.Name
	acc := ""
	for _, p := range []string{"[get]", "[set]"} {
		if i := strings.Index(name, p); i >= 0 {
			acc = pascal(p[1 : len(p)-1])
			name = name[:i] + name[i+len(p):]
		}
	}
	switch b.f.Kind {
	case wit.Constructor:
		return "New" + pascal(b.f.Resource)
	case wit.Method, wit.Static:
		_, m, _ := strings.Cut(name[strings.Index(name, "]")+1:], ".")
		if b.onResource {
			return acc + pascal(m)
		}
		return pascal(b.f.Resource) + acc + pascal(m)
	}
	return acc + pascal(name)
}

// goParams is the method's parameter list after ctx, from its skip'th WIT
// parameter on, and the names used.
func (b *fn) goParams(c *unit, skip int) (string, []string) {
	var parts, names []string
	seen := map[string]bool{"ctx": true}
	for _, p := range b.params[skip:] {
		n := camel(p.wit)
		if reservedNames[n] || seen[n] || c.pkgNames[n] {
			n += "_"
		}
		seen[n] = true
		names = append(names, n)
		parts = append(parts, n+" "+p.t.goType(c))
	}
	return strings.Join(parts, ", "), names
}

// nestedHandles reports whether the function, as an import, passes a handle
// below the top level of an argument or result, which the host translates.
func (b *fn) nestedHandles() bool {
	if b.dropFor != nil {
		return false
	}
	for _, p := range b.params {
		if importHandles(p.t) != "nil" {
			return true
		}
	}
	return b.result != nil && importHandles(b.result) != "nil"
}

// handles reports whether any argument or result of the function holds a
// handle at all.
func (b *fn) handles() bool {
	for _, p := range b.params {
		if p.t.hasHandle() {
			return true
		}
	}
	return b.result.hasHandle()
}

// importHandles is the *witgo.Handles an import adapter converts t with: nil
// for a handle at the top level, which the engine has already translated to
// or from its rep, and for a type with none in it.
func importHandles(t *typ) string {
	if t.kind == kHandle || !t.hasHandle() {
		return "nil"
	}
	return "h"
}

// resultType is the Go type of the function's result, struct{} for none.
func (b *fn) resultType(c *unit) string { return b.result.armType(c) }

func (b *fn) goResults(c *unit) string {
	if b.result == nil {
		return "error"
	}
	return "(" + b.result.goType(c) + ", error)"
}

func (g *gen) emitImports(w *writer) {
	if len(g.imports) == 0 {
		return
	}
	w.p("")
	w.p("// Imports are the host's implementations of the world's imports.")
	w.p("type Imports struct {")
	for _, in := range g.imports {
		w.p("%s %s", in.field, g.root.qualify(in.unit, in.goName))
	}
	w.p("}")
	for _, in := range g.imports {
		if in.unit == g.root {
			g.emitImport(w, g.root, in)
		}
	}
}

func (g *gen) emitImport(w *writer, c *unit, in *iface) {
	w.p("")
	if in.name != "" {
		w.p("// %s is the host's implementation of %s.", in.goName, in.name)
	} else {
		w.p("// %s is the host's implementation of the functions world %s imports itself.", in.goName, g.world.Name)
	}
	w.p("type %s interface {", in.goName)
	names := map[string]bool{}
	for _, b := range in.funcs {
		b.goName = unique(names, b.methodName())
		if b.dropFor != nil {
			w.p("// %s releases a %s the guest dropped.", b.goName, b.dropFor.goName)
			w.p("%s(ctx context.Context, self %s) error", b.goName, c.qualify(b.dropFor.unit, b.dropFor.goName))
			continue
		}
		params, _ := b.goParams(c, 0)
		if params != "" {
			params = ", " + params
		}
		if b.async {
			w.p("// %s is async: it may return its result through ret after it returns.", b.goName)
			w.p("%s(ctx context.Context%s, ret witgo.Async[%s]) error", b.goName, params, b.resultType(c))
			continue
		}
		w.p("%s(ctx context.Context%s) %s", b.goName, params, b.goResults(c))
	}
	w.p("}")
	w.p("")
	if c.exported() {
		w.p("// %s are the options registering impl as the host's %s.", in.options, in.name)
	}
	w.p("func %s(impl %s) []component.Option {", in.options, in.goName)
	calls := slices.ContainsFunc(in.funcs, func(b *fn) bool { return b.dropFor == nil })
	if calls {
		w.p("tbl := component.NewTypeTable()")
	}
	names2 := map[*typ]string{}
	for _, b := range in.funcs {
		for _, p := range b.params {
			defineRefs(w, c, names2, p.t)
		}
		defineRefs(w, c, names2, b.result)
	}
	if calls {
		w.p("resolve := tbl.Resolver()")
	}
	nested := slices.ContainsFunc(in.funcs, (*fn).nestedHandles)
	if nested {
		w.p("// The table the handles nested in arguments and results live in.")
		w.p("var table *component.HandleTable")
	}
	ifaceName := in.name
	w.p("return []component.Option{")
	if nested {
		w.p("component.WithResourcesHook(func(t *component.HandleTable) { table = t }),")
	}
	for _, b := range in.funcs {
		if b.dropFor != nil {
			r := b.dropFor
			tag := c.qualify(r.unit, r.tagGo)
			w.p("component.WithResourceTag(%q, %q, %s),", ifaceName, r.key.Name, tag)
			w.p("component.WithHostResourceDtor(%s, func(ctx context.Context, rep uint32) error { return impl.%s(ctx, %s(rep)) }),", tag, b.goName, c.qualify(r.unit, r.goName))
			continue
		}
		var refs []string
		for _, p := range b.params {
			refs = append(refs, p.t.ref(c, names2))
		}
		res := "component.TypeRef{}"
		if b.result != nil {
			res = b.result.ref(c, names2)
		}
		if b.async {
			g.emitAsyncImport(w, c, b, ifaceName, refs, res)
			continue
		}
		w.p("component.WithImportCustom(%q, %q, func(ctx context.Context, args []component.Value) ([]component.Value, error) {", ifaceName, b.f.Name)
		w.p("if len(args) != %d { return nil, fmt.Errorf(\"%%s: %%d args\", %q, len(args)) }", len(b.params), b.f.Name)
		if b.nestedHandles() {
			w.p("h := &witgo.Handles{Table: table}")
		}
		_, pn := b.goParams(c, 0)
		for i, p := range b.params {
			w.p("%s, err := %s", pn[i], p.t.lift(c, importHandles(p.t), fmt.Sprintf("args[%d]", i)))
			w.p("if err != nil { return nil, fmt.Errorf(\"%%s: %%s: %%w\", %q, %q, err) }", b.f.Name, p.wit)
		}
		call := "impl." + b.goName + "(" + strings.Join(append([]string{"ctx"}, pn...), ", ") + ")"
		if b.result == nil {
			w.p("return nil, %s", call)
		} else {
			w.p("r, err := %s", call)
			w.p("if err != nil { return nil, err }")
			w.p("return []component.Value{%s}, nil", b.result.lower(c, importHandles(b.result), "r"))
		}
		w.p("}, tbl.Func([]component.TypeRef{%s}, %s), resolve),", strings.Join(refs, ", "), res)
	}
	w.p("}")
	w.p("}")
}

// emitAsyncImport writes the registration of the async import b, whose host
// function completes the call through a witgo.Async.
func (g *gen) emitAsyncImport(w *writer, c *unit, b *fn, ifaceName string, refs []string, res string) {
	w.p("component.WithAsyncImportCustom(%q, %q, func(ctx context.Context, args []component.Value, call *component.AsyncCall) error {", ifaceName, b.f.Name)
	w.p("if len(args) != %d { return fmt.Errorf(\"%%s: %%d args\", %q, len(args)) }", len(b.params), b.f.Name)
	if b.nestedHandles() {
		w.p("h := &witgo.Handles{Table: table}")
	}
	_, pn := b.goParams(c, 0)
	for i, p := range b.params {
		w.p("%s, err := %s", pn[i], p.t.lift(c, importHandles(p.t), fmt.Sprintf("args[%d]", i)))
		w.p("if err != nil { return fmt.Errorf(\"%%s: %%s: %%w\", %q, %q, err) }", b.f.Name, p.wit)
	}
	lower := "return nil"
	if b.result != nil {
		lower = "return []component.Value{" + b.result.lower(c, importHandles(b.result), "r") + "}"
	}
	w.p("ret := witgo.NewAsync(call, func(r %s) []component.Value { %s })", b.resultType(c), lower)
	w.p("return impl.%s(%s)", b.goName, strings.Join(append(append([]string{"ctx"}, pn...), "ret"), ", "))
	w.p("}, tbl.Func([]component.TypeRef{%s}, %s), resolve),", strings.Join(refs, ", "), res)
}

func (g *gen) emitExports(w *writer) {
	wg := g.worldGo
	w.p("")
	w.p("// %s is an instance of a component of the WIT world %s.", wg, g.world.Name)
	w.p("type %s struct{ inst *component.Instance }", wg)
	w.p("")
	w.p("// Instantiate instantiates a component of the world %s, with imports implementing", g.world.Name)
	w.p("// its imports. opts are passed on to component.Instantiate, for what the")
	w.p("// bindings leave to the host, such as WASI.")
	if len(g.imports) == 0 {
		w.p("func Instantiate(ctx context.Context, r wazy.Runtime, wasm []byte, opts ...component.Option) (*%s, error) {", wg)
		w.p("var o []component.Option")
	} else {
		w.p("func Instantiate(ctx context.Context, r wazy.Runtime, wasm []byte, imports Imports, opts ...component.Option) (*%s, error) {", wg)
		w.p("var o []component.Option")
		for _, in := range g.imports {
			w.p("if imports.%s == nil { return nil, fmt.Errorf(\"%s: imports.%s is nil\") }", in.field, g.world.Name, in.field)
			w.p("o = append(o, %s(imports.%s)...)", g.root.qualify(in.unit, in.options), in.field)
		}
	}
	w.p("inst, err := component.Instantiate(ctx, r, wasm, append(o, opts...)...)")
	w.p("if err != nil { return nil, err }")
	w.p("return &%s{inst: inst}, nil", wg)
	w.p("}")
	w.p("")
	w.p("// Instance is the underlying component instance.")
	w.p("func (x *%s) Instance() *component.Instance { return x.inst }", wg)
	w.p("")
	w.p("// Close closes the instance.")
	w.p("func (x *%s) Close(ctx context.Context) error { return x.inst.Close(ctx) }", wg)
	taken := map[string]bool{"Instance": true, "Close": true}
	g.emitStreamCtors(w, taken)
	for _, in := range g.exports {
		recv := "x *" + wg
		names := taken
		if in.name != "" {
			acc := unique(taken, in.field)
			w.p("")
			w.p("// %s is the export %s.", in.goName, in.name)
			w.p("type %s struct{ inst *component.Instance }", in.goName)
			w.p("")
			w.p("// %s is the instance's export %s.", acc, in.name)
			w.p("func (x *%s) %s() %s { return %s{x.inst} }", wg, acc, in.goName, in.goName)
			recv = "x " + in.goName
			names = map[string]bool{}
		}
		onRes := map[string]map[string]bool{}
		for _, b := range in.funcs {
			if b.f.Kind == wit.Method {
				b.onResource = true
				res := g.res[wit.ResourceKey{Interface: b.f.Interface, Name: b.f.Resource}]
				if onRes[res.goName] == nil {
					onRes[res.goName] = map[string]bool{"Handle": true, "Drop": true}
				}
				g.emitExportFunc(w, "x "+res.goName, in.name, b, onRes[res.goName], 1)
				continue
			}
			g.emitExportFunc(w, recv, in.name, b, names, 0)
		}
	}
}

// asyncTypes is every stream and future type the world's functions pass, at
// any depth, in the order they are first met.
func (g *gen) asyncTypes() []*typ {
	var out []*typ
	seen := map[*typ]bool{}
	var walk func(t *typ)
	walk = func(t *typ) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		if t.kind == kStream || t.kind == kFuture {
			out = append(out, t)
			return
		}
		for _, c := range append([]*typ{t.elem, t.key, t.ok, t.err}, t.elems...) {
			walk(c)
		}
		for _, m := range t.members {
			walk(m.t)
		}
	}
	for _, in := range append(slices.Clone(g.imports), g.exports...) {
		for _, b := range in.funcs {
			for _, t := range append(memberTypes(b.params), b.result) {
				walk(t)
			}
		}
	}
	return out
}

func memberTypes(ms []member) []*typ {
	out := make([]*typ, len(ms))
	for i, m := range ms {
		out[i] = m.t
	}
	return out
}

// emitStreamCtors writes, for each stream and future type, the world's
// method making one: the host keeps the writer, and passes the reader to the
// guest or reads it itself.
func (g *gen) emitStreamCtors(w *writer, taken map[string]bool) {
	c := g.root
	ts := g.asyncTypes()
	if len(ts) == 0 {
		return
	}
	// The element types, once each: two streams of one type share it.
	var distinct []*typ
	idx := map[*typ]int{}
	for _, t := range ts {
		if _, ok := idx[t.elem]; t.elem != nil && !ok {
			idx[t.elem] = len(distinct)
			distinct = append(distinct, t.elem)
		}
	}
	elems := ""
	if len(distinct) > 0 {
		elems = c.claim("elemTypes")
		w.p("")
		w.p("// %s are the element types of the world's streams and futures.", elems)
		w.p("var %s struct {", elems)
		w.p("resolve component.Resolver")
		w.p("t [%d]component.TypeRef", len(distinct))
		w.p("}")
		w.p("")
		w.p("func init() {")
		w.p("tbl := component.NewTypeTable()")
		names := map[*typ]string{}
		for i, t := range distinct {
			defineRefs(w, c, names, t)
			w.p("%s.t[%d] = %s", elems, i, t.ref(c, names))
		}
		w.p("%s.resolve = tbl.Resolver()", elems)
		w.p("}")
	}
	for _, t := range ts {
		k := "Stream"
		if t.kind == kFuture {
			k = "Future"
		}
		name := unique(taken, "New"+k+t.elem.label())
		et := t.elem.armType(c)
		elem, resolve := "nil", "nil"
		if t.elem != nil {
			elem, resolve = fmt.Sprintf("&%s.t[%d]", elems, idx[t.elem]), elems+".resolve"
		}
		w.p("")
		w.p("// %s makes a %s: write to the writer, and pass the reader to the", name, t.witString())
		w.p("// component or read it.")
		w.p("func (x *%s) %s() (*witgo.%sWriter[%s], *witgo.%sReader[%s], error) {", g.worldGo, name, k, et, k, et)
		w.p("return witgo.New%s(x.inst, %s, %s, %s, %s)", k, elem, resolve, t.elem.elemLift(c), t.elem.elemLower(c))
		w.p("}")
	}
}

// label is a Go name for t, for a stream constructor's.
func (t *typ) label() string {
	if t == nil {
		return ""
	}
	if t.goName != "" {
		return t.goName
	}
	switch t.kind {
	case kPrim:
		return pascal(t.prim)
	case kHandle:
		return t.res.goName
	case kList:
		return "List" + t.elem.label()
	case kFixed:
		return fmt.Sprintf("List%d", t.n) + t.elem.label()
	case kOption:
		return "Option" + t.elem.label()
	case kResult:
		return "Result" + t.ok.label() + t.err.label()
	case kMap:
		return "Map" + t.key.label() + t.elem.label()
	}
	var b strings.Builder
	b.WriteString("Tuple")
	for _, e := range t.elems {
		b.WriteString(e.label())
	}
	return b.String()
}

// emitExportFunc writes the method calling the export b on recv: from its
// skip'th parameter on, the first being a resource method's self, which the
// receiver is.
func (g *gen) emitExportFunc(w *writer, recv, ifaceName string, b *fn, names map[string]bool, skip int) {
	c := g.root
	b.goName = unique(names, b.methodName())
	params, pn := b.goParams(c, skip)
	if params != "" {
		params = ", " + params
	}
	b.skipSelf = skip == 1
	h := "nil"
	if b.handles() {
		h = "h"
	}
	var args []string
	if skip == 1 {
		args = append(args, "x.handle")
	}
	for i, p := range b.params[skip:] {
		args = append(args, p.t.lower(c, h, pn[i]))
	}
	call := fmt.Sprintf("x.inst.Call(ctx, %q", b.f.Name)
	table := "x.inst.Resources()"
	if ifaceName != "" {
		call = fmt.Sprintf("x.inst.CallExport(ctx, %q, %q", ifaceName, b.f.Name)
		table = fmt.Sprintf("x.inst.ResourcesFor(%q)", ifaceName)
	}
	if len(args) > 0 {
		call += ", " + strings.Join(args, ", ")
	}
	call += ")"
	w.p("")
	w.p("// %s calls the export %s.", b.goName, b.f.Name)
	w.p("func (%s) %s(ctx context.Context%s) %s {", recv, b.goName, params, b.goResults(c))
	if h == "h" {
		w.p("h := &witgo.Handles{Table: %s, Inst: x.inst}", table)
	}
	res := "res"
	if b.result == nil {
		res = "_"
	}
	w.p("%s, err := %s", res, call)
	if h == "h" {
		w.p("if rerr := h.Release(); err == nil { err = rerr }")
	}
	if b.result == nil {
		w.p("return err")
	} else {
		w.p("if err != nil { var zero %s; return zero, err }", b.result.goType(c))
		w.p("if len(res) != 1 { var zero %s; return zero, fmt.Errorf(\"%%s: %%d results\", %q, len(res)) }", b.result.goType(c), b.f.Name)
		w.p("return %s", b.result.lift(c, h, "res[0]"))
	}
	w.p("}")
	if b.async {
		g.emitExportAsync(w, recv, ifaceName, b, names, args, table, h)
	}
}

// emitExportAsync writes the CallAsync form of the async export b: it
// returns once the call first waits, for an import the host completes later
// from another goroutine.
func (g *gen) emitExportAsync(w *writer, recv, ifaceName string, b *fn, names map[string]bool, args []string, table, h string) {
	c := g.root
	name := unique(names, b.goName+"Async")
	params, _ := b.goParams(c, b.skip())
	if params != "" {
		params = ", " + params
	}
	export := b.f.Name
	if ifaceName != "" {
		export = ifaceName + "#" + b.f.Name
	}
	call := fmt.Sprintf("x.inst.CallAsync(ctx, %q", export)
	if len(args) > 0 {
		call += ", " + strings.Join(args, ", ")
	}
	call += ")"
	rt := b.resultType(c)
	w.p("")
	w.p("// %s starts the export %s and returns once it first waits; Await the result.", name, b.f.Name)
	w.p("func (%s) %s(ctx context.Context%s) (*witgo.Pending[%s], error) {", recv, name, params, rt)
	if h == "h" {
		w.p("h := &witgo.Handles{Table: %s, Inst: x.inst}", table)
	}
	w.p("p, err := %s", call)
	w.p("if err != nil {")
	if h == "h" {
		w.p("h.Release()")
	}
	w.p("return nil, err")
	w.p("}")
	w.p("return witgo.NewPending(p, func(res []component.Value) (%s, error) {", rt)
	w.p("var zero %s", rt)
	if h == "h" {
		w.p("if err := h.Release(); err != nil { return zero, err }")
	}
	if b.result == nil {
		w.p("return zero, nil")
	} else {
		w.p("if len(res) != 1 { return zero, fmt.Errorf(\"%%s: %%d results\", %q, len(res)) }", b.f.Name)
		w.p("return %s", b.result.lift(c, h, "res[0]"))
	}
	w.p("}), nil")
	w.p("}")
}

// shortName is an interface's name without namespace, package or version:
// "example:store/kv@1.0.0" is "kv".
func shortName(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	return name
}

// pascal turns a WIT kebab-case name into an exported Go name, writing
// each word that is a common initialism the way Go does: "user-id" is UserID.
func pascal(s string) string {
	var b strings.Builder
	for _, word := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ':' || r == '/'
	}) {
		if up := strings.ToUpper(word); initialisms[up] {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(word[:1]) + word[1:])
	}
	out := b.String()
	if out == "" || out[0] >= '0' && out[0] <= '9' {
		out = "X" + out
	}
	return out
}

// initialisms are the words golint writes in capitals.
var initialisms = map[string]bool{
	"ACL": true, "API": true, "ASCII": true, "CPU": true, "CSS": true, "DNS": true, "EOF": true,
	"GUID": true, "HTML": true, "HTTP": true, "HTTPS": true, "ID": true, "IP": true, "JSON": true,
	"QPS": true, "RAM": true, "RPC": true, "SLA": true, "SMTP": true, "SQL": true, "SSH": true,
	"TCP": true, "TLS": true, "TTL": true, "UDP": true, "UI": true, "UID": true, "UUID": true,
	"URI": true, "URL": true, "UTF8": true, "VM": true, "XML": true, "XMPP": true, "XSRF": true,
	"XSS": true,
}

// camel is pascal with its first word in lower case: "http-url" is httpURL.
func camel(s string) string {
	first, rest, _ := strings.Cut(s, "-")
	p := pascal(rest)
	if rest == "" {
		p = ""
	}
	f := strings.ToLower(first)
	if f == "" || f[0] >= '0' && f[0] <= '9' {
		f = "x" + f
	}
	return f + p
}

// reservedNames are Go's keywords, and the names generated functions use for
// their own locals, which a parameter must not shadow.
var reservedNames = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true, "default": true,
	"defer": true, "else": true, "fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true, "switch": true, "type": true,
	"var": true, "err": true, "args": true, "r": true, "res": true, "impl": true, "tbl": true,
	"resolve": true, "x": true, "inst": true,
}
