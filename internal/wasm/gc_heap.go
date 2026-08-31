package wasm

import (
	"sync"
	"sync/atomic"

	"github.com/samyfodil/wazy/internal/wasmruntime"
)

// This file holds the managed heap of the GC proposal: what a struct or array instance is, and how guest code
// names one.
//
// # How a reference is spelled
//
// Both engines carry a reference as an opaque uint64. Which hierarchy a given one belongs to is settled
// statically -- validation never lets a value cross between func, extern, exn and any -- so the same word can
// mean different things in different hierarchies without ambiguity:
//
//	0                      null, in every hierarchy
//	bit 63 set             an i31: the 31-bit payload sits in the low bits (any hierarchy only)
//	bit 62 set             a heap handle: a 1-based slot index in the low 32 bits and that slot's
//	                       generation in bits 32..61 (any hierarchy only)
//	otherwise              a pointer: the engine's function instance in the func hierarchy, and whatever
//	                       the embedder handed over in the extern hierarchy
//
// The two tags are what keep `any.convert_extern` honest. That instruction moves an embedder's externref into
// the any hierarchy without changing the word, so an untagged non-null value can turn up where a struct or an
// array is being tested for -- and a host pointer must not be mistaken for a heap index. Tagging the heap side
// rather than the host side is the only choice available: the embedder's pointer is not ours to encode.
//
// The index is what keeps this sound. A raw Go pointer parked in a []uint64 is invisible to Go's collector:
// nothing in a wasm local, a wasm global or a wasm table roots the object it points at, so the guest would be
// holding a dangling pointer as soon as the last Go reference went away. That is the same reasoning, and the
// same bug (https://github.com/tetratelabs/wazero/issues/2522), that put exnrefs behind ExceptionTable.

const (
	// GCRefNull is the reference every `ref.null` in the any hierarchy produces.
	GCRefNull uint64 = 0
	// gcRefI31Tag marks a reference as an i31 rather than an index into the heap.
	gcRefI31Tag uint64 = 1 << 63
	// gcRefHeapTag marks a reference as an index into GCHeap.live.
	gcRefHeapTag uint64 = 1 << 62
	// gcRefI31Mask is the payload of an i31 reference.
	gcRefI31Mask uint64 = (1 << 31) - 1
	// gcRefIndexMask is the slot index of a heap handle, and gcRefGenShift where its generation starts.
	gcRefIndexMask uint64 = (1 << 32) - 1
	gcRefGenShift         = 32
	// gcRefGenMask bounds the generation, which wraps rather than colliding with the two tag bits.
	gcRefGenMask uint64 = (1 << 30) - 1
)

// EncodeI31 returns the i31 reference holding the low 31 bits of v. It is never GCRefNull, because the tag bit
// is always set -- `ref.i31 (i32.const 0)` is a real reference, not null.
func EncodeI31(v uint32) uint64 { return gcRefI31Tag | (uint64(v) & gcRefI31Mask) }

// IsI31 reports whether a reference is an i31 rather than a heap object.
func IsI31(ref uint64) bool { return ref&gcRefI31Tag != 0 }

// IsGCHeapRef reports whether a reference names a struct or array instance on the managed heap. A non-null
// reference in the any hierarchy that is neither this nor an i31 came from the embedder through
// any.convert_extern, and there is nothing wazy can say about it beyond "it is a reference".
func IsGCHeapRef(ref uint64) bool { return ref&gcRefI31Tag == 0 && ref&gcRefHeapTag != 0 }

// DecodeI31S returns the signed value of an i31 reference, sign-extended from bit 30.
func DecodeI31S(ref uint64) uint32 {
	v := uint32(ref & gcRefI31Mask)
	if v&(1<<30) != 0 {
		v |= ^uint32(gcRefI31Mask)
	}
	return v
}

// DecodeI31U returns the unsigned value of an i31 reference.
func DecodeI31U(ref uint64) uint32 { return uint32(ref & gcRefI31Mask) }

// GCObject is one struct or array instance on the managed heap.
type GCObject struct {
	// Type is the defined type this was allocated from, which says whether Fields are struct fields or
	// array elements and what storage type each has.
	Type *FunctionType
	// TypeID is Type's store-wide identity, which is what ref.test and ref.cast compare against.
	TypeID FunctionTypeID
	// Fields holds a struct's fields or an array's elements, flattened into words. Every storage type takes
	// one word except v128, which takes two -- see FunctionType.FieldSlots for a struct's layout and
	// ElemSlots for an array's. A packed (i8/i16) field is stored truncated and widened on read; see Get.
	Fields []uint64
}

// gcInlineSlots is how many words ride in the object's own allocation, so a small struct or array costs one Go
// allocation rather than two. Three is where the header and its words still land in the 64-byte size class,
// which is what the header plus a separate backing already cost for two.
const gcInlineSlots = 3

// gcInlineObject is a header and its words in one allocation. See newGCObject.
type gcInlineObject struct {
	GCObject
	words [gcInlineSlots]uint64
}

