package frontend

import (
	"math"
	"math/rand"
	"testing"
)

// Test_magicS32 runs the arithmetic sdiv32 emits, in Go, against Go's own division, for every divisor
// sdiv32 takes up to a thousand either way and a spread of larger ones, on dividends at the edges and at
// random.
func Test_magicS32(t *testing.T) {
	divisors := []int32{math.MaxInt32, math.MinInt32 + 1, 1<<30 + 3, -(1<<30 + 3), 7919, -7919, 1000003, 641}
	for d := int32(3); d <= 1000; d++ {
		divisors = append(divisors, d, -d)
	}
	rng := rand.New(rand.NewSource(1))
	xs := []int32{0, 1, -1, 2, -2, math.MaxInt32, math.MinInt32, math.MaxInt32 - 1, math.MinInt32 + 1}
	for i := 0; i < 64; i++ {
		xs = append(xs, int32(rng.Uint32()), int32(rng.Uint32())>>rng.Intn(31))
	}
	for _, d := range divisors {
		if d&(d-1) == 0 || -d&(-d-1) == 0 {
			continue // A power of two, which sPow2 takes.
		}
		m, sh := magicS32(d)
		for _, x := range xs {
			hi := int32((int64(x) * int64(m)) >> 32)
			var q int32
			if d > 0 && m < 0 || d < 0 && m > 0 {
				if d > 0 {
					hi += x
				} else {
					hi -= x
				}
				q = hi >> sh
			} else {
				q = int32((int64(x) * int64(m)) >> (32 + sh))
			}
			q += int32(uint32(q) >> 31)
			if want := x / d; q != want {
				t.Fatalf("%d / %d: got %d, want %d (m=%d, sh=%d)", x, d, q, want, m, sh)
			}
		}
	}
}
