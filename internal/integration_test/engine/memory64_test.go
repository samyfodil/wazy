package adhoc

// End-to-end coverage of the memory64 proposal on both engines: a module whose
// memory and table are both i64-indexed, plus the embedder-facing surface --
// api.Memory's 64-bit accessors and the WithMemory64LimitPages ceiling.
//
// The specification's own suite (internal/integration_test/spectest/memory64)
// covers instruction semantics exhaustively; what is here is the part it cannot
// reach, being written in wasm.

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

//go:embed testdata/memory64.wasm
var memory64Wasm []byte

//go:embed testdata/memory64_mixed.wasm
var memory64MixedWasm []byte

//go:embed testdata/memory64_shared.wasm
var memory64SharedWasm []byte

//go:embed testdata/memory64_elem_global.wasm
var memory64ElemGlobalWasm []byte

//go:embed testdata/memory64_elem_global_env.wasm
var memory64ElemGlobalEnvWasm []byte

const memory64Features = api.CoreFeaturesV2 | api.CoreFeatureMemory64

func TestMemory64Compiler(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	runMemory64Tests(t, wazy.NewRuntimeConfigCompiler())
}

func TestMemory64Interpreter(t *testing.T) {
	runMemory64Tests(t, wazy.NewRuntimeConfigInterpreter())
}

func runMemory64Tests(t *testing.T, config wazy.RuntimeConfig) {
	for name, f := range map[string]func(*testing.T, wazy.RuntimeConfig){
		"instructions":                   testMemory64Instructions,
		"mixed index types":              testMemory64MixedIndexTypes,
		"vector access":                  testMemory64Vector,
		"shared atomics":                 testMemory64SharedAtomics,
		"element segment offset":         testMemory64ElementSegmentOffset,
		"wide offset on a 32-bit memory": testMemory64WideOffsetOn32BitMemory,
		"api surface":                    testMemory64APISurface,
		"allocation limits":              testMemory64AllocationLimits,
	} {
		t.Run(name, func(t *testing.T) { f(t, config) })
	}
}