// newGCObject returns a zeroed instance of def with n words of storage, in one allocation when it fits.
func newGCObject(def *FunctionType, id FunctionTypeID, n int) *GCObject {
	if n <= gcInlineSlots {
		o := &gcInlineObject{GCObject: GCObject{Type: def, TypeID: id}}
		// An interior pointer keeps the whole allocation alive, so Fields may name the tail of it.
		o.Fields = o.words[:n:n]
		return &o.GCObject
	}
	return &GCObject{Type: def, TypeID: id, Fields: make([]uint64, n)}
}

// ElemSlots is how many words one element of an array occupies, and is 1 for a struct. It is derived from the
// type rather than stored, so a hand-built GCObject cannot get it wrong.
func (o *GCObject) ElemSlots() uint32 {
	if o.IsArray() {
		return SlotsForStorageType(o.Type.Fields[0].Type)
	}
	return 1
}

// Len returns the number of elements in an array, which is not the number of words when the element is a
// vector.
func (o *GCObject) Len() int { return len(o.Fields) / int(o.ElemSlots()) }

// IsArray reports whether this object is an array instance.
func (o *GCObject) IsArray() bool { return o.Type.CompositeKind == CompositeKindArray }

// slotStorage returns the storage type of the value occupying a word. For an array every element shares one;
// for a struct the word is the field's own index, since a vector field is the only thing that makes a field
// span two words. A vector's two words both answer v128, which is what keeps the packing below from ever
// looking at one.
func (o *GCObject) slotStorage(slot int) ValueType {
	fields := o.Type.Fields
	if o.IsArray() {
		return fields[0].Type
	}
	// A type built without CacheFieldSlots has no vector fields to lay out, so a slot is a field there too.
	if slots := o.Type.FieldSlots; slots != nil && int(slots[len(fields)]) != len(fields) {
		return o.Type.vectorSlotStorage(slot)
	}
	if slot < len(fields) {
		return fields[slot].Type
	}
	return ValueTypeI64
}

// vectorSlotStorage maps a word back through the slot layout, for the one shape slotStorage cannot answer by
// indexing: a struct with a vector field.
func (t *FunctionType) vectorSlotStorage(slot int) ValueType {
	for i := range t.Fields {
		if uint32(slot) < t.FieldSlots[i+1] {
			return t.Fields[i].Type
		}
	}
	return ValueTypeI64
}

// Get reads the word at slot, widening a packed value per signed. A non-packed field ignores signed, as the
// spec requires struct.get (no suffix) for those and struct.get_s/_u for packed ones.
func (o *GCObject) Get(i int, signed bool) uint64 {
	v := o.Fields[i]
	switch o.slotStorage(i) {
	case ValueTypeI8:
		if signed {
			return uint64(uint32(int32(int8(v))))
		}
		return uint64(uint8(v))
	case ValueTypeI16:
		if signed {
			return uint64(uint32(int32(int16(v))))
		}
		return uint64(uint16(v))
	}
	return v
}

// Set writes the word at slot, truncating to the storage type so a later signed read widens from the right
// bit. Storing the truncated form is what makes Get's widening the only place packing is thought about.
func (o *GCObject) Set(i int, v uint64) {
	o.Fields[i] = packStorage(o.slotStorage(i), v)
}

// packStorage truncates a value to its storage type. See Set.
func packStorage(st ValueType, v uint64) uint64 {
	switch st {
	case ValueTypeI8:
		return uint64(uint8(v))
	case ValueTypeI16:
		return uint64(uint16(v))
	}
	return v
}

// gcSlot is one entry of the heap table.
type gcSlot struct {
	// obj is nil once the slot has been swept and is waiting to be handed out again.
	obj *GCObject
	// gen is bumped every time the slot changes hands -- once when it is swept, once when it is handed out
	// again -- so a handle the guest kept past a collection no longer matches. That is what makes slot reuse
	// safe: a stale handle names a generation that no longer exists and traps, rather than silently aliasing
	// whatever object took the slot over. A slot never handed out has generation zero, which no handle
	// carries.
	//
	// It is also what publishes obj to the lock-free read path: Alloc writes obj and then stores gen with
	// sync/atomic, so a reader that observes its own handle's generation has synchronised with that store
	// and its read of obj is ordered after the write. See slotOf.
	gen uint32
	// marked is scratch for one mark phase; see (*Store).collectGarbage.
	marked bool
}

