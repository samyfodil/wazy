package native_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestSIMDEmulationE2E runs v128 through the compiler on whatever vector unit the
// host has, including none.
//
// NewRuntimeConfigCompiler deliberately: it does not fall back to the interpreter,
// so on a machine with no vector unit this fails or traps rather than quietly
// proving nothing. Under QEMU_CPU=rv64,v=false it is the check that the scalar
// lowering is both selected and correct.
func TestSIMDEmulationE2E(t *testing.T) {
	v128c := func(lo, hi uint64) []byte {
		b := []byte{wasm.OpcodeVecPrefix, wasm.OpcodeVecV128Const}
		for _, w := range []uint64{lo, hi} {
			for i := 0; i < 8; i++ {
				b = append(b, byte(w>>(8*i)))
			}
		}
		return b
	}
	store := func(off byte) []byte {
		return []byte{wasm.OpcodeVecPrefix, wasm.OpcodeVecV128Store, 0x04, off}
	}
	load := func(off byte) []byte {
		return []byte{wasm.OpcodeVecPrefix, wasm.OpcodeVecV128Load, 0x04, off}
	}
	// The vector opcode is a LEB128 u32, so anything from 0x80 up takes two bytes.
	vec := func(op uint32) []byte {
		b := []byte{wasm.OpcodeVecPrefix}
		return append(b, leb128.EncodeUint32(op)...)
	}

	body := func(parts ...[]byte) wasm.Code {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return wasm.Code{Body: append(b, wasm.OpcodeEnd)}
	}
	get := func(i byte) []byte { return []byte{wasm.OpcodeLocalGet, i} }

	m := &wasm.Module{
		TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{i32}}},
		FunctionSection: []wasm.Index{0, 0, 0, 0, 0},
		MemorySection:   []wasm.Memory{{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true}},
		CodeSection: []wasm.Code{
			// const+store: the two words must land little-endian, low first.
			// v128.store pops the value then the address, so the address goes first.
			body(get(0), v128c(0x1111, 0x2222), store(0)),
			// i64x2.add of the stored vector with itself: one add per word, no carry
			// crossing the lane boundary. Each v128.load needs its own address.
			body(get(0), get(0), load(0), get(0), load(0), vec(0xce), store(16)),
			// i64x2.sub with itself is zero.
			body(get(0), get(0), load(0), get(0), load(0), vec(0xd1), store(32)),
			// v128.xor with itself is zero; v128.not of that is all ones.
			body(get(0), get(0), load(0), get(0), load(0), vec(0x51), vec(0x4d), store(48)),
			// and with all-ones is the value; or with zero leaves it alone.
			body(get(0), get(0), load(0), v128c(^uint64(0), ^uint64(0)), vec(0x4e),
				v128c(0, 0), vec(0x50), store(64)),
		},
		ExportSection: []wasm.Export{
			{Name: "conststore", Type: wasm.ExternTypeFunc, Index: 0},
			{Name: "add", Type: wasm.ExternTypeFunc, Index: 1},
			{Name: "sub", Type: wasm.ExternTypeFunc, Index: 2},
			{Name: "xornot", Type: wasm.ExternTypeFunc, Index: 3},
			{Name: "andor", Type: wasm.ExternTypeFunc, Index: 4},
		},
	}

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx,
		wazy.NewRuntimeConfigCompiler().WithCoreFeatures(api.CoreFeaturesV2))
	defer r.Close(ctx)

	inst, err := r.Instantiate(ctx, binaryencoding.EncodeModule(m))
	require.NoError(t, err)
	mem := inst.Memory()
	call := func(name string) {
		_, err := inst.ExportedFunction(name).Call(ctx, 0)
		require.NoError(t, err, name)
	}
	words := func(off uint32) (lo, hi uint64) {
		lo, ok := mem.ReadUint64Le(off)
		require.True(t, ok)
		hi, ok = mem.ReadUint64Le(off + 8)
		require.True(t, ok)
		return
	}

	call("conststore")
	lo, hi := words(0)
	require.Equal(t, uint64(0x1111), lo, "v128.const low word")
	require.Equal(t, uint64(0x2222), hi, "v128.const high word")

	call("add")
	lo, hi = words(16)
	require.Equal(t, uint64(0x2222), lo, "i64x2.add low lane")
	require.Equal(t, uint64(0x4444), hi, "i64x2.add high lane")

	call("sub")
	lo, hi = words(32)
	require.Equal(t, uint64(0), lo, "i64x2.sub low lane")
	require.Equal(t, uint64(0), hi, "i64x2.sub high lane")

	call("xornot")
	lo, hi = words(48)
	require.Equal(t, ^uint64(0), lo, "v128.not of xor-with-self")
	require.Equal(t, ^uint64(0), hi, "v128.not of xor-with-self")

	call("andor")
	lo, hi = words(64)
	require.Equal(t, uint64(0x1111), lo, "v128.and/or round trip")
	require.Equal(t, uint64(0x2222), hi, "v128.and/or round trip")
}
