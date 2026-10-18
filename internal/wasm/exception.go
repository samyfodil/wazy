package wasm

import "unsafe"

// Exception represents a thrown WebAssembly exception.
type Exception struct {
	// obj is this exception's object on the store's managed heap, which every exnref naming it points at.
	// It comes first so that Lookup can get from the object back to the exception, and lives here rather than
	// as a field of GCObject so that struct and array objects stay in their size class. Its Type is nil,
	// which is how an exception object is told from a struct or an array.
	obj GCObject
	// Tag is the tag instance that was thrown.
	Tag *TagInstance
	// Params holds the argument values matching the tag's function type params.
	Params []uint64
}

// ExnRefNull is the exnref a `ref.null exn` produces, and never names an
// exception.
const ExnRefNull uint64 = 0

// ExceptionTable hands out the exnref values guest code holds.
//
// # Why a handle
//
// An exnref must not be a raw pointer to the Exception. The value is opaque to
// the guest, but it is a Go heap address the collector cannot see: nothing in
// wasm memory, in a wasm local, or in an exnref-typed global roots the object
// it points at. A guest that keeps an exnref past the point where the runtime
// stops naming the exception would then hold a dangling pointer, and
// `throw_ref` on it would read -- and the throw path write -- reclaimed Go
// heap. That is https://github.com/tetratelabs/wazero/issues/2522, which wazy
// shared: with the pointer form, a guest that caught by reference, threw
// again, and re-threw the first reference read the wrong tag or crashed the
// process, reproducibly.
//
// # Lifetime
//
// An exnref is a handle on the store's managed heap (GCHeap), the same kind of
// handle a struct or an array has, so the store's collector decides when the
// exception it names can go: it stays while any wasm stack, global, table, GC
// object field, or the params of another live exception holds the handle, and
// is reclaimed once none does. Exceptions are garbage-collected values in the
// spec, and before this a store kept every one it ever handed out until it was
// closed -- two entries for every throw a guest caught, which a long-running
// C++ guest turns into an unbounded leak.
//
// An exception object's fields are its params, so whatever references the
// exception carries -- a struct, another exnref -- stay alive with it.
//
// The handle carries its slot's generation, so one kept past a collection that
// reclaimed it names nothing, and `throw_ref` on it traps rather than reaching
// whatever took the slot over. Only wasm can keep an exnref alive: one handed
// to the host outlives nothing.
type ExceptionTable struct {
	s *Store
}

// Ref returns a new exnref naming exn. It may ask the store for a collection,
// which happens at the next safepoint, never here.
func (t ExceptionTable) Ref(exn *Exception) uint64 {
	exn.obj.Fields = exn.Params
	ref, collect := t.s.GC.Alloc(&exn.obj)
	if collect {
		t.s.RequestGC()
	}
	return ref
}

// Lookup returns the exception an exnref names, or false if it names none.
// Guest code cannot forge an exnref -- there is no instruction converting an
// integer to one -- but a handle from a different Store, or one handed to the
// host and since reclaimed, would land here, and must trap rather than resolve.
func (t ExceptionTable) Lookup(ref uint64) (*Exception, bool) {
	if ref == ExnRefNull {
		return nil, false
	}
	s, ok := t.s.GC.slotOf(ref)
	if !ok || s.obj.Type != nil {
		return nil, false
	}
	return (*Exception)(unsafe.Pointer(s.obj)), true
}
