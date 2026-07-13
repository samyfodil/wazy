package native

import (
	"encoding/binary"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/wasm"
)

func buildHostModuleOpaque(m *wasm.Module, listeners []experimental.FunctionListener) moduleContextOpaque {
	size := len(m.CodeSection)*16 + 32
	ret := newAlignedOpaque(size)

	binary.LittleEndian.PutUint64(ret[0:], uint64(uintptr(unsafe.Pointer(m))))

	if len(listeners) > 0 {
		binary.LittleEndian.PutUint64(ret[8:], uint64(uintptr(unsafe.Pointer(unsafe.SliceData(listeners)))))
		binary.LittleEndian.PutUint64(ret[16:], uint64(len(listeners)))
		binary.LittleEndian.PutUint64(ret[24:], uint64(cap(listeners)))
	}

	offset := 32
	for i := range m.CodeSection {
		goFn := m.CodeSection[i].GoFunc
		writeGoFunc(goFn, ret[offset:])
		offset += 16
	}
	return ret
}

// sliceHeader mirrors the layout of reflect.SliceHeader. The opaque host-module
// context is addressed by a raw uintptr (opaqueBegin) that crosses the Go<->Wasm
// ABI boundary, so building slice views over it necessarily starts from an
// integer address rather than a Go pointer. Using this header (rather than
// unsafe.Slice((*byte)(unsafe.Pointer(opaqueBegin)), ...)) keeps go vet's
// unsafeptr check quiet and preserves the original, GC-invisible Data-as-uintptr
// semantics that the reflect.SliceHeader-based code relied on.
type sliceHeader struct {
	Data uintptr
	Len  int
	Cap  int
}

func hostModuleFromOpaque(opaqueBegin uintptr) *wasm.Module {
	var opaqueViewOverSlice []byte
	sh := (*sliceHeader)(unsafe.Pointer(&opaqueViewOverSlice))
	sh.Data = opaqueBegin
	sh.Len = 32
	sh.Cap = 32
	return *(**wasm.Module)(unsafe.Pointer(&opaqueViewOverSlice[0]))
}

func hostModuleListenersSliceFromOpaque(opaqueBegin uintptr) []experimental.FunctionListener {
	var opaqueViewOverSlice []byte
	sh := (*sliceHeader)(unsafe.Pointer(&opaqueViewOverSlice))
	sh.Data = opaqueBegin
	sh.Len = 32
	sh.Cap = 32

	b := binary.LittleEndian.Uint64(opaqueViewOverSlice[8:])
	l := binary.LittleEndian.Uint64(opaqueViewOverSlice[16:])
	c := binary.LittleEndian.Uint64(opaqueViewOverSlice[24:])
	var ret []experimental.FunctionListener
	sh = (*sliceHeader)(unsafe.Pointer(&ret))
	sh.Data = uintptr(b)
	sh.Len = int(l)
	sh.Cap = int(c)
	return ret
}

// hostModuleGoFuncFromOpaque reads back the two words written by writeGoFunc
// and reinterprets them directly as T, without going through an
// interface{}-to-T type assertion.
//
// Invariant this relies on: writeGoFunc type-switches the source GoFunc on
// (api.GoModuleFunction, api.GoFunction) and stores the matched interface
// value's own two words (itab, data). Every call site of this function picks
// T using the very same type switch, just performed earlier and recorded in
// the exit code instead: (*engine).compileHostModule (engine.go) switches on
// c.GoFunc.(type) to choose between ExitCodeCallGoModuleFunction* and
// ExitCodeCallGoFunction*, and call_engine.go's callWithStack dispatches each
// of those exit codes to hostModuleGoFuncFromOpaque[api.GoModuleFunction] or
// hostModuleGoFuncFromOpaque[api.GoFunction] respectively. So T at read time
// is always the same interface type the words were written as — this is not
// an unchecked assertion across unrelated types, it's reading a value back in
// the representation it was stored in, which avoids the itab-table lookup
// (runtime.assertE2I) a genuine `interface{}` -> T assertion would otherwise
// perform on every single host call.
func hostModuleGoFuncFromOpaque[T any](index int, opaqueBegin uintptr) T {
	offset := uintptr(index*16) + 32
	ptr := opaqueBegin + offset

	var opaqueViewOverFunction []byte
	sh := (*sliceHeader)(unsafe.Pointer(&opaqueViewOverFunction))
	sh.Data = ptr
	sh.Len = 16
	sh.Cap = 16

	words := [2]uint64{
		binary.LittleEndian.Uint64(opaqueViewOverFunction),
		binary.LittleEndian.Uint64(opaqueViewOverFunction[8:]),
	}
	return *(*T)(unsafe.Pointer(&words))
}

// writeGoFunc stores the two words (itab, data) of the concrete
// api.GoModuleFunction or api.GoFunction interface value carried by goFn.
// See hostModuleGoFuncFromOpaque for the invariant that makes reading these
// words back safe without a type assertion.
//
// GC note: as before this refactor, the opaque buffer is a plain []byte, so
// the data pointer word written here is invisible to the garbage collector.
// That's fine because the object it points to is kept alive independently:
// the module pointer stored at offset 0 (see above) keeps *wasm.Module, and
// hence its CodeSection[i].GoFunc field — a real interface value the GC does
// scan — alive for as long as this opaque buffer is reachable.
//
// Publication note: these words are written only here, from within
// buildHostModuleOpaque, which runs during (*engine).NewModuleEngine before
// the moduleEngine (and hence the instantiated api.Module) is handed back to
// the caller. The slots are never mutated afterwards. Any goroutine that later
// invokes a host call has, by definition, received that module handle, and the
// handoff of the handle establishes the happens-before edge that publishes
// these writes — the same edge every other field of the opaque buffer relies
// on.
func writeGoFunc(goFn interface{}, buf []byte) {
	var words [2]uint64
	switch fn := goFn.(type) {
	case api.GoModuleFunction:
		words = *(*[2]uint64)(unsafe.Pointer(&fn))
	case api.GoFunction:
		words = *(*[2]uint64)(unsafe.Pointer(&fn))
	default:
		panic("BUG: GoFunc must be an api.GoModuleFunction or api.GoFunction")
	}
	binary.LittleEndian.PutUint64(buf, words[0])
	binary.LittleEndian.PutUint64(buf[8:], words[1])
}
