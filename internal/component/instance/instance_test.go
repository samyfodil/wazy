package instance

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

//go:embed testdata/return42.wasm
var return42Wasm []byte

//go:embed testdata/returnneg7.wasm
var returnNeg7Wasm []byte

//go:embed testdata/withimport.wasm
var withImportWasm []byte

//go:embed testdata/empty_core.wasm
var emptyCoreWasm []byte

//go:embed testdata/dummy_core.wasm
var dummyCoreWasm []byte

//go:embed testdata/addone_core.wasm
var addoneCoreWasm []byte

//go:embed testdata/strmod_core.wasm
var strmodCoreWasm []byte

// ------- end-to-end (black box, via real component bytes) -------

// TestReturn42 is the M3 STEP 2 milestone proof: a real no-import component
// is instantiated end to end and its exported function returns the
// correctly lifted result.
func TestReturn42(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, return42Wasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got, ok := results[0].(uint32)
	if !ok {
		t.Fatalf("expected uint32 result, got %T (%v)", results[0], results[0])
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

// TestReturnNeg7 proves lifting isn't hardcoded to u32: a different result
// type (s64) is lifted correctly too.
func TestReturnNeg7(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, returnNeg7Wasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	got, ok := results[0].(int64)
	if !ok {
		t.Fatalf("expected int64 result, got %T (%v)", results[0], results[0])
	}
	if got != -7 {
		t.Fatalf("expected -7, got %d", got)
	}
}

func TestInstantiate_ComponentWithImport(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := Instantiate(ctx, r, withImportWasm)
	if err == nil {
		t.Fatal("expected an error instantiating a component with an import, got nil")
	}
	if !strings.Contains(err.Error(), "import") {
		t.Fatalf("expected error to mention imports, got: %v", err)
	}
}

func TestInstantiate_DecodeError(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	_, err := Instantiate(ctx, r, []byte("not a component"))
	if err == nil {
		t.Fatal("expected a decode error, got nil")
	}
}

func TestCall_UnknownExport(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, return42Wasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected an error calling an unknown export, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected error to mention 'not found', got: %v", err)
	}
}

func TestCall_WrongArgCount(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, return42Wasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run", uint32(1))
	if err == nil {
		t.Fatal("expected an error calling with the wrong number of args, got nil")
	}
	if !strings.Contains(err.Error(), "parameter") {
		t.Fatalf("expected error to mention parameters, got: %v", err)
	}
}

func TestClose(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, return42Wasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ------- white-box: instantiateComponent structural validation -------
//
// These build a binary.Component directly rather than through
// binary.Decode, so each validation branch in instantiateComponent can be
// exercised precisely without needing to coax wasm-tools into emitting a
// specific (and sometimes invalid-by-the-spec) shape.

func newRuntime(t *testing.T) (context.Context, wazy.Runtime) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	return ctx, r
}

func u32Type() binary.TypeDesc { return binary.PrimitiveDesc{Prim: "u32"} }

// funcType returns a func type descriptor and, when withResultTypeIdx is
// non-nil, a result referencing that type index instead of a primitive.
func funcType(params []binary.FuncParam, result binary.TypeRef) binary.FuncDesc {
	return binary.FuncDesc{Params: params, Results: binary.FuncResults{Unnamed: &result}}
}

// baseValidComponent returns a Component that instantiateComponent accepts:
// one core module (given by coreBytes), one core instance, one core func
// alias to exportName, one canon lift of a func type with no params and a
// u32 result, and one component export of that canon.
func baseValidComponent(exportName string) *binary.Component {
	return &binary.Component{
		Types: []binary.Type{{Index: 0, Descriptor: funcType(nil, binary.TypeRef{Primitive: "u32"})}},
		CoreModules: []binary.CoreModule{
			{Offset: 0, Size: 0}, // filled in by caller via componentBytes length
		},
		CoreInstances: []binary.CoreInstance{{Kind: 0x00, ModuleIdx: 0}},
		Aliases: []binary.AliasDef{
			{Sort: 0x00, TargetKind: 0x01, InstanceIdx: 0, Name: exportName},
		},
		Canons: []binary.Canon{
			{Kind: 0x00, CoreFuncIdx: 0, TypeIdx: 0},
		},
		Exports: []binary.Export{
			{Name: "run", ExternType: 0x01, ExternIndex: 0},
		},
	}
}

func TestInstantiateComponent_Success(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 0 {
		t.Fatalf("expected uint32(0) (dummy_core's \"f\" always returns 0), got %#v", results[0])
	}
}

func TestInstantiateComponent_NestedInstances(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Instances = []binary.Instance{{Kind: 0x00, ComponentIdx: 0}}

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "nested component instance")
}

