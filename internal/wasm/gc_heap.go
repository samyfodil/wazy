package wasm

import (
	"sync"

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
//	bit 62 set             a 1-based index into GCHeap.live (any hierarchy only)
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
	// Fields holds a struct's fields or an array's elements, one uint64 each. A packed (i8/i16) field is
	// stored truncated and widened on read, per the storage type -- see GCObject.Get.
	Fields []uint64
}

// IsArray reports whether this object is an array instance.
func (o *GCObject) IsArray() bool { return o.Type.CompositeKind == CompositeKindArray }

// storageType returns the storage type of field i: for an array, every element shares Fields[0]'s.
func (o *GCObject) storageType(i int) ValueType {
	if o.IsArray() {
		return o.Type.Fields[0].Type
	}
	return o.Type.Fields[i].Type
}

// Get reads field or element i, widening a packed value per signed. A non-packed field ignores signed, as the
// spec requires struct.get (no suffix) for those and struct.get_s/_u for packed ones.
func (o *GCObject) Get(i int, signed bool) uint64 {
	v := o.Fields[i]
	switch o.storageType(i) {
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

// Set writes field or element i, truncating to the storage type so a later signed read widens from the right
// bit. Storing the truncated form is what makes Get's widening the only place packing is thought about.
func (o *GCObject) Set(i int, v uint64) {
	switch o.storageType(i) {
	case ValueTypeI8:
		v = uint64(uint8(v))
	case ValueTypeI16:
		v = uint64(uint16(v))
	}
	o.Fields[i] = v
}

// GCHeap hands out the references guest code holds for struct and array instances.
//
// It lives on the Store because a reference crosses module instances, exactly as an exnref does.
//
// ponytail: entries are held until the Store is closed. Reclamation is the separate half of this proposal --
// it needs roots from every engine's live value stacks, not just the Go-visible globals and tables -- and
// until it lands a program that allocates in a loop grows this table without bound.
type GCHeap struct {
	mu sync.RWMutex
	// live is indexed by reference-1, so that the zero reference stays GCRefNull.
	live []*GCObject
}

// Alloc returns the reference naming o.
func (h *GCHeap) Alloc(o *GCObject) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live = append(h.live, o)
	return gcRefHeapTag | uint64(len(h.live)) // index+1, so the first is tagged 1, never GCRefNull
}

// Deref returns the object a reference names, trapping on null. It panics rather than returning an error
// because every caller is executing guest code, where an unusable reference is a trap.
func (h *GCHeap) Deref(ref uint64) *GCObject {
	if ref == GCRefNull {
		panic(wasmruntime.ErrRuntimeNullReference)
	}
	if !IsGCHeapRef(ref) {
		// An i31, or a value the embedder handed over: neither is a struct or an array, and validation
		// keeps both away from every instruction that dereferences one.
		panic(wasmruntime.ErrRuntimeCastFailure)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	i := ref&^gcRefHeapTag - 1
	if i >= uint64(len(h.live)) {
		panic(wasmruntime.ErrRuntimeCastFailure)
	}
	return h.live[i]
}

// TypeIDOf returns the store-wide type of a non-null, non-i31 reference in the any hierarchy.
func (h *GCHeap) TypeIDOf(ref uint64) (FunctionTypeID, bool) {
	if !IsGCHeapRef(ref) {
		return 0, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	i := ref&^gcRefHeapTag - 1
	if i >= uint64(len(h.live)) {
		return 0, false
	}
	return h.live[i].TypeID, true
}
