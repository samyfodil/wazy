package wasm

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestMemoryPool_NoBleed is the non-negotiable security/correctness test for
// the linear-memory buffer pool: instantiate a memory, write a recognizable
// pattern across the whole thing, close it (which should recycle the
// backing array into the pool), then instantiate a memory of the identical
// shape again -- forcing pool reuse -- and assert the new memory is entirely
// zero. A failure here means guest data from one instance leaked into
// another, which is a correctness and security bug (see memory_pool.go's
// doc for why the pool must always zero on acquire).
func TestMemoryPool_NoBleed(t *testing.T) {
	memSec := &Memory{Min: 2, Cap: 2, Max: 2}
	owner := &mockModuleEngine{}
	capBytes := MemoryPagesToBytesNum(2)

	// Part 1 (recycle path): an owner's Close returns its Buffer to the pool and
	// clears the field, so a stale post-Close read can't observe a future
	// tenant's data.
	first := NewMemoryInstance(memSec, nil, owner)
	require.Equal(t, int(capBytes), len(first.Buffer))
	for i := range first.Buffer {
		first.Buffer[i] = 0xAA
	}
	m := &ModuleInstance{MemoryInstance: first, Engine: owner}
	require.NoError(t, m.ensureResourcesClosed(context.Background()))
	require.Nil(t, first.Buffer, "owner's Buffer field must be cleared once pooled")

	// Part 2 (the actual no-bleed property): getPooledMemoryBuffer MUST hand back
	// a zeroed buffer, so guest data can never bleed across instances. The pool
	// now holds only dirty buffers -- first.Buffer (0xAA, recycled above) plus the
	// 0xBB one put here -- so whichever getPooledMemoryBuffer returns was dirty and
	// must come back clear()'d. This asserts the invariant directly, without the
	// flaky "same backing array" check (sync.Pool may hand back either dirty
	// buffer, or drop one on GC -- non-deterministic under -race / slow emulation).
	// Retry put->get until the pool returns a buffer: under -race's aggressive GC,
	// sync.Pool can drop a just-put buffer, so a single get may miss. A fresh put
	// each attempt makes this converge in a couple of iterations (GC can't drop
	// every one), keeping the test deterministic without a flaky nil-check.
	var got []byte
	for try := 0; got == nil && try < 1000; try++ {
		dirty := make([]byte, capBytes)
		for i := range dirty {
			dirty[i] = 0xBB
		}
		putPooledMemoryBuffer(dirty)
		got = getPooledMemoryBuffer(capBytes)
	}
	require.NotNil(t, got, "pool returned no buffer across many put/get attempts")
	for i, b := range got[:cap(got)] {
		if b != 0 {
			t.Fatalf("byte %d of a reacquired (previously dirty) buffer is %#x, want 0 (data bled across tenants)", i, b)
		}
	}
}

