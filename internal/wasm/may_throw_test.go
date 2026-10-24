package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestMayThrowBuilder(t *testing.T) {
	// Function 0 is imported. Local function i is function i+1: 1 calls 3, 3 calls 4, 4 calls 2 (a cycle
	// through 2, which throws); 5 calls itself; 7 calls 1; 8 calls 0, the import; 9 calls 10, which does
	// nothing.
	b := &mayThrowBuilder{direct: make([]bool, 11), imports: 1}
	b.call(0, 3)
	b.call(2, 4)
	b.call(3, 2)
	b.throws(1)
	b.call(4, 5)
	b.call(6, 1)
	b.call(7, 0)
	b.call(8, 10)
	require.Equal(t, []bool{true, true, true, true, true, false, false, true, true, false, false}, b.solve())
}

func TestModule_FunctionMayThrow(t *testing.T) {
	m := &Module{
		TypeSection:     []FunctionType{{}},
		FunctionSection: []Index{0, 0, 0, 0, 0},
		TagSection:      []Tag{{Type: 0}},
		CodeSection: []Code{
			{Body: []byte{OpcodeCall, 1, OpcodeEnd}},                               // calls one that throws
			{Body: []byte{OpcodeThrow, 0, OpcodeEnd}},                              // throws
			{Body: []byte{OpcodeNop, OpcodeEnd}},                                   // does not
			{Body: []byte{OpcodeI32Const, 0, OpcodeCallIndirect, 0, 0, OpcodeEnd}}, // calls an unknown target
			{Body: []byte{OpcodeTailCallReturnCall, 2, OpcodeEnd}},                 // tail-calls one that does not
		},
		TableSection: []Table{{Type: RefTypeFuncref}},
	}
	features := api.CoreFeaturesV2 | api.CoreFeatureExceptionHandling | api.CoreFeatureTailCall
	functions := []Index{0, 0, 0, 0, 0}
	tags := []Index{0}
	tables := []Table{{Type: RefTypeFuncref}}
	require.NoError(t, m.validateFunctions(features, functions, nil, nil, tables, tags, MaximumFunctionIndex))
	require.Equal(t, []bool{true, true, false, true, false}, m.FunctionMayThrow)

	// Without exception handling nothing can throw, and there is nothing to record.
	m.CodeSection[1].Body = []byte{OpcodeNop, OpcodeEnd}
	require.NoError(t, m.validateFunctions(features&^api.CoreFeatureExceptionHandling, functions, nil, nil, tables, tags, MaximumFunctionIndex))
	require.Nil(t, m.FunctionMayThrow)
}
