package wasm

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/internalapi"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

const (
	// MemoryPageSize is the unit of memory length in WebAssembly,
	// and is defined as 2^16 = 65536.
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#memory-instances%E2%91%A0
	MemoryPageSize = uint32(65536)
	// MemoryLimitPages is maximum number of pages defined (2^16).
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#grow-mem
	MemoryLimitPages = uint32(65536)
	// MemoryPageSizeInBits satisfies the relation: "1 << MemoryPageSizeInBits == MemoryPageSize".
	MemoryPageSizeInBits = 16
)

// compile-time check to ensure MemoryInstance implements api.Memory
var _ api.Memory = &MemoryInstance{}

type waiters struct {
	mux sync.Mutex
	l   *list.List
}

// MemoryInstance represents a memory instance in a store, and implements api.Memory.
//
// Note: In WebAssembly 1.0 (20191205), there may be up to one Memory per store, which means the precise memory is always
// wasm.Store Memories index zero: `store.Memories[0]`
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#memory-instances%E2%91%A0.
type MemoryInstance struct {
	internalapi.WazyOnlyType

	Buffer        []byte
	Min, Cap, Max uint32
	Shared        bool
	// nativeGrowCap is the byte length that the native engine may expose
	// without calling back into Go. It is zero for shared and custom-allocator
	// memories, whose growth must always use Grow.
	nativeGrowCap uint64
	// growReservePages is the configured spare capacity to allocate after a Go
	// fallback grows this memory's backing buffer.
	growReservePages uint32
	// definition is known at compile time.
	definition api.MemoryDefinition

	// Mux is used in interpreter mode to prevent overlapping calls to atomic instructions,
	// introduced with WebAssembly threads proposal, and in compiler mode to make memory modifications
	// within Grow non-racy for the Go race detector.
	Mux sync.Mutex

	// waiters implements atomic wait and notify. It is implemented similarly to golang.org/x/sync/semaphore,
	// with a fixed weight of 1 and no spurious notifications.
	waiters sync.Map

	// ownerModuleEngine is the module engine that owns this memory instance.
	ownerModuleEngine ModuleEngine

	expBuffer api.LinearMemory

	// importers counts the live ModuleInstances -- other than the owner --
	// that have imported this memory via cross-module resolution (see store.go's
	// resolveImports, ExternTypeMemory case); each decrements it on its own
	// Close. Guarded by Mux. (imported == importers > 0.)
	//
	// ownerClosed is true once the owning module's ensureResourcesClosed has
	// run. Guarded by Mux.
	//
	// Together these let the owner's Close, an importer's Close, and a
	// concurrent resolveImports agree, race-free, on when it is safe to return
	// Buffer to the linear-memory buffer pool (memory_pool.go): only once the
	// owner has closed AND importers has fallen to 0. Whichever close observes
	// that last claims Buffer under Mux (sets it nil) and pools it, so it is
	// recycled exactly once. See memory_pool.go's doc for the full argument.
	importers   int
	ownerClosed bool

	// poolable is set once at construction: true exactly for a plain
	// make([]byte)/pooled buffer (allocator == nil, not shared), the only kind
	// that goes through the buffer pool. It is immutable, so both an owner's and
	// an importer's Close read it without the Mux -- and it stays mutually
	// exclusive with expBuffer (allocator-backed) and Shared memories, which are
	// never pooled.
	poolable bool
}

