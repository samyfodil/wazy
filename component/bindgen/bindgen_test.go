package bindgen

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/component/wit"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// The committed bindings of each fixture are what Generate writes today: a
// change to the generator shows up as a diff in them, and their packages'
// tests then run the new output against real guests.
func TestGolden(t *testing.T) {
	for _, c := range []struct{ wit, world, pkg string }{
		{"../../examples/custom-wit/testdata/store.wit", "app", "storebind"},
		{"internal/kitchen/guest/wit", "kitchen", "kitchen"},
		{"internal/convtest/conv.wit", "conv", "convtest"},
	} {
		t.Run(c.pkg, func(t *testing.T) {
			got, err := generate(c.wit, Options{Package: c.pkg, World: c.world})
			require.NoError(t, err)
			want, err := os.ReadFile("internal/" + c.pkg + "/bindings.go")
			require.NoError(t, err)
			if string(want) != string(got) {
				t.Fatalf("internal/%s/bindings.go is stale: regenerate it with\n"+
					"  go run ./cmd/wazy-bindgen -wit %s -world %s -package %s -o component/bindgen/internal/%s/bindings.go",
					c.pkg, c.wit, c.world, c.pkg, c.pkg)
			}
		})
	}
}

func generate(path string, opts Options) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	var pkg *wit.Package
	if fi.IsDir() {
		pkg, err = wit.ParseDir(path)
	} else {
		pkg, err = wit.ParseFile(path)
	}
	if err != nil {
		return nil, err
	}
	return Generate(pkg, opts)
}

func genSrc(t *testing.T, src string, opts Options) (string, error) {
	t.Helper()
	pkg, err := wit.Parse("t.wit", src)
	require.NoError(t, err)
	if opts.Package == "" {
		opts.Package = "p"
	}
	if opts.World == "" {
		opts.World = "w"
	}
	out, err := Generate(pkg, opts)
	if err == nil {
		typecheck(t, out)
	}
	return string(out), err
}

// srcImporter is shared so each check reuses the packages the last imported.
var (
	srcFset     = token.NewFileSet()
	srcImporter = importer.ForCompiler(srcFset, "source", nil)
)

// typecheck fails t unless src is a Go package that compiles against wazy.
func typecheck(t *testing.T, src []byte) {
	t.Helper()
	f, err := parser.ParseFile(srcFset, "bindings.go", src, 0)
	require.NoError(t, err)
	conf := types.Config{Importer: srcImporter}
	if _, err := conf.Check("p", srcFset, []*ast.File{f}, nil); err != nil {
		t.Fatalf("generated code does not type-check: %v\n%s", err, src)
	}
}

// What the bindings cannot express yet fails, naming the function and why,
// rather than generating code that would misbehave.
func TestGenerateRefuses(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{
			"stream", `package a:b; world w { export f: func() -> stream<u8>; }`,
			"f: result: stream types are not supported yet",
		},
		{
			"error-context", `package a:b; world w { export f: func() -> error-context; }`,
			"f: result: type error-context is not supported yet",
		},
		{"in a result", `package a:b; world w { export f: func() -> result<stream<u8>>; }`, "f: result: stream types"},
		{"in an error arm", `package a:b; world w { export f: func() -> result<_, stream<u8>>; }`, "f: result: stream types"},
		{"in a tuple", `package a:b; world w { export f: func() -> tuple<u8, stream<u8>>; }`, "f: result: stream types"},
		{
			"in a record", `package a:b; interface t { record r { s: stream<u8> } } world w { use t.{r}; export f: func() -> r; }`,
			"f: result: field s: stream types",
		},
		{
			"in a variant", `package a:b; interface t { variant v { s(stream<u8>) } } world w { use t.{v}; export f: func() -> v; }`,
			"f: result: case s: stream types",
		},
		{"in a map", `package a:b; world w { export f: func() -> map<string, stream<u8>>; }`, "f: result: stream types"},
		{"in an option", `package a:b; world w { export f: func() -> option<stream<u8>>; }`, "f: result: stream types"},
		{
			"long tuple", `package a:b; world w { export f: func() -> tuple<u8, u8, u8, u8, u8, u8, u8, u8, u8>; }`,
			"f: result: a tuple of 9 elements is longer than witgo has a type for",
		},
		{
			"unbound resource", `package a:b; interface t { resource r; } interface i { use t.{r}; g: func(x: borrow<r>); } world w { import i; }`,
			"a:b/i#g: param x: a handle to a:b/t.r, of an interface left unbound",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			opts := Options{}
			if c.name == "unbound resource" {
				opts.Skip = []string{"a:b/t"}
			}
			_, err := genSrc(t, c.src, opts)
			require.Error(t, err)
			require.Contains(t, err.Error(), c.want)
		})
	}

	pkg, err := wit.Parse("t.wit", `package a:b; world w {}`)
	require.NoError(t, err)
	_, err = Generate(pkg, Options{World: "w"})
	require.EqualError(t, err, "bindgen: no Go package name")
	_, err = Generate(pkg, Options{Package: "p", World: "nope"})
	require.EqualError(t, err, "bindgen: no world \"nope\" in package a:b")
}

