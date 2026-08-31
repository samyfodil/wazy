package wasm

import (
	"fmt"
	"os"
	"sync"
)

// reproNoMemPool disables this pool entirely when WAZY_REPRO_NO_MEMPOOL=1, so
// every linear memory gets a fresh make([]byte, ...) and no buffer is ever
// recycled. It is a discriminator for the open Windows corruption in TODOS.md.
//
// Why this pool is the suspect: it is the only place in the codebase that
// hands back a buffer another holder might still reference AND explicitly
// zeroes it (the clear below), and every corruption victim on record reads
// back as zero. It also explains the GC result -- with GOGC=off the crash rate
// is 7-8/15 and with GOGC=1 it is 0/15, which is backwards for a
// collector-driven use-after-free but exactly right here, because sync.Pool is
// DRAINED BY THE GC: no GC means maximum reuse, constant GC means Get almost
// always misses and every memory is freshly allocated.
var reproNoMemPool = os.Getenv("WAZY_REPRO_NO_MEMPOOL") == "1"

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
// and custom-allocator (api.MemoryAllocator, tracked via expBuffer)
// memories never go through this pool at all -- see NewMemoryInstance and
// ensureResourcesClosed, which gate pooling to exactly the plain
// make([]byte, minBytes, capBytes) case.
//
// Every buffer handed out by getPooledMemoryBuffer is fully zeroed across its
// entire capacity, because wasm linear memory MUST start all-zero. Only the
// logically exposed prefix can have been written, so the pool records that
// prefix in len(buf) and clears only it. The unexposed reserve remains
// known-zero without touching its lazily backed pages.
var memoryBufferPools sync.Map // map[uint64]*sync.Pool, keyed by cap(buffer) in bytes.

// getPooledMemoryBuffer returns a zeroed buffer with cap() == capBytes from
// the pool, or nil if none is available -- the caller should fall back to
// make([]byte, ...) in that case.
func getPooledMemoryBuffer(capBytes uint64) []byte {
	if capBytes == 0 || reproNoMemPool {
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
	// Only this prefix was logically visible and therefore writable. Bytes
	// above len(buf) were never addressable and remain zero by construction.
	clear(buf)
	return buf[:cap(buf)]
}

// putPooledMemoryBuffer returns buf to the pool, bucketed by its capacity, for
// reuse by a future MemoryInstance of the same shape. len(buf) must be the
// logically exposed (and potentially dirty) prefix; cap(buf) is the complete
// known-zero backing allocation. See the package doc above for the ownership
// safety argument the caller must uphold before calling this.
func putPooledMemoryBuffer(buf []byte) {
	capBytes := uint64(cap(buf))
	if capBytes == 0 || reproNoMemPool {
		return
	}
	// Load first: LoadOrStore's argument is evaluated on every call, so passing
	// &sync.Pool{} directly heap-allocates a pool per close and throws it away
	// the moment the bucket already exists, which after the first close it always does.
	v, ok := memoryBufferPools.Load(capBytes)
	if !ok {
		v, _ = memoryBufferPools.LoadOrStore(capBytes, &sync.Pool{})
	}
	// Store *[]byte, not []byte: sync.Pool.Put([]byte) boxes the slice header
	// into interface{}, which heap-allocates it (staticcheck SA6002) -- the
	// opposite of what a pool that exists to avoid allocations wants.
	v.(*sync.Pool).Put(&buf)
}

// --- reference audit (WAZY_REPRO_POOL_AUDIT=1) ---
//
// A buffer may be recycled only once every ModuleInstance that can reach it is
// gone. The code decides that with a COUNTER (mem.importers) plus
// mem.ownerClosed. A counter is precisely what a double-decrement corrupts,
// and this one has been wrong before: commit 9755f90 fixed FailIfClosed
// re-running the deferred cleanup and over-decrementing it. An over-decrement
// reaches zero early and pools a buffer a live module is still pointing at,
// and getPooledMemoryBuffer's clear() then zeroes memory that module is still
// reading -- which is exactly the corruption signature in TODOS.md's OPEN BUG,
// where every victim reads back as zero.
//
// So audit it with a SET of the live ModuleInstances holding each memory,
// rather than a count. A set is idempotent: closing the same module twice
// removes one entry, while the counter decrements twice. That makes this an
// independent check of the counter rather than a restatement of it, and it
// works on any platform, without waiting for the corruption to manifest.
var (
	reproPoolAudit = os.Getenv("WAZY_REPRO_POOL_AUDIT") == "1"
	poolAuditMu    sync.Mutex
	poolAuditRefs  = map[*MemoryInstance]map[*ModuleInstance]struct{}{}
)

// poolAuditHold records that mod can reach mem.
func poolAuditHold(mem *MemoryInstance, mod *ModuleInstance) {
	if !reproPoolAudit || mem == nil || mod == nil {
		return
	}
	poolAuditMu.Lock()
	set, ok := poolAuditRefs[mem]
	if !ok {
		set = map[*ModuleInstance]struct{}{}
		poolAuditRefs[mem] = set
	}
	set[mod] = struct{}{}
	poolAuditMu.Unlock()
}

// poolAuditRelease drops mod's hold on mem. Idempotent by construction, which
// is the whole point: a second close of the same module is a no-op here while
// it would decrement mem.importers a second time.
func poolAuditRelease(mem *MemoryInstance, mod *ModuleInstance) {
	if !reproPoolAudit || mem == nil || mod == nil {
		return
	}
	poolAuditMu.Lock()
	if set, ok := poolAuditRefs[mem]; ok {
		delete(set, mod)
	}
	poolAuditMu.Unlock()
}

// poolAuditPut panics if any module can still reach mem at the moment its
// buffer is handed back to the pool.
func poolAuditPut(mem *MemoryInstance) {
	if !reproPoolAudit || mem == nil {
		return
	}
	poolAuditMu.Lock()
	defer poolAuditMu.Unlock()
	if n := len(poolAuditRefs[mem]); n != 0 {
		panic(fmt.Sprintf("REPRO pool-audit: buffer pooled while %d module(s) can still reach it (mem.importers=%d ownerClosed=%v)",
			n, mem.importers, mem.ownerClosed))
	}
	delete(poolAuditRefs, mem)
}