// NewMemoryInstance creates a new instance based on the parameters in the SectionIDMemory.
func NewMemoryInstance(memSec *Memory, allocator api.MemoryAllocator, moduleEngine ModuleEngine) *MemoryInstance {
	minBytes := MemoryPagesToBytesNum(memSec.Min)
	capBytes := MemoryPagesToBytesNum(memSec.Cap)
	maxBytes := MemoryPagesToBytesNum(memSec.Max)

	var buffer []byte
	var expBuffer api.LinearMemory
	if allocator != nil {
		expBuffer = allocator.Allocate(capBytes, maxBytes)
		buffer = expBuffer.Reallocate(minBytes)
		_ = buffer[:minBytes] // Bounds check that the minimum was allocated.
	} else if memSec.IsShared {
		// Shared memory needs a fixed buffer, so allocate with the maximum size.
		//
		// The rationale as to why we can simply use make([]byte) to a fixed buffer is that Go's GC is non-relocating.
		// That is not a part of Go spec, but is well-known thing in Go community (wazy's compiler heavily relies on it!)
		// 	* https://github.com/go4org/unsafe-assume-no-moving-gc
		//
		// Also, allocating Max here isn't harmful as the Go runtime uses mmap for large allocations, therefore,
		// the memory buffer allocation here is virtual and doesn't consume physical memory until it's used.
		// 	* https://github.com/golang/go/blob/go1.24.0/src/runtime/malloc.go#L1059
		buffer = make([]byte, minBytes, maxBytes)
	} else if pooled := getPooledMemoryBuffer(capBytes); pooled != nil {
		buffer = pooled[:minBytes]
	} else {
		buffer = make([]byte, minBytes, capBytes)
	}
	poolable := allocator == nil && !memSec.IsShared
	growReservePages := uint32(0)
	if memSec.Cap > memSec.Min {
		growReservePages = memSec.Cap - memSec.Min
	}
	nativeGrowCap := uint64(0)
	if poolable {
		nativeGrowCap = MemoryPagesToBytesNum(memoryBytesNumToPages(uint64(cap(buffer))))
	}
	return &MemoryInstance{
		Buffer:            buffer,
		Min:               memSec.Min,
		Cap:               memoryBytesNumToPages(uint64(cap(buffer))),
		Max:               memSec.Max,
		Shared:            memSec.IsShared,
		nativeGrowCap:     nativeGrowCap,
		growReservePages:  growReservePages,
		expBuffer:         expBuffer,
		ownerModuleEngine: moduleEngine,
		// Only a plain make([]byte)/pooled buffer is pool-eligible: not a custom
		// allocator's buffer (expBuffer) and not shared (fixed max buffer).
		poolable: poolable,
	}
}

// Definition implements the same method as documented on api.Memory.
func (m *MemoryInstance) Definition() api.MemoryDefinition {
	return m.definition
}

// Size implements the same method as documented on api.Memory.
func (m *MemoryInstance) Size() uint32 {
	return uint32(len(m.Buffer))
}

// ReadByte implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadByte(offset uint32) (byte, bool) {
	if !m.hasSize(offset, 1) {
		return 0, false
	}
	return m.Buffer[offset], true
}

// ReadUint16Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadUint16Le(offset uint32) (uint16, bool) {
	if !m.hasSize(offset, 2) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(m.Buffer[offset : offset+2]), true
}

// ReadUint32Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadUint32Le(offset uint32) (uint32, bool) {
	return m.readUint32Le(offset)
}

// ReadFloat32Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadFloat32Le(offset uint32) (float32, bool) {
	v, ok := m.readUint32Le(offset)
	if !ok {
		return 0, false
	}
	return math.Float32frombits(v), true
}

// ReadUint64Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadUint64Le(offset uint32) (uint64, bool) {
	return m.readUint64Le(offset)
}

// ReadFloat64Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) ReadFloat64Le(offset uint32) (float64, bool) {
	v, ok := m.readUint64Le(offset)
	if !ok {
		return 0, false
	}
	return math.Float64frombits(v), true
}

// Read implements the same method as documented on api.Memory.
func (m *MemoryInstance) Read(offset, byteCount uint32) ([]byte, bool) {
	if !m.hasSize(offset, uint64(byteCount)) {
		return nil, false
	}
	return m.Buffer[offset : offset+byteCount : offset+byteCount], true
}

// WriteByte implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteByte(offset uint32, v byte) bool {
	if !m.hasSize(offset, 1) {
		return false
	}
	m.Buffer[offset] = v
	return true
}

// WriteUint16Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteUint16Le(offset uint32, v uint16) bool {
	if !m.hasSize(offset, 2) {
		return false
	}
	binary.LittleEndian.PutUint16(m.Buffer[offset:], v)
	return true
}

