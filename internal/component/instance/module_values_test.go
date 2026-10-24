package instance

import (
	"context"
	"embed"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
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

	_, err = Instantiate(ctx, r, moduleValuesWasm(t, "bundle_arg_unresolved"))
	requireErrContains(t, err, `arg "b": export "f": func arg index 1 aliases component instance 0, which is not a prior nested instantiation`)
}
