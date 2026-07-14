package platform

import (
	"io"
	"math/rand"
)

// seed is a fixed seed value for NewFakeRandSource.
//
// Trivia: While arbitrary, 42 was chosen as it is the "Ultimate Answer" in
// the Douglas Adams novel "The Hitchhiker's Guide to the Galaxy."
const seed = int64(42)

// NewFakeRandSource returns a deterministic source of random values.
//
// Construction is lazy: math/rand's generator seeds a 607-int64 lagged-Fibonacci
// array (~4.9 KB + ~1800 iterations), and this source is created on *every*
// instantiate that doesn't set WithRandSource -- yet the common guest never reads
// random bytes at all. Deferring the generator to the first Read makes those
// instantiations pay nothing while keeping the output byte-identical (same
// algorithm, same seed) for guests that do read.
func NewFakeRandSource() io.Reader {
	return &lazyFakeRandSource{}
}

type lazyFakeRandSource struct{ r io.Reader }

func (l *lazyFakeRandSource) Read(p []byte) (int, error) {
	if l.r == nil {
		l.r = rand.New(rand.NewSource(seed))
	}
	return l.r.Read(p)
}