// Handles in exports and an exported resource generate: the resource as a Go
// type holding its handle, with its methods; the host's handles converted
// through the instance's table, borrows released after the call.
func TestGenerateExportHandles(t *testing.T) {
	src := `package a:b;
interface t { resource r; }
interface i {
  use t.{r};
  resource blob {
    constructor(init: string);
    read: func() -> string;
    join: static func(a: borrow<blob>, b: borrow<blob>) -> blob;
    drop: func();
  }
  keep: func(x: r) -> list<blob>;
  lend: func(x: borrow<r>, xs: list<borrow<r>>) -> tuple<r, option<r>>;
}
world w { import t; export i; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"type Blob struct {",
		"func (x Blob) Drop(ctx context.Context) error {",
		`return x.inst.DropResource(ctx, "a:b/i", "blob", x.handle)`,
		"func (x Blob) Read(ctx context.Context) (string, error) {",
		`x.inst.CallExport(ctx, "a:b/i", "[method]blob.read", x.handle)`,
		"func (x Blob) Drop_(ctx context.Context) error {",
		"func (x I) NewBlob(ctx context.Context, init string) (Blob, error) {",
		"func (x I) BlobJoin(ctx context.Context, a Blob, b Blob) (Blob, error) {",
		"func (x I) Keep(ctx context.Context, x_ R) ([]Blob, error) {",
		`h := &witgo.Handles{Table: x.inst.ResourcesFor("a:b/i"), Inst: x.inst}`,
		"if rerr := h.Release(); err == nil {",
		"lowerR(h, x_, true)",
		"DropR(ctx context.Context, self R) error",
	} {
		require.Contains(t, out, want)
	}
}

// An import's handles below the top level go through the table the
// resources hook captures; those at the top level are the engine's to
// translate.
func TestGenerateNestedImportHandles(t *testing.T) {
	src := `package a:b;
interface i {
  resource r;
  pair: func() -> tuple<r, r>;
  all: func(xs: list<borrow<r>>, one: borrow<r>) -> result<r, string>;
  plain: func(x: borrow<r>) -> r;
}
world w { import i; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"var table *component.HandleTable",
		"component.WithResourcesHook(func(t *component.HandleTable) { table = t }),",
		"h := &witgo.Handles{Table: table}",
		"xs, err := liftT",
		"one, err := liftR(nil, args[1], false)",
		"x_, err := liftR(nil, args[0], false)",
		"return []component.Value{lowerR(nil, r, false)}, nil",
	} {
		require.Contains(t, out, want)
	}
}