// TestMemoryPool_ImportedMemoryNotRecycled proves that a memory instance
// which was ever shared with another ModuleInstance via cross-module import
// resolution is never returned to the pool when its owner closes, even
// though the owner-closing code path is otherwise identical. Pooling it
// would let a completely unrelated, later-instantiated module's data land in
// memory the importer still believes is its own -- a cross-tenant
// corruption bug.
func TestMemoryPool_ImportedMemoryNotRecycled(t *testing.T) {
	const moduleName, exportName = "test", "target"
	memSec := &Memory{Min: 1, Cap: 1, Max: 1}
	ownerEngine := &mockModuleEngine{}

	mem := NewMemoryInstance(memSec, nil, ownerEngine)
	for i := range mem.Buffer {
		mem.Buffer[i] = 0xBB
	}

	s := newStore()
	owner := &ModuleInstance{
		MemoryInstance: mem,
		Exports:        map[string]*Export{exportName: {Type: ExternTypeMemory}},
		ModuleName:     moduleName,
		Engine:         ownerEngine,
		s:              s,
	}
	s.nameToModule[moduleName] = owner

	importer := &ModuleInstance{s: s, Engine: &mockModuleEngine{resolveImportsCalled: map[Index]Index{}}}
	err := importer.resolveImports(context.Background(), &Module{
		ImportPerModule: map[string][]*Import{
			moduleName: {{Module: moduleName, Name: exportName, Type: ExternTypeMemory, DescMem: &Memory{Max: 1}}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, mem, importer.MemoryInstance)
	require.Equal(t, 1, mem.importers)

	// Close the owner. Because an importer is still live (importers > 0), this must NOT return
	// Buffer to the pool or clear it out from under the still-live importer.
	require.NoError(t, owner.ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "an imported memory's Buffer must survive its owner's Close")
	for i, b := range mem.Buffer {
		if b != 0xBB {
			t.Fatalf("byte %d changed to %#x after owner Close; imported memory must stay untouched", i, b)
		}
	}

	// The importer's view is unaffected -- same object, same data.
	require.Equal(t, mem, importer.MemoryInstance)
	for i, b := range importer.MemoryInstance.Buffer {
		if b != 0xBB {
			t.Fatalf("importer observed corrupted byte %d = %#x", i, b)
		}
	}
}

// TestMemoryPool_ImportAfterOwnerClosed_Errors exercises the mem.Mux-guarded
// handshake's other outcome: if the owner's Close already committed (set
// ownerClosed) before a resolveImports call reaches the same memory, the
// import must fail with a clear error instead of silently handing the
// importer a stale-and-possibly-already-recycled buffer.
func TestMemoryPool_ImportAfterOwnerClosed_Errors(t *testing.T) {
	const moduleName, exportName = "test", "target"
	memSec := &Memory{Min: 1, Cap: 1, Max: 1}
	ownerEngine := &mockModuleEngine{}

	mem := NewMemoryInstance(memSec, nil, ownerEngine)

	s := newStore()
	owner := &ModuleInstance{
		MemoryInstance: mem,
		Exports:        map[string]*Export{exportName: {Type: ExternTypeMemory}},
		ModuleName:     moduleName,
		Engine:         ownerEngine,
		s:              s,
	}
	s.nameToModule[moduleName] = owner

	// Simulate the owner having already committed to closing (e.g. a
	// concurrent Close raced ahead of this import resolution) without
	// actually unregistering it from the store, to isolate exactly the
	// mem.Mux handshake this test targets.
	mem.Mux.Lock()
	mem.ownerClosed = true
	mem.Mux.Unlock()

	importer := &ModuleInstance{s: s, Engine: &mockModuleEngine{resolveImportsCalled: map[Index]Index{}}}
	err := importer.resolveImports(context.Background(), &Module{
		ImportPerModule: map[string][]*Import{
			moduleName: {{Module: moduleName, Name: exportName, Type: ExternTypeMemory, DescMem: &Memory{Max: 1}}},
		},
	})
	require.EqualError(t, err, "import memory[test.target]: memory owner module was closed concurrently")
	require.Nil(t, importer.MemoryInstance)
}

// TestMemoryPool_SharedMemoryNotRecycled proves shared (wasm-threads)
// memories, which use a fixed max-sized buffer with different growth
// semantics and can be referenced by multiple concurrent agents for their
// entire lifetime, are never routed through the pool.
func TestMemoryPool_SharedMemoryNotRecycled(t *testing.T) {
	memSec := &Memory{Min: 1, Cap: 1, Max: 1, IsShared: true}
	owner := &mockModuleEngine{}

	mem := NewMemoryInstance(memSec, nil, owner)
	require.True(t, mem.Shared)

	m := &ModuleInstance{MemoryInstance: mem, Engine: owner}
	require.NoError(t, m.ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "shared memory's Buffer must not be cleared/pooled on Close")
}

// TestMemoryPool_ConcurrentCreateCloseRace hammers the pool from many
// goroutines simultaneously, each allocating a memory, stamping it with a
// goroutine-specific pattern, closing it (recycling the buffer), and
// re-checking freshly-acquired buffers are always zero. Run with -race to
// confirm the pool itself introduces no data races, and the assertions catch
// any cross-goroutine bleed a race might cause.
func TestMemoryPool_ConcurrentCreateCloseRace(t *testing.T) {
	memSec := &Memory{Min: 1, Cap: 1, Max: 1}
	const goroutines = 32
	const iterations = 64

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			owner := &mockModuleEngine{}
			pattern := byte(g%255 + 1) // never 0, so we can detect bleed
			for it := 0; it < iterations; it++ {
				mem := NewMemoryInstance(memSec, nil, owner)
				for i, b := range mem.Buffer {
					if b != 0 {
						t.Errorf("goroutine %d iter %d: byte %d not zero on acquire: %#x", g, it, i, b)
						return
					}
				}
				for i := range mem.Buffer {
					mem.Buffer[i] = pattern
				}
				mi := &ModuleInstance{MemoryInstance: mem, Engine: owner}
				if err := mi.ensureResourcesClosed(context.Background()); err != nil {
					t.Errorf("goroutine %d iter %d: ensureResourcesClosed: %v", g, it, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// poolImportSetup builds an owner ModuleInstance holding a fresh poolable
// memory (registered in a store) and n importer ModuleInstances that have each
// resolved it, so mem.importers == n. Used by the import-path recycling tests.
func poolImportSetup(t *testing.T, n int) (*MemoryInstance, *ModuleInstance, []*ModuleInstance) {
	t.Helper()
	const moduleName, exportName = "test", "target"
	memSec := &Memory{Min: 1, Cap: 1, Max: 1}
	ownerEngine := &mockModuleEngine{}
	mem := NewMemoryInstance(memSec, nil, ownerEngine)

	s := newStore()
	owner := &ModuleInstance{
		MemoryInstance: mem,
		Exports:        map[string]*Export{exportName: {Type: ExternTypeMemory}},
		ModuleName:     moduleName,
		Engine:         ownerEngine,
		s:              s,
	}
	s.nameToModule[moduleName] = owner

	importers := make([]*ModuleInstance, n)
	for i := range n {
		imp := &ModuleInstance{s: s, Engine: &mockModuleEngine{resolveImportsCalled: map[Index]Index{}}}
		err := imp.resolveImports(context.Background(), &Module{
			ImportPerModule: map[string][]*Import{
				moduleName: {{Module: moduleName, Name: exportName, Type: ExternTypeMemory, DescMem: &Memory{Max: 1}}},
			},
		})
		require.NoError(t, err)
		importers[i] = imp
	}
	require.Equal(t, n, mem.importers)
	return mem, owner, importers
}

// TestMemoryPool_LastImporterCloseRecycles: with an importer still live, the
// owner's Close must NOT recycle; the LAST importer's Close (after the owner
// closed) must. Recycling is observed deterministically via mem.Buffer being
// claimed (nil) -- that means it was handed to putPooledMemoryBuffer. The pool's
// zero-on-acquire (no cross-instance bleed) is the same code path for any recycle
// and is covered by TestMemoryPool_NoBleed; asserting the pool hands back this
// exact backing array is intentionally omitted, as sync.Pool may drop it on GC
// (non-deterministic under -race / slow emulation).
func TestMemoryPool_LastImporterCloseRecycles(t *testing.T) {
	mem, owner, imps := poolImportSetup(t, 1)

	require.NoError(t, owner.ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "owner close with a live importer must not recycle")

	require.NoError(t, imps[0].ensureResourcesClosed(context.Background()))
	require.Nil(t, mem.Buffer, "last importer close after owner close must recycle")
}

// TestMemoryPool_ImporterThenOwnerCloseRecycles: the other close order. The
// importer closes first (importers -> 0, but owner not closed yet => no
// recycle), then the owner closes as the last closer => recycle.
func TestMemoryPool_ImporterThenOwnerCloseRecycles(t *testing.T) {
	mem, owner, imps := poolImportSetup(t, 1)

	require.NoError(t, imps[0].ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "importer close before the owner closes must not recycle")
	require.Equal(t, 0, mem.importers)

	require.NoError(t, owner.ensureResourcesClosed(context.Background()))
	require.Nil(t, mem.Buffer, "owner close as the last closer must recycle")
}

// TestMemoryPool_TwoImportersRecycleOnLast: two importers; recycling waits for
// BOTH plus the owner to close, whichever is last.
func TestMemoryPool_TwoImportersRecycleOnLast(t *testing.T) {
	mem, owner, imps := poolImportSetup(t, 2)

	require.NoError(t, owner.ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "two importers still live")
	require.NoError(t, imps[0].ensureResourcesClosed(context.Background()))
	require.NotNil(t, mem.Buffer, "one importer still live")
	require.NoError(t, imps[1].ensureResourcesClosed(context.Background()))
	require.Nil(t, mem.Buffer, "last importer closes after the owner => recycle")
}

// TestMemoryPool_ConcurrentOwnerImporterCloseRace hammers the owner and all
// importers closing concurrently on one shared memory. Every access to the
// shared MemoryInstance is under mem.Mux, and the Buffer != nil claim guard
// makes recycling single-shot regardless of order -- so under -race this must
// stay clean and end with Buffer claimed exactly once (nil).
func TestMemoryPool_ConcurrentOwnerImporterCloseRace(t *testing.T) {
	for range 300 {
		mem, owner, imps := poolImportSetup(t, 3)
		closers := append([]*ModuleInstance{owner}, imps...)
		var wg sync.WaitGroup
		wg.Add(len(closers))
		for _, c := range closers {
			go func(mi *ModuleInstance) {
				defer wg.Done()
				_ = mi.ensureResourcesClosed(context.Background())
			}(c)
		}
		wg.Wait()
		require.Nil(t, mem.Buffer, "after every closer, Buffer must be claimed exactly once")
	}
}