func TestInstantiateComponent_CoreModuleCount(t *testing.T) {
	for name, mutate := range map[string]func(*binary.Component){
		"zero": func(c *binary.Component) { c.CoreModules = nil },
		"two": func(c *binary.Component) {
			c.CoreModules = append(c.CoreModules, binary.CoreModule{Offset: 0, Size: len(emptyCoreWasm)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, r := newRuntime(t)
			comp := baseValidComponent("f")
			comp.CoreModules[0].Size = len(emptyCoreWasm)
			mutate(comp)

			_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
			requireErrContains(t, err, "core module")
		})
	}
}

func TestInstantiateComponent_CoreInstanceCount(t *testing.T) {
	for name, mutate := range map[string]func(*binary.Component){
		"zero": func(c *binary.Component) { c.CoreInstances = nil },
		"two": func(c *binary.Component) {
			c.CoreInstances = append(c.CoreInstances, binary.CoreInstance{Kind: 0x00, ModuleIdx: 0})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, r := newRuntime(t)
			comp := baseValidComponent("f")
			comp.CoreModules[0].Size = len(emptyCoreWasm)
			mutate(comp)

			_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
			requireErrContains(t, err, "core instance")
		})
	}
}

func TestInstantiateComponent_CoreInstanceKind(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.CoreInstances[0].Kind = 0x01 // inline exports

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "inline-export")
}

func TestInstantiateComponent_CoreInstanceModuleIdx(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.CoreInstances[0].ModuleIdx = 5

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "module index")
}

func TestInstantiateComponent_CoreInstanceArgs(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.CoreInstances[0].Args = []binary.CoreInstantiateArg{{Name: "env", InstanceIdx: 0}}

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "argument")
}

func TestInstantiateComponent_CoreModuleOutOfBounds(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm) + 100

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "out of bounds")
}

func TestInstantiateComponent_InvalidCoreWasm(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	garbage := []byte("this is not wasm")
	comp.CoreModules[0].Size = len(garbage)

	_, err := instantiateComponent(ctx, r, comp, garbage)
	requireErrContains(t, err, "instantiate embedded core module")
}

func TestInstantiateComponent_AliasSort(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Aliases[0].Sort = 0x01 // non-core (component-level func)

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "non-core sort")
}

func TestInstantiateComponent_AliasTargetKind(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Aliases[0].TargetKind = 0x00 // export, not core export

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "targets kind")
}

func TestInstantiateComponent_AliasCoreSort(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Aliases[0].CoreSort = 0x02 // memory, not func

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "core:sort")
}

func TestInstantiateComponent_AliasInstanceIdx(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Aliases[0].InstanceIdx = 3

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "core instance 3")
}

func TestInstantiateComponent_CanonKind(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Canons[0].Kind = 0x01 // lower

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "canon lift")
}

func TestInstantiateComponent_CanonCoreFuncIdxOutOfRange(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Canons[0].CoreFuncIdx = 99

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "core func index space")
}

func TestInstantiateComponent_CanonTypeIdxOutOfRange(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Canons[0].TypeIdx = 99

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "out of range of")
}

func TestInstantiateComponent_CanonTypeNotFunc(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Types[0].Descriptor = u32Type() // not a FuncDesc

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "not a func type")
}

func TestInstantiateComponent_ExportExternType(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Exports[0].ExternType = 0x02 // value

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "only func exports")
}

func TestInstantiateComponent_ExportIndexOutOfRange(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(emptyCoreWasm)
	comp.Exports[0].ExternIndex = 42

	_, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	requireErrContains(t, err, "component func index space")
}

