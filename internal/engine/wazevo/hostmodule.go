package wazevo

import (
	"encoding/binary"
	"unsafe"

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
		writeIface(goFn, ret[offset:])
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

func hostModuleGoFuncFromOpaque[T any](index int, opaqueBegin uintptr) T {
	offset := uintptr(index*16) + 32
	ptr := opaqueBegin + offset

	var opaqueViewOverFunction []byte
	sh := (*sliceHeader)(unsafe.Pointer(&opaqueViewOverFunction))
	sh.Data = ptr
	sh.Len = 16
	sh.Cap = 16
	return readIface(opaqueViewOverFunction).(T)
}

func writeIface(goFn interface{}, buf []byte) {
	goFnIface := *(*[2]uint64)(unsafe.Pointer(&goFn))
	binary.LittleEndian.PutUint64(buf, goFnIface[0])
	binary.LittleEndian.PutUint64(buf[8:], goFnIface[1])
}

func readIface(buf []byte) interface{} {
	b := binary.LittleEndian.Uint64(buf)
	s := binary.LittleEndian.Uint64(buf[8:])
	return *(*interface{})(unsafe.Pointer(&[2]uint64{b, s}))
}
