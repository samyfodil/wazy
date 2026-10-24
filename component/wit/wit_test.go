package wit_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/wit"
)

// everything exercises every type form and every kind of world item.
const everything = `package acme:app@1.0.0;

interface types {
  type bytes = list<u8>;
  record point { x: s32, y: s32 }
  variant shape { none, circle(u32), poly(list<point>) }
  enum color { red, green }
  flags perms { read, write }
  resource blob {
    constructor(data: bytes);
    size: func() -> u64;
    join: static func(a: borrow<blob>, b: borrow<blob>) -> blob;
  }
  resource fallible {
    constructor() -> result<fallible, string>;
  }
}

interface api {
  use types.{point, shape as figure, blob, color, perms};
  type p2 = point;
  type again = p2;
  all: func(a: bool, b: char, c: string, d: f64, e: error-context,
    l: list<point>, o: option<figure>, r: result<_, string>, bare: result,
    t: tuple<u8, p2>, m: map<string, color>, f: future<u32>, fb: future,
    s: stream<u8>, sb: stream, owned: own<blob>, lent: borrow<blob>, plain: blob,
    again: again, pm: perms) -> result<p2>;
  nothing: func();
  later: async func(x: u32) -> u32;
}

world base {
  import tick: func();
  import api;
}

world app {
  import api;
  import types;
  import log: func(msg: string);
  import inline: interface { ping: func() -> u8; }
  record req { body: list<u8> }
  resource local { poke: func(); }
  export handle: func(r: req) -> u16;
  export api;
  export out: interface { pong: func(); }
  include base with { tick as tock }
  include base;
}
`

var blobKey = wit.ResourceKey{Interface: "acme:app/types@1.0.0", Name: "blob"}

