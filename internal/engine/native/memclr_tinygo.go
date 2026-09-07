//go:build tinygo

package native

// TinyGo uses a different func-value representation and does not support
// //go:linkname to runtime.memclrNoHeapPointers. The native engine is not
// usable under TinyGo, so memclrPtr is set to zero; the interpreter is used
// instead. See memmove_tinygo.go.
var memclrPtr uintptr
