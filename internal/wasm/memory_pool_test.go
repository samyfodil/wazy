package wasm

import (
	"context"
	"sync"
	"testing"
	"unsafe"

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

	first := NewMemoryInstance(memSec, nil, owner)
	require.Equal(t, int(MemoryPagesToBytesNum(2)), len(first.Buffer))

	// Write a recognizable, non-zero pattern across the *entire* backing
	// array (not just len, since cap == len here anyway given Min == Cap).
	firstAddr := unsafe.Pointer(unsafe.SliceData(first.Buffer))
	for i := range first.Buffer {
		first.Buffer[i] = 0xAA
	}

	// Close the owning module: this must recycle first.Buffer into the pool.
	m := &ModuleInstance{MemoryInstance: first, Engine: owner}
	require.NoError(t, m.ensureResourcesClosed(context.Background()))
	require.Nil(t, first.Buffer, "owner's Buffer field must be cleared once pooled, so a stale post-Close read can't observe a future tenant's data")

	// Instantiate a memory of the exact same shape again: this should reuse
	// the pooled backing array (same underlying allocation)...
	second := NewMemoryInstance(memSec, nil, owner)
	secondAddr := unsafe.Pointer(unsafe.SliceData(second.Buffer))
	require.Equal(t, firstAddr, secondAddr, "expected the pool to actually hand back the same backing array -- otherwise this test isn't exercising reuse at all")

	// ...but it MUST be entirely zeroed, with no trace of the first
	// instance's data.
	for i, b := range second.Buffer {
		if b != 0 {
			t.Fatalf("byte %d of reused memory is %#x, want 0 (data bled across instances)", i, b)
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
	require.True(t, mem.imported)

	// Close the owner. Because mem.imported is true, this must NOT return
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