// ------- white-box: Call-level branches -------

func TestCall_CoreFuncMissing(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("does-not-exist-in-core-module")
	comp.CoreModules[0].Size = len(emptyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, emptyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	requireErrContains(t, err, "has no exported function")
}

func TestCall_FlattenFuncError(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	// A param type reference with neither a primitive name nor a type
	// index is invalid and should surface as a FlattenFunc error.
	comp.Types[0] = binary.Type{Descriptor: funcType(
		[]binary.FuncParam{{Name: "bad", Type: binary.TypeRef{}}},
		binary.TypeRef{Primitive: "u32"},
	)}

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run", uint32(1))
	requireErrContains(t, err, "flatten func type")
}

func TestCall_StringParamRequiresMemory(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: funcType(
		[]binary.FuncParam{{Name: "s", Type: binary.TypeRef{Primitive: "string"}}},
		binary.TypeRef{Primitive: "u32"},
	)}

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run", "hello")
	requireErrContains(t, err, "requires linear memory")
}

func TestCall_StringResultRequiresMemory(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: funcType(nil, binary.TypeRef{Primitive: "string"})}

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	requireErrContains(t, err, "requires linear memory")
}

// TestCall_SpilledResultRequiresMemory proves the "returned via a memory
// pointer" spill path (a result flattening to more than MaxFlatResults core
// values, e.g. a record{u64,u64} -> [i64,i64]) needs linear memory as
// scratch space for the pointer indirection even when the value's own type
// otherwise wouldn't (per usesMemory, a record of two u64s doesn't need
// memory to lower/lift directly) -- dummy_core.wasm exports no memory, so
// this must fail loud rather than dereference a garbage/absent pointer.
func TestCall_SpilledResultRequiresMemory(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	recordType := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "b", Type: binary.TypeRef{Primitive: "u64"}},
	}}
	comp.Types = []binary.Type{
		{Index: 0, Descriptor: recordType},
		{Index: 1, Descriptor: funcType(nil, binary.TypeRef{TypeIndex: idxPtr(0)})},
	}
	comp.Canons[0].TypeIdx = 1

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	requireErrContains(t, err, "must be returned via a memory pointer")
}

func TestCall_MultipleNamedResultsNotSupported(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: binary.FuncDesc{
		Results: binary.FuncResults{Named: []binary.FuncResult{
			{Name: "a", Type: binary.TypeRef{Primitive: "u32"}},
			{Name: "b", Type: binary.TypeRef{Primitive: "u32"}},
		}},
	}}

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	requireErrContains(t, err, "named results")
}

func TestCall_ZeroResults(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("g") // dummy_core's "g": no params, no results
	comp.CoreModules[0].Size = len(dummyCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: binary.FuncDesc{}} // no params, no results

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %#v", results)
	}
}

func TestCall_ResultCountMismatch(t *testing.T) {
	ctx, r := newRuntime(t)
	// "g" returns zero core values, but the component func type declares a
	// u32 result (1 flat value): the raw result count won't match.
	comp := baseValidComponent("g")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	requireErrContains(t, err, "expected 1")
}

// ------- white-box: params, memory, and realloc happy paths -------

// TestCall_WithU32Param exercises a genuine one-argument call (lowerParams'
// success path, not just its error branches): the core func adds 1 to its
// argument.
func TestCall_WithU32Param(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("addone")
	comp.CoreModules[0].Size = len(addoneCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: funcType(
		[]binary.FuncParam{{Name: "x", Type: binary.TypeRef{Primitive: "u32"}}},
		binary.TypeRef{Primitive: "u32"},
	)}

	inst, err := instantiateComponent(ctx, r, comp, addoneCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run", uint32(41))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 42 {
		t.Fatalf("expected uint32(42), got %#v", results[0])
	}
}

