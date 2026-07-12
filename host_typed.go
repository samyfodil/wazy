package wazy

import (
	"context"
	"math"

	"github.com/samyfodil/wazy/api"
)

// HostValue is the set of Go types that HostFunc0-HostFunc8 and
// HostProc0-HostProc8 accept as a parameter or result. Each corresponds 1:1
// to a WebAssembly numeric api.ValueType:
//
//   - uint32, int32 map to api.ValueTypeI32
//   - uint64, int64 map to api.ValueTypeI64
//   - float32 maps to api.ValueTypeF32
//   - float64 maps to api.ValueTypeF64
//   - uintptr maps to api.ValueTypeExternref
//
// These are the exact predeclared types: a named type such as
// `type Pages uint32` is not a HostValue and will not compile as a type
// argument. Convert explicitly at the call boundary instead (for example,
// take a uint32 parameter and cast it to Pages inside the function body).
// Keeping the constraint to exact types lets decodeHostValue/encodeHostValue
// resolve entirely through a literal type switch, with no reflection and no
// per-call allocation.
type HostValue interface {
	uint32 | int32 | uint64 | int64 | float32 | float64 | uintptr
}

// hostValueType returns the api.ValueType used on the wire for a HostValue
// of type T.
func hostValueType[T HostValue]() api.ValueType {
	var zero T
	switch any(zero).(type) {
	case uint32, int32:
		return api.ValueTypeI32
	case uint64, int64:
		return api.ValueTypeI64
	case float32:
		return api.ValueTypeF32
	case float64:
		return api.ValueTypeF64
	case uintptr:
		return api.ValueTypeExternref
	default:
		panic("wazy: BUG: unreachable, T is constrained to HostValue")
	}
}

// decodeHostValue decodes a stack slot into a HostValue of type T, following
// the encoding conventions documented on api.ValueType.
//
// Because HostValue is constrained to the exact predeclared numeric types,
// the type switch below always matches one of its cases; it costs no more
// than a hand-written WithGoModuleFunction adapter (no reflection, no
// per-call allocation; see TestHostFunc_zeroAllocs).
func decodeHostValue[T HostValue](raw uint64) T {
	var zero T
	switch any(zero).(type) {
	case uint32:
		return T(uint32(raw))
	case int32:
		return T(int32(raw))
	case uint64:
		return T(raw)
	case int64:
		return T(int64(raw))
	case float32:
		return T(math.Float32frombits(uint32(raw)))
	case float64:
		return T(math.Float64frombits(raw))
	case uintptr:
		return T(uintptr(raw))
	default:
		panic("wazy: BUG: unreachable, T is constrained to HostValue")
	}
}

// encodeHostValue encodes v as a stack slot, following the encoding
// conventions documented on api.ValueType. See decodeHostValue.
func encodeHostValue[T HostValue](v T) uint64 {
	switch x := any(v).(type) {
	case uint32:
		return uint64(x)
	case int32:
		return uint64(int64(x))
	case uint64:
		return x
	case int64:
		return uint64(x)
	case float32:
		return uint64(math.Float32bits(x))
	case float64:
		return math.Float64bits(x)
	case uintptr:
		return uint64(x)
	default:
		panic("wazy: BUG: unreachable, T is constrained to HostValue")
	}
}

// HostFunc0 defines a host function taking no parameters besides the
// implicit context.Context and api.Module, returning a single HostValue.
//
// HostFunc0-HostFunc8 (and HostProc0-HostProc8 for functions that return
// nothing) are the compile-time-typed way to register a host function whose
// signature is "numeric-only, with a context.Context and api.Module prefix".
// The WebAssembly api.ValueType signature is derived from Go's type system,
// and the call decodes parameters and encodes the result directly, with no
// reflection and no per-call allocation - the same cost as hand-writing
// HostFunctionBuilder.WithGoModuleFunction.
//
// Use these when your function's arity is fixed at compile time and every
// parameter and result is a HostValue. Reach for WithGoFunction or
// WithGoModuleFunction directly when you need something these can't
// express: more than 8 parameters, more than one result, a signature
// without the context.Context/api.Module prefix, or fine control over the
// raw stack.
//
// Here's the addition example from HostFunctionBuilder's docs, this time
// with HostFunc2 (two parameters, one result):
//
//	wazy.HostFunc2(builder, func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
//		return x + y
//	}).Export("add")
func HostFunc0[R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = encodeHostValue(fn(ctx, mod))
	}), nil, []api.ValueType{hostValueType[R]()})
}

// HostFunc1 is HostFunc0 with one parameter. See HostFunc0.
func HostFunc1[P1, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		stack[0] = encodeHostValue(fn(ctx, mod, p1))
	}), []api.ValueType{hostValueType[P1]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc2 is HostFunc0 with two parameters. See HostFunc0.
func HostFunc2[P1, P2, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc3 is HostFunc0 with three parameters. See HostFunc0.
func HostFunc3[P1, P2, P3, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc4 is HostFunc0 with four parameters. See HostFunc0.
func HostFunc4[P1, P2, P3, P4, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc5 is HostFunc0 with five parameters. See HostFunc0.
func HostFunc5[P1, P2, P3, P4, P5, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc6 is HostFunc0 with six parameters. See HostFunc0.
func HostFunc6[P1, P2, P3, P4, P5, P6, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc7 is HostFunc0 with seven parameters. See HostFunc0.
func HostFunc7[P1, P2, P3, P4, P5, P6, P7, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc8 is HostFunc0 with eight parameters. See HostFunc0.
func HostFunc8[P1, P2, P3, P4, P5, P6, P7, P8, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8]()}, []api.ValueType{hostValueType[R]()})
}

// HostProc0 defines a host function with no result, taking no parameters
// besides the implicit context.Context and api.Module. See HostFunc0 for
// when to use this family of functions instead of WithGoModuleFunction.
func HostProc0(b HostFunctionBuilder, fn func(context.Context, api.Module)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fn(ctx, mod)
	}), nil, nil)
}

// HostProc1 is HostProc0 with one parameter. See HostFunc0 and HostProc0.
func HostProc1[P1 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		fn(ctx, mod, p1)
	}), []api.ValueType{hostValueType[P1]()}, nil)
}

// HostProc2 is HostProc0 with two parameters. See HostFunc0 and HostProc0.
func HostProc2[P1, P2 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		fn(ctx, mod, p1, p2)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2]()}, nil)
}

// HostProc3 is HostProc0 with three parameters. See HostFunc0 and HostProc0.
func HostProc3[P1, P2, P3 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		fn(ctx, mod, p1, p2, p3)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3]()}, nil)
}

// HostProc4 is HostProc0 with four parameters. See HostFunc0 and HostProc0.
func HostProc4[P1, P2, P3, P4 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		fn(ctx, mod, p1, p2, p3, p4)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4]()}, nil)
}

// HostProc5 is HostProc0 with five parameters. See HostFunc0 and HostProc0.
func HostProc5[P1, P2, P3, P4, P5 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		fn(ctx, mod, p1, p2, p3, p4, p5)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5]()}, nil)
}

// HostProc6 is HostProc0 with six parameters. See HostFunc0 and HostProc0.
func HostProc6[P1, P2, P3, P4, P5, P6 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6]()}, nil)
}

// HostProc7 is HostProc0 with seven parameters. See HostFunc0 and HostProc0.
func HostProc7[P1, P2, P3, P4, P5, P6, P7 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7]()}, nil)
}

// HostProc8 is HostProc0 with eight parameters. See HostFunc0 and HostProc0.
func HostProc8[P1, P2, P3, P4, P5, P6, P7, P8 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8]()}, nil)
}
