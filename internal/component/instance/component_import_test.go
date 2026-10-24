package instance

import (
	"context"
	"embed"
	"path"
	"testing"

	"github.com/samyfodil/wazy"
)

// componentImportsFS holds the hand-written fixtures of component imports
// (testdata/component_imports/*.wat, compiled with wasm-tools 1.262.0): a
// root that imports a component and the components supplied for it, which
// match its declared type exactly, by subtyping, or not at all. They cover
// what the vendored suites cannot -- a supplied component actually running
// -- since a validator suite has no embedder to supply one.
//
//go:embed testdata/component_imports/*.wasm
var componentImportsFS embed.FS

func componentImportFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := componentImportsFS.ReadFile(path.Join("testdata/component_imports", name+".wasm"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A root component instantiates the component supplied for its component
// import and runs it: calc_root passes its own list<s32, 4> type into the
// supplied calc, which sums four values, and re-exports the sum.
func TestComponentImport_SuppliedComponentRuns(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	root := componentImportFixture(t, "calc_root")
	for _, supplied := range []string{
		"calc_exact",   // exactly the declared type
		"calc_subtype", // imports less and exports more
	} {
		t.Run(supplied, func(t *testing.T) {
			in, err := Instantiate(ctx, r, root, WithComponentImport("calc", componentImportFixture(t, supplied)))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close(ctx)
			got, err := in.Call(ctx, "sum", []int32{1, 2, 3, 40})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0] != int32(46) {
				t.Fatalf("sum = %v, want [46]", got)
			}
		})
	}
}

// A component that is not a subtype of the declared type is refused before
// anything is instantiated, saying why.
func TestComponentImport_SuppliedComponentRefused(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	root := componentImportFixture(t, "calc_root")
	for _, tc := range []struct {
		name     string
		supplied []byte
		opts     []Option
		want     string
	}{
		{
			name:     "an export of the wrong type",
			supplied: componentImportFixture(t, "calc_wrong_param"),
			want:     "component import \"calc\": the supplied component does not match the declared type: type mismatch for export `sum`: type mismatch in function parameter `xs`: expected fixed-length 4, found size 3",
		},
		{
			name:     "an import the importer does not supply",
			supplied: componentImportFixture(t, "calc_extra_import"),
			want:     "the supplied component does not match the declared type: missing import named `log`",
		},
		{
			name:     "an export missing",
			supplied: []byte("\x00asm\x0d\x00\x01\x00"), // (component)
			want:     "the supplied component does not match the declared type: missing export named `sum`",
		},
		{
			name:     "not a component",
			supplied: []byte("not wasm"),
			want:     "component import \"calc\": decode the supplied component",
		},
		{
			// wasm-tools' fixed-length-lists.6: a component that is itself
			// invalid -- one of its own instantiations has an argument of
			// the wrong type -- is refused as that, whatever it is supplied
			// for.
			name:     "itself invalid",
			supplied: mustWastFile(t, "testdata/wast/wasm-tools-fixed-length-lists/fixed-length-lists.6.wasm"),
			want:     "the supplied component is invalid: instance 0: type mismatch for import `x`",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := Instantiate(ctx, r, root, append(tc.opts, WithComponentImport("calc", tc.supplied))...)
			if err == nil {
				in.Close(ctx)
				t.Fatal("instantiated; want it refused")
			}
			requireErrContains(t, err, tc.want)
		})
	}
}

func mustWastFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := wastFS.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// With nothing supplied, the root is refused for that -- and told what
// supplies one. A component supplied under a name the root does not import
// changes nothing.
func TestComponentImport_NotSupplied(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	_, err := Instantiate(ctx, r, componentImportFixture(t, "calc_root"), WithComponentImport("calculator", componentImportFixture(t, "calc_exact")))
	requireErrContains(t, err, `component import "calc" was not supplied; supply a component for it with WithComponentImport`)
}

// The case wasm-tools' fixed-length-lists.wast implies but does not state:
// with a type argument that matches the imported component's (eq (list s32
// 10)) bound, and a component supplied for the import, the root
// instantiates. The two that do not match are refused for the mismatch even
// with the component supplied -- the argument is checked against the
// DECLARED type, whatever is supplied.
func TestComponentImport_WasmToolsFixedLengthListsMatch(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	y := WithComponentImport("y", componentImportFixture(t, "fixedlist_match_y"))
	in, err := Instantiate(ctx, r, componentImportFixture(t, "fixedlist_match_root"), y)
	if err != nil {
		t.Fatal(err)
	}
	in.Close(ctx)
	for _, f := range []string{"fixed-length-lists.5.wasm", "fixed-length-lists.6.wasm"} {
		_, err := Instantiate(ctx, r, mustWastFile(t, "testdata/wast/wasm-tools-fixed-length-lists/"+f), y)
		requireErrContains(t, err, "type mismatch for import `x`")
	}
}

// A CompileCache remembers a type check that passed, keyed by the decoded
// components it shares across instantiations, so a second instantiation is
// not checked again -- and still runs.
func TestComponentImport_CompileCacheRemembersChecks(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := NewCompileCache()
	defer cache.Close(ctx)
	root, calc := componentImportFixture(t, "calc_root"), componentImportFixture(t, "calc_exact")
	for i := range 2 {
		in, err := Instantiate(ctx, r, root, WithCompileCache(cache), WithComponentImport("calc", calc))
		if err != nil {
			t.Fatalf("instantiation %d: %v", i, err)
		}
		got, err := in.Call(ctx, "sum", []int32{1, 1, 1, 1})
		in.Close(ctx)
		if err != nil || len(got) != 1 || got[0] != int32(4) {
			t.Fatalf("instantiation %d: sum = %v, %v; want [4]", i, got, err)
		}
	}
	rootComp, err := cache.getOrDecode(root)
	if err != nil {
		t.Fatal(err)
	}
	calcComp, err := cache.getOrDecode(calc)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{compileCache: cache}
	if !cfg.typeChecked(ctCheckKey{comp: rootComp}) {
		t.Error("the root's own check is not remembered")
	}
	if !cfg.typeChecked(ctCheckKey{comp: rootComp, supplied: calcComp, name: "calc"}) {
		t.Error("the check of the supplied calc is not remembered")
	}
	if (&config{}).typeChecked(ctCheckKey{comp: rootComp}) {
		t.Error("without a cache, nothing is remembered")
	}
	(&config{}).rememberTypeChecked(ctCheckKey{comp: rootComp}) // a no-op without a cache
}

// Without a cache the root's env, built by its own check, is reused for the
// supplied component's -- and with a cache whose own-check entry is warm,
// the supplied component's check builds the env it needs itself.
func TestComponentImport_CheckSuppliedBuildsEnvOnWarmCache(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := NewCompileCache()
	defer cache.Close(ctx)
	root := componentImportFixture(t, "calc_root")
	rootComp, err := cache.getOrDecode(root)
	if err != nil {
		t.Fatal(err)
	}
	(&config{compileCache: cache}).rememberTypeChecked(ctCheckKey{comp: rootComp})
	_, err = Instantiate(ctx, r, root, WithCompileCache(cache), WithComponentImport("calc", componentImportFixture(t, "calc_wrong_param")))
	requireErrContains(t, err, "type mismatch for export `sum`")
	in, err := Instantiate(ctx, r, root, WithCompileCache(cache), WithComponentImport("calc", componentImportFixture(t, "calc_subtype")))
	if err != nil {
		t.Fatal(err)
	}
	in.Close(ctx)
}