// TestCall_WithStringParam exercises the full memory-backed path: a real
// "memory" export, a real "cabi_realloc" export, LowerFlat actually storing
// the string's UTF-8 bytes into linear memory, and the core func reading
// them back (here, just returning the length it was given).
func TestCall_WithStringParam(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("strlen")
	comp.CoreModules[0].Size = len(strmodCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: funcType(
		[]binary.FuncParam{{Name: "s", Type: binary.TypeRef{Primitive: "string"}}},
		binary.TypeRef{Primitive: "u32"},
	)}

	inst, err := instantiateComponent(ctx, r, comp, strmodCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run", "hello")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 5 {
		t.Fatalf("expected uint32(5), got %#v", results[0])
	}

	mem, ok := memoryBytesOf(inst.exports["run"].mod)
	if !ok || mem == nil {
		t.Fatal("expected the strmod core module's memory to be available")
	}
}

func TestReallocFn_Success(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("strlen")
	comp.CoreModules[0].Size = len(strmodCoreWasm)
	comp.Types[0] = binary.Type{Descriptor: funcType(
		[]binary.FuncParam{{Name: "s", Type: binary.TypeRef{Primitive: "string"}}},
		binary.TypeRef{Primitive: "u32"},
	)}

	inst, err := instantiateComponent(ctx, r, comp, strmodCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	realloc := reallocOf(ctx, inst.exports["run"].mod)
	ptr, err := realloc(0, 0, 4, 8)
	if err != nil {
		t.Fatalf("realloc: %v", err)
	}
	if ptr == 0 {
		t.Fatalf("expected a non-zero pointer from the bump allocator, got %d", ptr)
	}
}

func TestLowerParams_ResolveError(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	fd := binary.FuncDesc{Params: []binary.FuncParam{{Name: "bad", Type: binary.TypeRef{}}}}
	be := &boundExport{mod: inst.exports["run"].mod, funcName: "run", fd: fd}
	finalizeBoundExport(be, inst.resolve)
	_, err = inst.lowerParams(be, []any{uint32(1)}, nil, false, reallocOf(ctx, inst.exports["run"].mod), "x", nil)
	requireErrContains(t, err, "type reference has neither")
}

func TestLowerParams_LowerFlatError(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	fd := binary.FuncDesc{Params: []binary.FuncParam{{Name: "x", Type: binary.TypeRef{Primitive: "u32"}}}}
	be := &boundExport{mod: inst.exports["run"].mod, funcName: "run", fd: fd}
	finalizeBoundExport(be, inst.resolve)
	// A string where a u32 is expected: LowerFlat itself should reject it.
	_, err = inst.lowerParams(be, []any{"not a u32"}, nil, false, reallocOf(ctx, inst.exports["run"].mod), "x", nil)
	requireErrContains(t, err, "lower:")
}

// ------- unit tests for small helpers -------

func TestUsesMemory(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc {
		switch idx {
		case 0:
			return binary.PrimitiveDesc{Prim: "string"}
		case 1:
			return binary.PrimitiveDesc{Prim: "u32"}
		}
		return nil
	}

	tests := []struct {
		name string
		t    binary.TypeDesc
		want bool
	}{
		{"primitive-u32", binary.PrimitiveDesc{Prim: "u32"}, false},
		{"primitive-string", binary.PrimitiveDesc{Prim: "string"}, true},
		{"list", binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}}, true},
		{"record-no-string", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}}}, false},
		{"record-with-string", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{Primitive: "string"}}}}, true},
		{"record-bad-ref", binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{}}}}, false},
		{"tuple-no-string", binary.TupleDesc{Elements: []binary.TypeRef{{Primitive: "u32"}}}, false},
		{"tuple-with-string", binary.TupleDesc{Elements: []binary.TypeRef{{Primitive: "string"}}}, true},
		{"variant-no-payload", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: nil}}}, false},
		{"variant-with-string", binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: &binary.TypeRef{Primitive: "string"}}}}, true},
		{"option-no-string", binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}}, false},
		{"option-with-string", binary.OptionDesc{Element: binary.TypeRef{Primitive: "string"}}, true},
		{"result-ok-string", binary.ResultDesc{Ok: &binary.TypeRef{Primitive: "string"}}, true},
		{"result-err-string", binary.ResultDesc{Err: &binary.TypeRef{Primitive: "string"}}, true},
		{"result-neither", binary.ResultDesc{}, false},
		{"result-ok-plain", binary.ResultDesc{Ok: &binary.TypeRef{Primitive: "u32"}}, false},
		{"by-index-string", binary.PrimitiveDesc{Prim: "u32"}, false}, // placeholder, overwritten below
		{"func-desc-default", binary.FuncDesc{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usesMemory(tt.t, resolve); got != tt.want {
				t.Errorf("usesMemory(%v) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}

	// Resolved-by-index cases, exercised separately since they need the
	// closure above.
	if !usesMemory(binary.ListDesc{Element: binary.TypeRef{TypeIndex: idxPtr(0)}}, resolve) {
		t.Error("list element resolved by index to string should use memory")
	}
	if usesMemory(binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: binary.TypeRef{TypeIndex: idxPtr(1)}}}}, resolve) {
		t.Error("record field resolved by index to u32 should not use memory")
	}
}

