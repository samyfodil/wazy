package wasm

import (
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// A guest whose allocator grows the heap even once used to make every later
// instantiation of the same module repeat the work: NewMemoryInstance sized the
// backing to Cap, the first memory.grow reallocated and copied the whole
// memory, and -- because the buffer pool buckets by exact capacity -- the grown
// buffer was filed under a size no later instantiation ever asked for, so the
// pool never hit either. Memory.capHighWaterPages carries the size the module
// settles at from one instantiation to the next. These tests pin that; without
// it every one of them fails.

// TestMemoryHighWater_SecondInstanceDoesNotRealloc is the whole point of the
// mechanism: once one instance has had to grow, the next starts big enough that
// the same growth is satisfied in place.
func TestMemoryHighWater_SecondInstanceDoesNotRealloc(t *testing.T) {
	memSec := &Memory{Min: 1, Cap: 1, Max: 4}
	owner := &mockModuleEngine{}

	first := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))
	require.Equal(t, uint64(1), first.Cap)
	firstBacking := unsafe.SliceData(first.backing)
	_, ok := first.Grow(1)
	require.True(t, ok)
	// Growing past Cap has to reallocate, so this one does pay the copy.
	require.False(t, firstBacking == unsafe.SliceData(first.backing))
	require.Equal(t, uint64(2), first.Cap)

	second := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))
	require.Equal(t, uint64(2), second.Cap, "second instance should start at the observed high-water")
	require.Equal(t, MemoryPagesToBytesNum(1), second.byteSize(), "logical size still starts at Min")

	secondBacking := unsafe.SliceData(second.backing)
	_, ok = second.Grow(1)
	require.True(t, ok)
	require.Equal(t, secondBacking, unsafe.SliceData(second.backing),
		"the same growth should now be satisfied in place, with no reallocation or copy")
	require.Equal(t, MemoryPagesToBytesNum(2), second.byteSize())
}

// TestMemoryHighWater_CappedByMax keeps the hint from ever allocating past what
// the module (or the embedder) allows, however large a mark an earlier instance
// left behind.
func TestMemoryHighWater_CappedByMax(t *testing.T) {
	t.Run("module maximum", func(t *testing.T) {
		memSec := &Memory{Min: 1, Cap: 1, Max: 2}
		atomic.StoreUint32(&memSec.capHighWaterPages, 100)
		mi := NewMemoryInstance(memSec, nil, &mockModuleEngine{}, uint64(MemoryLimitPages))
		require.Equal(t, uint64(2), mi.Cap)
	})
	t.Run("embedder limit", func(t *testing.T) {
		memSec := &Memory{Min: 1, Cap: 1, Max: 100}
		atomic.StoreUint32(&memSec.capHighWaterPages, 100)
		mi := NewMemoryInstance(memSec, nil, &mockModuleEngine{}, 3)
		require.Equal(t, uint64(3), mi.Cap)
	})
}

// TestMemoryHighWater_NoBleedIntoPreSizedInstance is the security case. The
// pool hands a pre-sized instance a buffer a previous tenant grew into and
// wrote across; every byte of it must still read back zero, because wasm linear
// memory starts zero and the reserve above the previous tenant's logical size
// is only assumed zero.
func TestMemoryHighWater_NoBleedIntoPreSizedInstance(t *testing.T) {
	memSec := &Memory{Min: 1, Cap: 1, Max: 4}
	owner := &mockModuleEngine{}

	// Establish the high-water so later instances are pre-sized and therefore
	// ask the pool for the grown capacity.
	seed := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))
	_, ok := seed.Grow(1)
	require.True(t, ok)

	// sync.Pool can drop a just-put buffer on a concurrent GC, so a single
	// round may not reuse anything; retry until it does (same idiom as
	// TestMemoryPool_NoBleed).
	reused := false
	for try := 0; !reused && try < 1000; try++ {
		dirty := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))
		require.Equal(t, uint64(2), dirty.Cap)
		_, ok := dirty.Grow(1)
		require.True(t, ok)
		for i := range dirty.Buffer {
			dirty.Buffer[i] = 0xAA
		}
		dirtyBacking := unsafe.SliceData(dirty.backing)
		putPooledMemoryBuffer(dirty.Buffer)

		next := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))
		if unsafe.SliceData(next.backing) != dirtyBacking {
			continue // pool missed; try again
		}
		reused = true
		for i, value := range next.backing[:cap(next.backing)] {
			if value != 0 {
				t.Fatalf("byte %d of a recycled pre-sized memory is %#x, want zero", i, value)
			}
		}
	}
	require.True(t, reused, "pool never returned the grown buffer across many attempts")
}

// TestMemoryHighWater_Monotonic pins that concurrent instantiations of one
// compiled module settle on the largest backing any of them needed, not
// whichever finished last.
func TestMemoryHighWater_Monotonic(t *testing.T) {
	memSec := &Memory{Min: 1, Cap: 1, Max: 64}
	owner := &mockModuleEngine{}
	mi := NewMemoryInstance(memSec, nil, owner, uint64(MemoryLimitPages))

	var wg sync.WaitGroup
	for _, pages := range []uint64{2, 16, 8, 4} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mi.raiseCapHighWater(pages)
		}()
	}
	wg.Wait()
	require.Equal(t, uint32(16), atomic.LoadUint32(&memSec.capHighWaterPages))
}

// TestMemoryHighWater_NilIsSafe covers a MemoryInstance built without a module
// behind it -- what tests and the component-model host memories do -- which has
// nowhere to record a mark and must simply grow.
func TestMemoryHighWater_NilIsSafe(t *testing.T) {
	mi := &MemoryInstance{Min: 1, Cap: 1, Max: 4, Buffer: make([]byte, MemoryPagesToBytesNum(1)),
		sizeBytes: MemoryPagesToBytesNum(1), ownerModuleEngine: &mockModuleEngine{}}
	require.Nil(t, mi.capHighWater)
	_, ok := mi.Grow(1)
	require.True(t, ok)
	require.Equal(t, MemoryPagesToBytesNum(2), mi.byteSize())
}
