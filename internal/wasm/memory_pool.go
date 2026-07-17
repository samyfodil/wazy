package wasm

import "sync"

// memoryBufferPools holds free linear-memory buffers, bucketed by their
// exact byte capacity, for reuse across MemoryInstance create/close cycles.
// This exists purely to amortize the make([]byte, ...) + zero-fill cost that
// NewMemoryInstance otherwise pays on every Instantiate call -- profiling
// showed this dominates a cached (CompileCache-backed) Instantiate's
// allocation cost (96%+ of bytes, ~25-30% of CPU time in
// BenchmarkInstantiateCached), since a fresh multi-hundred-KB-to-multi-MB
// slice is allocated and cleared from scratch every time even though the
// compiled module (and therefore the memory's Min/Cap/Max shape) is
// identical across calls.
//
// # Safety
//
// A buffer is returned to this pool only once EVERY ModuleInstance that could
// reach it is gone: the owning module has closed (mem.ownerClosed) AND every
// importer has closed (mem.importers has fallen to 0). A MemoryInstance is
// shared, unchanged, with each ModuleInstance that imports it (store.go's
// resolveImports, ExternTypeMemory case, which increments mem.importers); each
// such importer decrements on its own Close (ensureResourcesClosed in
// module_instance.go). While any importer is still live, its (identical,
// aliased) MemoryInstance.Buffer is the same backing array, so recycling it
// then would let some unrelated, later-instantiated module's data land in
// memory the importer believes is still its own -- a cross-tenant correctness
// and security bug. So whichever close (owner or the last importer) observes
// "owner closed AND importers == 0" claims Buffer under mem.Mux (takes it, sets
// it nil) and pools it; the Buffer != nil claim guard makes that exactly-once
// regardless of close order or concurrency. mem.Mux also serializes this
// against a concurrent resolveImports (which refuses to increment once
// ownerClosed is set -- see resolveImports and TestMemoryPool_ImportAfterOwnerClosed_Errors).
//
// Shared (memSec.IsShared, i.e. the wasm threads proposal's shared memory)
// and custom-allocator (experimental.MemoryAllocator, tracked via expBuffer)
// memories never go through this pool at all -- see NewMemoryInstance and
// ensureResourcesClosed, which gate pooling to exactly the plain
// make([]byte, minBytes, capBytes) case.
//
// Every buffer handed out by getPooledMemoryBuffer is fully zeroed across its
// entire capacity (not just the requested length) before being returned,
// because wasm linear memory MUST start all-zero, and MemoryInstance.Grow can
// later re-slice into previously-hidden capacity without any further
// zeroing pass of its own (see Grow's "we already have the capacity we
// need" branch).
var memoryBufferPools sync.Map // map[uint64]*sync.Pool, keyed by cap(buffer) in bytes.

// getPooledMemoryBuffer returns a zeroed buffer with cap() == capBytes from
// the pool, or nil if none is available -- the caller should fall back to
// make([]byte, ...) in that case.
func getPooledMemoryBuffer(capBytes uint64) []byte {
	if capBytes == 0 {
		return nil
	}
	v, ok := memoryBufferPools.Load(capBytes)
	if !ok {
		return nil
	}
	got := v.(*sync.Pool).Get()
	if got == nil {
		return nil
	}
	buf := *got.(*[]byte)
	// Linear memory must start all-zero: clear the whole capacity, not just
	// the length, since Grow can later expose more of it without its own
	// zeroing pass.
	clear(buf[:cap(buf)])
	return buf
}

// putPooledMemoryBuffer returns buf to the pool, bucketed by its capacity,
// for reuse by a future MemoryInstance of the same shape. See the package
// doc above for the safety argument the caller (ensureResourcesClosed) must
// uphold before calling this.
func putPooledMemoryBuffer(buf []byte) {
	capBytes := uint64(cap(buf))
	if capBytes == 0 {
		return
	}
	v, _ := memoryBufferPools.LoadOrStore(capBytes, &sync.Pool{})
	// Store *[]byte, not []byte: sync.Pool.Put([]byte) boxes the slice header
	// into interface{}, which heap-allocates it (staticcheck SA6002) -- the
	// opposite of what a pool that exists to avoid allocations wants.
	full := buf[:cap(buf)]
	v.(*sync.Pool).Put(&full)
}