func testMemory64Instructions(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(memory64Features))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, memory64Wasm)
	require.NoError(t, err)

	call := func(name string, args ...uint64) []uint64 {
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}
	callErr := func(name string, args ...uint64) error {
		_, err := mod.ExportedFunction(name).Call(ctx, args...)
		return err
	}

	t.Run("loads and stores", func(t *testing.T) {
		require.Equal(t, uint64('h'), call("load8", 0)[0])
		call("store8", 60000, 42)
		require.Equal(t, uint64(42), call("load8", 60000)[0])
		call("store64", 128, ^uint64(0))
		require.Equal(t, ^uint64(0), call("load64", 128)[0])
	})

	t.Run("an address past the end traps, including at the top of the range", func(t *testing.T) {
		for _, addr := range []uint64{1 << 17, 1 << 20, 1 << 40, ^uint64(0), ^uint64(0) - 7} {
			require.ErrorIs(t, callErr("load8", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
			require.ErrorIs(t, callErr("load64", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
			require.ErrorIs(t, callErr("store64", addr, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		}
	})

	t.Run("an offset immediate past four gibibytes traps rather than wrapping", func(t *testing.T) {
		// offset=2^32 plus a zero address is out of bounds, and must stay out of
		// bounds rather than wrapping around to address zero.
		require.ErrorIs(t, callErr("load_off", 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		// address = -2^32 makes address+offset wrap to zero in 64-bit
		// arithmetic; the carry has to be caught.
		require.ErrorIs(t, callErr("load_off", ^uint64(0)-(1<<32)+1), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	})

	t.Run("memory.size and memory.grow are 64-bit", func(t *testing.T) {
		require.Equal(t, uint64(1), call("mem_size")[0])
		require.Equal(t, uint64(1), call("mem_grow", 2)[0])
		require.Equal(t, uint64(3), call("mem_size")[0])
		// Past the declared maximum of four pages.
		require.Equal(t, ^uint64(0), call("mem_grow", 2)[0])
		// A delta that would wrap the page count must fail, not wrap.
		require.Equal(t, ^uint64(0), call("mem_grow", ^uint64(0))[0])
		require.Equal(t, uint64(3), call("mem_size")[0])
	})

	t.Run("bulk memory operations are 64-bit", func(t *testing.T) {
		call("mem_fill", 1024, 7, 16)
		require.Equal(t, uint64(7), call("load8", 1039)[0])
		call("mem_copy", 2048, 1024, 16)
		require.Equal(t, uint64(7), call("load8", 2063)[0])
		call("mem_init", 4096, 0, 3)
		require.Equal(t, uint64('x'), call("load8", 4096)[0])

		// A destination whose end wraps a uint64 must trap.
		require.ErrorIs(t, callErr("mem_fill", ^uint64(0), 0, 2), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("mem_copy", ^uint64(0), 0, 2), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("mem_init", ^uint64(0), 0, 2), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	})

	t.Run("table instructions are 64-bit", func(t *testing.T) {
		require.Equal(t, uint64(3), call("table_size")[0])
		require.Equal(t, uint64(100), call("call_indirect", 0)[0])
		require.Equal(t, uint64(200), call("call_indirect", 1)[0])
		require.Equal(t, uint64(1), call("table_is_null", 2)[0])

		call("table_set_two", 2)
		require.Equal(t, uint64(0), call("table_is_null", 2)[0])
		require.Equal(t, uint64(200), call("call_indirect", 2)[0])

		require.Equal(t, uint64(3), call("table_grow", 2)[0])
		require.Equal(t, uint64(5), call("table_size")[0])
		// Past the declared maximum of eight entries, and past what any table
		// can hold.
		require.Equal(t, ^uint64(0), call("table_grow", 4)[0])
		require.Equal(t, ^uint64(0), call("table_grow", 1<<32)[0])

		call("table_fill", 3, 2)
		call("table_copy", 3, 0, 2)
		require.Equal(t, uint64(100), call("call_indirect", 3)[0])
		call("table_init", 4, 0, 1)
		require.Equal(t, uint64(100), call("call_indirect", 4)[0])

		for _, idx := range []uint64{1 << 20, 1 << 40, ^uint64(0)} {
			require.ErrorIs(t, callErr("call_indirect", idx), wasmruntime.ErrRuntimeInvalidTableAccess)
			require.ErrorIs(t, callErr("table_is_null", idx), wasmruntime.ErrRuntimeInvalidTableAccess)
		}
		// A maximum in the upper half of the u64 range is still a maximum, not a
		// negative number that blocks every growth.
		require.Equal(t, uint64(1), call("big_table_size")[0])
		require.Equal(t, uint64(1), call("big_table_grow", 2)[0])
		require.Equal(t, uint64(3), call("big_table_size")[0])

		require.ErrorIs(t, callErr("table_fill", ^uint64(0), 2), wasmruntime.ErrRuntimeInvalidTableAccess)
		require.ErrorIs(t, callErr("table_copy", ^uint64(0), 0, 2), wasmruntime.ErrRuntimeInvalidTableAccess)
		require.ErrorIs(t, callErr("table_init", ^uint64(0), 0, 1), wasmruntime.ErrRuntimeInvalidTableAccess)
	})
}

// testMemory64Vector covers the vector loads and stores, whose address operand
// follows the memory's index type like every other memory instruction's.
func testMemory64Vector(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(memory64Features))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, memory64Wasm)
	require.NoError(t, err)

	call := func(name string, args ...uint64) []uint64 {
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}
	callErr := func(name string, args ...uint64) error {
		_, err := mod.ExportedFunction(name).Call(ctx, args...)
		return err
	}

	call("v128_store", 256)
	require.Equal(t, uint64(1), call("v128_load_lane0", 256)[0])
	require.Equal(t, uint64(4), call("v128_load_lane3", 256)[0])
	require.Equal(t, uint64(2), call("v128_load32_zero", 260)[0])
	require.Equal(t, uint64(1), call("v128_load8_lane", 256)[0])
	require.Equal(t, uint64(2), call("v128_load8_splat", 260)[0])

	// store8_lane writes lane 5 of an all-zero vector, so the byte it lands on
	// reads back as zero.
	call("v128_store8_lane", 256)
	require.Equal(t, uint64(0), call("v128_load8_splat", 256)[0])

	for _, addr := range []uint64{1 << 17, 1 << 40, ^uint64(0), ^uint64(0) - 15} {
		require.ErrorIs(t, callErr("v128_load_lane0", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("v128_store", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("v128_load8_splat", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("v128_store8_lane", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("v128_load8_lane", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("v128_load32_zero", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	}
	// An offset immediate past four gibibytes, and an address that would make
	// address+offset wrap back to zero.
	require.ErrorIs(t, callErr("v128_load_off", 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	require.ErrorIs(t, callErr("v128_load_off", ^uint64(0)-(1<<32)+1), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)

	// A v128 access spans sixteen bytes, so an address eight below the top of
	// the range makes address+8 carry back to zero. The whole span has to be
	// bounds-checked before either half is touched, or the store lands its upper
	// half at address zero and only then traps -- a trapping store that mutated
	// memory.
	call("v128_store", 0)
	require.Equal(t, uint64(1), call("v128_load_lane0", 0)[0])
	require.ErrorIs(t, callErr("v128_store", ^uint64(0)-7), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	require.Equal(t, uint64(1), call("v128_load_lane0", 0)[0])
	require.ErrorIs(t, callErr("v128_load_lane0", ^uint64(0)-7), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
}

// testMemory64SharedAtomics covers the atomic instructions against a shared
// memory with an i64 index type: their address operand follows the index type,
// and the runtime resolves it back to an offset when it re-enters Go.
func testMemory64SharedAtomics(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx,
		config.WithCoreFeatures(memory64Features|experimental.CoreFeaturesThreads))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, memory64SharedWasm)
	require.NoError(t, err)

	call := func(name string, args ...uint64) []uint64 {
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}
	callErr := func(name string, args ...uint64) error {
		_, err := mod.ExportedFunction(name).Call(ctx, args...)
		return err
	}

	call("store", 64, 7)
	require.Equal(t, uint64(7), call("load", 64)[0])
	require.Equal(t, uint64(7), call("rmw_add", 64, 5)[0])
	require.Equal(t, uint64(12), call("load", 64)[0])
	require.Equal(t, uint64(12), call("cmpxchg", 64, 12, 99)[0])
	require.Equal(t, uint64(99), call("load", 64)[0])

	call("store64", 128, ^uint64(0))
	require.Equal(t, ^uint64(0), call("load64", 128)[0])

	// Nothing is waiting, so notify wakes nobody, and a wait whose expectation
	// does not match the memory returns 1 ("not equal") immediately.
	require.Equal(t, uint64(0), call("notify", 64)[0])
	require.Equal(t, uint64(1), call("wait32", 64, 0)[0])
	require.Equal(t, uint64(1), call("wait64", 128, 0)[0])

	// The i64-result read-modify-writes take their address in the index type too.
	require.Equal(t, ^uint64(0), call("rmw_add64", 128, 1)[0])
	require.Equal(t, uint64(0), call("load64", 128)[0])
	require.Equal(t, uint64(0), call("cmpxchg64", 128, 0, 5)[0])
	require.Equal(t, uint64(5), call("load64", 128)[0])

	for _, addr := range []uint64{1 << 17, 1 << 40, ^uint64(0) &^ 3} {
		require.ErrorIs(t, callErr("load", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("store", addr, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("rmw_add", addr, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("notify", addr), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("wait32", addr, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("wait64", addr&^7, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("rmw_add64", addr&^7, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
		require.ErrorIs(t, callErr("cmpxchg64", addr&^7, 0, 0), wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	}
	// An unaligned atomic address is a separate trap.
	require.ErrorIs(t, callErr("load", 1), wasmruntime.ErrRuntimeUnalignedAtomic)
}

// testMemory64ElementSegmentOffset covers the instantiation-time bounds check on
// an active element segment of a 64-bit table. Reference types are disabled here
// on purpose: that is what defers the check from validation to instantiation,
// and an offset supplied by an imported global is what gets it there.
func testMemory64ElementSegmentOffset(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx,
		config.WithCoreFeatures(api.CoreFeaturesV1|api.CoreFeatureMemory64))
	defer r.Close(ctx)

	_, err := r.Instantiate(ctx, memory64ElemGlobalEnvWasm)
	require.NoError(t, err)

	// The global holds 2^32, which does not fit a one-entry table. Narrowing it
	// to a uint32 would make it zero, and the segment would be accepted.
	_, err = r.Instantiate(ctx, memory64ElemGlobalWasm)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds min table size")
}

// testMemory64MixedIndexTypes covers memory.copy and table.copy between a
// 32-bit and a 64-bit one, whose length operand takes the narrower of the two
// index types -- the one shape of the proposal a single-index-type module
// cannot reach.
func testMemory64MixedIndexTypes(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx,
		config.WithCoreFeatures(memory64Features|api.CoreFeatureMultiMemory))
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, memory64MixedWasm)
	require.NoError(t, err)

	call := func(name string, args ...uint64) []uint64 {
		res, err := mod.ExportedFunction(name).Call(ctx, args...)
		require.NoError(t, err, name)
		return res
	}

	require.Equal(t, uint64('a'), call("load32", 0)[0])
	require.Equal(t, uint64('w'), call("load64", 0)[0])

	// i64 destination, i32 source, i32 length.
	call("copy_64_from_32", 16, 0, 4)
	require.Equal(t, uint64('a'), call("load64", 16)[0])
	require.Equal(t, uint64('d'), call("load64", 19)[0])

	// i32 destination, i64 source, i32 length.
	call("copy_32_from_64", 32, 0, 4)
	require.Equal(t, uint64('w'), call("load32", 32)[0])
	require.Equal(t, uint64('z'), call("load32", 35)[0])

	require.Equal(t, uint64(100), call("call32", 0)[0])
	require.Equal(t, uint64(200), call("call64", 0)[0])
	call("tcopy_64_from_32", 1, 0, 1)
	require.Equal(t, uint64(100), call("call64", 1)[0])
	call("tcopy_32_from_64", 2, 0, 1)
	require.Equal(t, uint64(200), call("call32", 2)[0])
}

func testMemory64APISurface(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(memory64Features))
	defer r.Close(ctx)
	compiled, err := r.CompileModule(ctx, memory64Wasm)
	require.NoError(t, err)

	def, ok := compiled.ExportedMemories()["memory"]
	require.True(t, ok)
	require.True(t, def.IsMemory64())
	require.Equal(t, uint64(1), def.Min64())
	max, encoded := def.Max64()
	require.True(t, encoded)
	require.Equal(t, uint64(4), max)

	mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig())
	require.NoError(t, err)
	mem := mod.Memory()

	require.Equal(t, uint64(wasm.MemoryPageSize), mem.Size64())
	require.Equal(t, uint32(wasm.MemoryPageSize), mem.Size())

	buf, ok := mem.Read64(0, 5)
	require.True(t, ok)
	require.Equal(t, "hello", string(buf))
	require.True(t, mem.WriteString64(8, "wazy"))
	buf, ok = mem.Read64(8, 4)
	require.True(t, ok)
	require.Equal(t, "wazy", string(buf))
	require.True(t, mem.Write64(16, []byte{1, 2, 3}))
	buf, ok = mem.Read64(16, 3)
	require.True(t, ok)
	require.Equal(t, []byte{1, 2, 3}, buf)

	// Out of range, including a length whose end wraps a uint64.
	_, ok = mem.Read64(uint64(wasm.MemoryPageSize), 1)
	require.False(t, ok)
	_, ok = mem.Read64(^uint64(0), 1)
	require.False(t, ok)
	require.False(t, mem.Write64(^uint64(0), []byte{0}))
	require.False(t, mem.WriteString64(^uint64(0), "x"))

	previous, ok := mem.Grow64(1)
	require.True(t, ok)
	require.Equal(t, uint64(1), previous)
	require.Equal(t, uint64(2*wasm.MemoryPageSize), mem.Size64())
	// Past the declared maximum.
	_, ok = mem.Grow64(3)
	require.False(t, ok)
	// A delta that would wrap the page count must fail, not wrap.
	_, ok = mem.Grow64(^uint64(0))
	require.False(t, ok)
	require.Equal(t, uint64(2*wasm.MemoryPageSize), mem.Size64())
}

// testMemory64WideOffsetOn32BitMemory covers the one place the proposal changes
// a module that has no 64-bit memory at all: enabling the feature widens every
// memarg's offset immediate to a u64, so an encoding longer than a u32's five
// bytes becomes legal. Both engines have to consume exactly the bytes the
// validator did, or they desync from the instruction stream.
func testMemory64WideOffsetOn32BitMemory(t *testing.T, config wazy.RuntimeConfig) {
	// (func (result i32) (i32.load align=2 offset=2 (i32.const 0))) with the
	// offset written in six LEB128 bytes instead of one.
	body := []byte{wasm.OpcodeI32Const, 0x00, wasm.OpcodeI32Load, 0x02}
	body = append(body, 0x82, 0x80, 0x80, 0x80, 0x80, 0x00) // offset 2, non-minimal
	body = append(body, wasm.OpcodeEnd)
	bin := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection:     []wasm.FunctionType{{Results: []wasm.ValueType{i32}, ResultNumInUint64: 1}},
		FunctionSection: []wasm.Index{0},
		CodeSection:     []wasm.Code{{Body: body}},
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		ExportSection:   []wasm.Export{{Name: "load", Type: wasm.ExternTypeFunc, Index: 0}},
		DataSection: []wasm.DataSegment{{
			OffsetExpression: wasm.NewConstantExpressionFromI32(0),
			Init:             []byte{0, 0, 7, 0},
		}},
	})

	ctx := context.Background()
	t.Run("accepted with memory64 enabled", func(t *testing.T) {
		r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(memory64Features))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, bin)
		require.NoError(t, err)
		res, err := mod.ExportedFunction("load").Call(ctx)
		require.NoError(t, err)
		require.Equal(t, uint64(7), res[0]) // the byte at offset 2
	})
	t.Run("malformed without it", func(t *testing.T) {
		r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(api.CoreFeaturesV2))
		defer r.Close(ctx)
		_, err := r.Instantiate(ctx, bin)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read memory offset")
	})
}

func testMemory64AllocationLimits(t *testing.T, config wazy.RuntimeConfig) {
	ctx := context.Background()

	// A 64-bit memory declaring more pages than any host could allocate is
	// valid, so it compiles; only instantiating it fails.
	huge := binaryencoding.EncodeModule(&wasm.Module{
		MemorySection: []wasm.Memory{{
			Min: wasm.Memory64LimitPages, Cap: wasm.Memory64LimitPages, Max: wasm.Memory64LimitPages,
			IsMaxEncoded: true, IsMemory64: true,
		}},
	})
	t.Run("a minimum past the limit compiles but does not instantiate", func(t *testing.T) {
		r := wazy.NewRuntimeWithConfig(ctx, config.WithCoreFeatures(memory64Features))
		defer r.Close(ctx)
		compiled, err := r.CompileModule(ctx, huge)
		require.NoError(t, err)
		_, err = r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig())
		require.Error(t, err)
		// The default limit is 65536 pages, itself clamped to what a slice on
		// this platform can address -- 32767 pages where an int is 32 bits.
		require.Contains(t, err.Error(), "exceeds the limit of")
		require.Contains(t, err.Error(), "minimum of 281474976710656 pages")
	})

	t.Run("WithMemory64LimitPages raises and lowers the ceiling", func(t *testing.T) {
		// Declared max is four pages; a limit of two stops growth at two.
		r := wazy.NewRuntimeWithConfig(ctx,
			config.WithCoreFeatures(memory64Features).WithMemory64LimitPages(2))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, memory64Wasm)
		require.NoError(t, err)
		grow := mod.ExportedFunction("mem_grow")
		res, err := grow.Call(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, uint64(1), res[0])
		res, err = grow.Call(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, ^uint64(0), res[0]) // the host limit, not the module's max, binds
	})

	t.Run("a limit of zero forbids a 64-bit memory outright", func(t *testing.T) {
		// Zero is a real setting, not "unset": WithMemoryLimitPages(0) already
		// rejects any 32-bit memory, and the 64-bit knob has to agree. Reading
		// it as "use the default" would turn the strictest value the knob
		// accepts into the most permissive one.
		r := wazy.NewRuntimeWithConfig(ctx,
			config.WithCoreFeatures(memory64Features).WithMemory64LimitPages(0))
		defer r.Close(ctx)
		_, err := r.Instantiate(ctx, memory64Wasm)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds the limit of 0 pages")
	})

	t.Run("the 32-bit limit does not bind a 64-bit memory", func(t *testing.T) {
		// WithMemoryLimitPages tops out at 65536; a 64-bit memory needs its own
		// knob to go past that, and must not be silently capped by this one.
		r := wazy.NewRuntimeWithConfig(ctx,
			config.WithCoreFeatures(memory64Features).WithMemoryLimitPages(1))
		defer r.Close(ctx)
		mod, err := r.Instantiate(ctx, memory64Wasm)
		require.NoError(t, err)
		res, err := mod.ExportedFunction("mem_grow").Call(ctx, 3)
		require.NoError(t, err)
		require.Equal(t, uint64(1), res[0])
	})
}
