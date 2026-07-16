package instance

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

func noopLog(context.Context, []abi.Value) ([]abi.Value, error) { return nil, nil }

func stringLogOpt(fn HostFunc) Option {
	return WithImport("test:pkg/host", "log", fn,
		[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil)
}

// decodeLogHello returns a fresh decode of the log_hello fixture, so a test
// can mutate the decoded structure to trip a specific validation branch while
// keeping the real embedded core-module byte ranges valid.
func decodeLogHello(t *testing.T) *binary.Component {
	t.Helper()
	comp, err := binary.Decode(bytes.NewReader(logHelloWasm))
	if err != nil {
		t.Fatalf("decode log_hello: %v", err)
	}
	return comp
}

func runImport(t *testing.T, comp *binary.Component, opts ...Option) (*Instance, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	return instantiateWithImports(ctx, r, comp, logHelloWasm, newConfig(opts))
}

func TestImports_NonInstanceImport(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Imports[0].ExternType = 0x01 // func
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "only instance imports")
}

// TestImports_UnreferencedNestedInstance proves a nested component instance
// that no export points at is simply unused, not an error -- only the
// component-export -> Instance -> NestedComponent chain (bindInstanceExport)
// is walked, matching how comp.Canons entries that no export reaches are
// also never independently validated.
func TestImports_UnreferencedNestedInstance(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Instances = []binary.Instance{{Kind: 0x00}}
	inst, err := runImport(t, comp, stringLogOpt(noopLog))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	inst.Close(context.Background())
}

// TestImports_InstanceExportNestedComponentOutOfRange exercises the
// instance-export resolution path (bindInstanceExport): an export naming an
// Instance whose ComponentIdx has no matching decoded NestedComponent (e.g.
// log_hello, which has none) fails loud rather than panicking or silently
// producing a broken export.
func TestImports_InstanceExportNestedComponentOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Instances = []binary.Instance{{Kind: 0x00, ComponentIdx: 0}}
	comp.Exports = append(comp.Exports, binary.Export{Name: "extra", ExternType: 0x05, ExternIndex: 0})
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "decoded nested component")
}

// TestImports_InstanceExportInlineKindUnsupported exercises the inline-export
// (Kind == 0x01) Instance branch of bindInstanceExport, which is not a
// component instantiation and so cannot name a nested component to resolve.
func TestImports_InstanceExportInlineKindUnsupported(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Instances = []binary.Instance{{Kind: 0x01}}
	comp.Exports = append(comp.Exports, binary.Export{Name: "extra", ExternType: 0x05, ExternIndex: 0})
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "not a component instantiation")
}

// TestImports_InstanceExportIndexOutOfRange exercises the export.ExternIndex
// out of range of comp.Instances branch of bindInstanceExport.
func TestImports_InstanceExportIndexOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Exports = append(comp.Exports, binary.Export{Name: "extra", ExternType: 0x05, ExternIndex: 99})
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "out of range of 0 instance(s)")
}

func TestImports_UnsupportedCanonKind(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Canons = append(comp.Canons, binary.Canon{Kind: 0xff}) // not a real canon kind
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "only canon lift")
}

func TestImports_InlineExportNonFuncSort(t *testing.T) {
	comp := decodeLogHello(t)
	comp.CoreInstances[1].Exports[0].Sort = 0x02 // memory
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "only core funcs")
}

func TestImports_InlineExportNonLoweredFunc(t *testing.T) {
	comp := decodeLogHello(t)
	comp.CoreInstances[1].Exports[0].CoreSortIdx = 99
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "not one of the")
}

func TestImports_MissingHostImpl(t *testing.T) {
	comp := decodeLogHello(t)
	_, err := runImport(t, comp) // no WithImport
	requireErrContains(t, err, "no host implementation")
}

func TestImports_CoreModuleIdxOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.CoreInstances[0].ModuleIdx = 99
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "out of range")
}

