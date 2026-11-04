package adhoc

import (
	"context"
	_ "embed"
	"encoding/binary"
	"math/bits"
	"math/rand"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

//go:embed testdata/wide_mul.wasm
var wideMulWasm []byte

// TestWideMul runs the 64x64->128 multiply idioms the compiler fuses into a single multiply,
// and one near miss it must leave alone, against math/bits and the interpreter.
func TestWideMul(t *testing.T) {
	ctx := context.Background()
	configs := map[string]wazy.RuntimeConfig{"interpreter": wazy.NewRuntimeConfigInterpreter()}
	if platform.CompilerSupported() {
		configs["compiler"] = wazy.NewRuntimeConfigCompiler()
	}
	edge := []uint64{0, 1, 2, 0xffffffff, 0x100000000, 0xffffffffffffffff, 0x8000000000000000, 0x7fffffffffffffff, 0xfffffffe, 0xffffffff00000000}
	for name, config := range configs {
		t.Run(name, func(t *testing.T) {
			r := wazy.NewRuntimeWithConfig(ctx, config)
			defer r.Close(ctx)
			mod, err := r.Instantiate(ctx, wideMulWasm)
			require.NoError(t, err)
			gomul, multi3, multi3_4, notmul := mod.ExportedFunction("gomul"), mod.ExportedFunction("multi3"),
				mod.ExportedFunction("multi3_4"), mod.ExportedFunction("notmul")
			swapmul, lo31 := mod.ExportedFunction("swapmul"), mod.ExportedFunction("lo31")
			product := func() (uint64, uint64) {
				buf, ok := mod.Memory().Read(64, 16)
				require.True(t, ok)
				return binary.LittleEndian.Uint64(buf[8:]), binary.LittleEndian.Uint64(buf)
			}

			rnd := rand.New(rand.NewSource(1))
			operand := func() uint64 {
				if rnd.Intn(4) == 0 {
					return edge[rnd.Intn(len(edge))]
				}
				return rnd.Uint64() >> (rnd.Intn(4) * 16)
			}
			for i := 0; i < 20000; i++ {
				a, ah, b, bh := operand(), operand(), operand(), operand()
				hi, lo := bits.Mul64(a, b)

				res, err := gomul.Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, []uint64{hi, lo}, res, "gomul(%#x, %#x)", a, b)

				res, err = swapmul.Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, []uint64{hi, lo}, res, "swapmul(%#x, %#x)", a, b)

				_, err = multi3.Call(ctx, 64, a, ah, b, bh)
				require.NoError(t, err)
				gh, gl := product()
				require.Equal(t, [2]uint64{hi + a*bh + ah*b, lo}, [2]uint64{gh, gl}, "multi3(%#x:%#x, %#x:%#x)", ah, a, bh, b)

				_, err = multi3_4.Call(ctx, 64, a, ah, b)
				require.NoError(t, err)
				gh, gl = product()
				require.Equal(t, [2]uint64{hi + ah*b, lo}, [2]uint64{gh, gl}, "multi3_4(%#x:%#x, %#x)", ah, a, b)

				x0, x1, y0 := a&0xffffffff, a>>32, b&0xffffffff
				u := x1*y0 + (x0*y0)>>32
				v := u&0xffffffff + x1*y0
				res, err = notmul.Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, x1*(b>>32)+u>>32+v>>32, res[0], "notmul(%#x, %#x)", a, b)

				w := ((x1*y0+(x0*y0)>>32)&0xffffffff + x0*(b>>32))
				res, err = lo31.Call(ctx, a, b)
				require.NoError(t, err)
				require.Equal(t, (x0*y0)&0xffffffff|w<<31, res[0], "lo31(%#x, %#x)", a, b)
			}
		})
	}
}
