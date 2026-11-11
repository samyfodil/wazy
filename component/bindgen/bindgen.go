// Package bindgen generates Go bindings for a WIT world, so a host calls and
// implements a component's interfaces in Go types rather than in
// component.Values:
//
//   - a Go type for each record, variant, enum, flags and resource the world's
//     functions use;
//   - for each import, a Go interface the host implements, and the options
//     that register it;
//   - for the exports, a type wrapping the component.Instance, with a method
//     per exported function.
//
// The generated code depends only on package component and package witgo.
// cmd/wazy-bindgen runs Generate from the command line, for go generate.
//
// A world's WASI imports are not bound by default: wazy's imports/wasip2
// implements them, and its options go to Instantiate next to the bindings.
package bindgen

import (
	"bytes"
	"fmt"
	"go/format"
	"hash/fnv"
	"slices"
	"strings"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/wit"
	"github.com/samyfodil/wazy/component/witgo"
)

// Options says what to generate.
type Options struct {
	// Package is the Go package name of the generated file.
	Package string
	// World is the name of the world to bind, in the parsed package.
	World string
	// Skip lists prefixes of imported interface names to leave unbound, for
	// a host that implements them by other means. Nil means "wasi:".
	Skip []string
	// TagBase is the first resource tag the bindings mint host handles
	// under. Zero picks one from the world's name, well clear of the small
	// tags imports/wasip2 uses.
	TagBase uint32
}

// Generate returns the Go source of the bindings for opts.World in pkg.
func Generate(pkg *wit.Package, opts Options) ([]byte, error) {
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
	g := &gen{
		opts:  opts,
		world: w,
		used:  map[string]bool{},
		byKey: map[string]*typ{},
		res:   map[wit.ResourceKey]*resource{},
	}
	for _, name := range []string{"Imports", "Instantiate", "Result", "Tuple"} {
		g.used[name] = true
	}
	g.worldGo = g.claim(pascal(w.Name))
	if err := g.collect(); err != nil {
		return nil, err
	}
	src := g.emit()
	out, err := format.Source(src)
	if err != nil {
		return src, fmt.Errorf("bindgen: generated code does not parse (a bindgen bug): %w", err)
	}
	return out, nil
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
)

// typ is a WIT type, as the generator writes Go for it.
type typ struct {
	kind    kind
	prim    string
	elem    *typ   // list, fixed list, option; map's value
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
	conv    int // converter number, for an anonymous type that needs one
}

type member struct {
	wit, goName string
	t           *typ // nil for a variant case without a payload
}

type resource struct {
	key    wit.ResourceKey
	tag    uint32
	goName string
	tagGo  string
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
}

// iface is one bound interface, or the world's own functions.
type iface struct {
	name   string // as imported or exported; "" for world-level functions
	goName string
	field  string
	funcs  []*fn
	res    []*resource
}

type gen struct {
	opts    Options
	world   *wit.World
	worldGo string
	used    map[string]bool
	byKey   map[string]*typ
	named   []*typ
	anon    []*typ
	res     map[wit.ResourceKey]*resource
	imports []*iface
	exports []*iface
	tuples  bool
}

// claim reserves a package-level Go name, suffixing a number on a clash.
func (g *gen) claim(name string) string {
	n := name
	for i := 2; g.used[n]; i++ {
		n = fmt.Sprintf("%s%d", name, i)
	}
	g.used[n] = true
	return n
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
	for _, items := range [][]wit.WorldItem{g.world.Imports, g.world.Exports} {
		for _, it := range items {
			if it.Interface == nil {
				continue
			}
			for _, r := range it.Interface.Resources {
				k := wit.ResourceKey{Interface: r.Interface, Name: r.Name}
				if _, ok := g.res[k]; ok {
					continue
				}
				tags[k] = next
				g.res[k] = &resource{key: k, tag: next}
				next++
			}
		}
	}
	for _, it := range g.world.Imports {
		if it.Interface != nil && g.skipped(it.Name) {
			continue
		}
		in, err := g.bind(it, tags, true)
		if err != nil {
			return err
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
	// A resource is registered, with its destructor, by the interface that
	// declares it, once any bound function uses it: maybe one of a later
	// interface, as wasi:io/streams' are used by every interface after it.
	// An interface that is left with nothing to register is only types.
	bound := g.imports[:0]
	for _, in := range g.imports {
		for _, r := range in.res {
			if r.goName != "" {
				in.funcs = append(in.funcs, &fn{dropFor: r})
			}
		}
		if len(in.funcs) > 0 {
			bound = append(bound, in)
		}
	}
	g.imports = bound
	g.imports = mergeWorldFuncs(g.imports, g.worldGo+"Funcs")
	g.exports = mergeWorldFuncs(g.exports, "")
	for _, in := range g.imports {
		if in.name != "" {
			in.goName = g.claim(pascal(shortName(in.name)))
		} else {
			in.goName = g.claim(in.goName)
		}
		in.field = in.goName
	}
	for _, in := range g.exports {
		if in.name != "" {
			in.goName = g.claim(pascal(shortName(in.name)))
			in.field = in.goName
		}
	}
	return nil
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
			world = &iface{goName: goName}
		}
		world.funcs = append(world.funcs, in.funcs...)
	}
	if world != nil {
		out = append(out, world)
	}
	return out
}

