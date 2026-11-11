package adhoc

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestFarPreemptCheck runs a loop whose check's slow path is further away than
// a tbnz, which arm64 checks with, can branch: ±32KB. Its body holds 64KB
// of code it never runs, stores the compiler cannot fold away, so the loop is
// cheap enough to yield many times while collections run, and each yield takes
// the far branch.
func TestFarPreemptCheck(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	// (global $g (mut i32) (i32.const 0))
	// (func (param $n i32) (result i32)
	//   loop
	//     call $leaf                ;; it loops, so the loop keeps a full check
	//     (if (i32.eq (local.get $n) (i32.const -1))
	//       (then g += 0; g += 1; ...))   ;; never runs
	//     br_if 0 (n = n - 1)
	//   end
	//   g)
	body := []byte{
		wasm.OpcodeLoop, 0x40, wasm.OpcodeCall, 0,
		wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 0x7f, wasm.OpcodeI32Eq, wasm.OpcodeIf, 0x40,
	}
	for k := range 8000 { // an add and a store each on arm64: 64KB
		body = append(body, wasm.OpcodeGlobalGet, 0, wasm.OpcodeI32Const)
		body = append(body, leb128s(int64(k%64))...)
		body = append(body, wasm.OpcodeI32Add, wasm.OpcodeGlobalSet, 0)
	}
	body = append(body, wasm.OpcodeEnd, wasm.OpcodeLocalGet, 0, wasm.OpcodeI32Const, 1, wasm.OpcodeI32Sub,
		wasm.OpcodeLocalTee, 0, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd, wasm.OpcodeGlobalGet, 0, wasm.OpcodeEnd)
	bin := binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{
			{},
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}},
		},
		FunctionSection: []wasm.Index{0, 1},
		GlobalSection: []wasm.Global{{
			Type: wasm.GlobalType{ValType: wasm.ValueTypeI32, Mutable: true},
			Init: wasm.ConstantExpression{Data: []byte{wasm.OpcodeI32Const, 0, wasm.OpcodeEnd}},
		}},
		CodeSection: []wasm.Code{
			{Body: []byte{wasm.OpcodeLoop, 0x40, wasm.OpcodeEnd, wasm.OpcodeEnd}},
			{Body: body},
		},
		ExportSection: []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 1}},
	})

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	mod, err := r.Instantiate(ctx, bin)
	require.NoError(t, err)

	callYielding(t, func() {
		res, err := mod.ExportedFunction("f").Call(ctx, 20_000_000)
		require.NoError(t, err)
		require.Equal(t, uint64(0), res[0])
	})
}
