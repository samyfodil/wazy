package abi

import (
	"fmt"
	"math"
)

// CoreValue represents a single core WebAssembly value in the flat ABI.
// Kind is one of "i32", "i64", "f32", "f64".
// Bits holds the value: for integers, the low bits; for floats, the IEEE bits.
type CoreValue struct {
	Kind string // "i32", "i64", "f32", "f64"
	Bits uint64 // the actual value (ints in low bits, floats as IEEE bits)
}

// NewCoreValueI32 constructs a CoreValue from a 32-bit integer.
func NewCoreValueI32(v uint32) CoreValue {
	return CoreValue{Kind: "i32", Bits: uint64(v)}
}

// NewCoreValueI64 constructs a CoreValue from a 64-bit integer.
func NewCoreValueI64(v uint64) CoreValue {
	return CoreValue{Kind: "i64", Bits: v}
}

// NewCoreValueF32 constructs a CoreValue from a 32-bit float.
func NewCoreValueF32(v float32) CoreValue {
	bits := math.Float32bits(v)
	return CoreValue{Kind: "f32", Bits: uint64(bits)}
}

// NewCoreValueF64 constructs a CoreValue from a 64-bit float.
func NewCoreValueF64(v float64) CoreValue {
	bits := math.Float64bits(v)
	return CoreValue{Kind: "f64", Bits: bits}
}

// AsI32 extracts a CoreValue as a 32-bit integer.
// Panics if Kind is not "i32".
func (cv CoreValue) AsI32() uint32 {
	if cv.Kind != "i32" {
		panic(fmt.Sprintf("CoreValue.AsI32: expected i32, got %s", cv.Kind))
	}
	return uint32(cv.Bits)
}

// AsI64 extracts a CoreValue as a 64-bit integer.
// Panics if Kind is not "i64".
func (cv CoreValue) AsI64() uint64 {
	if cv.Kind != "i64" {
		panic(fmt.Sprintf("CoreValue.AsI64: expected i64, got %s", cv.Kind))
	}
	return cv.Bits
}

// AsF32 extracts a CoreValue as a 32-bit float.
// Panics if Kind is not "f32".
func (cv CoreValue) AsF32() float32 {
	if cv.Kind != "f32" {
		panic(fmt.Sprintf("CoreValue.AsF32: expected f32, got %s", cv.Kind))
	}
	return math.Float32frombits(uint32(cv.Bits))
}

// AsF64 extracts a CoreValue as a 64-bit float.
// Panics if Kind is not "f64".
func (cv CoreValue) AsF64() float64 {
	if cv.Kind != "f64" {
		panic(fmt.Sprintf("CoreValue.AsF64: expected f64, got %s", cv.Kind))
	}
	return math.Float64frombits(cv.Bits)
}

// CoreValueIter iterates over a slice of CoreValues, consuming them in order.
type CoreValueIter struct {
	values []CoreValue
	i      int
}

// NewCoreValueIter creates a new iterator over a slice of CoreValues.
func NewCoreValueIter(values []CoreValue) *CoreValueIter {
	return &CoreValueIter{values: values, i: 0}
}

// Next returns the next CoreValue and advances the iterator.
func (cvi *CoreValueIter) Next() (CoreValue, error) {
	if cvi.i >= len(cvi.values) {
		return CoreValue{}, fmt.Errorf("CoreValueIter: index out of range")
	}
	cv := cvi.values[cvi.i]
	cvi.i++
	return cv, nil
}

// Done reports whether all CoreValues have been consumed.
func (cvi *CoreValueIter) Done() bool {
	return cvi.i == len(cvi.values)
}