func mustParse(t *testing.T, src string) *wit.Package {
	t.Helper()
	pkg, err := wit.Parse("test.wit", src)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func funcNames(fs []*wit.Func) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func itemNames(items []wit.WorldItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func desc(sig *wit.Signature, ref component.TypeRef) component.TypeDesc {
	return sig.Types.Resolver()(*ref.TypeIndex)
}

func TestParse_Interfaces(t *testing.T) {
	pkg := mustParse(t, everything)
	if pkg.Name != "acme:app@1.0.0" || len(pkg.Interfaces) != 2 || len(pkg.Worlds) != 2 || len(pkg.Deps) != 0 {
		t.Fatalf("package %s: %d interfaces, %d worlds, %d deps", pkg.Name, len(pkg.Interfaces), len(pkg.Worlds), len(pkg.Deps))
	}
	types := pkg.Interface("types")
	if types == nil || types != pkg.Interface("acme:app/types@1.0.0") || types.Name != "acme:app/types@1.0.0" {
		t.Fatalf("Interface by short and qualified name disagree: %v", types)
	}
	if pkg.Interface("nope") != nil || pkg.World("nope") != nil || types.Func("nope") != nil {
		t.Error("lookup of a missing name returned something")
	}
	want := []string{"[constructor]blob", "[method]blob.size", "[static]blob.join", "[constructor]fallible"}
	if got := funcNames(types.Funcs); !slices.Equal(got, want) {
		t.Errorf("types funcs %v, want %v", got, want)
	}
	if len(types.Resources) != 2 || types.Resources[0].Name != "blob" || types.Resources[0].Interface != types.Name ||
		len(types.Resources[0].Funcs) != 3 || types.Resources[1].Name != "fallible" {
		t.Errorf("resources %+v", types.Resources)
	}
	kinds := []wit.FuncKind{wit.Constructor, wit.Method, wit.Static, wit.Constructor}
	for i, f := range types.Funcs {
		if f.Kind != kinds[i] || f.Resource == "" || f.Interface != types.Name {
			t.Errorf("%s: kind %v resource %q interface %q", f.Name, f.Kind, f.Resource, f.Interface)
		}
	}
}

func TestSignature_ResourceFuncs(t *testing.T) {
	types := mustParse(t, everything).Interface("types")
	tags := wit.Tags{blobKey: 7, {Interface: types.Name, Name: "fallible"}: 8}

	ctor, err := types.Func("[constructor]blob").Signature(tags)
	if err != nil {
		t.Fatal(err)
	}
	// constructor(data: bytes) -> own<blob>, bytes being an alias of list<u8>.
	if _, ok := desc(ctor, ctor.Desc.Params[0].Type).(component.ListDesc); !ok || ctor.Desc.Params[0].Name != "data" {
		t.Errorf("constructor param %+v", ctor.Desc.Params[0])
	}
	if d := desc(ctor, *ctor.Desc.Results.Unnamed); d != (component.OwnDesc{ResourceType: 7}) {
		t.Errorf("constructor result %#v, want own<blob> tagged 7", d)
	}
	if ctor.TypeNames[*ctor.Desc.Params[0].Type.TypeIndex] != "bytes" || ctor.TypeNames[*ctor.Desc.Results.Unnamed.TypeIndex] != "blob" {
		t.Errorf("type names %v", ctor.TypeNames)
	}

	size, err := types.Func("[method]blob.size").Signature(tags)
	if err != nil {
		t.Fatal(err)
	}
	if len(size.Desc.Params) != 1 || size.Desc.Params[0].Name != "self" ||
		desc(size, size.Desc.Params[0].Type) != (component.BorrowDesc{ResourceType: 7}) {
		t.Errorf("method params %+v, want self: borrow<blob>", size.Desc.Params)
	}

	join, err := types.Func("[static]blob.join").Signature(tags)
	if err != nil {
		t.Fatal(err)
	}
	// Both params borrow the same resource, so they share one slot.
	if len(join.Desc.Params) != 2 || *join.Desc.Params[0].Type.TypeIndex != *join.Desc.Params[1].Type.TypeIndex {
		t.Errorf("static params %+v, want two borrows sharing a slot", join.Desc.Params)
	}

	fallible, err := types.Func("[constructor]fallible").Signature(tags)
	if err != nil {
		t.Fatal(err)
	}
	res, ok := desc(fallible, *fallible.Desc.Results.Unnamed).(component.ResultDesc)
	if !ok || desc(fallible, *res.Ok) != (component.OwnDesc{ResourceType: 8}) || res.Err.Primitive != "string" {
		t.Errorf("constructor with a declared result: %#v", res)
	}

	if _, err := types.Func("[method]blob.size").Signature(nil); err == nil {
		t.Error("a signature reaching an untagged resource built without error")
	}
}

func TestSignature_EveryTypeForm(t *testing.T) {
	api := mustParse(t, everything).Interface("api")
	sig, err := api.Func("all").Signature(wit.Tags{blobKey: 3})
	if err != nil {
		t.Fatal(err)
	}
	p := map[string]component.TypeRef{}
	for _, param := range sig.Desc.Params {
		p[param.Name] = param.Type
	}
	for name, prim := range map[string]string{"a": "bool", "b": "char", "c": "string", "d": "f64", "e": "error-context"} {
		if p[name].Primitive != prim {
			t.Errorf("param %s: %+v, want %s", name, p[name], prim)
		}
	}
	point := component.RecordDesc{Fields: []component.RecordField{{Name: "x", Type: component.Prim("s32")}, {Name: "y", Type: component.Prim("s32")}}}
	if l := desc(sig, p["l"]).(component.ListDesc); !reflect.DeepEqual(desc(sig, l.Element), point) {
		t.Errorf("list<point> element %#v", desc(sig, l.Element))
	}
	o := desc(sig, p["o"]).(component.OptionDesc)
	shape := desc(sig, o.Element).(component.VariantDesc)
	if len(shape.Cases) != 3 || shape.Cases[0].Name != "none" || shape.Cases[0].Type != nil || shape.Cases[1].Type.Primitive != "u32" {
		t.Errorf("option<figure> %#v", shape)
	}
	if r := desc(sig, p["r"]).(component.ResultDesc); r.Ok != nil || r.Err.Primitive != "string" {
		t.Errorf("result<_, string> %#v", r)
	}
	if r := desc(sig, p["bare"]).(component.ResultDesc); r.Ok != nil || r.Err != nil {
		t.Errorf("bare result %#v", r)
	}
	tuple := desc(sig, p["t"]).(component.TupleDesc)
	if len(tuple.Elements) != 2 || tuple.Elements[0].Primitive != "u8" || *tuple.Elements[1].TypeIndex != *desc(sig, p["l"]).(component.ListDesc).Element.TypeIndex {
		t.Errorf("tuple<u8, p2> %#v: p2 should share point's slot", tuple)
	}
	m := desc(sig, p["m"]).(component.MapDesc)
	if m.Key.Primitive != "string" || !reflect.DeepEqual(desc(sig, m.Value), component.EnumDesc{Cases: []string{"red", "green"}}) {
		t.Errorf("map<string, color> %#v", m)
	}
	if f := desc(sig, p["f"]).(component.FutureDesc); f.Element.Primitive != "u32" {
		t.Errorf("future<u32> %#v", f)
	}
	if f := desc(sig, p["fb"]).(component.FutureDesc); f.Element != nil {
		t.Errorf("bare future %#v", f)
	}
	if s := desc(sig, p["s"]).(component.StreamDesc); s.Element.Primitive != "u8" {
		t.Errorf("stream<u8> %#v", s)
	}
	if s := desc(sig, p["sb"]).(component.StreamDesc); s.Element != nil {
		t.Errorf("bare stream %#v", s)
	}
	if desc(sig, p["owned"]) != (component.OwnDesc{ResourceType: 3}) || desc(sig, p["lent"]) != (component.BorrowDesc{ResourceType: 3}) ||
		*p["plain"].TypeIndex != *p["owned"].TypeIndex {
		t.Errorf("own/borrow/plain resource: %v %v %v", desc(sig, p["owned"]), desc(sig, p["lent"]), p["plain"])
	}
	if *p["again"].TypeIndex != *tuple.Elements[1].TypeIndex {
		t.Error("an alias of an alias did not share the record's slot")
	}
	if !reflect.DeepEqual(desc(sig, p["pm"]), component.FlagsDesc{Names: []string{"read", "write"}}) {
		t.Errorf("flags %#v", desc(sig, p["pm"]))
	}
	if r := desc(sig, *sig.Desc.Results.Unnamed).(component.ResultDesc); *r.Ok.TypeIndex != *p["again"].TypeIndex || r.Err != nil {
		t.Errorf("result<p2> %#v", r)
	}
	// The defining name wins over the aliases that reach it.
	if sig.TypeNames[*p["again"].TypeIndex] != "point" || sig.TypeNames[*o.Element.TypeIndex] != "shape" {
		t.Errorf("type names %v", sig.TypeNames)
	}

	nothing, err := api.Func("nothing").Signature(nil)
	if err != nil || nothing.Desc.Params != nil || nothing.Desc.Results.Unnamed != nil {
		t.Errorf("nothing: %+v, %v", nothing, err)
	}
	later, err := api.Func("later").Signature(nil)
	if err != nil || !later.Desc.Async {
		t.Errorf("async func: %+v, %v", later, err)
	}
}

func TestParse_Worlds(t *testing.T) {
	pkg := mustParse(t, everything)
	app := pkg.World("app")
	// api uses types, so types comes first, though app names it second.
	wantImports := []string{"[method]local.poke", "acme:app/types@1.0.0", "acme:app/api@1.0.0", "log", "inline", "tock", "tick"}
	if got := itemNames(app.Imports); !slices.Equal(got, wantImports) {
		t.Errorf("imports %v, want %v", got, wantImports)
	}
	if got := itemNames(app.Exports); !slices.Equal(got, []string{"handle", "acme:app/api@1.0.0", "out"}) {
		t.Errorf("exports %v", got)
	}
	if app.Imports[2].Interface != pkg.Interface("api") || app.Imports[3].Func == nil || app.Imports[3].Func.Interface != "" {
		t.Errorf("import items %+v", app.Imports)
	}
	inline := app.Imports[4].Interface
	if inline.Name != "inline" || inline.Func("ping") == nil || inline.Func("ping").Interface != "inline" {
		t.Errorf("inline interface %+v", inline)
	}
	if tock := app.Imports[5].Func; tock.Name != "tock" || pkg.World("base").Imports[0].Func.Name != "tick" {
		t.Error("include rename touched the included world or missed the copy")
	}
	poke, err := app.Imports[0].Func.Signature(wit.Tags{{Name: "local"}: 1})
	if err != nil || desc(poke, poke.Desc.Params[0].Type) != (component.BorrowDesc{ResourceType: 1}) {
		t.Errorf("world-level resource method: %+v, %v", poke, err)
	}
	handle, err := app.Exports[0].Func.Signature(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := desc(handle, handle.Desc.Params[0].Type).(component.RecordDesc); !ok || handle.Desc.Results.Unnamed.Primitive != "u16" {
		t.Errorf("handle: %+v", handle.Desc)
	}
}

// depsFS is a package split over two files, with a dep directory and a
// single-file dep, plus files ParseFS must ignore.
func depsFS() fstest.MapFS {
	return fstest.MapFS{
		"wit/a.wit": {Data: []byte(`
use wasi:io/streams@0.2.0;
use acme:single/x as sx;
use other as local-alias;
interface host {
  use streams.{input-stream as stream-in};
  use sx.{token};
  use local-alias.{thing};
  read: func(s: borrow<stream-in>, t: token, th: thing) -> own<stream-in>;
}`)},
		"wit/b.wit": {Data: []byte(`package acme:app@1.0.0;
interface other { type thing = u8; }
world w { import wasi:io/streams@0.2.0; import host; }`)},
		"wit/notes.txt":                 {Data: []byte("not wit")},
		"wit/sub/ignored.wit":           {Data: []byte("not even parsed")},
		"wit/deps/README.md":            {Data: []byte("ignored")},
		"wit/deps/wasi-io/streams.wit":  {Data: []byte(`package wasi:io@0.2.0; interface streams { use error.{err}; resource input-stream { read: func(n: u64) -> result<list<u8>, err>; } }`)},
		"wit/deps/wasi-io/error.wit":    {Data: []byte(`package wasi:io@0.2.0; interface error { type err = string; }`)},
		"wit/deps/single.wit":           {Data: []byte(`package acme:single; interface x { type token = u32; }`)},
		"wit/deps/wasi-io/sub/skip.txt": {Data: []byte("")},
	}
}

func TestParseFS_Deps(t *testing.T) {
	pkg, err := wit.ParseFS(depsFS(), "wit")
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "acme:app@1.0.0" || len(pkg.Deps) != 2 || pkg.Deps[0].Name != "acme:single" || pkg.Deps[1].Name != "wasi:io@0.2.0" {
		t.Fatalf("package %s with deps %+v", pkg.Name, pkg.Deps)
	}
	streams := pkg.Interface("wasi:io/streams@0.2.0")
	if streams == nil || streams != pkg.Deps[1].Interface("streams") {
		t.Fatal("dep interface not found by qualified name")
	}
	// Elaborated as wit-parser does: what host uses comes in ahead of it.
	wantImports := []string{"wasi:io/error@0.2.0", "wasi:io/streams@0.2.0", "acme:single/x", "acme:app/other@1.0.0", "acme:app/host@1.0.0"}
	if got := itemNames(pkg.World("w").Imports); !slices.Equal(got, wantImports) {
		t.Errorf("world imports %v", got)
	}

	inKey := wit.ResourceKey{Interface: "wasi:io/streams@0.2.0", Name: "input-stream"}
	sig, err := pkg.Interface("host").Func("read").Signature(wit.Tags{inKey: 5})
	if err != nil {
		t.Fatal(err)
	}
	if desc(sig, sig.Desc.Params[0].Type) != (component.BorrowDesc{ResourceType: 5}) ||
		sig.Desc.Params[1].Type.Primitive != "u32" || sig.Desc.Params[2].Type.Primitive != "u8" ||
		desc(sig, *sig.Desc.Results.Unnamed) != (component.OwnDesc{ResourceType: 5}) {
		t.Errorf("read: %+v", sig.Desc)
	}
	if sig.TypeNames[*sig.Desc.Params[0].Type.TypeIndex] != "input-stream" {
		t.Errorf("a renamed resource keeps its own name: %v", sig.TypeNames)
	}
	read, err := streams.Func("[method]input-stream.read").Signature(wit.Tags{inKey: 5})
	if err != nil {
		t.Fatal(err)
	}
	res := desc(read, *read.Desc.Results.Unnamed).(component.ResultDesc)
	if res.Err.Primitive != "string" {
		t.Errorf("a use within a dep: %#v", res)
	}
}

func TestParseDir_AndFile(t *testing.T) {
	dir := t.TempDir()
	for name, f := range depsFS() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg, err := wit.ParseDir(filepath.Join(dir, "wit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Deps) != 2 || pkg.Interface("host") == nil {
		t.Errorf("ParseDir: %+v", pkg)
	}

	single, err := wit.ParseFile(filepath.Join(dir, "wit", "deps", "single.wit"))
	if err != nil {
		t.Fatal(err)
	}
	if single.Name != "acme:single" || single.Interface("acme:single/x") == nil {
		t.Errorf("ParseFile: %+v", single)
	}
	if _, err := wit.ParseFile(filepath.Join(dir, "missing.wit")); err == nil {
		t.Error("ParseFile of a missing file succeeded")
	}
}

// Parse a WIT source, list a world's imports and exports, and build one
// import's signature for component.WithImportCustom.
func Example() {
	pkg, err := wit.Parse("app.wit", `
package acme:api@1.0.0;

interface host {
  record query { name: string, limit: u32 }
  lookup: func(q: query) -> list<string>;
}

world app {
  import host;
  export run: func() -> string;
}`)
	if err != nil {
		panic(err)
	}
	app := pkg.World("app")
	for _, imp := range app.Imports {
		fmt.Println("import", imp.Name)
	}
	for _, exp := range app.Exports {
		fmt.Println("export", exp.Name)
	}

	host := pkg.Interface("host")
	sig, err := host.Func("lookup").Signature(nil)
	if err != nil {
		panic(err)
	}
	q := sig.Desc.Params[0]
	fmt.Println("lookup param", q.Name, "is a", sig.TypeNames[*q.Type.TypeIndex])
	// Register it with component.WithImportCustom(host.Name, "lookup", fn, sig.Desc, sig.Types.Resolver()).

	// Output:
	// import acme:api/host@1.0.0
	// export run
	// lookup param q is a query
}

// Exports that share a dependency the world does not export: it is imported
// once, ahead of nothing, and both exports come after it.
func TestParse_ElaborateSharedExportDep(t *testing.T) {
	pkg := mustParse(t, `package a:b;
interface d { type t = u32; }
interface e1 { use d.{t}; f: func(x: t); }
interface e2 { use d.{t}; }
world w { export e1; export e2; }`)
	w := pkg.World("w")
	if got := itemNames(w.Imports); !slices.Equal(got, []string{"a:b/d"}) {
		t.Errorf("imports %v", got)
	}
	if got := itemNames(w.Exports); !slices.Equal(got, []string{"a:b/e1", "a:b/e2"}) {
		t.Errorf("exports %v", got)
	}
}

// An own or borrow may name a resource through an alias.
func TestSignature_HandleThroughAlias(t *testing.T) {
	pkg := mustParse(t, `package a:b;
interface i {
  resource r;
  type r2 = r;
  f: func(x: borrow<r2>) -> own<r2>;
}`)
	key := wit.ResourceKey{Interface: "a:b/i", Name: "r"}
	sig, err := pkg.Interface("i").Func("f").Signature(wit.Tags{key: 7})
	if err != nil {
		t.Fatal(err)
	}
	if desc(sig, sig.Desc.Params[0].Type) != (component.BorrowDesc{ResourceType: 7}) ||
		desc(sig, *sig.Desc.Results.Unnamed) != (component.OwnDesc{ResourceType: 7}) {
		t.Errorf("signature %+v", sig.Desc)
	}
}

// include ... with renames an inline interface along with everything in it:
// its funcs and resources say the new name, and the resource's tag is keyed
// by it, since that is the name the component imports it by.
func TestInclude_RenamesInlineInterface(t *testing.T) {
	pkg := mustParse(t, `package a:b;
world a {
  import x: interface {
    resource r {
      constructor();
      m: func();
    }
    f: func(y: borrow<r>);
  }
}
world b { include a with { x as y } }
world c { include b with { y as z } }`)
	orig := pkg.World("a").Imports[0].Interface
	for _, name := range []string{"y", "z"} {
		w := pkg.World(map[string]string{"y": "b", "z": "c"}[name])
		if len(w.Imports) != 1 || w.Imports[0].Name != name {
			t.Fatalf("%s: imports %v", name, itemNames(w.Imports))
		}
		i := w.Imports[0].Interface
		if i == orig || i.Name != name || len(i.Resources) != 1 || i.Resources[0].Interface != name {
			t.Fatalf("%s: interface %+v", name, i)
		}
		m := i.Func("[method]r.m")
		if m == nil || m.Interface != name || i.Resources[0].Funcs[1] != m {
			t.Fatalf("%s: method %+v", name, m)
		}
		sig, err := m.Signature(wit.Tags{{Interface: name, Name: "r"}: 9})
		if err != nil {
			t.Fatal(err)
		}
		if desc(sig, sig.Desc.Params[0].Type) != (component.BorrowDesc{ResourceType: 9}) {
			t.Errorf("%s: self %+v", name, sig.Desc.Params[0])
		}
		if _, err := m.Signature(wit.Tags{{Interface: "x", Name: "r"}: 9}); err == nil {
			t.Errorf("%s: the old name's tag still applies", name)
		}
	}
	if orig.Name != "x" || orig.Func("f").Interface != "x" {
		t.Errorf("the included world's interface changed: %+v", orig)
	}
}

// Renaming a world's resource renames its funcs and every use of it in the
// world's funcs, through any number of includes; renaming a record renames
// it in signatures.
func TestInclude_RenamesWorldTypes(t *testing.T) {
	pkg := mustParse(t, `package a:b;
interface i { type t = u32; }
world a {
  resource r {
    constructor();
    m: func();
  }
  record p { q: u32 }
  use i.{t};
  import f: func(x: r, y: p, z: t) -> option<r>;
}
world b { include a with { r as s, p as p2, t as t2 } }
world c { include b with { s as u } }`)
	for world, res := range map[string]string{"a": "r", "b": "s", "c": "u"} {
		w := pkg.World(world)
		want := []string{"[constructor]" + res, "[method]" + res + ".m", "f"}
		if got := itemNames(w.Imports); !slices.Equal(got[1:], want) || got[0] != "a:b/i" {
			t.Fatalf("%s: imports %v", world, got)
		}
		m := w.Imports[2].Func
		if m.Resource != res {
			t.Errorf("%s: method of %q", world, m.Resource)
		}
		tags := wit.Tags{{Name: res}: 4}
		if _, err := m.Signature(tags); err != nil {
			t.Errorf("%s: method: %v", world, err)
		}
		sig, err := w.Imports[3].Func.Signature(tags)
		if err != nil {
			t.Fatalf("%s: %v", world, err)
		}
		names := slices.Sorted(maps.Values(sig.TypeNames))
		wantNames := map[string][]string{"a": {"p", "r"}, "b": {"p2", "s"}, "c": {"p2", "u"}}[world]
		if !slices.Equal(names, wantNames) {
			t.Errorf("%s: type names %v, want %v", world, names, wantNames)
		}
		if desc(sig, sig.Desc.Params[0].Type) != (component.OwnDesc{ResourceType: 4}) {
			t.Errorf("%s: param %+v", world, sig.Desc.Params[0])
		}
	}
}

// list<T, N> keeps N: each lowers to a FixedListDesc, nested or not.
func TestSignature_FixedLengthList(t *testing.T) {
	pkg := mustParse(t, `package a:b;
interface i {
  record point { x: s32, y: s32 }
  type quad = list<u32, 4>;
  f: func(a: quad, b: list<string, 2>, c: list<list<u8, 3>, 2>) -> list<point, 2>;
}`)
	sig, err := pkg.Interface("i").Func("f").Signature(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := sig.Desc.Params
	if desc(sig, p[0].Type) != (component.FixedListDesc{Element: component.Prim("u32"), Length: 4}) ||
		desc(sig, p[1].Type) != (component.FixedListDesc{Element: component.Prim("string"), Length: 2}) {
		t.Errorf("params %#v, %#v", desc(sig, p[0].Type), desc(sig, p[1].Type))
	}
	outer, ok := desc(sig, p[2].Type).(component.FixedListDesc)
	if !ok || outer.Length != 2 || desc(sig, outer.Element) != (component.FixedListDesc{Element: component.Prim("u8"), Length: 3}) {
		t.Errorf("nested %#v", desc(sig, p[2].Type))
	}
	res, ok := desc(sig, *sig.Desc.Results.Unnamed).(component.FixedListDesc)
	if !ok || res.Length != 2 || sig.TypeNames[*res.Element.TypeIndex] != "point" {
		t.Errorf("result %#v, names %v", res, sig.TypeNames)
	}
	if sig.TypeNames[*p[0].Type.TypeIndex] != "quad" {
		t.Errorf("alias name %v", sig.TypeNames)
	}
}

// A Signature builds what a parse only checked, so it fails where the
// binary format or the host's tags do.
func TestSignature_Errors(t *testing.T) {
	pkg := mustParse(t, `package a:b;
interface i {
  resource r;
  variant v { a(own<r>) }
  owned: func() -> r;
  pair: func(x: tuple<u8, own<r>>);
  fallible: func(x: result<_, own<r>>);
  shaped: func(x: v);
  empty: func(x: list<u8, 0>);
}
world w {
  resource local;
  import make: func() -> local;
}`)
	for name, want := range map[string]string{
		"owned":    "a:b/i#owned: result: no tag for resource r of interface a:b/i",
		"pair":     "a:b/i#pair: param x: no tag for resource r of interface a:b/i",
		"fallible": "a:b/i#fallible: param x: no tag for resource r of interface a:b/i",
		"shaped":   "a:b/i#shaped: param x: interface a:b/i: type v: case a: no tag for resource r of interface a:b/i",
		"empty":    "a:b/i#empty: param x: list<u8, 0>: a fixed-length list needs at least one element",
	} {
		if _, err := pkg.Interface("i").Func(name).Signature(nil); err == nil || err.Error() != "wit: "+want {
			t.Errorf("%s: %v\nwant %s", name, err, want)
		}
	}
	if _, err := pkg.World("w").Imports[0].Func.Signature(nil); err == nil || err.Error() != "wit: make: result: no tag for resource local of world w" {
		t.Errorf("world func: %v", err)
	}
}
