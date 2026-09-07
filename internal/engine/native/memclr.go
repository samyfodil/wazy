//go:build !tinygo

package native

import (
	"unsafe"
)

// Adapted from wazero (wazero/wazero#2538, Apache-2.0), mirroring memmove.go's
// funcval trick rather than upstream's reflect.ValueOf, since wazy takes no
// dependency on reflect.
//
//go:linkname memclrNoHeapPointers runtime.memclrNoHeapPointers
func memclrNoHeapPointers(_ unsafe.Pointer, _ uintptr)

// memclrHolder holds a func value referencing runtime.memclrNoHeapPointers so
// its entry PC can be read without reflect, exactly as memmoveHolder does.
var memclrHolder = memclrNoHeapPointers

// memclrPtr is the entry PC of runtime.memclrNoHeapPointers, passed to the
// compiler backend to emit direct calls to it for large zero memory.fill.
// Like runtime.memmove -- which the memory.copy lowering already calls the same
// way -- it is nosplit, allocation-free, and writes no pointers the collector
// cares about, so it is safe to call from generated code.
var memclrPtr = **(**uintptr)(unsafe.Pointer(&memclrHolder))
