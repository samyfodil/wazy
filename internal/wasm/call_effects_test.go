package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestModule_FunctionCallEffects(t *testing.T) {
	const grow, set = CallEffectGrowMemory, CallEffectSetGlobal
	m := Module{
		ImportFunctionCount: 1,
		ImportSection:       []Import{{Module: "m", Name: "f", Type: ExternTypeFunc, DescFunc: 0}},
		TypeSection:         []FunctionType{v_v},
		FunctionSection:     []Index{0, 0, 0, 0, 0, 0, 0, 0, 0},
		MemorySection:       []Memory{{Min: 1, Max: 2}},
		GlobalSection:       []Global{{Type: GlobalType{ValType: ValueTypeI32, Mutable: true}, Init: ConstantExpression{Data: []byte{OpcodeI32Const, 0, OpcodeEnd}}}},
		TableSection:        []Table{{Min: 1, Type: RefTypeFuncref}},
		CodeSection: []Code{
			// 1: pure leaf.
			{Body: []byte{OpcodeEnd}},
			// 2: grows memory itself.
			{Body: []byte{OpcodeI32Const, 1, OpcodeMemoryGrow, 0, OpcodeDrop, OpcodeEnd}},
			// 3: sets a global itself.
			{Body: []byte{OpcodeI32Const, 1, OpcodeGlobalSet, 0, OpcodeEnd}},
			// 4: calls 2 and the pure leaf.
			{Body: []byte{OpcodeCall, 1, OpcodeCall, 2, OpcodeEnd}},
			// 5 and 6 call each other, and 6 calls 3: the cycle sets a global.
			{Body: []byte{OpcodeCall, 6, OpcodeEnd}},
			{Body: []byte{OpcodeCall, 5, OpcodeCall, 3, OpcodeEnd}},
			// 7: calls the import.
			{Body: []byte{OpcodeCall, 0, OpcodeEnd}},
			// 8: calls indirectly.
			{Body: []byte{OpcodeI32Const, 0, OpcodeCallIndirect, 0, 0, OpcodeEnd}},
			// 9: implemented in Go.
			{GoFunc: func() {}},
		},
	}
	require.NoError(t, m.Validate(api.CoreFeaturesV2))
	require.Equal(t, []CallEffects{0, grow, set, grow, set, set, grow | set, grow | set, grow | set}, m.FunctionCallEffects)
}