// Async functions generate: an import's host function completes through a
// witgo.Async; an export has its blocking method and a CallAsync form.
func TestGenerateAsync(t *testing.T) {
	src := `package a:b;
interface types { record q { n: u32 } resource r; }
interface host {
  use types.{q, r};
  get: async func(x: u32) -> result<q, string>;
  poke: async func();
  pair: async func() -> tuple<r, r>;
}
interface api {
  use types.{q};
  resource job { constructor(); run: async func(n: u32) -> u32; }
  fetch: async func(x: u32) -> option<q>;
  fire: async func();
}
world w { import host; export api; export top: async func() -> string; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"Get(ctx context.Context, x_ uint32, ret witgo.Async[witgo.Result[Q, string]]) error",
		"Poke(ctx context.Context, ret witgo.Async[struct{}]) error",
		`component.WithAsyncImportCustom("a:b/host", "get", func(ctx context.Context, args []component.Value, call *component.AsyncCall) error {`,
		"ret := witgo.NewAsync(call, func(r struct{}) []component.Value { return nil })",
		"h := &witgo.Handles{Table: table}",
		"func (x API) Fetch(ctx context.Context, x_ uint32) (*Q, error) {",
		"func (x API) FetchAsync(ctx context.Context, x_ uint32) (*witgo.Pending[*Q], error) {",
		`p, err := x.inst.CallAsync(ctx, "a:b/api#fetch", x_)`,
		"func (x API) FireAsync(ctx context.Context) (*witgo.Pending[struct{}], error) {",
		"func (x Job) RunAsync(ctx context.Context, n uint32) (*witgo.Pending[uint32], error) {",
		`x.inst.CallAsync(ctx, "a:b/api#[method]job.run", x.handle, n)`,
		"func (x *W) TopAsync(ctx context.Context) (*witgo.Pending[string], error) {",
		`x.inst.CallAsync(ctx, "top")`,
	} {
		require.Contains(t, out, want)
	}
}

// A world with no imports takes none, and WASI is left to the host by
// default: its interfaces are not bound, while one left out of Skip is.
func TestGenerateImports(t *testing.T) {
	src := `package a:b;
interface wasi-like { f: func() -> u32; }
world w { export g: func(); }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	require.Contains(t, out, "func Instantiate(ctx context.Context, r wazy.Runtime, wasm []byte, opts ...component.Option) (*W, error)")
	require.False(t, strings.Contains(out, "type Imports struct"))
	require.False(t, strings.Contains(out, `"fmt"`))
	require.False(t, strings.Contains(out, "witgo"))

	src = `package wasi:x;
interface y { f: func() -> u32; }
world w { import y; export g: func(); }`
	out, err = genSrc(t, src, Options{})
	require.NoError(t, err)
	require.False(t, strings.Contains(out, "type Imports struct"), "wasi: is skipped by default")
	out, err = genSrc(t, src, Options{Skip: []string{}})
	require.NoError(t, err)
	require.Contains(t, out, "type Y interface {")
}

// Resource tags come from the world's name unless given, and are distinct
// per resource.
func TestGenerateTags(t *testing.T) {
	src := `package a:b;
interface i { resource r; resource s; f: func(x: borrow<r>, y: borrow<s>); }
world w { import i; }`
	out, err := genSrc(t, src, Options{TagBase: 0x100})
	require.NoError(t, err)
	require.Contains(t, out, "tagR uint32 = 0x100")
	require.Contains(t, out, "tagS uint32 = 0x101")
	out, err = genSrc(t, src, Options{})
	require.NoError(t, err)
	require.Contains(t, out, "tagR uint32 = 0x")
	require.False(t, strings.Contains(out, "tagR uint32 = 0x1\n"))
}

// Accessors, constructors, statics and clashing names all get distinct,
// legal Go names.
func TestGenerateNames(t *testing.T) {
	src := `package a:b;
interface i {
  resource r {
    constructor();
    m: func();
    s: static func();
    %close: func();
    prop: get() -> u8;
    prop: set(v: u8);
  }
  record instance { close: u32 }
  f: func(x: borrow<r>);
  size: get() -> u32;
  size: set(v: u32);
  each: func(xs: list<borrow<r>>);
  r-m: func();
}
interface j { record instance { other: u8 } g: func(i: instance); }
world w {
  use i.{instance};
  import i;
  import j;
  export close: func();
  export instance: func() -> instance;
}`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"GetSize(ctx context.Context) (uint32, error)",
		"SetSize(ctx context.Context, v uint32) error",
		"RGetProp(ctx context.Context, self R) (uint8, error)",
		"RSetProp(ctx context.Context, self R, v uint8) error",
		"Each(ctx context.Context, xs []R) error",
		"RM_(ctx context.Context) error",
		"type Instance2 struct",
		"NewR(ctx context.Context) (R, error)",
		"RM(ctx context.Context, self R) error",
		"RS(ctx context.Context) error",
		"RClose(ctx context.Context, self R) error",
		"func (x *W) Close_(ctx context.Context) error",
		"func (x *W) Instance_(ctx context.Context) (Instance2, error)",
		"type Instance struct",
	} {
		require.Contains(t, out, want)
	}
}

// A resource used only by a later interface's functions is still registered,
// with its destructor, by the interface that declares it; one no function
// uses is not; and an interface of types alone is not bound at all.
func TestGenerateResourceElsewhere(t *testing.T) {
	src := `package a:b;
interface t { resource r; resource unused; }
interface only-types { record p { x: u8 } }
interface i { use t.{r}; use only-types.{p}; f: func(q: p) -> r; }
world w { import i; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	require.Contains(t, out, "type T interface {\n\t// DropR releases a R the guest dropped.\n\tDropR(ctx context.Context, self R) error\n}")
	require.Contains(t, out, `component.WithResourceTag("a:b/t", "r", tagR)`)
	require.Contains(t, out, "F(ctx context.Context, q P) (R, error)")
	require.False(t, strings.Contains(out, "Unused"), "an unused resource is not bound")
	require.False(t, strings.Contains(out, "OnlyTypes"), "an interface of types alone is not bound")
}

// An exported interface whose accessor would shadow the instance's own
// methods gets a suffix instead.
func TestGenerateExportedInterfaceNames(t *testing.T) {
	out, err := genSrc(t, `package a:b; interface close { f: func(); } world w { export close; }`, Options{})
	require.NoError(t, err)
	require.Contains(t, out, "func (x *W) Close_() Close {")
	require.Contains(t, out, `x.inst.CallExport(ctx, "a:b/close", "f")`)
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"list-keys": "ListKeys", "user-id": "UserID", "http-api": "HTTPAPI", "x": "X",
		"9lives": "X9lives", "a.b/c": "ABC", "json": "JSON",
	} {
		require.Equal(t, want, pascal(in), in)
	}
	for in, want := range map[string]string{"list-keys": "listKeys", "id": "id", "http-url": "httpURL", "url-path": "urlPath", "9x": "x9x"} {
		require.Equal(t, want, camel(in), in)
	}
	require.Equal(t, "kv", shortName("example:store/kv@1.0.0"))
	require.Equal(t, "plain", shortName("plain"))
	if !strings.HasPrefix(pascal("-"), "X") {
		t.Fatal("an empty name must still be a Go name")
	}
}