// WriteUint32Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteUint32Le(offset, v uint32) bool {
	return m.writeUint32Le(offset, v)
}

// WriteFloat32Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteFloat32Le(offset uint32, v float32) bool {
	return m.writeUint32Le(offset, math.Float32bits(v))
}

// WriteUint64Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteUint64Le(offset uint32, v uint64) bool {
	return m.writeUint64Le(offset, v)
}

// WriteFloat64Le implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteFloat64Le(offset uint32, v float64) bool {
	return m.writeUint64Le(offset, math.Float64bits(v))
}

// Write implements the same method as documented on api.Memory.
func (m *MemoryInstance) Write(offset uint32, val []byte) bool {
	if !m.hasSize(offset, uint64(len(val))) {
		return false
	}
	copy(m.Buffer[offset:], val)
	return true
}

// WriteString implements the same method as documented on api.Memory.
func (m *MemoryInstance) WriteString(offset uint32, val string) bool {
	if !m.hasSize(offset, uint64(len(val))) {
		return false
	}
	copy(m.Buffer[offset:], val)
	return true
}

// MemoryPagesToBytesNum converts the given pages into the number of bytes contained in these pages.
func MemoryPagesToBytesNum(pages uint32) (bytesNum uint64) {
	return uint64(pages) << MemoryPageSizeInBits
}

// Grow implements the same method as documented on api.Memory.
func (m *MemoryInstance) Grow(delta uint32) (result uint32, ok bool) {
	if m.Shared {
		m.Mux.Lock()
		defer m.Mux.Unlock()
	}

	currentPages := m.Pages()
	if delta == 0 {
		return currentPages, true
	}

	newPages := currentPages + delta
	if newPages > m.Max || int32(delta) < 0 {
		return 0, false
	} else if m.expBuffer != nil {
		buffer := m.expBuffer.Reallocate(MemoryPagesToBytesNum(newPages))
		if buffer == nil {
			// Allocator failed to grow.
			return 0, false
		}
		if m.Shared {
			if unsafe.SliceData(buffer) != unsafe.SliceData(m.Buffer) {
				panic("shared memory cannot move, this is a bug in the memory allocator")
			}
			// We assume grow is called under a guest lock.
			// But the memory length is accessed elsewhere,
			// so use atomic to make the new length visible across threads.
			atomicStoreLengthAndCap(&m.Buffer, uintptr(len(buffer)), uintptr(cap(buffer)))
			m.Cap = memoryBytesNumToPages(uint64(cap(buffer)))
		} else {
			m.Buffer = buffer
			m.Cap = newPages
		}
	} else if newPages > m.Cap { // grow the memory.
		if m.Shared {
			panic("shared memory cannot be grown, this is a bug in wazy")
		}
		if m.growReservePages == 0 {
			m.Buffer = append(m.Buffer, make([]byte, MemoryPagesToBytesNum(delta))...)
		} else {
			newCapPages := newPages
			if m.growReservePages >= m.Max-newPages {
				newCapPages = m.Max
			} else {
				newCapPages += m.growReservePages
			}
			newLenBytes := MemoryPagesToBytesNum(newPages)
			newCapBytes := MemoryPagesToBytesNum(newCapPages)
			if newCapBytesInt := int(newCapBytes); newCapBytesInt >= 0 && uint64(newCapBytesInt) == newCapBytes {
				m.Buffer = slices.Grow(m.Buffer, newCapBytesInt-len(m.Buffer))
				m.Buffer = m.Buffer[:newLenBytes]
			} else {
				// A 32-bit host cannot represent the requested reserve. Preserve
				// the existing growth behavior, which will fail naturally if even
				// the new logical size cannot fit in an int-sized slice.
				m.Buffer = append(m.Buffer, make([]byte, MemoryPagesToBytesNum(delta))...)
			}
		}
		// Record all complete pages that can be exposed without another
		// allocation. With an explicit reserve this is newPages+reserve; without
		// one it includes any spare capacity chosen by append. The unexposed
		// capacity is known-zero because it came from Go's allocator.
		m.Cap = min(memoryBytesNumToPages(uint64(cap(m.Buffer))), m.Max)
		m.nativeGrowCap = MemoryPagesToBytesNum(m.Cap)
	} else { // We already have the capacity we need.
		if m.Shared {
			// We assume grow is called under a guest lock.
			// But the memory length is accessed elsewhere,
			// so use atomic to make the new length visible across threads.
			atomicStoreLength(&m.Buffer, uintptr(MemoryPagesToBytesNum(newPages)))
		} else {
			m.Buffer = m.Buffer[:MemoryPagesToBytesNum(newPages)]
		}
	}
	m.ownerModuleEngine.MemoryGrown()
	return currentPages, true
}

