package wasm

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// readUntilClosed hammers mem through the api.Memory surface from one
// goroutine while close runs on another, which is issue #69's shape: an
// embedder that cannot order the two because the close is a kill (see
// WithCloseOnContextDone) and the reader is a call still unwinding through
// host code. Run under -race.
func readUntilClosed(t *testing.T, mem *MemoryInstance, closeFn func()) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100000; i++ {
			if _, ok := mem.Read(0, 8); !ok {
				return // observed the close, which is the documented outcome
			}
		}
	}()
	closeFn()
	wg.Wait()
}

// Closing the owner while another goroutine reads its memory must not be a
// data race. It used to be: the close nil-ed Buffer and backing, which every
// accessor reads through visibleBuffer with no lock.
func TestMemoryClose_ReadDuringOwnerCloseIsRaceFree(t *testing.T) {
	owner := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 2, Cap: 2, Max: 2}, nil, owner, uint64(MemoryLimitPages))
	m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

	readUntilClosed(t, mem, func() {
		require.NoError(t, m.ensureResourcesClosed(context.Background()))
	})

	// Once the buffer is pooled the memory must read as empty, not as whatever
	// the pool hands that array to next.
	_, ok := mem.Read(0, 1)
	require.False(t, ok, "a read after the buffer was pooled must fail")
	require.Equal(t, uint32(0), mem.Size())
}

// The same race on the other close path: the owner defers recycling to the
// last importer, so that Close is the one that releases the storage.
func TestMemoryClose_ReadDuringImporterCloseIsRaceFree(t *testing.T) {
	owner := &mockModuleEngine{}
	importer := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 2, Cap: 2, Max: 2}, nil, owner, uint64(MemoryLimitPages))
	mem.importers = 1

	ownerMod := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}
	importerMod := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: importer}

	require.NoError(t, ownerMod.ensureResourcesClosed(context.Background()))
	_, ok := mem.Read(0, 1)
	require.True(t, ok, "an importer is still live, so the memory must still read")

	readUntilClosed(t, mem, func() {
		require.NoError(t, importerMod.ensureResourcesClosed(context.Background()))
	})

	_, ok = mem.Read(0, 1)
	require.False(t, ok, "a read after the last importer pooled the buffer must fail")
}

// Concurrent closes must release the storage exactly once -- putting one array
// into the pool twice would hand it to two live instances. The claim used to be
// "whoever nils Buffer wins"; released.Swap is what carries it now.
func TestMemoryClose_ConcurrentClosesReleaseOnce(t *testing.T) {
	owner := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1}, nil, owner, uint64(MemoryLimitPages))
	mem.importers = 2

	ownerMod := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}
	imp1 := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: &mockModuleEngine{}}
	imp2 := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: &mockModuleEngine{}}

	var wg sync.WaitGroup
	for _, m := range []*ModuleInstance{ownerMod, imp1, imp2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, m.ensureResourcesClosed(context.Background()))
		}()
	}
	wg.Wait()

	require.True(t, mem.released.Load(), "every holder closed, so the buffer must have been released")
	_, ok := mem.Read(0, 1)
	require.False(t, ok)
}

// The engines index mem.Buffer directly and never consult released, so a close
// landing while a call is still executing must NOT hand that buffer to the
// pool -- the next instantiation would get the same array and write it under
// the running call. Observed in go-pdfium's suite as one instance's i64.store
// racing another's i32.load at one address. The release waits for the last call
// out instead.
func TestMemoryClose_DoesNotRecycleWhileACallIsInFlight(t *testing.T) {
	owner := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1}, nil, owner, uint64(MemoryLimitPages))
	m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

	var slot CallSlot
	m.RegisterCallSlot(&slot)
	slot.Enter() // a call is running against this memory
	require.NoError(t, m.ensureResourcesClosed(context.Background()))

	// Closed for api.Memory purposes, but the buffer is still ours.
	require.True(t, mem.released.Load(), "the close must still mark the memory released")
	_, ok := mem.Read(0, 1)
	require.False(t, ok)

	mem.Mux.Lock()
	pending := mem.pendingRelease.recycle
	mem.Mux.Unlock()
	require.NotNil(t, pending, "the recycle must be deferred while a call is in flight")

	m.ExitCall(&slot) // last call out performs it

	mem.Mux.Lock()
	pending = mem.pendingRelease.recycle
	mem.Mux.Unlock()
	require.Nil(t, pending, "the last call out must have performed the deferred recycle")
}