func TestImports_CoreModuleBytesOOB(t *testing.T) {
	comp := decodeLogHello(t)
	comp.CoreModules[0].Size = len(logHelloWasm) + 100
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "out of bounds")
}

func TestImports_FromExportsMultipleNames(t *testing.T) {
	comp := decodeLogHello(t)
	// Reference the inline-export instance (index 1) under a second arg name.
	comp.CoreInstances[2].Args = append(comp.CoreInstances[2].Args,
		binary.CoreInstantiateArg{Name: "second", InstanceIdx: 1})
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "referenced under 2 name(s)")
}

func TestImports_ExportNotFuncOrInstance(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Exports[0].ExternType = 0x02 // value
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "only func and instance exports")
}

func TestImports_ExportResolvesToImport(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Exports[0].ExternIndex = 0 // the log alias (a component-func alias), not the lift
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "resolves to an imported func")
}

func TestImports_ExportLiftsLoweredFunc(t *testing.T) {
	comp := decodeLogHello(t)
	// Point the lift's core func at the lowered import func (index 0).
	comp.Canons[1].CoreFuncIdx = 0
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "rather than a real core export")
}

func TestImports_ExportCoreFuncAliasOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Canons[1].CoreFuncIdx = 99
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "out of range of the core func index space")
}

func TestImports_ExportComponentFuncOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Exports[0].ExternIndex = 99
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "component func index space")
}

// TestImports_ExportPostReturnOutOfRange exercises bindFuncExport's
// post-return wiring: an out-of-range post-return CanonOpt index on the
// "run" lift fails loud (via resolvePostReturnFunc) instead of being
// silently ignored.
func TestImports_ExportPostReturnOutOfRange(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Canons[1].Opts = append(comp.Canons[1].Opts, binary.CanonOpt{Kind: 0x05, Idx: 99})
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "out of range of the core func index space")
}

// ------- resolvePostReturnFunc (pure unit tests) -------

func TestResolvePostReturnFunc_NoOpt(t *testing.T) {
	name, err := resolvePostReturnFunc(binary.Canon{}, nil, 0, 0)
	if err != nil || name != "" {
		t.Fatalf("got (%q, %v), want (\"\", nil)", name, err)
	}
}

func TestResolvePostReturnFunc_CanonProducedFunc(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 0}}}
	_, err := resolvePostReturnFunc(canon, nil, 1, 0) // idx 0 < numProducedCoreFuncs (1)
	requireErrContains(t, err, "canon-produced func")
}

func TestResolvePostReturnFunc_OutOfRange(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 5}}}
	_, err := resolvePostReturnFunc(canon, []aliasTarget{{name: "a"}}, 0, 0)
	requireErrContains(t, err, "out of range of the core func index space")
}

func TestResolvePostReturnFunc_CrossInstanceMismatch(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 0}}}
	_, err := resolvePostReturnFunc(canon, []aliasTarget{{instIdx: 1, name: "pr"}}, 0, 0)
	requireErrContains(t, err, "cross-instance post-return is not supported")
}

