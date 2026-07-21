//go:build !tinygo

package native

import (
	"unsafe"
)

//go:linkname memmove runtime.memmove
func memmove(_, _ unsafe.Pointer, _ uintptr)

// memmoveHolder holds a func value that references runtime.memmove so its
// entry PC can be read without reflect. A Go func value is represented as a
// pointer to a "funcval" whose first word is the function's entry PC; that is
// exactly what reflect.ValueOf(fn).Pointer() returns, so dereferencing the
// funcval's first word reproduces it.
var memmoveHolder = memmove

// memmovPtr is the entry PC of runtime.memmove, passed to the compiler backend
// to emit direct calls to it (see module_engine.go and frontend/lower.go).
var memmovPtr = **(**uintptr)(unsafe.Pointer(&memmoveHolder))