// Nested and concurrent calls: only the LAST one out releases.
func TestMemoryClose_ReleasesOnLastCallOut(t *testing.T) {
	owner := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1}, nil, owner, uint64(MemoryLimitPages))
	m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

	var s1, s2, s3 CallSlot
	for _, s := range []*CallSlot{&s1, &s2, &s3} {
		m.RegisterCallSlot(s)
		s.Enter()
	}
	require.NoError(t, m.ensureResourcesClosed(context.Background()))

	pendingRecycle := func() []byte {
		mem.Mux.Lock()
		defer mem.Mux.Unlock()
		return mem.pendingRelease.recycle
	}

	m.ExitCall(&s1)
	require.NotNil(t, pendingRecycle(), "two calls still in flight")
	m.ExitCall(&s2)
	require.NotNil(t, pendingRecycle(), "one call still in flight")
	m.ExitCall(&s3)
	require.Nil(t, pendingRecycle(), "the last call out releases")
}

// A close that finds no call in flight releases inline, as before -- the
// deferral must not cost the common path a trip through pendingRelease.
func TestMemoryClose_ReleasesInlineWithNoCallInFlight(t *testing.T) {
	owner := &mockModuleEngine{}
	mem := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1}, nil, owner, uint64(MemoryLimitPages))
	m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

	require.NoError(t, m.ensureResourcesClosed(context.Background()))
	require.True(t, mem.released.Load())

	mem.Mux.Lock()
	defer mem.Mux.Unlock()
	require.Nil(t, mem.pendingRelease.recycle, "nothing to defer when no call is running")
	require.Nil(t, mem.pendingRelease.free)
}

// The close and the last ExitCall can interleave either way; the buffer must be
// released exactly once regardless. Run under -race.
func TestMemoryClose_ConcurrentCloseAndCallExitReleaseOnce(t *testing.T) {
	for range 200 {
		owner := &mockModuleEngine{}
		mem := NewMemoryInstance(&Memory{Min: 1, Cap: 1, Max: 1}, nil, owner, uint64(MemoryLimitPages))
		m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

		var slot CallSlot
		m.RegisterCallSlot(&slot)
		slot.Enter()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); require.NoError(t, m.ensureResourcesClosed(context.Background())) }()
		go func() { defer wg.Done(); m.ExitCall(&slot) }()
		wg.Wait()

		mem.Mux.Lock()
		leftover := mem.pendingRelease.recycle != nil || mem.pendingRelease.free != nil
		mem.Mux.Unlock()
		require.False(t, leftover, "the buffer was claimed but never released")
		require.True(t, mem.released.Load())
	}
}

// An allocator-backed memory frees its pages at close. A read racing that used
// to reach freed memory; now it fails, for the same reason and by the same
// flag as the pooled case.
func TestMemoryClose_AllocatorFreeIsRaceFree(t *testing.T) {
	owner := &mockModuleEngine{}
	expBuffer := sliceAllocator(MemoryPagesToBytesNum(1), MemoryPagesToBytesNum(1))
	mem := &MemoryInstance{
		Buffer:            expBuffer.Reallocate(MemoryPagesToBytesNum(1)),
		Min:               1,
		Cap:               1,
		Max:               1,
		expBuffer:         expBuffer,
		ownerModuleEngine: owner,
	}
	mem.sizeBytes = uint64(len(mem.Buffer))
	m := &ModuleInstance{Memories: []*MemoryInstance{mem}, Engine: owner}

	readUntilClosed(t, mem, func() {
		require.NoError(t, m.ensureResourcesClosed(context.Background()))
	})

	_, ok := mem.Read(0, 1)
	require.False(t, ok, "a read after the allocator freed the pages must fail")
}
