// Package witgo holds the types and conversions that Go bindings generated
// by wazy-bindgen (see package bindgen) are written in terms of: WIT's result
// and tuple types, and the reading of a component.Value into the Go type a
// WIT primitive maps to.
package witgo

import (
	"fmt"

	"github.com/samyfodil/wazy/component"
)

// Result is WIT's result<T, E>. An arm WIT leaves out is struct{}.
type Result[T, E any] struct {
	Ok    T
	Err   E
	IsErr bool
}

// Ok is a Result holding v as its ok arm.
func Ok[T, E any](v T) Result[T, E] { return Result[T, E]{Ok: v} }

// Err is a Result holding e as its error arm.
func Err[T, E any](e E) Result[T, E] { return Result[T, E]{Err: e, IsErr: true} }

// Tuple2 to Tuple8 are WIT's tuples, their elements in order.
type (
	Tuple2[T0, T1 any] struct {
		F0 T0
		F1 T1
	}
	Tuple3[T0, T1, T2 any] struct {
		F0 T0
		F1 T1
		F2 T2
	}
	Tuple4[T0, T1, T2, T3 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
	}
	Tuple5[T0, T1, T2, T3, T4 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
	}
	Tuple6[T0, T1, T2, T3, T4, T5 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
	}
	Tuple7[T0, T1, T2, T3, T4, T5, T6 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
		F6 T6
	}
	Tuple8[T0, T1, T2, T3, T4, T5, T6, T7 any] struct {
		F0 T0
		F1 T1
		F2 T2
		F3 T3
		F4 T4
		F5 T5
		F6 T6
		F7 T7
	}
)

// MaxTuple is the longest tuple witgo has a type for.
const MaxTuple = 8

func want[T any](v component.Value, wit string) (T, error) {
	t, ok := v.(T)
	if !ok {
		var zero T
		return zero, fmt.Errorf("witgo: %s: got %T", wit, v)
	}
	return t, nil
}

// Bool reads a bool.
func Bool(v component.Value) (bool, error) { return want[bool](v, "bool") }

// S8 reads an s8, which lifts as an int32.
func S8(v component.Value) (int8, error) {
	x, err := want[int32](v, "s8")
	return int8(x), err
}

// S16 reads an s16, which lifts as an int32.
func S16(v component.Value) (int16, error) {
	x, err := want[int32](v, "s16")
	return int16(x), err
}

// S32 reads an s32.
func S32(v component.Value) (int32, error) { return want[int32](v, "s32") }

// S64 reads an s64.
func S64(v component.Value) (int64, error) { return want[int64](v, "s64") }

// U8 reads a u8, which lifts as a uint32.
func U8(v component.Value) (uint8, error) {
	x, err := want[uint32](v, "u8")
	return uint8(x), err
}

// U16 reads a u16, which lifts as a uint32.
func U16(v component.Value) (uint16, error) {
	x, err := want[uint32](v, "u16")
	return uint16(x), err
}

// U32 reads a u32, and an enum's case, a flags value or a resource handle,
// which lift as one.
func U32(v component.Value) (uint32, error) { return want[uint32](v, "u32") }

// U64 reads a u64.
func U64(v component.Value) (uint64, error) { return want[uint64](v, "u64") }

// F32 reads an f32.
func F32(v component.Value) (float32, error) { return want[float32](v, "f32") }

// F64 reads an f64.
func F64(v component.Value) (float64, error) { return want[float64](v, "f64") }

// Char reads a char.
func Char(v component.Value) (rune, error) { return want[rune](v, "char") }

// String reads a string.
func String(v component.Value) (string, error) { return want[string](v, "string") }

// Values reads a list, record or tuple that lifts as a []component.Value,
// checking its length when n is not negative.
func Values(v component.Value, n int, wit string) ([]component.Value, error) {
	vs, err := want[[]component.Value](v, wit)
	if err == nil && n >= 0 && len(vs) != n {
		return nil, fmt.Errorf("witgo: %s: %d values, want %d", wit, len(vs), n)
	}
	return vs, err
}

// Variant reads a variant.
func Variant(v component.Value, wit string) (component.VariantValue, error) {
	return want[component.VariantValue](v, wit)
}

// ResultValue reads a result.
func ResultValue(v component.Value, wit string) (component.ResultValue, error) {
	return want[component.ResultValue](v, wit)
}