func TestResolvePostReturnFunc_Success(t *testing.T) {
	canon := binary.Canon{Opts: []binary.CanonOpt{{Kind: 0x05, Idx: 0}}}
	name, err := resolvePostReturnFunc(canon, []aliasTarget{{instIdx: 0, name: "pr"}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if name != "pr" {
		t.Fatalf("got %q, want %q", name, "pr")
	}
}

// ------- validateShimComponent / bindInstanceExport (pure unit tests) -------

func TestValidateShimComponent(t *testing.T) {
	if err := validateShimComponent(&binary.Component{}); err != nil {
		t.Fatalf("empty nested component should validate: %v", err)
	}
	if err := validateShimComponent(&binary.Component{Imports: []binary.Import{{ExternType: 0x01}}}); err != nil {
		t.Fatalf("func imports only should validate: %v", err)
	}

	cases := []struct {
		name  string
		shim  *binary.Component
		wants string
	}{
		{"core module", &binary.Component{CoreModules: []binary.CoreModule{{}}}, "not a pure re-export shim"},
		{"core instance", &binary.Component{CoreInstances: []binary.CoreInstance{{}}}, "not a pure re-export shim"},
		{"canon", &binary.Component{Canons: []binary.Canon{{}}}, "not a pure re-export shim"},
		{"nested instance", &binary.Component{Instances: []binary.Instance{{}}}, "not a pure re-export shim"},
		{"further nesting", &binary.Component{NestedComponents: []*binary.Component{{}}}, "not a pure re-export shim"},
		{"func-sort alias", &binary.Component{Aliases: []binary.AliasDef{{Sort: 0x01}}}, "not a pure re-export shim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateShimComponent(tc.shim)
			requireErrContains(t, err, tc.wants)
		})
	}
}

func TestShimFuncImportNames(t *testing.T) {
	nested := &binary.Component{Imports: []binary.Import{
		{Name: "t", ExternType: 0x03}, // type import: not a func-sort item
		{Name: "a", ExternType: 0x01},
		{Name: "b", ExternType: 0x01},
	}}
	got := shimFuncImportNames(nested)
	want := []string{"a", "b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// noopComponentFunc is a componentFunc stub for bindInstanceExport unit
// tests that only need to exercise validation branches which bail before
// ever calling componentFunc.
func noopComponentFunc(uint32) (bool, int, aliasTarget, error) {
	return false, 0, aliasTarget{}, nil
}

func TestBindInstanceExport_InstanceIndexOutOfRange(t *testing.T) {
	comp := &binary.Component{}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "out of range of 0 instance(s)")
}

func TestBindInstanceExport_InlineKindUnsupported(t *testing.T) {
	comp := &binary.Component{Instances: []binary.Instance{{Kind: 0x01}}}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "not a component instantiation")
}

func TestBindInstanceExport_NestedComponentOutOfRange(t *testing.T) {
	comp := &binary.Component{Instances: []binary.Instance{{Kind: 0x00, ComponentIdx: 0}}}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "decoded nested component")
}

func TestBindInstanceExport_NotPureShim(t *testing.T) {
	comp := &binary.Component{
		Instances:        []binary.Instance{{Kind: 0x00, ComponentIdx: 0}},
		NestedComponents: []*binary.Component{{CoreModules: []binary.CoreModule{{}}}},
	}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "out of scope for this milestone")
}

func TestBindInstanceExport_MemberNotFunc(t *testing.T) {
	comp := &binary.Component{
		Instances: []binary.Instance{{Kind: 0x00, ComponentIdx: 0}},
		NestedComponents: []*binary.Component{{
			Exports: []binary.Export{{Name: "x", ExternType: 0x02}}, // value, not func
		}},
	}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "only func members are supported")
}

func TestBindInstanceExport_MemberFuncIdxOutOfRange(t *testing.T) {
	comp := &binary.Component{
		Instances: []binary.Instance{{Kind: 0x00, ComponentIdx: 0}},
		NestedComponents: []*binary.Component{{
			Exports: []binary.Export{{Name: "add", ExternType: 0x01, ExternIndex: 5}}, // no imports at all
		}},
	}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "out of range of the shim's")
}

func TestBindInstanceExport_ShimImportNoMatchingArg(t *testing.T) {
	comp := &binary.Component{
		Instances: []binary.Instance{{Kind: 0x00, ComponentIdx: 0}}, // no Args
		NestedComponents: []*binary.Component{{
			Imports: []binary.Import{{Name: "import-func-add", ExternType: 0x01}},
			Exports: []binary.Export{{Name: "add", ExternType: 0x01, ExternIndex: 0}},
		}},
	}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "no matching instantiate-arg")
}

