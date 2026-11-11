package adhoc

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestStripMinedLoops runs counted loops of every shape the compiler strip-mines,
// and of the shapes it should leave alone, against the interpreter: same
// iteration counts, same final counters. Each loop is capped, so a shape that
// never meets its bound still ends, and spans run long enough for many refills.
func TestStripMinedLoops(t *testing.T) {
	if !platform.CompilerSupported() {
		t.Skip()
	}
	const loopCap = 50_000
	ctx := context.Background()
	compiler := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer compiler.Close(ctx)
	interpreter := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
	defer interpreter.Close(ctx)

	type width struct {
		name                                           string
		vt                                             wasm.ValueType
		bits                                           uint
		eqz, add, eq, ne, ltS, ltU, gtS, gtU, geU, leU byte
	}
	widths := []width{
		{"i32", wasm.ValueTypeI32, 32, wasm.OpcodeI32Eqz, wasm.OpcodeI32Add, wasm.OpcodeI32Eq, wasm.OpcodeI32Ne, wasm.OpcodeI32LtS, wasm.OpcodeI32LtU, wasm.OpcodeI32GtS, wasm.OpcodeI32GtU, wasm.OpcodeI32GeU, wasm.OpcodeI32LeU},
		{"i64", wasm.ValueTypeI64, 64, wasm.OpcodeI64Eqz, wasm.OpcodeI64Add, wasm.OpcodeI64Eq, wasm.OpcodeI64Ne, wasm.OpcodeI64LtS, wasm.OpcodeI64LtU, wasm.OpcodeI64GtS, wasm.OpcodeI64GtU, wasm.OpcodeI64GeU, wasm.OpcodeI64LeU},
	}
	rng := rand.New(rand.NewSource(1))

	for _, w := range widths {
		mask := uint64(1)<<w.bits - 1
		if w.bits == 64 {
			mask = ^uint64(0)
		}
		konst := func(v uint64) []byte {
			if w.bits == 32 {
				return append([]byte{wasm.OpcodeI32Const}, leb128s(int64(int32(uint32(v))))...)
			}
			return append([]byte{wasm.OpcodeI64Const}, leb128s(int64(v))...)
		}
		// The bound: local 1, or a constant.
		bound := func(constBound bool, n uint64) []byte {
			if constBound {
				return konst(n)
			}
			return []byte{wasm.OpcodeLocalGet, 1}
		}
		// Locals: 0 is x, 1 is n, 2 is the i64 iteration count.
		capCheck := append(append([]byte{wasm.OpcodeLocalGet, 2}, append([]byte{wasm.OpcodeI64Const}, leb128s(loopCap)...)...),
			wasm.OpcodeI64GeU, wasm.OpcodeBrIf)
		bump := []byte{
			wasm.OpcodeLocalGet, 2, wasm.OpcodeI64Const, 1, wasm.OpcodeI64Add, wasm.OpcodeLocalSet, 2,
		}
		step := func(s uint64) []byte {
			return append(append([]byte{wasm.OpcodeLocalGet, 0}, konst(s)...), w.add, wasm.OpcodeLocalSet, 0)
		}

		type shape struct {
			name string
			body func(s, n uint64, constBound bool) []byte
		}
		var shapes []shape
		// The test at the top leaves when x cmp n.
		for _, cmp := range []struct {
			name string
			op   byte
		}{{"eq", w.eq}, {"ge_u", w.geU}, {"le_u", w.leU}, {"gt_s", w.gtS}, {"ne", w.ne}} {
			shapes = append(shapes, shape{"top/leave-" + cmp.name, func(s, n uint64, constBound bool) []byte {
				var b []byte
				b = append(b, wasm.OpcodeBlock, 0x40, wasm.OpcodeLoop, 0x40, wasm.OpcodeLocalGet, 0)
				b = append(b, bound(constBound, n)...)
				b = append(b, cmp.op, wasm.OpcodeBrIf, 1)
				b = append(b, capCheck...)
				b = append(b, 1)
				b = append(b, bump...)
				b = append(b, step(s)...)
				return append(b, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd)
			}})
		}
		shapes = append(shapes, shape{"top/leave-eqz", func(s, n uint64, _ bool) []byte {
			var b []byte
			b = append(b, wasm.OpcodeBlock, 0x40, wasm.OpcodeLoop, 0x40, wasm.OpcodeLocalGet, 0, w.eqz, wasm.OpcodeBrIf, 1)
			b = append(b, capCheck...)
			b = append(b, 1)
			b = append(b, bump...)
			b = append(b, step(s)...)
			return append(b, wasm.OpcodeBr, 0, wasm.OpcodeEnd, wasm.OpcodeEnd)
		}})
		// The back edge goes on while x cmp n, x already stepped.
		for _, cmp := range []struct {
			name string
			op   byte
		}{{"ne", w.ne}, {"lt_u", w.ltU}, {"gt_u", w.gtU}, {"lt_s", w.ltS}, {"le_u", w.leU}} {
			shapes = append(shapes, shape{"back/go-" + cmp.name, func(s, n uint64, constBound bool) []byte {
				var b []byte
				b = append(b, wasm.OpcodeBlock, 0x40, wasm.OpcodeLoop, 0x40)
				b = append(b, capCheck...)
				b = append(b, 1)
				b = append(b, bump...)
				b = append(b, step(s)...)
				b = append(b, wasm.OpcodeLocalGet, 0)
				b = append(b, bound(constBound, n)...)
				return append(b, cmp.op, wasm.OpcodeBrIf, 0, wasm.OpcodeEnd, wasm.OpcodeEnd)
			}})
		}

		steps := []uint64{1, mask, 2, mask - 1, 4, mask - 3, 3, mask - 2, 16}
		for _, sh := range shapes {
			for _, s := range steps {
				for _, constBound := range []bool{false, true} {
					// A constant bound is drawn once per module.
					n := pick(rng, mask)
					name := fmt.Sprintf("%s/%s/step=%#x/const=%v", w.name, sh.name, s, constBound)
					t.Run(name, func(t *testing.T) {
						body := append(sh.body(s, n, constBound), wasm.OpcodeLocalGet, 2, wasm.OpcodeLocalGet, 0, wasm.OpcodeEnd)
						m := &wasm.Module{
							TypeSection: []wasm.FunctionType{{
								Params:  []wasm.ValueType{w.vt, w.vt},
								Results: []wasm.ValueType{wasm.ValueTypeI64, w.vt},
							}},
							FunctionSection: []wasm.Index{0},
							CodeSection:     []wasm.Code{{LocalTypes: []wasm.ValueType{wasm.ValueTypeI64}, Body: body}},
							ExportSection:   []wasm.Export{{Name: "f", Type: wasm.ExternTypeFunc, Index: 0}},
						}
						bin := binaryencoding.EncodeModule(m)
						cm, err := compiler.Instantiate(ctx, bin)
						require.NoError(t, err)
						defer cm.Close(ctx)
						im, err := interpreter.Instantiate(ctx, bin)
						require.NoError(t, err)
						defer im.Close(ctx)

						for i := 0; i < 12; i++ {
							x := pick(rng, mask)
							nn := n
							if !constBound {
								nn = pick(rng, mask)
							}
							if i%3 == 0 { // a bound the counter does reach, far off
								nn = (x + s*uint64(rng.Intn(3*loopCap/2))) & mask
							}
							want, err := im.ExportedFunction("f").Call(ctx, x, nn)
							require.NoError(t, err)
							got, err := cm.ExportedFunction("f").Call(ctx, x, nn)
							require.NoError(t, err)
							require.Equal(t, want, got, "x=%#x n=%#x", x, nn)
						}
					})
				}
			}
		}
	}
}

// pick draws a value at the edges of the range as often as from its middle.
func pick(rng *rand.Rand, mask uint64) uint64 {
	switch rng.Intn(6) {
	case 0:
		return uint64(rng.Intn(4))
	case 1:
		return (mask - uint64(rng.Intn(4))) & mask
	case 2:
		return (mask>>1 + uint64(rng.Intn(5)) - 2) & mask
	default:
		return rng.Uint64() & mask
	}
}

// leb128s encodes v as a signed LEB128.
func leb128s(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}