func (g *gen) bind(it wit.WorldItem, tags wit.Tags, imported bool) (*iface, error) {
	in := &iface{}
	funcs := []*wit.Func{it.Func}
	if it.Interface != nil {
		in.name = it.Name
		funcs = it.Interface.Funcs
		for _, r := range it.Interface.Resources {
			if !imported {
				return nil, fmt.Errorf("bindgen: export %s: exported resource %s is not supported yet", it.Name, r.Name)
			}
			res := g.res[wit.ResourceKey{Interface: r.Interface, Name: r.Name}]
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
	if sig.Desc.Async {
		return nil, fmt.Errorf("async functions are not supported yet")
	}
	b := &fn{f: f, iface: ifaceName, sig: sig}
	for _, p := range sig.Desc.Params {
		t, err := g.typeOf(p.Type, sig, ifaceName)
		if err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}
		if !imported && t.hasHandle() {
			return nil, fmt.Errorf("param %s: a resource handle in an export is not supported yet", p.Name)
		}
		b.params = append(b.params, member{wit: p.Name, t: t})
	}
	if r := sig.Desc.Results.Unnamed; r != nil {
		if b.result, err = g.typeOf(*r, sig, ifaceName); err != nil {
			return nil, fmt.Errorf("result: %w", err)
		}
		if !imported && b.result.hasHandle() {
			return nil, fmt.Errorf("result: a resource handle in an export is not supported yet")
		}
	}
	return b, nil
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

// typeOf reads ref into a typ, interning it.
func (g *gen) typeOf(ref component.TypeRef, sig *wit.Signature, ifaceName string) (*typ, error) {
	if ref.Primitive != "" {
		if _, ok := goPrims[ref.Primitive]; !ok {
			return nil, fmt.Errorf("type %s is not supported yet", ref.Primitive)
		}
		return g.intern(&typ{kind: kPrim, prim: ref.Primitive, id: "p:" + ref.Primitive}, ifaceName), nil
	}
	idx := *ref.TypeIndex
	d := sig.Types.Resolver()(idx)
	t := &typ{witName: sig.TypeNames[idx]}
	sub := func(r component.TypeRef) (*typ, error) { return g.typeOf(r, sig, ifaceName) }
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
		if t.elem, err = sub(d.Element); err == nil && t.elem.kind == kOption {
			err = fmt.Errorf("option<option<T>> is not supported: none and some(none) lift to the same value")
		}
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
		if len(d.Elements) > witgo.MaxTuple {
			return nil, fmt.Errorf("a tuple of %d elements is longer than witgo has a type for", len(d.Elements))
		}
		for _, e := range d.Elements {
			et, err := sub(e)
			if err != nil {
				return nil, err
			}
			t.elems = append(t.elems, et)
		}
		g.tuples = true
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
		if g.skipped(t.res.key.Interface) {
			return nil, fmt.Errorf("a handle to %s.%s, of an interface left unbound", t.res.key.Interface, t.res.key.Name)
		}
		t.witName = ""
	case component.MapDesc:
		t.kind = kMap
		if t.key, err = sub(d.Key); err == nil { // WIT keys are primitives
			t.elem, err = sub(d.Value)
		}
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
	return b.String()
}

func (g *gen) intern(t *typ, ifaceName string) *typ {
	if have, ok := g.byKey[t.id]; ok {
		return have
	}
	g.byKey[t.id] = t
	switch {
	case t.kind == kHandle:
		if t.res.goName == "" {
			t.res.goName = g.claim(pascal(t.res.key.Name))
			t.res.tagGo = g.claim("tag" + t.res.goName)
		}
	case t.witName != "":
		name := pascal(t.witName)
		if g.used[name] && ifaceName != "" {
			name = pascal(shortName(ifaceName)) + name
		}
		t.goName = g.claim(name)
		g.named = append(g.named, t)
	case t.needsConv():
		t.conv = len(g.anon) + 1
		g.anon = append(g.anon, t)
	}
	return t
}

// needsConv reports whether lifting or lowering t takes a function of its own.
func (t *typ) needsConv() bool {
	switch t.kind {
	case kPrim, kHandle:
		return false
	case kList:
		return !t.elem.fixedWidth()
	}
	return true
}

// fixedWidth reports whether a list of t lifts as the Go slice of t itself.
func (t *typ) fixedWidth() bool { return t.kind == kPrim && t.prim != "string" }

func (t *typ) goType() string {
	switch t.kind {
	case kPrim:
		return goPrims[t.prim]
	case kList:
		if t.elem.kind == kPrim && t.elem.prim == "u8" {
			return "[]byte"
		}
		return "[]" + t.elem.goType()
	case kFixed:
		return fmt.Sprintf("[%d]%s", t.n, t.elem.goType())
	case kOption:
		return "*" + t.elem.goType()
	case kResult:
		return "witgo.Result[" + t.ok.armType() + ", " + t.err.armType() + "]"
	case kTuple:
		parts := make([]string, len(t.elems))
		for i, e := range t.elems {
			parts[i] = e.goType()
		}
		return fmt.Sprintf("witgo.Tuple%d[%s]", len(t.elems), strings.Join(parts, ", "))
	case kHandle:
		return t.res.goName
	case kMap:
		return "map[" + t.key.goType() + "]" + t.elem.goType()
	}
	return t.goName
}

func (t *typ) armType() string {
	if t == nil {
		return "struct{}"
	}
	return t.goType()
}

// convName is the suffix of t's lift and lower functions.
func (t *typ) convName() string {
	if t.goName != "" {
		return t.goName
	}
	return fmt.Sprintf("T%d", t.conv)
}

// lift is an expression of type (t's Go type, error) reading the
// component.Value v.
func (t *typ) lift(v string) string {
	switch t.kind {
	case kPrim:
		return "witgo." + pascal(t.prim) + "(" + v + ")"
	case kHandle:
		return "lift" + t.res.goName + "(" + v + ")"
	case kList:
		if t.elem.fixedWidth() {
			return "component.ListOf[" + t.elem.goType() + "](" + v + ")"
		}
	}
	return "lift" + t.convName() + "(" + v + ")"
}

// lower is an expression of type component.Value holding the Go value x.
func (t *typ) lower(x string) string {
	switch t.kind {
	case kPrim:
		switch t.prim {
		case "u8", "u16":
			return "uint32(" + x + ")"
		case "s8", "s16":
			return "int32(" + x + ")"
		}
		return x
	case kHandle, kEnum, kFlags:
		return "uint32(" + x + ")"
	case kList:
		if t.elem.fixedWidth() {
			return x
		}
	case kFixed:
		if t.elem.fixedWidth() {
			return "append([]" + t.elem.goType() + "(nil), " + x + "[:]...)"
		}
	}
	return "lower" + t.convName() + "(" + x + ")"
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
		return "result"
	case kTuple:
		return "tuple"
	case kMap:
		return "map"
	}
	return t.res.key.Name // a handle: every other kind is named, or above
}

type writer struct{ bytes.Buffer }

func (w *writer) p(format string, args ...any) {
	fmt.Fprintf(&w.Buffer, format, args...)
	w.WriteByte('\n')
}

func (g *gen) emit() []byte {
	body := &writer{}
	g.emitResources(body)
	for _, t := range g.named {
		g.emitType(body, t)
	}
	for _, t := range append(slices.Clone(g.named), g.anon...) {
		g.emitConv(body, t)
	}
	g.emitImports(body)
	g.emitExports(body)

	w := &writer{}
	w.p("// Code generated by wazy-bindgen. DO NOT EDIT.")
	w.p("")
	w.p("// Package %s binds the WIT world %s.", g.opts.Package, g.world.Name)
	w.p("package %s", g.opts.Package)
	w.p("")
	w.p("import (")
	w.p("%q", "context")
	if bytes.Contains(body.Bytes(), []byte("fmt.")) {
		w.p("%q", "fmt")
	}
	w.p("")
	w.p("%q", "github.com/samyfodil/wazy")
	w.p("%q", "github.com/samyfodil/wazy/component")
	if bytes.Contains(body.Bytes(), []byte("witgo.")) {
		w.p("%q", "github.com/samyfodil/wazy/component/witgo")
	}
	w.p(")")
	w.Write(body.Bytes())
	return w.Bytes()
}

func (g *gen) resources() []*resource {
	var out []*resource
	for _, r := range g.res {
		if r.goName != "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b *resource) int { return int(int64(a.tag) - int64(b.tag)) })
	return out
}

func (g *gen) emitResources(w *writer) {
	rs := g.resources()
	if len(rs) == 0 {
		return
	}
	w.p("// The tags the bindings mint host resource handles under.")
	w.p("const (")
	for _, r := range rs {
		w.p("%s uint32 = %#x", r.tagGo, r.tag)
	}
	w.p(")")
	for _, r := range rs {
		w.p("")
		w.p("// %s is the host's representation of the WIT resource %s.%s: the", r.goName, r.key.Interface, r.key.Name)
		w.p("// value the host returns from its constructor, and receives back as self.")
		w.p("type %s uint32", r.goName)
		w.p("")
		w.p("func lift%s(v component.Value) (%s, error) {", r.goName, r.goName)
		w.p("x, err := witgo.U32(v)")
		w.p("return %s(x), err", r.goName)
		w.p("}")
	}
}

func (g *gen) emitType(w *writer, t *typ) {
	w.p("")
	switch t.kind {
	case kRecord:
		w.p("// %s is the WIT record %s.", t.goName, t.witName)
		w.p("type %s struct {", t.goName)
		for _, m := range t.members {
			w.p("%s %s", m.goName, m.t.goType())
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
			w.p(val, g.claim(t.goName+pascal(l)), t.goName, i)
		}
		w.p(")")
	case kVariant:
		caseType := g.claim(t.goName + "Case")
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
			consts[i] = g.claim(caseType + m.goName)
			w.p("%s %s = %d", consts[i], caseType, i)
		}
		w.p(")")
		for i, m := range t.members {
			ctor := g.claim(t.goName + m.goName)
			w.p("")
			if m.t == nil {
				w.p("// %s is the %s case of %s.", ctor, m.wit, t.goName)
				w.p("func %s() %s { return %s{Case: %s} }", ctor, t.goName, t.goName, consts[i])
				continue
			}
			w.p("// %s is the %s case of %s, holding v.", ctor, m.wit, t.goName)
			w.p("func %s(v %s) %s { return %s{Case: %s, payload: v} }", ctor, m.t.goType(), t.goName, t.goName, consts[i])
			w.p("")
			w.p("// %s is the %s case's payload, and whether x holds that case.", m.goName, m.wit)
			w.p("func (x %s) %s() (%s, bool) {", t.goName, m.goName, m.t.goType())
			w.p("v, ok := x.payload.(%s)", m.t.goType())
			w.p("return v, ok && x.Case == %s", consts[i])
			w.p("}")
		}
	}
}

func (g *gen) emitConv(w *writer, t *typ) {
	gt, n := t.goType(), t.convName()
	ws := t.witString()
	w.p("")
	w.p("func lift%s(v component.Value) (%s, error) {", n, gt)
	switch t.kind {
	case kList:
		w.p("vs, err := witgo.Values(v, -1, %q)", ws)
		w.p("if err != nil { return nil, err }")
		w.p("out := make(%s, len(vs))", gt)
		w.p("for i, e := range vs {")
		w.p("if out[i], err = %s; err != nil { return nil, err }", t.elem.lift("e"))
		w.p("}")
		w.p("return out, nil")
	case kFixed:
		w.p("var out %s", gt)
		if t.elem.fixedWidth() {
			w.p("s, err := component.ListOf[%s](v)", t.elem.goType())
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
			w.p("if out[i], err = %s; err != nil { return out, err }", t.elem.lift("e"))
			w.p("}")
		}
		w.p("return out, nil")
	case kOption:
		w.p("if v == nil { return nil, nil }")
		w.p("x, err := %s", t.elem.lift("v"))
		w.p("if err != nil { return nil, err }")
		w.p("return &x, nil")
	case kResult:
		w.p("var out %s", gt)
		w.p("r, err := witgo.ResultValue(v, %q)", ws)
		w.p("if err != nil { return out, err }")
		w.p("out.IsErr = r.IsErr")
		if t.ok != nil {
			w.p("if !r.IsErr { out.Ok, err = %s }", t.ok.lift("r.Payload"))
		}
		if t.err != nil {
			w.p("if r.IsErr { out.Err, err = %s }", t.err.lift("r.Payload"))
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
			w.p("if out.%s, err = %s; err != nil { return out, err }", field, et.lift(fmt.Sprintf("vs[%d]", i)))
		}
		w.p("return out, nil")
	case kVariant:
		w.p("var out %s", gt)
		w.p("vv, err := witgo.Variant(v, %q)", ws)
		w.p("if err != nil { return out, err }")
		w.p("if vv.Disc >= %d { return out, fmt.Errorf(\"%%s: case %%d\", %q, vv.Disc) }", len(t.members), ws)
		w.p("out.Case = %sCase(vv.Disc)", t.goName)
		w.p("switch vv.Disc {")
		for i, m := range t.members {
			if m.t == nil {
				continue
			}
			w.p("case %d:", i)
			w.p("p, err := %s", m.t.lift("vv.Payload"))
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
		w.p("k, err := %s", t.key.lift("kv[0]"))
		w.p("if err != nil { return nil, err }")
		w.p("if out[k], err = %s; err != nil { return nil, err }", t.elem.lift("kv[1]"))
		w.p("}")
		w.p("return out, nil")
	}
	w.p("}")
	w.p("")
	w.p("func lower%s(x %s) component.Value {", n, gt)
	switch t.kind {
	case kList, kFixed:
		w.p("out := make([]component.Value, len(x))")
		w.p("for i, e := range x { out[i] = %s }", t.elem.lower("e"))
		w.p("return out")
	case kOption:
		w.p("if x == nil { return nil }")
		w.p("return %s", t.elem.lower("*x"))
	case kResult:
		okv, errv := "nil", "nil"
		if t.ok != nil {
			okv = t.ok.lower("x.Ok")
		}
		if t.err != nil {
			errv = t.err.lower("x.Err")
		}
		w.p("if x.IsErr { return component.ResultValue{IsErr: true, Payload: %s} }", errv)
		w.p("return component.ResultValue{Payload: %s}", okv)
	case kTuple, kRecord:
		var parts []string
		for i, e := range t.elems {
			parts = append(parts, e.lower(fmt.Sprintf("x.F%d", i)))
		}
		for _, m := range t.members {
			parts = append(parts, m.t.lower("x."+m.goName))
		}
		w.p("return []component.Value{%s}", strings.Join(parts, ", "))
	case kVariant:
		w.p("switch x.Case {")
		for i, m := range t.members {
			if m.t == nil {
				continue
			}
			w.p("case %d:", i)
			w.p("p, _ := x.payload.(%s)", m.t.goType())
			w.p("return component.VariantValue{Disc: %d, Payload: %s}", i, m.t.lower("p"))
		}
		w.p("}")
		w.p("return component.VariantValue{Disc: uint32(x.Case)}")
	case kEnum, kFlags:
		w.p("return uint32(x)")
	case kMap:
		w.p("out := make([]component.Value, 0, len(x))")
		w.p("for k, e := range x { out = append(out, []component.Value{%s, %s}) }", t.key.lower("k"), t.elem.lower("e"))
		w.p("return out")
	}
	w.p("}")
}

// ref is a Go expression building t's TypeRef in the TypeTable tbl.
func (t *typ) ref(names map[*typ]string) string {
	if v, ok := names[t]; ok {
		return v
	}
	switch t.kind {
	case kPrim:
		return fmt.Sprintf("component.Prim(%q)", t.prim)
	case kList:
		return "tbl.List(" + t.elem.ref(names) + ")"
	case kFixed:
		return fmt.Sprintf("tbl.FixedList(%s, %d)", t.elem.ref(names), t.n)
	case kOption:
		return "tbl.Option(" + t.elem.ref(names) + ")"
	case kResult:
		return "tbl.Result(" + t.ok.armRef(names) + ", " + t.err.armRef(names) + ")"
	case kTuple:
		parts := make([]string, len(t.elems))
		for i, e := range t.elems {
			parts[i] = e.ref(names)
		}
		return "tbl.Tuple(" + strings.Join(parts, ", ") + ")"
	case kRecord:
		var parts []string
		for _, m := range t.members {
			parts = append(parts, fmt.Sprintf("%q", m.wit), m.t.ref(names))
		}
		return "tbl.Record(" + strings.Join(parts, ", ") + ")"
	case kVariant:
		var parts []string
		for _, m := range t.members {
			if m.t == nil {
				parts = append(parts, fmt.Sprintf("component.VariantCaseSpec{Name: %q}", m.wit))
			} else {
				parts = append(parts, fmt.Sprintf("component.VariantCaseSpec{Name: %q, Type: %s}", m.wit, m.t.ref(names)))
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
		if t.own {
			return "tbl.Own(" + t.res.tagGo + ")"
		}
		return "tbl.Borrow(" + t.res.tagGo + ")"
	}
	return "tbl.Map(" + t.key.ref(names) + ", " + t.elem.ref(names) + ")"
}

func (t *typ) armRef(names map[*typ]string) string {
	if t == nil {
		return "component.TypeRef{}"
	}
	return t.ref(names)
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
		return pascal(b.f.Resource) + acc + pascal(m)
	}
	return acc + pascal(name)
}

// goParams is the method's parameter list after ctx, and the names used.
func (b *fn) goParams() (string, []string) {
	var parts, names []string
	seen := map[string]bool{"ctx": true}
	for _, p := range b.params {
		n := camel(p.wit)
		if reservedNames[n] || seen[n] {
			n += "_"
		}
		seen[n] = true
		names = append(names, n)
		parts = append(parts, n+" "+p.t.goType())
	}
	return strings.Join(parts, ", "), names
}

func (b *fn) goResults() string {
	if b.result == nil {
		return "error"
	}
	return "(" + b.result.goType() + ", error)"
}

func (g *gen) emitImports(w *writer) {
	if len(g.imports) == 0 {
		return
	}
	w.p("")
	w.p("// Imports are the host's implementations of the world's imports.")
	w.p("type Imports struct {")
	for _, in := range g.imports {
		w.p("%s %s", in.field, in.goName)
	}
	w.p("}")
	for _, in := range g.imports {
		g.emitImport(w, in)
	}
}

func (g *gen) emitImport(w *writer, in *iface) {
	w.p("")
	if in.name != "" {
		w.p("// %s is the host's implementation of %s.", in.goName, in.name)
	} else {
		w.p("// %s is the host's implementation of the functions world %s imports itself.", in.goName, g.world.Name)
	}
	w.p("type %s interface {", in.goName)
	names := map[string]bool{}
	for _, b := range in.funcs {
		b.goName = b.methodName()
		for n := b.goName; names[b.goName]; {
			b.goName = n + "_"
		}
		names[b.goName] = true
		if b.dropFor != nil {
			w.p("// %s releases a %s the guest dropped.", b.goName, b.dropFor.goName)
			w.p("%s(ctx context.Context, self %s) error", b.goName, b.dropFor.goName)
			continue
		}
		params, _ := b.goParams()
		if params != "" {
			params = ", " + params
		}
		w.p("%s(ctx context.Context%s) %s", b.goName, params, b.goResults())
	}
	w.p("}")
	w.p("")
	w.p("func options%s(impl %s) []component.Option {", in.goName, in.goName)
	calls := slices.ContainsFunc(in.funcs, func(b *fn) bool { return b.dropFor == nil })
	if calls {
		w.p("tbl := component.NewTypeTable()")
	}
	names2 := map[*typ]string{}
	n := 0
	var define func(t *typ)
	define = func(t *typ) {
		if t == nil {
			return
		}
		for _, c := range []*typ{t.elem, t.key, t.ok, t.err} {
			define(c)
		}
		for _, c := range t.elems {
			define(c)
		}
		for _, m := range t.members {
			define(m.t)
		}
		if t.goName != "" {
			if _, ok := names2[t]; !ok {
				n++
				v := fmt.Sprintf("t%d", n)
				w.p("%s := %s", v, t.ref(names2))
				names2[t] = v
			}
		}
	}
	for _, b := range in.funcs {
		for _, p := range b.params {
			define(p.t)
		}
		define(b.result)
	}
	if calls {
		w.p("resolve := tbl.Resolver()")
	}
	ifaceName := in.name
	w.p("return []component.Option{")
	for _, b := range in.funcs {
		if b.dropFor != nil {
			r := b.dropFor
			w.p("component.WithResourceTag(%q, %q, %s),", ifaceName, r.key.Name, r.tagGo)
			w.p("component.WithHostResourceDtor(%s, func(ctx context.Context, rep uint32) error { return impl.%s(ctx, %s(rep)) }),", r.tagGo, b.goName, r.goName)
			continue
		}
		var refs []string
		for _, p := range b.params {
			refs = append(refs, p.t.ref(names2))
		}
		res := "component.TypeRef{}"
		if b.result != nil {
			res = b.result.ref(names2)
		}
		w.p("component.WithImportCustom(%q, %q, func(ctx context.Context, args []component.Value) ([]component.Value, error) {", ifaceName, b.f.Name)
		w.p("if len(args) != %d { return nil, fmt.Errorf(\"%%s: %%d args\", %q, len(args)) }", len(b.params), b.f.Name)
		_, pn := b.goParams()
		for i, p := range b.params {
			w.p("%s, err := %s", pn[i], p.t.lift(fmt.Sprintf("args[%d]", i)))
			w.p("if err != nil { return nil, fmt.Errorf(\"%%s: %%s: %%w\", %q, %q, err) }", b.f.Name, p.wit)
		}
		call := "impl." + b.goName + "(" + strings.Join(append([]string{"ctx"}, pn...), ", ") + ")"
		if b.result == nil {
			w.p("return nil, %s", call)
		} else {
			w.p("r, err := %s", call)
			w.p("if err != nil { return nil, err }")
			w.p("return []component.Value{%s}, nil", b.result.lower("r"))
		}
		w.p("}, tbl.Func([]component.TypeRef{%s}, %s), resolve),", strings.Join(refs, ", "), res)
	}
	w.p("}")
	w.p("}")
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
			w.p("o = append(o, options%s(imports.%s)...)", in.goName, in.field)
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
	for _, in := range g.exports {
		recv := "x *" + wg
		if in.name != "" {
			acc := in.field
			for taken[acc] {
				acc += "_"
			}
			taken[acc] = true
			w.p("")
			w.p("// %s is the export %s.", in.goName, in.name)
			w.p("type %s struct{ inst *component.Instance }", in.goName)
			w.p("")
			w.p("// %s is the instance's export %s.", acc, in.name)
			w.p("func (x *%s) %s() %s { return %s{x.inst} }", wg, acc, in.goName, in.goName)
			recv = "x " + in.goName
		}
		names := map[string]bool{}
		if in.name == "" {
			names = taken
		}
		for _, b := range in.funcs {
			b.goName = b.methodName()
			for n := b.goName; names[b.goName]; {
				b.goName = n + "_"
			}
			names[b.goName] = true
			params, pn := b.goParams()
			if params != "" {
				params = ", " + params
			}
			var args []string
			for i, p := range b.params {
				args = append(args, p.t.lower(pn[i]))
			}
			call := fmt.Sprintf("x.inst.Call(ctx, %q", b.f.Name)
			if in.name != "" {
				call = fmt.Sprintf("x.inst.CallExport(ctx, %q, %q", in.name, b.f.Name)
			}
			if len(args) > 0 {
				call += ", " + strings.Join(args, ", ")
			}
			call += ")"
			w.p("")
			w.p("// %s calls the export %s.", b.goName, b.f.Name)
			w.p("func (%s) %s(ctx context.Context%s) %s {", recv, b.goName, params, b.goResults())
			if b.result == nil {
				w.p("_, err := %s", call)
				w.p("return err")
			} else {
				w.p("res, err := %s", call)
				w.p("if err != nil { var zero %s; return zero, err }", b.result.goType())
				w.p("if len(res) != 1 { var zero %s; return zero, fmt.Errorf(\"%%s: %%d results\", %q, len(res)) }", b.result.goType(), b.f.Name)
				w.p("return %s", b.result.lift("res[0]"))
			}
			w.p("}")
		}
	}
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
