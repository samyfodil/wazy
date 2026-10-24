package instance

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"path"
	"slices"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
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
	fetched(t, path.Dir(name))
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

// The root re-exports the supplied component's instance whole -- an
// interface whose members are its exports -- which works the same for a
// supplied component as for one nested in the root.
func TestComponentImport_InstanceExportedWhole(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, componentImportFixture(t, "calc_reexport_root"), WithComponentImport("calc", componentImportFixture(t, "calc_exact")))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	got, err := in.CallExport(ctx, "calc", "sum", []int32{1, 2, 3, 4})
	if err != nil || len(got) != 1 || got[0] != int32(10) {
		t.Fatalf("calc#sum = %v, %v; want [10]", got, err)
	}
	if exports := in.InstanceExports(); len(exports) != 1 || exports[0] != "calc" {
		t.Errorf("InstanceExports() = %v, want [calc]", exports)
	}

	nested, err := Instantiate(ctx, r, componentImportFixture(t, "nested_whole_root"))
	if err != nil {
		t.Fatal(err)
	}
	defer nested.Close(ctx)
	if got, err := nested.CallExport(ctx, "c", "f"); err != nil || len(got) != 1 || got[0] != uint32(1) {
		t.Fatalf("c#f = %v, %v; want [1]", got, err)
	}

	// An instance exported whole whose component exports an interface of its
	// own: the interface is "d#iface", the path of names leading to it.
	deep, err := Instantiate(ctx, r, componentImportFixture(t, "nested_whole_nested_iface_root"))
	if err != nil {
		t.Fatal(err)
	}
	defer deep.Close(ctx)
	if got, err := deep.CallExport(ctx, "d#iface", "f"); err != nil || len(got) != 1 || got[0] != uint32(1) {
		t.Fatalf("d#iface#f = %v, %v; want [1]", got, err)
	}
	if exports := deep.InstanceExports(); len(exports) != 1 || exports[0] != "d#iface" {
		t.Errorf("InstanceExports() = %v, want [d#iface]", exports)
	}
	if deep.ResourcesFor("d#iface") == deep.Resources() {
		t.Error("ResourcesFor(d#iface) is the root's table, not the defining component's")
	}
}

// An interface nested in an exported instance is reached every way a
// component can reach one: through an alias two levels deep, a func alias
// off that, an instantiate-arg, and a further whole export, three levels
// down. See nested_iface_paths_root.wat.
func TestNestedInterfacePaths(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, componentImportFixture(t, "nested_iface_paths_root"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	for _, tc := range []struct {
		iface, member string
		want          uint32
	}{{"x", "f", 1}, {"", "g", 1}, {"", "h", 2}, {"whole#d#iface", "f", 1}} {
		var got []abi.Value
		if tc.iface == "" {
			got, err = in.Call(ctx, tc.member)
		} else {
			got, err = in.CallExport(ctx, tc.iface, tc.member)
		}
		if err != nil || len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s#%s = %v, %v; want [%d]", tc.iface, tc.member, got, err, tc.want)
		}
	}
	got := in.InstanceExports()
	slices.Sort(got)
	if want := []string{"whole#d#iface", "x"}; !slices.Equal(got, want) {
		t.Errorf("InstanceExports() = %v, want %v", got, want)
	}
	// Each nested interface's handles live with the component defining it,
	// $D's instance inside $W -- one table, whichever path names it.
	d := in.ResourcesFor("x")
	if d == in.Resources() || in.ResourcesFor("whole#d#iface") != d || in.ResourcesFor("whole#d") != d {
		t.Error("ResourcesFor does not reach $D's table through every path")
	}
}

// A pure re-export shim -- a component with no code -- supplied for a
// component import is seen through like a nested one: its export is the
// func the root passed it, reached through an alias or its instance whole.
func TestComponentImport_SuppliedShim(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	in, err := Instantiate(ctx, r, componentImportFixture(t, "shim_root"), WithComponentImport("shim", componentImportFixture(t, "shim")))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	if got, err := in.Call(ctx, "g"); err != nil || len(got) != 1 || got[0] != uint32(7) {
		t.Fatalf("g = %v, %v; want [7]", got, err)
	}
	if got, err := in.CallExport(ctx, "s", "g"); err != nil || len(got) != 1 || got[0] != uint32(7) {
		t.Fatalf("s#g = %v, %v; want [7]", got, err)
	}
}

// The two ways a func alias can fail to see through a shim, on components
// the type check would have refused first (instantiated here without it):
// the shim's export names no import, or nothing was passed for the import it
// re-exports.
func TestComponentImport_ShimSeeThroughErrors(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	for name, want := range map[string]string{
		"shim_bad_index_root":   `export "g": shim export "g": func index 5 out of range of the shim's 1 func import(s)`,
		"shim_missing_arg_root": `export "g": shim export "g" re-exports the shim's import "f", which no func instantiate-arg supplies`,
	} {
		wasm := componentImportFixture(t, name)
		comp, err := binary.Decode(bytes.NewReader(wasm))
		if err != nil {
			t.Fatal(err)
		}
		_, err = instantiateGraph(ctx, r, comp, wasm, newConfig(nil))
		requireErrContains(t, err, want)
	}
}

