package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/wasm"
)

// storedValue is what a full-width store wrote: value, at base plus offset in memory mem, in block blk.
type storedValue struct {
	blk    ssa.BasicBlock
	base   ssa.Value
	offset uint64
	mem    wasm.Index
	size   uint64
	value  ssa.Value
}

// forward returns the value s stored when the load op of base plus offset in memory mem, in blk, reads
// exactly it back: the same address, of the same width and type, with nothing since that may have
// written memory. The store's bounds check covers the load's.
func (s *storedValue) forward(blk ssa.BasicBlock, base ssa.Value, offset uint64, mem wasm.Index, op wasm.Opcode) (ssa.Value, bool) {
	if s.blk == nil || s.blk != blk || s.base != base || s.offset != offset || s.mem != mem {
		return ssa.ValueInvalid, false
	}
	var t ssa.Type
	switch op {
	case wasm.OpcodeI32Load:
		t = ssa.TypeI32
	case wasm.OpcodeI64Load:
		t = ssa.TypeI64
	case wasm.OpcodeF32Load:
		t = ssa.TypeF32
	case wasm.OpcodeF64Load:
		t = ssa.TypeF64
	default:
		return ssa.ValueInvalid, false
	}
	if s.value.Type() != t {
		return ssa.ValueInvalid, false
	}
	return s.value, true
}

// keepsMemory reports whether the instruction op can never write memory, nor let anything else run:
// locals, globals, constants, arithmetic, comparisons, conversions, selects and loads. A store sets
// what it wrote again itself.
func keepsMemory(op wasm.Opcode) bool {
	switch {
	case op == wasm.OpcodeLocalGet, op == wasm.OpcodeLocalSet, op == wasm.OpcodeLocalTee,
		op == wasm.OpcodeGlobalGet, op == wasm.OpcodeGlobalSet, op == wasm.OpcodeDrop,
		op == wasm.OpcodeSelect, op == wasm.OpcodeTypedSelect, op == wasm.OpcodeNop:
		return true
	case wasm.OpcodeI32Load <= op && op <= wasm.OpcodeI64Load32U:
		return true
	case wasm.OpcodeI32Const <= op && op <= wasm.OpcodeI64Extend32S:
		return true
	}
	return false
}