func TestBindInstanceExport_ArgNonFuncSort(t *testing.T) {
	comp := &binary.Component{
		Instances: []binary.Instance{{Kind: 0x00, ComponentIdx: 0, Args: []binary.InstantiateArg{
			{Name: "import-func-add", Sort: 0x03, SortIdx: 0}, // type sort, not func
		}}},
		NestedComponents: []*binary.Component{{
			Imports: []binary.Import{{Name: "import-func-add", ExternType: 0x01}},
			Exports: []binary.Export{{Name: "add", ExternType: 0x01, ExternIndex: 0}},
		}},
	}
	exp := binary.Export{Name: "inst", ExternType: 0x05, ExternIndex: 0}
	err := bindInstanceExport(comp, exp, noopComponentFunc, nil, nil, 0, nil, map[string]*boundExport{})
	requireErrContains(t, err, "non-func sort")
}

// TestImports_CoreExportAlias_CoreSortMismatchFallsBackToProbe simulates a
// pre-CoreSort AliasDef (its zero value, 0x00, is indistinguishable from a
// real func classification) on log_hello's "memory" core-export alias, and
// proves the classifier falls back to probing the instantiated module
// (which correctly reports "memory" is not a func) rather than trusting a
// CoreSort that disagrees with what the alias actually names. If the
// fallback didn't fire, "memory" would be misclassified as a func alias,
// corrupting the core func index space that "run"'s export binding relies
// on (see host_import.go's coreFuncAliases construction).
func TestImports_CoreExportAlias_CoreSortMismatchFallsBackToProbe(t *testing.T) {
	comp := decodeLogHello(t)
	found := false
	for i := range comp.Aliases {
		al := &comp.Aliases[i]
		if al.Sort == 0x00 && al.TargetKind == 0x01 && al.Name == "memory" {
			al.CoreSort = 0x00 // simulate "unknown" (pre-CoreSort) reading as func
			found = true
		}
	}
	if !found {
		t.Fatal("log_hello fixture has no core-export alias named \"memory\"")
	}

	inst, err := runImport(t, comp, stringLogOpt(noopLog))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(context.Background())

	if _, err := inst.Call(context.Background(), "run"); err != nil {
		t.Fatalf("Call run: %v", err)
	}
}

func TestImports_LiftTypeNotFunc(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Types[1] = binary.Type{Descriptor: binary.PrimitiveDesc{Prim: "u32"}}
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "not a func type")
}

// ------- pure helper unit tests -------

func TestModuleNameFor(t *testing.T) {
	if n, err := moduleNameFor(2, nil, "wazy:component/"); err != nil || n != "wazy:component/core2" {
		t.Fatalf("root: got %q, %v", n, err)
	}
	if n, err := moduleNameFor(0, []string{"libc"}, "wazy:component/"); err != nil || n != "libc" {
		t.Fatalf("single: got %q, %v", n, err)
	}
	if _, err := moduleNameFor(0, []string{"a", "b"}, "wazy:component/"); err == nil {
		t.Fatal("expected error for multiple names")
	}
}

// TestMkImportKeyVersionTolerant proves host-import matching ignores the wasi
// 0.2.x patch version: a guest built against @0.2.12 resolves against an impl
// registered under @0.2.3 (or any other patch), and an unversioned name is
// left untouched.
func TestMkImportKeyVersionTolerant(t *testing.T) {
	a := mkImportKey("wasi:io/streams@0.2.3", "write")
	b := mkImportKey("wasi:io/streams@0.2.12", "write")
	if a != b {
		t.Fatalf("version-tolerant match failed: %v != %v", a, b)
	}
	if a.iface != "wasi:io/streams" {
		t.Fatalf("version not stripped: %q", a.iface)
	}
	if got := mkImportKey("test:pkg/host", "log"); got.iface != "test:pkg/host" {
		t.Fatalf("unversioned name altered: %q", got.iface)
	}
	// Different interface or func must still differ.
	if mkImportKey("wasi:io/streams@0.2.3", "write") == mkImportKey("wasi:io/streams@0.2.3", "read") {
		t.Fatal("distinct func names collapsed")
	}
}

