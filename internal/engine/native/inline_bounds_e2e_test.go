package native_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestInlinedSelfCallBoundsE2E calls a function that, given 2, calls itself with 0 or 1 in place of it, a
// call the compiler lowers as the body inline. Both arms of the copy's inner if load from the same address:
// the one the call takes must still check it, though the other was lowered first.
func TestInlinedSelfCallBoundsE2E(t *testing.T) {
	i32 := wasm.ValueTypeI32
	m := &wasm.Module{
		TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{i32, i32, i32}, Results: []wasm.ValueType{i32}}},
		FunctionSection: []wasm.Index{0},
		MemorySection:   []wasm.Memory{{Min: 1}},
		CodeSection: []wasm.Code{{Body: []byte{
			wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 2, wasm.OpcodeI32Eq,
			wasm.OpcodeIf, 0x7f,
			wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 1, wasm.OpcodeLocalGet, 2, wasm.OpcodeCall, 0,
			wasm.OpcodeElse,
			wasm.OpcodeLocalGet, 0,
			wasm.OpcodeIf, 0x7f,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Load, 2, 0,
			wasm.OpcodeElse,
			wasm.OpcodeLocalGet, 1, wasm.OpcodeI32Load, 2, 0,
			wasm.OpcodeEnd,
			wasm.OpcodeEnd,
			wasm.OpcodeEnd,
		}}},
		ExportSection: []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 0}},
	}
	bin := binaryencoding.EncodeModule(m)

	ctx := context.Background()
	for _, p := range []uint64{0, 1, 2} {
		for _, q := range []uint64{0, 1} {
			for _, a := range []uint64{0, 65532, 65533, 65536, 1 << 31} {
				t.Run(fmt.Sprintf("%d/%d/%d", p, q, a), func(t *testing.T) {
					var results [2][]uint64
					var errs [2]error
					for k, cfg := range []wazy.RuntimeConfig{wazy.NewRuntimeConfigCompiler(), wazy.NewRuntimeConfigInterpreter()} {
						r := wazy.NewRuntimeWithConfig(ctx, cfg)
						inst, err := r.Instantiate(ctx, bin)
						require.NoError(t, err)
						results[k], errs[k] = inst.ExportedFunction("f").Call(ctx, p, a, q)
						require.NoError(t, r.Close(ctx))
					}
					require.Equal(t, errs[1] != nil, errs[0] != nil, "compiler %v, interpreter %v", errs[0], errs[1])
					require.Equal(t, results[1], results[0])
				})
			}
		}
	}
}
