package wazy

import (
	"context"
	"math"
	"unsafe"

	"github.com/samyfodil/wazy/api"
)

// HostValue is the set of Go types that HostFunc0-HostFunc16 and
// HostProc0-HostProc16 accept as a parameter or result. Each corresponds 1:1
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
// resolve at compile time, with no reflection and no per-call allocation.
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
// Both tests are on constants of T, so the compiler folds them and the decode
// is the single move it would be in a hand-written WithGoModuleFunction
// adapter. A type switch on any(zero) does not fold: the dynamic type comes
// from the generic dictionary, so it stays a real switch on every host call.
func decodeHostValue[T HostValue](raw uint64) T {
	var zero T
	if T(1)/T(2) != 0 { // integer division truncates to 0, so T is a float
		if unsafe.Sizeof(zero) == 4 {
			return T(math.Float32frombits(uint32(raw)))
		}
		return T(math.Float64frombits(raw))
	}
	// Every integer HostValue, signed or not, is the slot truncated to its own
	// width, which is exactly what converting from uint64 does.
	return T(raw)
}

// encodeHostValue encodes v as a stack slot, following the encoding
// conventions documented on api.ValueType. See decodeHostValue.
func encodeHostValue[T HostValue](v T) uint64 {
	if T(1)/T(2) != 0 { // T is a float, see decodeHostValue
		if unsafe.Sizeof(v) == 4 {
			return uint64(math.Float32bits(float32(v)))
		}
		return math.Float64bits(float64(v))
	}
	// Converting to uint64 sign-extends a signed HostValue and zero-extends an
	// unsigned one, which is what api.ValueType asks for.
	return uint64(v)
}

// HostFunc0 defines a host function taking no parameters besides the
// implicit context.Context and api.Module, returning a single HostValue.
//
// HostFunc0-HostFunc16 (and HostProc0-HostProc16 for functions that return
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
// express: more than 16 parameters, more than one result, a signature
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

// HostFunc9 is HostFunc0 with nine parameters. See HostFunc0.
func HostFunc9[P1, P2, P3, P4, P5, P6, P7, P8, P9, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc10 is HostFunc0 with ten parameters. See HostFunc0.
func HostFunc10[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc11 is HostFunc0 with eleven parameters. See HostFunc0.
func HostFunc11[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc12 is HostFunc0 with twelve parameters. See HostFunc0.
func HostFunc12[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc13 is HostFunc0 with thirteen parameters. See HostFunc0.
func HostFunc13[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc14 is HostFunc0 with fourteen parameters. See HostFunc0.
func HostFunc14[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc15 is HostFunc0 with fifteen parameters. See HostFunc0.
func HostFunc15[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		p15 := decodeHostValue[P15](stack[14])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14, p15))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14](), hostValueType[P15]()}, []api.ValueType{hostValueType[R]()})
}

// HostFunc16 is HostFunc0 with sixteen parameters. See HostFunc0.
func HostFunc16[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15, P16, R HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15, P16) R) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		p15 := decodeHostValue[P15](stack[14])
		p16 := decodeHostValue[P16](stack[15])
		stack[0] = encodeHostValue(fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14, p15, p16))
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14](), hostValueType[P15](), hostValueType[P16]()}, []api.ValueType{hostValueType[R]()})
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

// HostProc9 is HostProc0 with nine parameters. See HostFunc0 and HostProc0.
func HostProc9[P1, P2, P3, P4, P5, P6, P7, P8, P9 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9]()}, nil)
}

// HostProc10 is HostProc0 with ten parameters. See HostFunc0 and HostProc0.
func HostProc10[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10]()}, nil)
}

// HostProc11 is HostProc0 with eleven parameters. See HostFunc0 and HostProc0.
func HostProc11[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11]()}, nil)
}

// HostProc12 is HostProc0 with twelve parameters. See HostFunc0 and HostProc0.
func HostProc12[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12]()}, nil)
}

// HostProc13 is HostProc0 with thirteen parameters. See HostFunc0 and HostProc0.
func HostProc13[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13]()}, nil)
}

// HostProc14 is HostProc0 with fourteen parameters. See HostFunc0 and HostProc0.
func HostProc14[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14]()}, nil)
}

// HostProc15 is HostProc0 with fifteen parameters. See HostFunc0 and HostProc0.
func HostProc15[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		p15 := decodeHostValue[P15](stack[14])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14, p15)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14](), hostValueType[P15]()}, nil)
}

// HostProc16 is HostProc0 with sixteen parameters. See HostFunc0 and HostProc0.
func HostProc16[P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15, P16 HostValue](b HostFunctionBuilder, fn func(context.Context, api.Module, P1, P2, P3, P4, P5, P6, P7, P8, P9, P10, P11, P12, P13, P14, P15, P16)) HostFunctionBuilder {
	return b.WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		p1 := decodeHostValue[P1](stack[0])
		p2 := decodeHostValue[P2](stack[1])
		p3 := decodeHostValue[P3](stack[2])
		p4 := decodeHostValue[P4](stack[3])
		p5 := decodeHostValue[P5](stack[4])
		p6 := decodeHostValue[P6](stack[5])
		p7 := decodeHostValue[P7](stack[6])
		p8 := decodeHostValue[P8](stack[7])
		p9 := decodeHostValue[P9](stack[8])
		p10 := decodeHostValue[P10](stack[9])
		p11 := decodeHostValue[P11](stack[10])
		p12 := decodeHostValue[P12](stack[11])
		p13 := decodeHostValue[P13](stack[12])
		p14 := decodeHostValue[P14](stack[13])
		p15 := decodeHostValue[P15](stack[14])
		p16 := decodeHostValue[P16](stack[15])
		fn(ctx, mod, p1, p2, p3, p4, p5, p6, p7, p8, p9, p10, p11, p12, p13, p14, p15, p16)
	}), []api.ValueType{hostValueType[P1](), hostValueType[P2](), hostValueType[P3](), hostValueType[P4](), hostValueType[P5](), hostValueType[P6](), hostValueType[P7](), hostValueType[P8](), hostValueType[P9](), hostValueType[P10](), hostValueType[P11](), hostValueType[P12](), hostValueType[P13](), hostValueType[P14](), hostValueType[P15](), hostValueType[P16]()}, nil)
}