func TestImportInterfaceName(t *testing.T) {
	comp := &binary.Component{Imports: []binary.Import{
		{Name: "test:pkg/host", ExternType: 0x05},
		{Name: "test:pkg/other", ExternType: 0x05},
	}}
	if n, err := importInterfaceName(comp, 0); err != nil || n != "test:pkg/host" {
		t.Fatalf("idx0: got %q, %v", n, err)
	}
	if n, err := importInterfaceName(comp, 1); err != nil || n != "test:pkg/other" {
		t.Fatalf("idx1: got %q, %v", n, err)
	}
	if _, err := importInterfaceName(comp, 5); err == nil {
		t.Fatal("expected out-of-range error")
	}
	// Non-instance imports are skipped when counting instance indices.
	comp2 := &binary.Component{Imports: []binary.Import{
		{Name: "f", ExternType: 0x01},
		{Name: "test:pkg/host", ExternType: 0x05},
	}}
	if n, err := importInterfaceName(comp2, 0); err != nil || n != "test:pkg/host" {
		t.Fatalf("skip-non-instance: got %q, %v", n, err)
	}
}

func TestToApiValueTypes(t *testing.T) {
	got, err := toApiValueTypes([]string{"i32", "i64", "f32", "f64"})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeF32, api.ValueTypeF64}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %#x want %#x", i, got[i], want[i])
		}
	}
	if v, err := toApiValueTypes(nil); v != nil || err != nil {
		t.Fatalf("empty: got %v, %v", v, err)
	}
	if _, err := toApiValueTypes([]string{"v128"}); err == nil {
		t.Fatal("expected error for unknown core type")
	}
}

func TestSynthFuncDesc(t *testing.T) {
	// Primitive params/results use inline TypeRefs (no type table).
	fd, resolve := synthFuncDesc(
		[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}},
		[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}},
	)
	if len(fd.Params) != 1 || fd.Params[0].Type.Primitive != "string" {
		t.Fatalf("params: %#v", fd.Params)
	}
	if len(fd.Results.Named) != 1 || fd.Results.Named[0].Type.Primitive != "u32" {
		t.Fatalf("results: %#v", fd.Results.Named)
	}
	// A composite descriptor goes into the local table, reachable via resolve.
	rec := binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}}}
	fd2, resolve2 := synthFuncDesc([]binary.TypeDesc{rec}, nil)
	ref := fd2.Params[0].Type
	if ref.TypeIndex == nil {
		t.Fatal("expected composite param to use a type index")
	}
	if _, ok := resolve2(*ref.TypeIndex).(binary.RecordDesc); !ok {
		t.Fatalf("resolve returned %T, want RecordDesc", resolve2(*ref.TypeIndex))
	}
	_ = resolve
}

func TestFlattenRefsAndResults(t *testing.T) {
	fd, resolve := synthFuncDesc(
		[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}, binary.PrimitiveDesc{Prim: "u32"}},
		[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "u64"}},
	)
	pf, err := flattenRefs(fd.Params, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(pf) != "[i32 i32 i32]" { // string=(i32,i32) + u32=i32
		t.Fatalf("param flat: %v", pf)
	}
	rf, err := flattenResultRefs(fd, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rf) != "[i64]" {
		t.Fatalf("result flat: %v", rf)
	}

	// A bad TypeRef (neither primitive nor index) surfaces an error.
	bad := binary.FuncDesc{Params: []binary.FuncParam{{Type: binary.TypeRef{}}}}
	if _, err := flattenRefs(bad.Params, resolve); err == nil {
		t.Fatal("expected flattenRefs error on bad ref")
	}
	badRes := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &binary.TypeRef{}}}
	if _, err := flattenResultRefs(badRes, resolve); err == nil {
		t.Fatal("expected flattenResultRefs error on bad ref")
	}
}