// A pure re-export shim passed whole to a nested component with code: each
// func the shim exports is wired in as the func it was passed, so $C's
// times(6, seven()) runs $P's mul and seven. A shim export that cannot be
// seen through, or that names no func, is refused against the arg and export,
// and an instance with code that is not instantiated yet is refused as before.
func TestShimPassedWholeToNestedComponent(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	wasm := componentImportFixture(t, "shim_whole_arg_root")
	in, err := Instantiate(ctx, r, wasm)
	if err != nil {
		t.Fatal(err)
	}
	got, err := in.Call(ctx, "run")
	in.Close(ctx)
	if err != nil || len(got) != 1 || got[0] != uint32(42) {
		t.Fatalf("run = %v, %v; want [42]", got, err)
	}

	// $S is the second nested component, $s the second instance.
	for _, tc := range []struct {
		name   string
		mangle func(c *binary.Component)
		want   string
	}{
		{
			"bad index", func(c *binary.Component) { c.NestedComponents[1].Exports[0].ExternIndex = 5 },
			`arg "calc": export "times": shim export "times": func index 5 out of range of the shim's 2 func import(s)`,
		},
		{
			"missing arg", func(c *binary.Component) { c.Instances[1].Args[0].Name = "x" },
			`arg "calc": export "times": shim export "times" re-exports the shim's import "times", which no func instantiate-arg supplies`,
		},
		{
			"bad func", func(c *binary.Component) { c.Instances[1].Args[0].SortIdx = 99 },
			`arg "calc": export "times": func arg index 99 out of range of the component func index space`,
		},
		// An instance with code, not instantiated yet: $c passed to itself.
		{
			"not yet", func(c *binary.Component) { c.Instances[2].Args[0].SortIdx = 2 },
			`arg "calc" references instance 2, which is neither a prior nested instantiation nor an imported instance: instance definition 2 was not instantiated`,
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

// A func alias sees through every instance this component never
// instantiates to the func it names: an inline-export instance's member,
// any one of a nested shim's exports. An alias naming nothing there is
// what it was before -- a func that is not this component's lift.
func TestFuncAliasSeesThroughUninstantiatedInstances(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	for name, want := range map[string]uint32{"inline_alias_root": 3, "shim_two_exports_root": 2} {
		in, err := Instantiate(ctx, r, componentImportFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		export := map[string]string{"inline_alias_root": "h", "shim_two_exports_root": "b"}[name]
		got, err := in.Call(ctx, export)
		in.Close(ctx)
		if err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("%s: %s = %v, %v; want [%d]", name, export, got, err, want)
		}
	}
	in, err := Instantiate(ctx, r, componentImportFixture(t, "inline_of_nested_root"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := in.CallExport(ctx, "i", "f")
	in.Close(ctx)
	if err != nil || len(got) != 1 || got[0] != uint32(5) {
		t.Fatalf("i#f = %v, %v; want [5]", got, err)
	}
	// A nested component with code is instantiated, never seen through.
	comp, err := binary.Decode(bytes.NewReader(componentImportFixture(t, "inline_of_nested_root")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := funcAliasSource(comp, nil, aliasTarget{instIdx: 0, name: "f"}); ok || err != nil {
		t.Errorf("funcAliasSource saw through an instance of a component with code: %v, %v", ok, err)
	}
	for _, name := range []string{"inline_alias_missing_root", "shim_no_export_root"} {
		wasm := componentImportFixture(t, name)
		comp, err := binary.Decode(bytes.NewReader(wasm))
		if err != nil {
			t.Fatal(err)
		}
		_, err = instantiateGraph(ctx, r, comp, wasm, newConfig(nil))
		requireErrContains(t, err, "resolves to an imported func rather than a lift")
	}
}

// A resource the supplied component defines reaches the host through the
// root's whole re-export of its instance: the constructor mints a handle,
// the method borrows it, and DropResource -- asked of the root -- drops it
// in the supplied component's table, where it lives. A component whose
// method takes the resource by own where the declared type borrows it is
// refused for that.
func TestComponentImport_ResourceThroughSuppliedComponent(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	root := componentImportFixture(t, "counter_root")
	in, err := Instantiate(ctx, r, root, WithComponentImport("counter", componentImportFixture(t, "counter")))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close(ctx)
	h, err := in.CallExport(ctx, "c", "[constructor]counter", uint32(41))
	if err != nil || len(h) != 1 {
		t.Fatalf("constructor = %v, %v", h, err)
	}
	got, err := in.CallExport(ctx, "c", "[method]counter.get", h[0])
	if err != nil || len(got) != 1 || got[0] != uint32(41) {
		t.Fatalf("get = %v, %v; want [41]", got, err)
	}
	// The handle lives in the supplied component's table, which is where a
	// host minting for "c" must mint; for anything else the root's is it.
	if in.ResourcesFor("c") == in.Resources() || in.ResourcesFor("other") != in.Resources() {
		t.Fatal("ResourcesFor does not name the defining component's table")
	}
	if err := in.DropResource(ctx, "c", "counter", h[0].(uint32)); err != nil {
		t.Fatalf("DropResource: %v", err)
	}
	err = in.DropResource(ctx, "c", "counter", h[0].(uint32))
	requireErrContains(t, err, "does not name a live resource")

	_, err = Instantiate(ctx, r, root, WithComponentImport("counter", componentImportFixture(t, "counter_own")))
	requireErrContains(t, err, "type mismatch for export `[method]counter.get`: type mismatch in function parameter `self`: expected borrow, found own")
}

// Instantiations racing on one CompileCache share its decode of the
// supplied component and its record of the checks that passed.
func TestComponentImport_ConcurrentOnOneCache(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	cache := NewCompileCache()
	defer cache.Close(ctx)
	root, calc := componentImportFixture(t, "calc_root"), componentImportFixture(t, "calc_exact")
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			in, err := Instantiate(ctx, r, root, WithCompileCache(cache), WithComponentImport("calc", calc))
			if err == nil {
				var got []abi.Value
				got, err = in.Call(ctx, "sum", []int32{2, 2, 2, 2})
				if err == nil && (len(got) != 1 || got[0] != int32(8)) {
					err = fmt.Errorf("sum = %v, want [8]", got)
				}
				in.Close(ctx)
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
