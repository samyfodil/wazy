package abi

import (
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// What the typed-slice rule buys, per element type, against the general
// []Value path it replaced. Both are the real lifting functions, not stand-ins.
//
// The general path is kept for lists whose elements are not fixed-width
// primitives, so it is not dead code being benchmarked for show: this is the
// comparison that decides which path a given element type should take.
func BenchmarkScalarListLift(b *testing.B) {
	const n = 256

	for _, prim := range []string{"u8", "u16", "u32", "u64", "s32", "f64", "bool"} {
		elemType := binary.PrimitiveDesc{Prim: prim}
		size, err := Size(elemType, nil)
		if err != nil {
			b.Fatal(err)
		}
		mem := make([]byte, 16+n*size)
		// Varied, non-zero bytes. Zero-filled memory would let every boxed
		// element on the general path hit the runtime's small-integer cache
		// and allocate nothing, which is not what real data does.
		for i := range mem {
			mem[i] = byte(i*7 + 1)
		}

		b.Run(fmt.Sprintf("%s/typed", prim), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v, _, err := liftScalarList(mem, 16, n, prim)
				if err != nil {
					b.Fatal(err)
				}
				_ = v
			}
		})

		b.Run(fmt.Sprintf("%s/general", prim), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				v, err := loadListFromRange(mem, 16, n, elemType, nil)
				if err != nil {
					b.Fatal(err)
				}
				_ = v
			}
		})
	}
}