func TestResolveTypeRef(t *testing.T) {
	resolve := func(idx uint32) binary.TypeDesc {
		if idx == 0 {
			return binary.PrimitiveDesc{Prim: "u32"}
		}
		return nil
	}

	if _, err := resolveTypeRef(&binary.TypeRef{Primitive: "u32"}, resolve); err != nil {
		t.Errorf("primitive ref: unexpected error: %v", err)
	}
	if got, err := resolveTypeRef(&binary.TypeRef{TypeIndex: idxPtr(0)}, resolve); err != nil || got == nil {
		t.Errorf("indexed ref: got %v, %v", got, err)
	}
	if _, err := resolveTypeRef(&binary.TypeRef{TypeIndex: idxPtr(99)}, resolve); err == nil {
		t.Error("expected an error resolving an out-of-range type index")
	}
	if _, err := resolveTypeRef(&binary.TypeRef{}, resolve); err == nil {
		t.Error("expected an error resolving a TypeRef with neither a primitive nor an index")
	}
}

func TestFuncResultTypeRefs(t *testing.T) {
	if got := funcResultTypeRefs(binary.FuncDesc{}); len(got) != 0 {
		t.Errorf("expected 0 result refs for a zero-value FuncDesc, got %d", len(got))
	}
	unnamed := binary.TypeRef{Primitive: "u32"}
	if got := funcResultTypeRefs(binary.FuncDesc{Results: binary.FuncResults{Unnamed: &unnamed}}); len(got) != 1 {
		t.Errorf("expected 1 result ref for an unnamed result, got %d", len(got))
	}
	named := []binary.FuncResult{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}, {Name: "b", Type: binary.TypeRef{Primitive: "u32"}}}
	if got := funcResultTypeRefs(binary.FuncDesc{Results: binary.FuncResults{Named: named}}); len(got) != 2 {
		t.Errorf("expected 2 result refs for two named results, got %d", len(got))
	}
}

func TestReallocFn_NoExport(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	realloc := reallocOf(ctx, inst.exports["run"].mod)
	if _, err := realloc(0, 0, 4, 8); err == nil {
		t.Fatal("expected an error calling realloc with no cabi_realloc export, got nil")
	}
}

func TestMemoryBytes_NoMemory(t *testing.T) {
	ctx, r := newRuntime(t)
	comp := baseValidComponent("f")
	comp.CoreModules[0].Size = len(dummyCoreWasm)

	inst, err := instantiateComponent(ctx, r, comp, dummyCoreWasm)
	if err != nil {
		t.Fatalf("instantiateComponent: %v", err)
	}
	defer inst.Close(ctx)

	mem, ok := memoryBytesOf(inst.exports["run"].mod)
	if ok || mem != nil {
		t.Fatalf("expected (nil, false) for a module with no memory export, got (%v, %v)", mem, ok)
	}
}

func idxPtr(i uint32) *uint32 { return &i }

func requireErrContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error to contain %q, got: %v", substr, err)
	}
}

// ------- import direction (M3 STEP 3): host function called with "hello" -------

//go:embed testdata/log_hello.wasm
var logHelloWasm []byte

