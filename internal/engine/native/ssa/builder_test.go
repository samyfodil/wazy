package ssa

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestBuilder_resolveAlias(t *testing.T) {
	b := NewBuilder().(*builder)
	v1 := b.allocateValue(TypeI32)
	v2 := b.allocateValue(TypeI32)
	v3 := b.allocateValue(TypeI32)
	v4 := b.allocateValue(TypeI32)
	v5 := b.allocateValue(TypeI32)

	b.alias(v1, v2)
	b.alias(v2, v3)
	b.alias(v3, v4)
	b.alias(v4, v5)
	require.Equal(t, v5, b.resolveAlias(v1))
	require.Equal(t, v5, b.resolveAlias(v2))
	require.Equal(t, v5, b.resolveAlias(v3))
	require.Equal(t, v5, b.resolveAlias(v4))
	require.Equal(t, v5, b.resolveAlias(v5))
}

// TestBuilder_variableDefinitions exercises the per-block windows into builder.varDefs: a
// definition is private to its block, survives a Variable being declared after the block
// already has a window, and is gone once the block is recycled for the next function.
func TestBuilder_variableDefinitions(t *testing.T) {
	b := NewBuilder().(*builder)
	blk1, blk2 := b.AllocateBasicBlock().(*basicBlock), b.AllocateBasicBlock().(*basicBlock)

	v1 := b.DeclareVariable(TypeI32)
	val1 := b.allocateValue(TypeI32)
	b.DefineVariable(v1, val1, blk1)

	got, ok := b.lastDefinition(blk1, v1)
	require.True(t, ok)
	require.Equal(t, val1, got)
	_, ok = b.lastDefinition(blk2, v1)
	require.False(t, ok)

	// Widening blk1's window for a later Variable must not lose the earlier definition.
	v2 := b.DeclareVariable(TypeI64)
	val2 := b.allocateValue(TypeI64)
	b.DefineVariable(v2, val2, blk1)

	got, ok = b.lastDefinition(blk1, v1)
	require.True(t, ok)
	require.Equal(t, val1, got)
	got, ok = b.lastDefinition(blk1, v2)
	require.True(t, ok)
	require.Equal(t, val2, got)
	_, ok = b.lastDefinition(blk2, v2)
	require.False(t, ok)

	resetBasicBlock(blk1)
	_, ok = b.lastDefinition(blk1, v1)
	require.False(t, ok)
}
