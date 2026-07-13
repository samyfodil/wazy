package wazevo

import (
	"runtime/debug"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// disableGC turns off automatic GC for the duration of the test and restores
// the previous setting on cleanup. sync.Pool contents are only ever cleared
// in lockstep with a GC cycle (see sync.Pool's own docs: "any item stored in
// the Pool may be removed automatically at any time without notification"),
// so a Put immediately followed by a Get is only guaranteed to observe the
// same object back if no GC lands in between -- which, left to chance, can
// and does happen under -race (heavier memory pressure from shadow-memory
// tracking triggers GC far more often) even in a tight, otherwise
// single-goroutine test.
func disableGC(tb testing.TB) {
	prev := debug.SetGCPercent(-1)
	tb.Cleanup(func() { debug.SetGCPercent(prev) })
}

func TestStackPoolClassForAcquire(t *testing.T) {
	tests := []struct {
		n   int
		exp int
	}{
		{n: 1, exp: 0},
		{n: stackPoolBaseSize - 1, exp: 0},
		{n: stackPoolBaseSize, exp: 0},
		{n: stackPoolBaseSize + 1, exp: 1},
		{n: stackPoolBaseSize * 2, exp: 1},
		{n: stackPoolBaseSize*2 + 1, exp: 2},
		{n: stackPoolBaseSize * 4, exp: 2},
		{n: stackPoolBaseSize << (stackPoolNumClasses - 1), exp: stackPoolNumClasses - 1},
		{n: stackPoolBaseSize<<(stackPoolNumClasses-1) + 1, exp: -1},
		{n: 1 << 62, exp: -1},
	}
	for _, tc := range tests {
		require.Equal(t, tc.exp, stackPoolClassForAcquire(tc.n))
	}
}

func TestStackPoolClassForRelease(t *testing.T) {
	tests := []struct {
		n   int
		exp int
	}{
		{n: stackPoolBaseSize - 1, exp: -1},
		{n: stackPoolBaseSize, exp: 0},
		// One byte over class 0's canonical size must NOT be filed under
		// class 1 (it doesn't actually have class 1's full capacity) --
		// this is the crux of why acquire (ceiling) and release (floor)
		// must be different functions. See stackPoolClassForRelease's doc
		// comment.
		{n: stackPoolBaseSize + 1, exp: 0},
		{n: stackPoolBaseSize*2 - 1, exp: 0},
		{n: stackPoolBaseSize * 2, exp: 1},
		{n: stackPoolBaseSize*2 + 1, exp: 1},
		{n: stackPoolBaseSize * 4, exp: 2},
		{n: stackPoolBaseSize << (stackPoolNumClasses - 1), exp: stackPoolNumClasses - 1},
		// Something absurdly large than every class still floors to the
		// largest class rather than erroring: it's just underutilized.
		{n: (stackPoolBaseSize << (stackPoolNumClasses - 1)) * 100, exp: stackPoolNumClasses - 1},
	}
	for _, tc := range tests {
		require.Equal(t, tc.exp, stackPoolClassForRelease(tc.n))
	}
}

// invariant is the property both classing functions must jointly uphold for
// pooling to be safe: any buffer released for a request of size n must, when
// later acquired for a request of the same size n (or smaller), be at least
// n bytes -- i.e. stackPoolClassForRelease(n) must never be an *earlier*
// (smaller) canonical size than what a request for exactly n would demand of
// whatever class it lands in.
func TestStackPoolClassing_neverUndersized(t *testing.T) {
	for n := stackPoolBaseSize; n <= stackPoolBaseSize<<8; n++ {
		relClass := stackPoolClassForRelease(n)
		if relClass < 0 {
			continue
		}
		relCanonical := stackPoolBaseSize << uint(relClass)
		if relCanonical > n {
			t.Fatalf("release class %d for n=%d has canonical size %d > n: buffer would be advertised as bigger than it is", relClass, n, relCanonical)
		}
		// A fresh request for exactly relCanonical bytes must resolve to
		// a class whose canonical size is relCanonical (i.e. this exact
		// bucket), so a buffer released into it is never handed out to a
		// caller needing more than it actually has.
		acqClass := stackPoolClassForAcquire(relCanonical)
		if acqClass != relClass {
			t.Fatalf("n=%d: release class %d (canonical %d) but acquiring exactly that size resolves to class %d", n, relClass, relCanonical, acqClass)
		}
	}
}

// maxPoolRetries bounds the retry loops below. sync.Pool provides no
// guarantee that an item Put is later returned by Get -- and, under the
// race detector specifically, Put deliberately drops the item on the floor
// about 1 in 4 calls on purpose (see sync/pool.go's race.Enabled fastrand
// check), precisely to help surface code that wrongly assumes Pool
// retention. Retrying a generous, bounded number of times keeps these tests
// deterministic (failure to observe a single hit in maxPoolRetries tries,
// each independently a 3-in-4 shot, is astronomically unlikely) without
// asserting something sync.Pool doesn't promise.
const maxPoolRetries = 50

func TestEngine_acquireReleaseStack_reusesBuffer(t *testing.T) {
	disableGC(t)
	e := &engine{}

	buf, boxed := e.acquireStack(stackPoolBaseSize)
	require.Equal(t, stackPoolBaseSize, len(buf))
	require.Nil(t, boxed) // pool starts empty: this is a cold miss.

	reused := false
	for i := 0; i < maxPoolRetries && !reused; i++ {
		// Capture the pointer of the buffer about to be released *this*
		// iteration -- not the original, pre-loop one: on an earlier
		// iteration's dropped Put, buf was reassigned to a fresh
		// (differently-addressed) allocation, and it's that current buffer
		// -- not the long-gone original -- that this iteration's release is
		// actually offering back to the pool.
		ptr := unsafe.Pointer(&buf[0])
		e.releaseStack(buf, boxed)
		buf, boxed = e.acquireStack(stackPoolBaseSize)
		if boxed != nil {
			reused = true
			require.Equal(t, ptr, unsafe.Pointer(&buf[0])) // the exact same backing array came back.
		}
	}
	require.True(t, reused, "never observed a pool hit in %d attempts", maxPoolRetries)
}

func TestEngine_releaseStack_grownBufferGoesToItsOwnClass(t *testing.T) {
	disableGC(t)
	e := &engine{}

	// Simulate a stack that grew from the default size to something in a
	// higher class, as growStack/cloneStack would produce.
	grown := make([]byte, stackPoolBaseSize*3) // floors into class 1 (2x base), not class 0 or 2.

	reused := false
	for i := 0; i < maxPoolRetries && !reused; i++ {
		e.releaseStack(grown, nil)

		// Class 0 must NOT have received it: an acquire for the default size
		// must not receive an oversized-relative-to-class-0 buffer, and more
		// importantly a request that specifically needs class 0 must still
		// get served (by a fresh allocation here, since this release never
		// touches class 0's pool) rather than silently starving.
		buf0, _ := e.acquireStack(stackPoolBaseSize)
		require.Equal(t, stackPoolBaseSize, len(buf0))

		// Class 1 (2x base) should have received it.
		buf1, boxed1 := e.acquireStack(stackPoolBaseSize + 1) // resolves to class 1.
		if boxed1 != nil {
			reused = true
			require.Equal(t, len(grown), len(buf1))
		}
	}
	require.True(t, reused, "never observed a pool hit in %d attempts", maxPoolRetries)
}

func TestEngine_acquireStack_tooLargeIsNotPooled(t *testing.T) {
	e := &engine{}
	n := (stackPoolBaseSize << (stackPoolNumClasses - 1)) + 1
	buf, boxed := e.acquireStack(n)
	require.Equal(t, n, len(buf))
	require.Nil(t, boxed)

	// Releasing an oversized buffer beyond the largest class floors into
	// the top class rather than panicking or growing the array.
	e.releaseStack(buf, nil)
}
