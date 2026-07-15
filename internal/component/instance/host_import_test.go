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

func TestImports_NestedInstances(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Instances = []binary.Instance{{Kind: 0x00}}
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "nested component instance")
}

func TestImports_UnsupportedCanonKind(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Canons = append(comp.Canons, binary.Canon{Kind: 0x02}) // resource.new
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

func TestImports_ExportNotFunc(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Exports[0].ExternType = 0x05 // instance
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "only func exports")
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
	requireErrContains(t, err, "lowered import func")
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

func TestImports_LiftTypeNotFunc(t *testing.T) {
	comp := decodeLogHello(t)
	comp.Types[1] = binary.Type{Descriptor: binary.PrimitiveDesc{Prim: "u32"}}
	_, err := runImport(t, comp, stringLogOpt(noopLog))
	requireErrContains(t, err, "not a func type")
}

// ------- pure helper unit tests -------

func TestModuleNameFor(t *testing.T) {
	if n, err := moduleNameFor(2, nil); err != nil || n != "wazy:component/core2" {
		t.Fatalf("root: got %q, %v", n, err)
	}
	if n, err := moduleNameFor(0, []string{"libc"}); err != nil || n != "libc" {
		t.Fatalf("single: got %q, %v", n, err)
	}
	if _, err := moduleNameFor(0, []string{"a", "b"}); err == nil {
		t.Fatal("expected error for multiple names")
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
	_, _, _, err := buildHostWrapper("i", "f", &hostImport{fn: noopLog, params: params})
	requireErrContains(t, err, "whole-parameter-list spilling")
}

func TestBuildHostWrapper_SpilledResults(t *testing.T) {
	// A record of two u64 flattens to [i64,i64] = 2 results, over MaxFlatResults.
	rec := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u64"}},
	}}
	_, _, _, err := buildHostWrapper("i", "f", &hostImport{fn: noopLog, results: []binary.TypeDesc{rec}})
	requireErrContains(t, err, "spilled results")
}

func TestBuildHostWrapper_Success(t *testing.T) {
	fn, params, results, err := buildHostWrapper("i", "f",
		&hostImport{fn: noopLog, params: []binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}})
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
	args, err := liftHostArgs(fd, resolve, []uint64{0, 2}, mod)
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
	if _, err := liftHostArgs(fd, resolve, []uint64{0}, mod); err == nil {
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
	if _, err := liftHostArgs(fd, resolve, []uint64{0, 0}, mod); err == nil {
		t.Fatal("expected memory-required error")
	}
}

func TestLowerHostResults(t *testing.T) {
	ctx, mod := memModule(t)
	fd, resolve := synthFuncDesc(nil, []binary.TypeDesc{binary.PrimitiveDesc{Prim: "u32"}})

	stack := make([]uint64, 1)
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{uint32(7)}, stack, mod); err != nil {
		t.Fatal(err)
	}
	if stack[0] != 7 {
		t.Fatalf("stack[0]=%d, want 7", stack[0])
	}

	// count mismatch
	if err := lowerHostResults(ctx, fd, resolve, nil, stack, mod); err == nil {
		t.Fatal("expected count-mismatch error")
	}

	// zero results is a no-op
	fdEmpty, resEmpty := synthFuncDesc(nil, nil)
	if err := lowerHostResults(ctx, fdEmpty, resEmpty, nil, nil, mod); err != nil {
		t.Fatalf("zero results: %v", err)
	}

	// multiple results unsupported
	fdMulti, resMulti := synthFuncDesc(nil, []binary.TypeDesc{
		binary.PrimitiveDesc{Prim: "u32"}, binary.PrimitiveDesc{Prim: "u32"},
	})
	if err := lowerHostResults(ctx, fdMulti, resMulti, []abi.Value{uint32(1), uint32(2)}, make([]uint64, 2), mod); err == nil {
		t.Fatal("expected multiple-results error")
	}
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
	if err := lowerHostResults(ctx, fd, resolve, []abi.Value{"x"}, make([]uint64, 2), mod); err == nil {
		t.Fatal("expected memory-required error")
	}
}
