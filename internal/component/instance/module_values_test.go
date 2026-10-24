package instance

import (
	"bytes"
	"context"
	"embed"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// The hand-written components of testdata/module_values (see its README).
//
//go:embed testdata/module_values/*.wasm
var moduleValuesFS embed.FS

func moduleValuesWasm(t *testing.T, name string) []byte {
	t.Helper()
	b, err := moduleValuesFS.ReadFile(path.Join("testdata/module_values", name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func nestedWasm(t *testing.T, n string) []byte {
	t.Helper()
	fetched(t, "testdata/wast/wasmtime-nested")
	b, err := wastFS.ReadFile(path.Join("testdata/wast/wasmtime-nested", "nested."+n+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A module reached through an import or an instance export is planned from
// its definition, and a CompileCache keeps that plan for the next
// instantiation of any component that reaches it.
func TestModuleValues_CachedPlan(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := NewCompileCache()
	defer cache.Close(ctx)
	for _, n := range []string{"5", "5", "14"} {
		in, err := Instantiate(ctx, r, nestedWasm(t, n), WithCompileCache(cache))
		if err != nil {
			t.Fatalf("nested.%s: %v", n, err)
		}
		if err := in.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.byModule) == 0 {
		t.Fatal("no module plan was cached")
	}
}

// A root component's instance import is the embedder's, which supplies
// functions and resources, never a core module: one aliased out of it, and
// passed down, names nothing.
func TestModuleValues_RootImportSuppliesNoModule(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err := Instantiate(ctx, r, nestedWasm(t, "8"))
	requireErrContains(t, err, `instance 0 names the instance import "c", which nothing supplied`)
}

// A runtime that cannot decode a core module cannot plan one, whether it is
// reached statically or through an instance export, cached or not.
func TestModuleValues_RuntimeCannotDecode(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	opaque := struct{ wazy.Runtime }{r} // hides DecodeModuleNoCompile
	cache := NewCompileCache()
	defer cache.Close(ctx)
	fetched(t, "testdata/wast/wasm-tools-instantiate")
	instantiate35, err := wastFS.ReadFile("testdata/wast/wasm-tools-instantiate/instantiate.35.wasm")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		wasm []byte
		opts []Option
	}{
		{"static", instantiate35, nil},
		{"through an instance export", nestedWasm(t, "3"), nil},
		{"through an instance export, cached", nestedWasm(t, "3"), []Option{WithCompileCache(cache)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Instantiate(ctx, opaque, tc.wasm, tc.opts...)
			if err == nil || !strings.Contains(err.Error(), "cannot decode core modules") {
				t.Fatalf("Instantiate = %v, want the runtime refused", err)
			}
		})
	}
}

// An inline bundle passed to a nested component with code of its own carries
// both sorts of thing in: the module, which the nested component's scope
// resolves, and the func, wired in as a func arg would be.
func TestModuleValues_BundleArg(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, moduleValuesWasm(t, "bundle_arg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// A func aliased out of a bundle or a shim, neither instantiated, is the func
// it holds: in a bundle passed on, and as a bare func arg, both calls land in
// the sibling the shim was given. One that is this component's own lift is
// refused when the nested component calls it, and an alias naming nothing
// this component holds is refused when it is wired.
func TestModuleValues_FuncAliasedOutOfBundleOrShim(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	call := func(name string) ([]abi.Value, error) {
		in, err := Instantiate(ctx, r, moduleValuesWasm(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer in.Close(ctx) //nolint:errcheck // test cleanup
		return in.Call(ctx, "run")
	}
	if got, err := call("bundle_arg_seen_through"); err != nil || len(got) != 1 || got[0] != uint32(84) {
		t.Fatalf("run = %v, %v; want [84]", got, err)
	}
	_, err := call("bundle_arg_parent_lift")
	requireErrContains(t, err, `host import "b" "f": wasm trap: cannot enter component instance`)

	// $p, $s, $b and $ci are instances 0..3; $pf, $sf and $bf funcs 0..2.
	wasm := moduleValuesWasm(t, "bundle_arg_seen_through")
	for _, tc := range []struct {
		name   string
		mangle func(c *binary.Component)
		want   string
	}{
		{
			"shim arg missing", func(c *binary.Component) { c.Instances[1].Args[0].Name = "x" },
			`arg "b": export "f": func arg index 1: shim export "f" re-exports the shim's import "f", which no func instantiate-arg supplies`,
		},
		{
			"alias of an instance with code", func(c *binary.Component) { c.Aliases[c.ComponentFuncSpace[2].Alias].InstanceIdx = 3 },
			`arg "g": func arg index 2 aliases component instance 3, which is not a prior nested instantiation`,
		},
		{
			"bundle names the alias itself", func(c *binary.Component) { c.Instances[2].Exports[0].SortIdx = 2 },
			`arg "b": export "f": func arg index 2 aliases "f" of component instance 2, which names func 2, not an earlier one`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comp, err := binary.Decode(bytes.NewReader(wasm))
			if err != nil {
				t.Fatal(err)
			}
			tc.mangle(comp)
			_, err = instantiateGraph(ctx, r, comp, wasm, newConfig(nil))
			requireErrContains(t, err, tc.want)
		})
	}
}
