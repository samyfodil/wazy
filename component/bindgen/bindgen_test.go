package bindgen

import (
	"embed"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/component/wit"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// The fixtures' WIT and their committed bindings, embedded: a scratch or
// BSD builder runs the test binary without the source tree.
//
//go:embed internal/kitchen/guest/wit internal/kitchen/bindings.go
//go:embed internal/convtest/conv.wit internal/convtest/bindings.go
//go:embed internal/storebind/store.wit internal/storebind/bindings.go
//go:embed internal/storebind/tree internal/storebind/with
var fixtures embed.FS

// The committed bindings of each fixture are what Generate writes today: a
// change to the generator shows up as a diff in them, and their packages'
// tests then run the new output against real guests.
func TestGolden(t *testing.T) {
	for _, c := range []struct{ wit, world, pkg string }{
		{"internal/storebind/store.wit", "app", "storebind"},
		{"internal/kitchen/guest/wit", "kitchen", "kitchen"},
		{"internal/convtest/conv.wit", "conv", "convtest"},
	} {
		t.Run(c.pkg, func(t *testing.T) {
			got, err := generate(c.wit, Options{Package: c.pkg, World: c.world})
			require.NoError(t, err)
			want, err := fixtures.ReadFile("internal/" + c.pkg + "/bindings.go")
			require.NoError(t, err)
			if string(want) != string(got) {
				t.Fatalf("internal/%s/bindings.go is stale: regenerate it with\n"+
					"  go run ./cmd/wazy-bindgen -wit component/bindgen/%s -world %s -package %s -o component/bindgen/internal/%s/bindings.go",
					c.pkg, c.wit, c.world, c.pkg, c.pkg)
			}
		})
	}
}

// generate parses the fixture WIT at path, a file or a directory of them.
// The store fixture as a tree, and as one file using the tree's kv, are
// what GenerateTree and Generate With write today.
func TestGoldenTree(t *testing.T) {
	const base = "github.com/samyfodil/wazy/component/bindgen/internal/storebind/tree"
	files, err := generateTree(t, "internal/storebind/store.wit", Options{Package: "app", World: "app"}, base)
	require.NoError(t, err)
	require.Equal(t, 2, len(files))
	with, err := generate("internal/storebind/store.wit", Options{
		Package: "with", World: "app",
		With: map[string]string{"example:store/kv": base + "/example/store/v1.0.0/kv"},
	})
	require.NoError(t, err)
	files = append(files, File{Path: "../with/bindings.go", Src: with})
	for _, f := range files {
		want, err := fixtures.ReadFile(path.Clean("internal/storebind/tree/" + f.Path))
		require.NoError(t, err)
		if string(want) != string(f.Src) {
			t.Fatalf("%s is stale: regenerate it with cmd/wazy-bindgen, as storebind/shared_test.go says", f.Path)
		}
	}
}

// The kitchen world as a tree, and as one file using the tree's interface
// packages, compiles: streams, futures, async, handles nested and not, and
// error contexts, across packages.
func TestGenerateKitchenTree(t *testing.T) {
	const base = "example.com/k"
	files, err := generateTree(t, "internal/kitchen/guest/wit", Options{Package: "kitchen", World: "kitchen"}, base)
	require.NoError(t, err)
	typecheckTree(t, base, files)
	with, err := generate("internal/kitchen/guest/wit", Options{
		Package: "with", World: "kitchen",
		With: map[string]string{
			"wazy:bindgen-test/types": base + "/wazy/bindgen-test/v0.1.0/types",
			"wazy:bindgen-test/host":  base + "/wazy/bindgen-test/v0.1.0/host",
		},
	})
	require.NoError(t, err)
	var shared []File
	for _, f := range files {
		if !strings.HasPrefix(f.Path, "kitchen/") {
			shared = append(shared, f)
		}
	}
	typecheckTree(t, base, append(shared, File{Path: "with/bindings.go", Src: with}))
	require.False(t, strings.Contains(string(with), "type Point struct"), "the types are the tree's")
}

func generate(path string, opts Options) ([]byte, error) {
	var pkg *wit.Package
	var err error
	if strings.HasSuffix(path, ".wit") {
		var src []byte
		if src, err = fixtures.ReadFile(path); err == nil {
			pkg, err = wit.Parse(path, string(src))
		}
	} else {
		pkg, err = wit.ParseFS(fixtures, path)
	}
	if err != nil {
		return nil, err
	}
	return Generate(pkg, opts)
}

// generateTree is GenerateTree on the fixture WIT at path.
func generateTree(t *testing.T, path string, opts Options, base string) ([]File, error) {
	t.Helper()
	var pkg *wit.Package
	var err error
	if strings.HasSuffix(path, ".wit") {
		var src []byte
		if src, err = fixtures.ReadFile(path); err == nil {
			pkg, err = wit.Parse(path, string(src))
		}
	} else {
		pkg, err = wit.ParseFS(fixtures, path)
	}
	if err != nil {
		return nil, err
	}
	return GenerateTree(pkg, opts, base)
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
	srcImporter = sourceImporter()
)

// typecheckTree fails t unless every package of files, a GenerateTree
// output under base, compiles against wazy and the others.
func typecheckTree(t *testing.T, base string, files []File) {
	t.Helper()
	if !haveToolchain() {
		t.Log("no Go toolchain to type-check the generated code against")
		return
	}
	ti := &treeImporter{files: map[string][]byte{}, pkgs: map[string]*types.Package{}}
	for _, f := range files {
		ti.files[base+"/"+path.Dir(f.Path)] = f.Src
	}
	for p := range ti.files {
		if _, err := ti.Import(p); err != nil {
			t.Fatalf("%s does not type-check: %v\n%s", p, err, ti.files[p])
		}
	}
}

// treeImporter type-checks a tree's packages from memory, and everything
// else from source.
type treeImporter struct {
	files map[string][]byte
	pkgs  map[string]*types.Package
}

func (ti *treeImporter) Import(p string) (*types.Package, error) {
	if pkg, ok := ti.pkgs[p]; ok {
		return pkg, nil
	}
	src, ok := ti.files[p]
	if !ok {
		return srcImporter.Import(p)
	}
	f, err := parser.ParseFile(srcFset, p+"/bindings.go", src, 0)
	if err != nil {
		return nil, err
	}
	pkg, err := (&types.Config{Importer: ti}).Check(p, srcFset, []*ast.File{f}, nil)
	if err != nil {
		return nil, err
	}
	ti.pkgs[p] = pkg
	return pkg, nil
}

var haveToolchain = sync.OnceValue(func() bool {
	_, err := os.Stat(filepath.Join(build.Default.GOROOT, "src", "context"))
	if err != nil {
		return false
	}
	_, err = exec.LookPath(filepath.Join(build.Default.GOROOT, "bin", "go"))
	return err == nil
})

// sourceImporter type-checks imports from source with cgo off, so net and
// the rest need no C toolchain, which the 386, scratch and BSD builders lack.
func sourceImporter() types.Importer {
	build.Default.CgoEnabled = false
	return importer.ForCompiler(srcFset, "source", nil)
}

// typecheck fails t unless src is a Go package that compiles against wazy.
// It needs the toolchain and its sources, as it type-checks the imports from
// source: a scratch image ships the test binary alone, and the generated
// code is checked on every builder that has them.
func typecheck(t *testing.T, src []byte) {
	t.Helper()
	if !haveToolchain() {
		t.Log("no Go toolchain to type-check the generated code against")
		return
	}
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
		{"stream of error-context", `package a:b; world w { export f: func(s: stream<error-context>); }`, "f: param s: a stream of error-context: an element holding"},
		{"of streams", `package a:b; world w { export f: func() -> stream<stream<u8>>; }`, "f: result: a stream of stream<u8>: an element holding a resource handle, stream, future or error-context is not supported"},
		{
			"of handles", `package a:b; interface i { resource r; f: func() -> future<r>; } world w { export i; }`,
			"a:b/i#f: result: a future of r: an element holding",
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

// Every stream<T> and future<T> is a typed reader, whichever way it crosses;
// the world makes them, one constructor per type, for the host to write.
func TestGenerateStreams(t *testing.T) {
	src := `package a:b;
interface types { record q { n: u32, tags: list<string> } type bytes = stream<u8>; }
interface host {
  use types.{q, bytes};
  pull: async func(n: u32) -> bytes;
  push: func(s: stream<q>) -> u32;
  later: func() -> future;
  wait: async func(f: future<option<q>>);
}
interface api {
  use types.{q};
  feed: async func(s: stream<u8>, f: future<list<u16>>) -> u32;
  make: async func() -> stream<q>;
  soon: func() -> future<result<q, string>>;
  ticks: func(s: stream);
  odd: func(a: stream<tuple<u8, string>>, b: stream<map<string, u8>>, c: stream<list<u8, 2>>, d: stream<result>);
}
world w { import host; export api; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"Pull(ctx context.Context, n uint32, ret witgo.Async[*witgo.StreamReader[uint8]]) error",
		"Push(ctx context.Context, s *witgo.StreamReader[Q]) (uint32, error)",
		"Later(ctx context.Context) (*witgo.FutureReader[struct{}], error)",
		"Wait(ctx context.Context, f *witgo.FutureReader[*Q], ret witgo.Async[struct{}]) error",
		"s, err := witgo.ReaderOf(args[0], func(v component.Value) (Q, error) { return liftQ(nil, v) })",
		"return []component.Value{r.Raw()}, nil",
		"tbl.Func([]component.TypeRef{tbl.Stream(t1)}, component.Prim(\"u32\"))",
		"tbl.Future(component.TypeRef{})",
		"func (x API) Feed(ctx context.Context, s *witgo.StreamReader[uint8], f *witgo.FutureReader[[]uint16]) (uint32, error) {",
		`x.inst.CallExport(ctx, "a:b/api", "feed", s.Raw(), f.Raw())`,
		`return witgo.ReaderOf(res[0], func(v component.Value) (Q, error) { return liftQ(nil, v) })`,
		`return witgo.FutureReaderOf(res[0], func(v component.Value) (witgo.Result[Q, string], error) { return liftT`,
		"func (x *W) NewStreamU8() (*witgo.StreamWriter[uint8], *witgo.StreamReader[uint8], error) {",
		"func (x *W) NewStreamQ() (*witgo.StreamWriter[Q], *witgo.StreamReader[Q], error) {",
		"func (x *W) NewFuture() (*witgo.FutureWriter[struct{}], *witgo.FutureReader[struct{}], error) {",
		"return witgo.NewFuture(x.inst, nil, nil, witgo.Unit, witgo.LowerUnit)",
		"func (x *W) NewFutureOptionQ() (",
		"func (x *W) NewFutureListU16() (",
		"component.ListOf[uint16], func(x []uint16) component.Value { return x })",
		"func (x *W) NewFutureResultQString() (",
		"func (x *W) NewStream() (",
		"func (x *W) NewStreamTupleU8String() (",
		"func (x *W) NewStreamMapStringU8() (",
		"func (x *W) NewStreamList2U8() (",
		"func (x *W) NewStreamResult() (",
		"// NewStreamTupleU8String makes a stream<tuple<u8, string>>: write to the writer, and pass the reader to the",
	} {
		require.Contains(t, out, want)
	}
	// One element type serves every stream and future of it.
	require.Equal(t, 1, strings.Count(out, "elemTypes.t[0] = component.Prim(\"u8\")"))
}

// A stream, future or error-context nested in another type converts with the
// rest of it, in either direction.
func TestGenerateNestedAsync(t *testing.T) {
	src := `package a:b;
interface types {
  record body { data: stream<u8>, trailer: future<u32> }
  variant event { data(stream<string>), done, failed(error-context) }
}
interface host {
  use types.{body, event};
  open: async func(n: u32) -> result<tuple<stream<u8>, future<u32>>, string>;
  sum: async func(b: body) -> u32;
  errors: func(n: u32) -> list<error-context>;
}
interface api {
  use types.{body, event};
  read: async func(b: body) -> u32;
  make: async func(n: u32) -> body;
  watch: func(e: list<event>) -> map<string, error-context>;
  maybe: func() -> option<future<string>>;
}
world w { import host; export api; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"type Body struct {\n\tData    *witgo.StreamReader[uint8]\n\tTrailer *witgo.FutureReader[uint32]\n}",
		"if out.Data, err = witgo.ReaderOf(vs[0], witgo.U8); err != nil {",
		"if out.Trailer, err = witgo.FutureReaderOf(vs[1], witgo.U32); err != nil {",
		"return []component.Value{x.Data.Raw(), x.Trailer.Raw()}",
		"func EventFailed(v *component.ErrorContext) Event {",
		"p, err := witgo.ErrorContextOf(vv.Payload)",
		"Open(ctx context.Context, n uint32, ret witgo.Async[witgo.Result[witgo.Tuple2[*witgo.StreamReader[uint8], *witgo.FutureReader[uint32]], string]]) error",
		"Errors(ctx context.Context, n uint32) ([]*component.ErrorContext, error)",
		"func (x API) Watch(ctx context.Context, e []Event) (map[string]*component.ErrorContext, error) {",
		"func (x API) Maybe(ctx context.Context) (**witgo.FutureReader[string], error) {",
		"return (*x).Raw()",
		"func (x *W) NewStreamString() (",
		"func (x *W) NewFutureString() (",
	} {
		require.Contains(t, out, want)
	}
}

// An interface whose name a type already took gets a suffixed Go type, but
// its Imports field and its accessor on the world keep the plain name: they
// only clash with their own kind.
func TestGenerateInterfaceNameClash(t *testing.T) {
	src := `package a:b;
interface types { record store { n: u32 } record cache { n: u32 } }
interface store { use types.{store}; get: func() -> store; }
interface cache { use types.{cache}; put: func(c: cache); }
world w { import store; export cache; }`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"type Store struct {",
		"type Store2 interface {",
		"\tStore Store2\n",
		"o = append(o, optionsStore2(imports.Store)...)",
		"type Cache struct {",
		"type Cache2 struct{ inst *component.Instance }",
		"func (x *W) Cache() Cache2 { return Cache2{x.inst} }",
	} {
		require.Contains(t, out, want)
	}
}

func TestUnique(t *testing.T) {
	set := map[string]bool{}
	for _, want := range []string{"A", "A_", "A__"} {
		require.Equal(t, want, unique(set, "A"))
	}
}

// An imported interface's own package is the same whichever world it is
// generated for: one importing more or less, in another order, skipping it
// or not, binding other interfaces that use its types first.
func TestGenerateTreeShared(t *testing.T) {
	src := `package a:b;
interface types {
  record point { x: s32, label: string }
  variant shape { dot(point), none }
  resource token { constructor(n: u32); peek: func() -> u32; }
  record unused { p: list<point> }
}
interface host {
  use types.{point, shape, token};
  record pair { a: point, b: token }
  mint: func(p: point) -> token;
  pick: func(s: list<shape>) -> option<pair>;
}
interface other { use types.{point}; f: func(p: list<point>) -> u32; }
world first { import host; import other; export run: func() -> u32; }
world second { import other; import types; import host; export api: interface { use types.{point, token}; g: func(p: point, t: token, types: u32) -> list<point>; } }`
	pkg, err := wit.Parse("t.wit", src)
	require.NoError(t, err)
	gen := func(world string, skip []string) map[string]string {
		files, err := GenerateTree(pkg, Options{Package: world, World: world, Skip: skip}, "example.com/b")
		require.NoError(t, err)
		typecheckTree(t, "example.com/b", files)
		out := map[string]string{}
		for _, f := range files {
			out[f.Path] = string(f.Src)
		}
		return out
	}
	a, b := gen("first", nil), gen("second", []string{"a:b/types"})
	for _, p := range []string{"a/b/types/bindings.go", "a/b/host/bindings.go", "a/b/other/bindings.go"} {
		require.NotEqual(t, "", a[p], p)
		if a[p] != b[p] {
			t.Fatalf("%s differs between worlds:\n%s", p, firstDiff(a[p], b[p]))
		}
	}
	types := a["a/b/types/bindings.go"]
	for _, want := range []string{
		"type Unused struct {", "func LiftPoint(h *witgo.Handles, v component.Value) (Point, error) {",
		"type Token uint32", "TagToken uint32 = ", "type Host interface {", "func Options(impl Host) []component.Option {",
		"NewToken(ctx context.Context, n uint32) (Token, error)", "DropToken(ctx context.Context, self Token) error",
	} {
		require.Contains(t, types, want)
	}
	require.Contains(t, a["a/b/host/bindings.go"], `types "example.com/b/a/b/types"`)
	require.Contains(t, a["a/b/host/bindings.go"], "A types.Point")
	world := b["second/bindings.go"]
	for _, want := range []string{
		"\tOther other.Host\n", "\tHost  host.Host\n", "o = append(o, host.Options(imports.Host)...)",
		"func (x API) G(ctx context.Context, p types.Point, t types.Token, types_ uint32) ([]types.Point, error) {",
	} {
		require.Contains(t, world, want)
	}
	require.False(t, strings.Contains(world, "types.Options"), "a skipped interface is left out of Imports")
}

// A tree with an interface mapped to another package writes none of its
// own for it, and its other packages use the mapped one.
func TestGenerateTreeWith(t *testing.T) {
	src := `package a:b;
interface types { record point { x: s32 } }
interface host { use types.{point}; get: func() -> point; }
world w { use types.{point}; import host; export run: func() -> point; }`
	pkg, err := wit.Parse("t.wit", src)
	require.NoError(t, err)
	files, err := GenerateTree(pkg, Options{Package: "w", World: "w", With: map[string]string{"a:b/types": "example.com/shared/types"}}, "example.com/b")
	require.NoError(t, err)
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if f.Path == "a/b/host/bindings.go" {
			require.Contains(t, string(f.Src), `types "example.com/shared/types"`)
			require.Contains(t, string(f.Src), "Get(ctx context.Context) (types.Point, error)")
		}
	}
	require.Equal(t, []string{"a/b/host/bindings.go", "w/bindings.go"}, paths)

	_, err = GenerateTree(pkg, Options{Package: "w", World: "w"}, "")
	require.EqualError(t, err, "bindgen: no base import path")
	_, err = GenerateTree(pkg, Options{Package: "w", World: "nope"}, "example.com/b")
	require.Error(t, err)
}

// Two packages of the same name, imported by one file, get aliases of their
// own; and their two equal records are two types, each its interface's.
func TestGenerateTreeAliases(t *testing.T) {
	src := `package r:x;
world w {
  use a:b/types.{p};
  use c:d/types.{p as q};
  export f: func(a: p, b: q);
}
package a:b { interface types { record p { x: u32 } } }
package c:d { interface types { record p { x: u32 } } }`
	pkg, err := wit.Parse("t.wit", src)
	require.NoError(t, err)
	files, err := GenerateTree(pkg, Options{Package: "w", World: "w"}, "example.com/b")
	require.NoError(t, err)
	typecheckTree(t, "example.com/b", files)
	world := string(files[len(files)-1].Src)
	require.Contains(t, world, `types "example.com/b/a/b/types"`)
	require.Contains(t, world, `types2 "example.com/b/c/d/types"`)
	require.Contains(t, world, "func (x *W) F(ctx context.Context, a types.P, b types2.P) error {")
}

func TestPackageNames(t *testing.T) {
	for name, want := range map[string]string{
		"wasi:io/streams@0.2.0": "wasi/io/v0.2.0/streams",
		"a:b/incoming-handler":  "a/b/incoming-handler",
		"label":                 "",
		"a:b":                   "",
	} {
		got, ok := witPackage(name)
		require.Equal(t, want, got, name)
		require.Equal(t, want != "", ok, name)
	}
	for name, want := range map[string]string{
		"wasi:io/streams@0.2.0":      "streams",
		"wasi:http/incoming-handler": "incominghandler",
		"wasi:io/error@0.2.0":        "ioerror",   // error is predeclared
		"a:my-pkg/type":              "mypkgtype", // type is a keyword
		"a:b/fmt":                    "bfmt",      // fmt is an import of every file
	} {
		require.Equal(t, want, goPackageName(name), name)
	}
}

// A tuple longer than witgo's is a struct of the bindings' own, named for
// its length, or for the WIT alias that names it.
func TestGenerateLongTuple(t *testing.T) {
	src := `package a:b;
interface t { type big = tuple<u8, u8, u8, u8, u8, u8, u8, u8, string>; }
world w {
  use t.{big};
  export f: func(x: tuple<u8, u8, u8, u8, u8, u8, u8, u8, u16, u32>) -> big;
}`
	out, err := genSrc(t, src, Options{})
	require.NoError(t, err)
	for _, want := range []string{
		"// Big is the WIT tuple big, its elements in order.\ntype Big struct {",
		"F8 string",
		"// Tuple10 is a WIT tuple<u8, u8, u8, u8, u8, u8, u8, u8, u16, u32>, its elements in order.",
		"F9 uint32",
		"func (x *W) F(ctx context.Context, x_ Tuple10) (Big, error) {",
		"func liftTuple10(h *witgo.Handles, v component.Value) (Tuple10, error) {",
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