// GCHeap hands out the references guest code holds for struct and array instances, and reclaims the ones the
// guest can no longer reach.
//
// It lives on the Store because a reference crosses module instances, exactly as an exnref does. Reclamation
// is driven from the Store as well, since the roots are spread across every module instance and every wasm
// call in flight: see (*Store).collectGarbage.
//
// It is read without a lock. Only a collection ever unpublishes a slot, and a collection runs with every
// execution that could be dereferencing one parked (see gcController), so a slot's contents are stable for the
// whole of a Deref. The one thing that does move underneath a reader is the table itself, which Alloc replaces
// with a larger copy; that is why it is behind an atomic pointer.
type GCHeap struct {
	// mu guards allocation against itself and against a collection. The read path takes nothing.
	mu sync.Mutex
	// slots is indexed by handle-1, so that the zero handle stays GCRefNull. The slice header it points at
	// is written once, before the store, so a reader that has loaded it may read it freely. Entries past
	// used are zero, and a zero generation matches no handle, so bounds-checking against the table's own
	// length is enough.
	slots atomic.Pointer[[]gcSlot]
	// used is how many entries of the table have been handed out at least once. Guarded by mu.
	used int
	// free holds the indices of swept slots, newest first.
	free []uint32
	// allocs counts allocations since the last collection, which is what triggers the next one.
	allocs int
	// nextAt is how many allocations may happen before a collection is asked for. It grows with the live
	// set so that a program with a large live heap does not collect on every allocation.
	nextAt int
}

// gcInitialThreshold is how many allocations happen before the first collection is asked for. It is small
// enough that a leak shows up in a short test and large enough that trivial programs never collect at all.
const gcInitialThreshold = 1024

// gcInitialSlots is the first table size. The table doubles from there, so growing it is amortised rather than
// once per allocation, which matters because every growth leaves the old copy behind for readers in flight.
const gcInitialSlots = 64

// table returns the slot table, which is nil until the first allocation.
func (h *GCHeap) table() []gcSlot {
	if p := h.slots.Load(); p != nil {
		return *p
	}
	return nil
}

// Alloc returns the handle naming o, and whether the heap would like a collection soon. The caller does not
// collect here: a collection has to happen where the wasm stacks are stable, which is a safepoint, not the
// middle of an instruction. See (*GCExecution).Safepoint.
func (h *GCHeap) Alloc(o *GCObject) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	slots := h.table()
	var index uint32
	if n := len(h.free); n > 0 {
		index = h.free[n-1]
		h.free = h.free[:n-1]
	} else {
		if h.used == len(slots) {
			grown := make([]gcSlot, max(2*len(slots), gcInitialSlots))
			copy(grown, slots)
			h.slots.Store(&grown)
			slots = grown
		}
		index = uint32(h.used)
		h.used++
	}
	s := &slots[index]
	s.obj = o
	gen := s.gen + 1
	// This is what publishes obj, and what makes the handle below name this object and no earlier one.
	atomic.StoreUint32(&s.gen, gen)

	h.allocs++
	if h.nextAt == 0 {
		h.nextAt = gcInitialThreshold
	}
	return handleFor(index, gen), h.allocs >= h.nextAt
}

// handleFor builds the reference naming a slot. The index is stored one-based so that slot zero is not the
// null reference.
func handleFor(index, gen uint32) uint64 {
	return gcRefHeapTag | (uint64(gen)&gcRefGenMask)<<gcRefGenShift | uint64(index) + 1
}

// slotOf resolves a handle to its slot, or false when it names none -- because it was never one, or because
// the slot has since been swept and handed out again under a new generation. The conservative scan reaches
// here with arbitrary stack words, so "names none" has to cover every shape a word can have.
//
// It takes no lock: the table pointer and the generation are both read atomically, and obj is read only once
// the generation has matched, which orders that read after the write Alloc published with it.
func (h *GCHeap) slotOf(ref uint64) (*gcSlot, bool) {
	if !IsGCHeapRef(ref) {
		return nil, false
	}
	slots := h.table()
	i := ref&gcRefIndexMask - 1
	if i >= uint64(len(slots)) {
		return nil, false
	}
	s := &slots[i]
	if uint64(atomic.LoadUint32(&s.gen))&gcRefGenMask != (ref>>gcRefGenShift)&gcRefGenMask {
		return nil, false
	}
	if s.obj == nil {
		// A word that only looks like a handle, naming a swept slot whose generation it happens to match.
		return nil, false
	}
	return s, true
}

// Deref returns the object a reference names, trapping on null. It panics rather than returning an error
// because every caller is executing guest code, where an unusable reference is a trap.
func (h *GCHeap) Deref(ref uint64) *GCObject {
	if ref == GCRefNull {
		panic(wasmruntime.ErrRuntimeNullReference)
	}
	s, ok := h.slotOf(ref)
	if !ok {
		// An i31, a value the embedder handed over, or a handle whose object has been collected: none of
		// them is a struct or an array, and validation keeps the first two away from every instruction
		// that dereferences one.
		panic(wasmruntime.ErrRuntimeCastFailure)
	}
	return s.obj
}

// TypeIDOf returns the store-wide type of a non-null, non-i31 reference in the any hierarchy.
func (h *GCHeap) TypeIDOf(ref uint64) (FunctionTypeID, bool) {
	s, ok := h.slotOf(ref)
	if !ok {
		return 0, false
	}
	return s.obj.TypeID, true
}

// Live returns how many objects the heap holds, which is what a test watches to see a collection happen.
func (h *GCHeap) Live() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.used - len(h.free)
}
