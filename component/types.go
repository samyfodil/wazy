package component

import (
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file is the WIT type vocabulary an embedder needs to declare a host
// import of any shape -- the same vocabulary wazy's own WASI 0.2
// implementation is written against. Every name here is an alias for the
// engine's internal type, not a parallel copy: there is exactly one
// representation of a WIT type in this codebase, and a descriptor an
// embedder hand-writes is indistinguishable from one the decoder produced
// from a real component binary.
//
// # Declaring a nested type
//
// A composite's children are TypeRefs. A TypeRef is either a primitive
// (inline) or a reference into a TypeTable, which is why nesting needs a
// table:
//
//	tbl := component.NewTypeTable()
//	inner := tbl.Add(component.ListDesc{Element: component.Prim("u8")})
//	outer := tbl.Add(component.ListDesc{Element: inner})
//
// The sugar constructors below (List, Result, Option, Tuple, Record,
// Variant) build the table for you, so the common case never mentions
// TypeRef:
//
//	tbl := component.NewTypeTable()
//	ty := tbl.Result(tbl.List(component.Prim("string")), component.Prim("u32"))
//
// # Values
//
// Declaring a type is half the job; a host func must also produce and
// consume values of it. See Value's doc for the Go shape of each WIT type,
// and VariantValue/ResultValue for the two that need a named struct.

// TypeDesc is one WIT type. The concrete descriptors below implement it.
// It is a sealed interface -- a type outside wazy cannot implement it,
// because the ABI dispatches on the concrete descriptors and an unknown one
// has no defined lowering.
type TypeDesc = binary.TypeDesc

// TypeRef refers to a type from inside a composite: either a primitive
// (spelled inline) or an index into the TypeTable the enclosing FuncDesc was
// built with. Prefer the TypeTable sugar constructors over building these by
// hand.
type TypeRef = binary.TypeRef

// The WIT type descriptors. These mirror the WIT type system one-for-one.
type (
	// PrimitiveDesc is bool, s8-s64, u8-u64, f32, f64, char, or string. See
	// Prim for the ergonomic spelling.
	PrimitiveDesc = binary.PrimitiveDesc
	// RecordDesc is a struct with named fields. Its Value is a []Value in
	// field order.
	RecordDesc = binary.RecordDesc
	// VariantDesc is a discriminated union. Its Value is a VariantValue.
	VariantDesc = binary.VariantDesc
	// ListDesc is an unbounded sequence. Its Value is the Go slice of the
	// element type when that element is a fixed-width primitive -- []byte for
	// list<u8>, []uint32 for list<u32>, []float64 for list<f64>, []bool,
	// []rune for list<char>, and so on -- and a []Value otherwise, as for a
	// list of strings or of records.
	//
	// Both shapes are accepted when lowering a scalar list, so a host func
	// may hand over either; lifting produces the typed one.
	ListDesc = binary.ListDesc
	// TupleDesc is a positional product type. Its Value is a []Value.
	TupleDesc = binary.TupleDesc
	// FlagsDesc is a named bitset. Its Value is a uint32 of set bits.
	FlagsDesc = binary.FlagsDesc
	// EnumDesc is a set of named cases with no payloads. Its Value is the
	// case index as a uint32.
	EnumDesc = binary.EnumDesc
	// OptionDesc is option<T>. Its Value is nil for none, or the inner value.
	OptionDesc = binary.OptionDesc
	// ResultDesc is result<T, E>; either arm may be absent. Its Value is a
	// ResultValue.
	ResultDesc = binary.ResultDesc
	// OwnDesc is own<R> -- an owned handle to a resource. Lifting one gives
	// the host the rep it names and consumes the guest's handle.
	OwnDesc = binary.OwnDesc
	// BorrowDesc is borrow<R> -- a handle lent for the duration of the call.
	BorrowDesc = binary.BorrowDesc
	// StreamDesc is stream<T>.
	StreamDesc = binary.StreamDesc
	// FutureDesc is future<T>.
	FutureDesc = binary.FutureDesc

	// RecordField is one named field of a RecordDesc.
	RecordField = binary.RecordField
	// VariantCase is one case of a VariantDesc; a nil Type means no payload.
	VariantCase = binary.VariantCase
)

// FuncDesc, and its parts, describe a whole host func signature. Build one
// with TypeTable.Func rather than by hand.
type (
	FuncDesc    = binary.FuncDesc
	FuncParam   = binary.FuncParam
	FuncResults = binary.FuncResults
	FuncResult  = binary.FuncResult
)

// Resolver maps a TypeRef's index back to its descriptor. TypeTable.Resolver
// produces the one matching a table.
type Resolver = abi.Resolver

// VariantValue is the Go shape of a variant value: Disc is the case index in
// declaration order, Payload is that case's value (nil when the case carries
// none).
type VariantValue = abi.VariantValue

// ResultValue is the Go shape of a result value. IsErr selects the arm;
// Payload is that arm's value, or nil for an arm declared without a type.
//
// A result is NOT a Go error: returning a Go error from a HostFunc traps the
// guest, whereas a ResultValue with IsErr set is an ordinary value the guest
// receives and can handle. Use the error return only for "this call cannot
// proceed", and ResultValue for a WIT-declared failure.
type ResultValue = abi.ResultValue

// Prim is the ergonomic spelling of a primitive TypeRef: Prim("u32"),
// Prim("string"). Valid names are bool, s8, s16, s32, s64, u8, u16, u32,
// u64, f32, f64, char, string.
func Prim(name string) TypeRef { return TypeRef{Primitive: name} }
