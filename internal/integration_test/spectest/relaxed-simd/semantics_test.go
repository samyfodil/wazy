package spectest

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The relaxed instructions may each return any one of several results, so the
// official suite cannot pin any of them down: its min/max cases, for instance,
// only cover NaN and signed zero, which f32x4.relaxed_min and f32x4.relaxed_max
// answer identically. These cases pin the result wazy documents in RATIONALE.md
// with operands that tell the permitted alternatives apart, on both engines.
var relaxedSemantics = []struct {
	name string
	op   wasm.OpcodeVec
	in   [][2]uint64
	want [2]uint64
	// nanLaneBits is the lane width of a result that has NaN lanes. Those lanes
	// are checked for being a NaN rather than compared bit for bit: the sign and
	// payload of a NaN result are unspecified, and wazy's engines have always
	// differed on the sign bit here, well before relaxed SIMD.
	nanLaneBits int
}{
	{
		// An index of 16 or more gives zero, as in i8x16.swizzle. Reading only
		// the low four bits, as x86's pshufb does, would give lane 0 instead.
		name: "i8x16.relaxed_swizzle",
		op:   wasm.OpcodeVecI8x16RelaxedSwizzle,
		in: [][2]uint64{
			i8x16(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
			i8x16(15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 16),
		},
		want: i8x16(15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0),
	},
	{
		name: "i32x4.relaxed_trunc_f32x4_s",
		op:   wasm.OpcodeVecI32x4RelaxedTruncF32x4S,
		in:   [][2]uint64{f32x4(1.5, -1.5, 3.9, -3.9)},
		want: i32x4(1, -1, 3, -3),
	},
	{
		name: "i32x4.relaxed_trunc_f32x4_u",
		op:   wasm.OpcodeVecI32x4RelaxedTruncF32x4U,
		in:   [][2]uint64{f32x4(1.5, 0, 3.9, 4.2)},
		want: i32x4(1, 0, 3, 4),
	},
	{
		name: "i32x4.relaxed_trunc_f64x2_s_zero",
		op:   wasm.OpcodeVecI32x4RelaxedTruncF64x2SZero,
		in:   [][2]uint64{f64x2(2.7, -2.7)},
		want: i32x4(2, -2, 0, 0),
	},
	{
		name: "i32x4.relaxed_trunc_f64x2_u_zero",
		op:   wasm.OpcodeVecI32x4RelaxedTruncF64x2UZero,
		in:   [][2]uint64{f64x2(2.7, 5.9)},
		want: i32x4(2, 5, 0, 0),
	},
	{
		name: "f32x4.relaxed_madd",
		op:   wasm.OpcodeVecF32x4RelaxedMadd,
		in:   [][2]uint64{f32x4(2, 3, 4, 5), f32x4(10, 10, 10, 10), f32x4(1, 2, 3, 4)},
		want: f32x4(21, 32, 43, 54),
	},
	{
		name: "f32x4.relaxed_nmadd",
		op:   wasm.OpcodeVecF32x4RelaxedNmadd,
		in:   [][2]uint64{f32x4(2, 3, 4, 5), f32x4(10, 10, 10, 10), f32x4(1, 2, 3, 4)},
		want: f32x4(-19, -28, -37, -46),
	},
	{
		name: "f64x2.relaxed_madd",
		op:   wasm.OpcodeVecF64x2RelaxedMadd,
		in:   [][2]uint64{f64x2(2, 3), f64x2(10, 10), f64x2(1, 2)},
		want: f64x2(21, 32),
	},
	{
		name: "f64x2.relaxed_nmadd",
		op:   wasm.OpcodeVecF64x2RelaxedNmadd,
		in:   [][2]uint64{f64x2(2, 3), f64x2(10, 10), f64x2(1, 2)},
		want: f64x2(-19, -28),
	},
	{
		// The mask mixes set and clear bits within a lane, which is what tells
		// v128.bitselect apart from the forms that consult only a top bit: those
		// would take lane 2 whole from one operand rather than byte by byte.
		name: "i8x16.relaxed_laneselect",
		op:   wasm.OpcodeVecI8x16RelaxedLaneselect,
		in: [][2]uint64{
			i8x16(0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11),
			i8x16(0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22),
			i8x16(0, -1, 0x0f, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
		},
		want: i8x16(0x22, 0x11, 0x21, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22),
	},
	{
		name: "i16x8.relaxed_laneselect",
		op:   wasm.OpcodeVecI16x8RelaxedLaneselect,
		in: [][2]uint64{
			i16x8(0x1111, 0x1111, 0x1111, 0x1111, 0x1111, 0x1111, 0x1111, 0x1111),
			i16x8(0x2222, 0x2222, 0x2222, 0x2222, 0x2222, 0x2222, 0x2222, 0x2222),
			i16x8(0, -1, 0x00ff, 0, 0, 0, 0, 0),
		},
		want: i16x8(0x2222, 0x1111, 0x2211, 0x2222, 0x2222, 0x2222, 0x2222, 0x2222),
	},
	{
		name: "i32x4.relaxed_laneselect",
		op:   wasm.OpcodeVecI32x4RelaxedLaneselect,
		in: [][2]uint64{
			i32x4(0x11111111, 0x11111111, 0x11111111, 0x11111111),
			i32x4(0x22222222, 0x22222222, 0x22222222, 0x22222222),
			i32x4(0, -1, 0x0000ffff, 0),
		},
		want: i32x4(0x22222222, 0x11111111, 0x22221111, 0x22222222),
	},
	{
		name: "i64x2.relaxed_laneselect",
		op:   wasm.OpcodeVecI64x2RelaxedLaneselect,
		in: [][2]uint64{
			i64x2(0x1111111111111111, 0x1111111111111111),
			i64x2(0x2222222222222222, 0x2222222222222222),
			i64x2(0x00000000ffffffff, 0),
		},
		want: i64x2(0x2222222211111111, 0x2222222222222222),
	},
	{
		// The official suite covers only NaN and signed zero, where relaxed_min
		// and relaxed_max agree with each other, so ordinary operands are the
		// only thing that tells the two instructions apart.
		name: "f32x4.relaxed_min",
		op:   wasm.OpcodeVecF32x4RelaxedMin,
		in:   [][2]uint64{f32x4(1, 5, -2, 0), f32x4(3, 2, -7, 1)},
		want: f32x4(1, 2, -7, 0),
	},
	{
		name: "f32x4.relaxed_max",
		op:   wasm.OpcodeVecF32x4RelaxedMax,
		in:   [][2]uint64{f32x4(1, 5, -2, 0), f32x4(3, 2, -7, 1)},
		want: f32x4(3, 5, -2, 1),
	},
	{
		name: "f64x2.relaxed_min",
		op:   wasm.OpcodeVecF64x2RelaxedMin,
		in:   [][2]uint64{f64x2(1, 5), f64x2(3, 2)},
		want: f64x2(1, 2),
	},
	{
		name: "f64x2.relaxed_max",
		op:   wasm.OpcodeVecF64x2RelaxedMax,
		in:   [][2]uint64{f64x2(1, 5), f64x2(3, 2)},
		want: f64x2(3, 5),
	},
	{
		// NaN and signed zero are where the permitted results diverge, and where
		// the deterministic profile requires plain fmin: lane 1 would keep 1.0
		// and lane 2 would keep +0.0 under the pseudo-min form.
		name:        "f32x4.relaxed_min/nan and signed zero",
		nanLaneBits: 32,
		op:          wasm.OpcodeVecF32x4RelaxedMin,
		in: [][2]uint64{
			f32x4(f32NaN, 1, +0.0, negZero32),
			f32x4(1, f32NaN, negZero32, +0.0),
		},
		want: f32x4(f32NaN, f32NaN, negZero32, negZero32),
	},
	{
		// Likewise lane 1 would keep 1.0 and lane 3 would keep -0.0 under the
		// pseudo-max form.
		name:        "f32x4.relaxed_max/nan and signed zero",
		nanLaneBits: 32,
		op:          wasm.OpcodeVecF32x4RelaxedMax,
		in: [][2]uint64{
			f32x4(f32NaN, 1, +0.0, negZero32),
			f32x4(1, f32NaN, negZero32, +0.0),
		},
		want: f32x4(f32NaN, f32NaN, +0.0, +0.0),
	},
	{
		name:        "f64x2.relaxed_min/nan and signed zero",
		nanLaneBits: 64,
		op:          wasm.OpcodeVecF64x2RelaxedMin,
		in: [][2]uint64{
			f64x2(1, +0.0),
			f64x2(f64NaN, negZero64),
		},
		want: f64x2(f64NaN, negZero64),
	},
	{
		name:        "f64x2.relaxed_max/nan and signed zero",
		nanLaneBits: 64,
		op:          wasm.OpcodeVecF64x2RelaxedMax,
		in: [][2]uint64{
			f64x2(1, negZero64),
			f64x2(f64NaN, +0.0),
		},
		want: f64x2(f64NaN, +0.0),
	},
	{
		// Lane 0 is the only product that overflows: it saturates to INT16_MAX
		// rather than wrapping to INT16_MIN as x86's pmulhrsw does.
		name: "i16x8.relaxed_q15mulr_s",
		op:   wasm.OpcodeVecI16x8RelaxedQ15mulrS,
		in: [][2]uint64{
			i16x8(-32768, 16384, -16384, 4096, 0, 1, -1, 32767),
			i16x8(-32768, 16384, 16384, 8192, 1234, 32767, 32767, 32767),
		},
		want: i16x8(32767, 8192, -8192, 1024, 0, 1, -1, 32766),
	},
	{
		// Lane 0 reads the second operand as signed: unsigned would make it
		// 1*255 + 1*0 = 255 rather than 1*-1 + 1*0 = -1.
		name: "i16x8.relaxed_dot_i8x16_i7x16_s",
		op:   wasm.OpcodeVecI16x8RelaxedDotI8x16I7x16S,
		in: [][2]uint64{
			i8x16(1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
			i8x16(-1, 0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
		},
		want: i16x8(-1, 13, 41, 85, 145, 221, 313, 421),
	},
	{
		// Only -128 * -128, twice, crosses the top of the i16 range; the bottom
		// is unreachable because a product of int8 lanes bottoms out at -16256.
		name: "i16x8.relaxed_dot_i8x16_i7x16_s/saturates",
		op:   wasm.OpcodeVecI16x8RelaxedDotI8x16I7x16S,
		in: [][2]uint64{
			i8x16(-128, -128, -128, 127, 127, 127, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
			i8x16(-128, -128, -128, -128, -128, -128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
		},
		want: i16x8(32767, 128, -32512, 0, 0, 0, 0, 0),
	},
	{
		name: "i32x4.relaxed_dot_i8x16_i7x16_add_s",
		op:   wasm.OpcodeVecI32x4RelaxedDotI8x16I7x16AddS,
		in: [][2]uint64{
			i8x16(1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
			i8x16(-1, 0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
			i32x4(1000, 2000, 3000, 4000),
		},
		want: i32x4(1012, 2126, 3366, 4734),
	},
}

func TestRelaxedSemantics(t *testing.T) {
	configs := map[string]wazy.RuntimeConfig{
		"interpreter": wazy.NewRuntimeConfigInterpreter().WithCoreFeatures(enabledFeatures),
	}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler().WithCoreFeatures(enabledFeatures)
	}

	for engine, config := range configs {
		t.Run(engine, func(t *testing.T) {
			for _, tc := range relaxedSemantics {
				t.Run(tc.name, func(t *testing.T) {
					var body []byte
					for _, operand := range tc.in {
						body = append(body, wasm.OpcodeVecPrefix, wasm.OpcodeVecV128Const)
						body = binary.LittleEndian.AppendUint64(body, operand[0])
						body = binary.LittleEndian.AppendUint64(body, operand[1])
					}
					body = append(wasm.AppendVecOpcode(append(body, wasm.OpcodeVecPrefix), tc.op), wasm.OpcodeEnd)

					bin := binaryencoding.EncodeModule(&wasm.Module{
						TypeSection: []wasm.FunctionType{
							{Results: []wasm.ValueType{wasm.ValueTypeV128}, ResultNumInUint64: 2},
						},
						FunctionSection: []wasm.Index{0},
						CodeSection:     []wasm.Code{{Body: body}},
						ExportSection:   []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 0}},
					})

					ctx := context.Background()
					r := wazy.NewRuntimeWithConfig(ctx, config)
					defer r.Close(ctx)

					mod, err := r.InstantiateWithConfig(ctx, bin, wazy.NewModuleConfig())
					require.NoError(t, err)

					results, err := mod.ExportedFunction("f").Call(ctx)
					require.NoError(t, err)
					require.Equal(t, 2, len(results))

					want, got := tc.want, [2]uint64{results[0], results[1]}
					if w := tc.nanLaneBits; w != 0 {
						mask := ^uint64(0)
						if w < 64 {
							mask = uint64(1)<<w - 1
						}
						for lane := 0; lane < 128/w; lane++ {
							i, shift := lane*w/64, uint(lane*w%64)
							if !isNaNLane(want[i]>>shift&mask, w) {
								continue
							}
							require.True(t, isNaNLane(got[i]>>shift&mask, w),
								"lane %d: want a NaN, have %016x", lane, got[i]>>shift&mask)
							want[i] &^= mask << shift
							got[i] &^= mask << shift
						}
					}
					require.Equal(t, want, got,
						"want %016x%016x, have %016x%016x",
						want[1], want[0], got[1], got[0])
				})
			}
		})
	}
}

func isNaNLane(bits uint64, width int) bool {
	if width == 32 {
		return math.IsNaN(float64(math.Float32frombits(uint32(bits))))
	}
	return math.IsNaN(math.Float64frombits(bits))
}

// A NaN operand comes back quieted, so a canonical NaN in gives one back out.
var (
	f32NaN    = float32(math.NaN())
	f64NaN    = math.NaN()
	negZero32 = float32(math.Copysign(0, -1))
	negZero64 = math.Copysign(0, -1)
)

func i8x16(v ...int8) (ret [2]uint64) {
	for i, lane := range v {
		ret[i/8] |= uint64(uint8(lane)) << ((i % 8) * 8)
	}
	return
}

func i16x8(v ...int16) (ret [2]uint64) {
	for i, lane := range v {
		ret[i/4] |= uint64(uint16(lane)) << ((i % 4) * 16)
	}
	return
}

func i32x4(v ...int32) (ret [2]uint64) {
	for i, lane := range v {
		ret[i/2] |= uint64(uint32(lane)) << ((i % 2) * 32)
	}
	return
}

func i64x2(a, b uint64) [2]uint64 { return [2]uint64{a, b} }

func f32x4(a, b, c, d float32) [2]uint64 {
	return [2]uint64{
		uint64(math.Float32bits(a)) | uint64(math.Float32bits(b))<<32,
		uint64(math.Float32bits(c)) | uint64(math.Float32bits(d))<<32,
	}
}

func f64x2(a, b float64) [2]uint64 {
	return [2]uint64{math.Float64bits(a), math.Float64bits(b)}
}