// TestLogHello is the M3 STEP 3 milestone proof: a component that imports
// log: func(msg: string) and whose exported run() calls it must invoke a host
// Go function with the Go string "hello".
func TestLogHello(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var captured string
	var calls int
	logFn := func(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
		calls++
		if len(args) != 1 {
			t.Fatalf("log: expected 1 arg, got %d", len(args))
		}
		s, ok := args[0].(string)
		if !ok {
			t.Fatalf("log: expected string arg, got %T", args[0])
		}
		captured = s
		return nil, nil
	}

	inst, err := Instantiate(ctx, r, logHelloWasm,
		WithImport("test:pkg/host", "log", logFn,
			[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "run")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if results != nil {
		t.Fatalf("run has no results, got %#v", results)
	}
	if calls != 1 {
		t.Fatalf("expected the host log to be called exactly once, got %d", calls)
	}
	if captured != "hello" {
		t.Fatalf("host log captured %q, want %q", captured, "hello")
	}
}

// TestTwoLogHelloCoexist proves two single-core host-import components (the
// path that used to be instantiateWithImports, now routed through the graph
// engine) coexist on one Runtime. Before internals were instantiated
// anonymously, the second collided on its "libc" core-module "with" name.
func TestTwoLogHelloCoexist(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var aCalls, bCalls int
	mk := func(n *int) *Instance {
		fn := func(context.Context, []abi.Value) ([]abi.Value, error) { *n++; return nil, nil }
		inst, err := Instantiate(ctx, r, logHelloWasm, stringLogOpt(fn))
		if err != nil {
			t.Fatalf("Instantiate: %v", err)
		}
		return inst
	}
	a := mk(&aCalls)
	defer a.Close(ctx)
	b := mk(&bCalls) // previously: module[libc] has already been instantiated
	defer b.Close(ctx)

	if _, err := a.Call(ctx, "run"); err != nil {
		t.Fatalf("a.run: %v", err)
	}
	if _, err := b.Call(ctx, "run"); err != nil {
		t.Fatalf("b.run: %v", err)
	}
	if aCalls != 1 || bCalls != 1 {
		t.Fatalf("each instance's own log should fire once: a=%d b=%d", aCalls, bCalls)
	}
}

// TestLogHello_MissingHostImpl fails loud when an import the component lowers is
// actually CALLED with no host implementation registered. Instantiation itself
// succeeds -- the graph engine wires an unimplemented import to a trap stub (an
// import you never call is fine, matching wasmtime) -- and the error surfaces
// when "run" invokes the missing log import.
func TestLogHello_MissingHostImpl(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, logHelloWasm) // no WithImport
	if err != nil {
		t.Fatalf("Instantiate should succeed (trap stub for the unimplemented import): %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	if err == nil {
		t.Fatal("expected calling run to fail loud on the unimplemented log import, got nil")
	}
}

// TestLogHello_WrongDeclaredResultCount registers a HostFunc that returns a
// result the import type says it shouldn't; the mismatch must surface as a
// call error, not be silently dropped.
func TestLogHello_HostResultCountMismatch(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// Declared type: (msg: string) with no results, but the impl returns one.
	badFn := func(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
		return []abi.Value{uint32(1)}, nil
	}
	inst, err := Instantiate(ctx, r, logHelloWasm,
		WithImport("test:pkg/host", "log", badFn,
			[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	if err == nil {
		t.Fatal("expected a call error from the result-count mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "result") {
		t.Fatalf("expected error to mention results, got: %v", err)
	}
}

// TestLogHello_HostFuncError propagates an error returned by the HostFunc out
// through the originating Call.
func TestLogHello_HostFuncError(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	boomFn := func(ctx context.Context, args []abi.Value) ([]abi.Value, error) {
		return nil, fmt.Errorf("boom from host")
	}
	inst, err := Instantiate(ctx, r, logHelloWasm,
		WithImport("test:pkg/host", "log", boomFn,
			[]binary.TypeDesc{binary.PrimitiveDesc{Prim: "string"}}, nil))
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.Call(ctx, "run")
	if err == nil {
		t.Fatal("expected the host error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "boom from host") {
		t.Fatalf("expected error to contain the host error, got: %v", err)
	}
}