// Pages implements the same method as documented on api.Memory.
func (m *MemoryInstance) Pages() (result uint32) {
	return memoryBytesNumToPages(uint64(len(m.Buffer)))
}

// PagesToUnitOfBytes converts the pages to a human-readable form similar to what's specified. e.g. 1 -> "64Ki"
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#memory-instances%E2%91%A0
func PagesToUnitOfBytes(pages uint32) string {
	k := pages * 64
	if k < 1024 {
		return fmt.Sprintf("%d Ki", k)
	}
	m := k / 1024
	if m < 1024 {
		return fmt.Sprintf("%d Mi", m)
	}
	g := m / 1024
	if g < 1024 {
		return fmt.Sprintf("%d Gi", g)
	}
	return fmt.Sprintf("%d Ti", g/1024)
}

// Below are raw functions used to implement the api.Memory API:

// sliceHeader mirrors the memory layout of a Go slice ([]byte). It is used to
// atomically overwrite the length and capacity words of a live slice in place,
// which unsafe.Slice cannot express (it only builds a fresh slice value). Len
// and Cap are declared as uintptr (the same width as the slice header's int
// fields) so their addresses can be handed directly to sync/atomic. The Data
// field is unsafe.Pointer, not uintptr, so the garbage collector keeps
// tracking the backing array while the length/cap words are updated.
type sliceHeader struct {
	Data unsafe.Pointer
	Len  uintptr
	Cap  uintptr
}

// Uses atomic write to update the length of a slice.
func atomicStoreLengthAndCap(slice *[]byte, length uintptr, cap uintptr) {
	slicePtr := (*sliceHeader)(unsafe.Pointer(slice))
	atomic.StoreUintptr(&slicePtr.Cap, cap)
	atomic.StoreUintptr(&slicePtr.Len, length)
}

// Uses atomic write to update the length of a slice.
func atomicStoreLength(slice *[]byte, length uintptr) {
	slicePtr := (*sliceHeader)(unsafe.Pointer(slice))
	atomic.StoreUintptr(&slicePtr.Len, length)
}

// memoryBytesNumToPages converts the given number of bytes into the number of pages.
func memoryBytesNumToPages(bytesNum uint64) (pages uint32) {
	return uint32(bytesNum >> MemoryPageSizeInBits)
}

// MemoryInstanceNativeGrowCapOffset returns the byte offset of nativeGrowCap
// for the native compiler's generated memory.grow fast path.
func MemoryInstanceNativeGrowCapOffset() uint32 {
	return uint32(unsafe.Offsetof(MemoryInstance{}.nativeGrowCap))
}

// hasSize returns true if Len is sufficient for byteCount at the given offset.
//
// Note: This is always fine, because memory can grow, but never shrink.
func (m *MemoryInstance) hasSize(offset uint32, byteCount uint64) bool {
	return uint64(offset)+byteCount <= uint64(len(m.Buffer)) // uint64 prevents overflow on add
}

// readUint32Le implements ReadUint32Le without using a context. This is extracted as both ints and floats are stored in
// memory as uint32le.
func (m *MemoryInstance) readUint32Le(offset uint32) (uint32, bool) {
	if !m.hasSize(offset, 4) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(m.Buffer[offset:]), true
}