func TestBuildHostWrapper_SpilledParams(t *testing.T) {
	// 17 u32 params flatten to 17 i32, exceeding MaxFlatParams (16).
	var params []binary.TypeDesc
	for i := 0; i < 17; i++ {
		params = append(params, binary.PrimitiveDesc{Prim: "u32"})
	}
	_, _, _, err := buildHostWrapper("i", "f", &hostImport{fn: noopLog, params: params}, newHandleTable(), nil, nil)
	requireErrContains(t, err, "whole-parameter-list spilling")
}

func TestBuildHostWrapper_SpilledResults(t *testing.T) {
	// A record of two u64 flattens to [i64,i64] = 2 results, over
	// MaxFlatResults -- buildHostWrapper supports this via the Canonical
	// ABI's "lower" spill convention (see lowerHostResults's doc) rather
	// than rejecting it: the wrapper takes one extra i32 out-pointer param
	// and returns nothing at the core level, matching what a real
	// wit-component-generated import of a wide-result WASI func (e.g.
	// wasi:io/streams' check-write) actually declares.
	rec := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u64"}},
	}}
	fn, params, results, err := buildHostWrapper("i", "f", &hostImport{fn: noopLog, results: []binary.TypeDesc{rec}}, newHandleTable(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fn == nil {
		t.Fatal("nil wrapper")
	}
	if len(params) != 1 || params[0] != api.ValueTypeI32 {
		t.Fatalf("params: %#v, want a single i32 out-pointer", params)
	}
	if results != nil {
		t.Fatalf("results: %#v, want none (spilled to memory)", results)
	}
}

func TestBuildHostWrapper_Success(t *testing.T) {
	fn, params, results, err := buildHostWrapper("i", "f",
		&hostImport{fn: noopLog, params: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}}, newHandleTable(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fn == nil {
		t.Fatal("nil wrapper")
	}
	if len(params) != 2 || params[0] != api.ValueTypeI32 || params[1] != api.ValueTypeI32 {
		t.Fatalf("params: %#v", params)
	}
	if results != nil {
		t.Fatalf("results: %#v", results)
	}
}

// memModule instantiates strmod_core (exports memory + cabi_realloc) so the
// wrapper helpers can be exercised against a real linear memory.
func memModule(t *testing.T) (context.Context, api.Module) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	mod, err := r.InstantiateWithConfig(ctx, strmodCoreWasm, wazy.NewModuleConfig().WithName("m"))
	if err != nil {
		t.Fatalf("instantiate strmod: %v", err)
	}
	return ctx, mod
}

func TestLiftHostArgs_String(t *testing.T) {
	_, mod := memModule(t)
	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil)

	// Write "hi" into the module's memory and pass (ptr,len).
	mem := mod.Memory()
	if !mem.WriteString(0, "hi") {
		t.Fatal("write failed")
	}
	args, err := liftHostArgs(fd, resolve, []uint64{0, 2}, mod, newHandleTable())
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args[0].(string) != "hi" {
		t.Fatalf("args: %#v", args)
	}
}

func TestLiftHostArgs_StackUnderflow(t *testing.T) {
	_, mod := memModule(t)
	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil)
	if _, err := liftHostArgs(fd, resolve, []uint64{0}, mod, newHandleTable()); err == nil {
		t.Fatal("expected stack underflow error")
	}
}

func TestLiftHostArgs_NeedsMemoryNone(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	mod, err := r.InstantiateWithConfig(ctx, dummyCoreWasm, wazy.NewModuleConfig().WithName("d"))
	if err != nil {
		t.Fatal(err)
	}
	fd, resolve := synthFuncDesc([]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil)
	if _, err := liftHostArgs(fd, resolve, []uint64{0, 0}, mod, newHandleTable()); err == nil {
		t.Fatal("expected memory-required error")
	}
}

