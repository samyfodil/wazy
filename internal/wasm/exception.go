package wasm

import "sync"

// Exception represents a thrown WebAssembly exception.
type Exception struct {
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
// # Why a table
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
// Handing out an index into this table instead keeps the Exception rooted for
// as long as any exnref can name it, so guest code can never dereference freed
// memory, and an unknown handle traps instead of being followed.
//
// # Lifetime
//
// The table lives on the Store, because an exnref crosses module instances:
// an exception thrown in a callee is caught, and can be re-thrown, in a
// caller from another module. Entries are held until the Store is closed --
// exnrefs are garbage-collected values in the spec, and wazy has no way to
// observe a guest dropping one, so the alternative to holding them is exactly
// the dangling pointer this replaces. Only `catch_ref`/`catch_all_ref` and a
// `throw` reaching guest code allocate an entry; a plain `catch` never does.
type ExceptionTable struct {
	mu sync.RWMutex
	// live is indexed by exnref-1, so that the zero exnref is ExnRefNull.
	live []*Exception
}

// Ref returns the exnref naming exn, allocating one if it has none yet.
func (t *ExceptionTable) Ref(exn *Exception) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.live = append(t.live, exn)
	return uint64(len(t.live)) // index+1, so the first is 1, not ExnRefNull
}

// Lookup returns the exception an exnref names, or false if it names none.
// Guest code cannot forge an exnref -- there is no instruction converting an
// integer to one -- but a handle from a different Store would land here, and
// must trap rather than resolve.
func (t *ExceptionTable) Lookup(ref uint64) (*Exception, bool) {
	if ref == ExnRefNull {
		return nil, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if ref > uint64(len(t.live)) {
		return nil, false
	}
	return t.live[ref-1], true
}