// readUint64Le implements ReadUint64Le without using a context. This is extracted as both ints and floats are stored in
// memory as uint64le.
func (m *MemoryInstance) readUint64Le(offset uint32) (uint64, bool) {
	if !m.hasSize(offset, 8) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(m.Buffer[offset:]), true
}

// writeUint32Le implements WriteUint32Le without using a context. This is extracted as both ints and floats are stored
// in memory as uint32le.
func (m *MemoryInstance) writeUint32Le(offset uint32, v uint32) bool {
	if !m.hasSize(offset, 4) {
		return false
	}
	binary.LittleEndian.PutUint32(m.Buffer[offset:], v)
	return true
}

// writeUint64Le implements WriteUint64Le without using a context. This is extracted as both ints and floats are stored
// in memory as uint64le.
func (m *MemoryInstance) writeUint64Le(offset uint32, v uint64) bool {
	if !m.hasSize(offset, 8) {
		return false
	}
	binary.LittleEndian.PutUint64(m.Buffer[offset:], v)
	return true
}

// Wait32 suspends the caller until the offset is notified by a different agent.
func (m *MemoryInstance) Wait32(offset uint32, exp uint32, timeout int64, reader func(mem *MemoryInstance, offset uint32) uint32) uint64 {
	w := m.getWaiters(offset)
	w.mux.Lock()

	cur := reader(m, offset)
	if cur != exp {
		w.mux.Unlock()
		return 1
	}

	return m.wait(w, timeout)
}

// Wait64 suspends the caller until the offset is notified by a different agent.
func (m *MemoryInstance) Wait64(offset uint32, exp uint64, timeout int64, reader func(mem *MemoryInstance, offset uint32) uint64) uint64 {
	w := m.getWaiters(offset)
	w.mux.Lock()

	cur := reader(m, offset)
	if cur != exp {
		w.mux.Unlock()
		return 1
	}

	return m.wait(w, timeout)
}

func (m *MemoryInstance) wait(w *waiters, timeout int64) uint64 {
	if w.l == nil {
		w.l = list.New()
	}

	// The specification requires a trap if the number of existing waiters + 1 == 2^32, so we add a check here.
	// In practice, it is unlikely the application would ever accumulate such a large number of waiters as it
	// indicates several GB of RAM used just for the list of waiters.
	// https://github.com/WebAssembly/threads/blob/main/proposals/threads/Overview.md#wait
	if uint64(w.l.Len()+1) == 1<<32 {
		w.mux.Unlock()
		panic(wasmruntime.ErrRuntimeTooManyWaiters)
	}

	ready := make(chan struct{})
	elem := w.l.PushBack(ready)
	w.mux.Unlock()

	if timeout < 0 {
		<-ready
		return 0
	} else {
		select {
		case <-ready:
			return 0
		case <-time.After(time.Duration(timeout)):
			// While we could see if the channel completed by now and ignore the timeout, similar to x/sync/semaphore,
			// the Wasm spec doesn't specify this behavior, so we keep things simple by prioritizing the timeout.
			w.mux.Lock()
			w.l.Remove(elem)
			w.mux.Unlock()
			return 2
		}
	}
}

func (m *MemoryInstance) getWaiters(offset uint32) *waiters {
	wAny, ok := m.waiters.Load(offset)
	if !ok {
		// The first time an address is waited on, simultaneous waits will cause extra allocations.
		// Further operations will be loaded above, which is also the general pattern of usage with
		// mutexes.
		wAny, _ = m.waiters.LoadOrStore(offset, &waiters{})
	}

	return wAny.(*waiters)
}

// Notify wakes up at most count waiters at the given offset.
func (m *MemoryInstance) Notify(offset uint32, count uint32) uint32 {
	wAny, ok := m.waiters.Load(offset)
	if !ok {
		return 0
	}
	w := wAny.(*waiters)

	w.mux.Lock()
	defer w.mux.Unlock()
	if w.l == nil {
		return 0
	}

	res := uint32(0)
	for num := w.l.Len(); num > 0 && res < count; num = w.l.Len() {
		w := w.l.Remove(w.l.Front()).(chan struct{})
		close(w)
		res++
	}

	return res
}