func TestLowerHostResults(t *testing.T) {
	ctx, mod := memModule(t)
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}})

	stack := make([]uint64, 1)
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{uint32(7)}, stack, mod, newHandleTable(), -1, nil); err != nil {
		t.Fatal(err)
	}
	if stack[0] != 7 {
		t.Fatalf("stack[0]=%d, want 7", stack[0])
	}

	// count mismatch
	if err := lowerHostResults(ctx, fd, resolve, nil, stack, mod, newHandleTable(), -1, nil); err == nil {
		t.Fatal("expected count-mismatch error")
	}

	// zero results is a no-op
	fdEmpty, resEmpty := synthFuncDesc(nil, nil)
	if err := lowerHostResults(ctx, fdEmpty, resEmpty, nil, nil, mod, newHandleTable(), -1, nil); err != nil {
		t.Fatalf("zero results: %v", err)
	}

	// multiple results unsupported
	fdMulti, resMulti := synthFuncDesc(nil, []binary.TypeDesc{
		binary.PrimitiveDesc{Prim: "u32"}, binary.PrimitiveDesc{Prim: "u32"},
	})
	if err := lowerHostResults(ctx, fdMulti, resMulti, []abi.Value{uint32(1), uint32(2)}, make([]uint64, 2), mod, newHandleTable(), -1, nil); err == nil {
		t.Fatal("expected multiple-results error")
	}
}

// TestLowerHostResults_Spilled proves the out-pointer path itself (not just
// buildHostWrapper's signature computation, see
// TestBuildHostWrapper_SpilledResults): given outPtrIdx naming the stack
// slot holding a guest-allocated buffer, lowerHostResults Store()s the full
// (non-flat) result there and leaves the core stack otherwise untouched --
// then reads it back via abi.Load to confirm the bytes really landed.
func TestLowerHostResults_Spilled(t *testing.T) {
	ctx, mod := memModule(t)
	rec := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u64"}},
	}}
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{rec})

	const outPtr = 64
	stack := []uint64{outPtr}
	rv := []abi.Value{[]abi.Value{uint64(11), uint64(22)}}
	if err := lowerHostResults(ctx, fd, resolve, rv, stack, mod, newHandleTable(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if stack[0] != outPtr {
		t.Fatalf("spilled lowering must not touch the out-pointer stack slot, got %d", stack[0])
	}

	mem, ok := memoryBytesOf(mod)
	if !ok {
		t.Fatal("expected memory")
	}
	got, err := abi.Load(mem, outPtr, rec, resolve)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fields, ok := got.([]abi.Value)
	if !ok || len(fields) != 2 {
		t.Fatalf("Load: got %#v", got)
	}
	if fields[0].(uint64) != 11 || fields[1].(uint64) != 22 {
		t.Fatalf("Load: got %#v, want [11 22]", fields)
	}
}

// TestLowerHostResults_SpilledNeedsMemory proves the spilled path fails
// loud (rather than dereferencing a bogus pointer into a nil mem slice) when
// the calling module has no linear memory at all.
func TestLowerHostResults_SpilledNeedsMemory(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	mod, err := r.InstantiateWithConfig(ctx, dummyCoreWasm, wazy.NewModuleConfig().WithName("d3"))
	if err != nil {
		t.Fatal(err)
	}
	rec := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u64"}},
	}}
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{rec})
	rv := []abi.Value{[]abi.Value{uint64(1), uint64(2)}}
	err = lowerHostResults(ctx, fd, resolve, rv, []uint64{0}, mod, newHandleTable(), 0, nil)
	requireErrContains(t, err, "no memory")
}

func TestLowerHostResults_NeedsMemoryNone(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	mod, err := r.InstantiateWithConfig(ctx, dummyCoreWasm, wazy.NewModuleConfig().WithName("d2"))
	if err != nil {
		t.Fatal(err)
	}
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}})
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{"x"}, make([]uint64, 2), mod, newHandleTable(), -1, nil); err == nil {
		t.Fatal("expected memory-required error")
	}
}
