//go:build tinygo

package native

// TinyGo uses a different func-value representation and does not support
// //go:linkname to runtime.memmove. The native engine is not usable under
// TinyGo, so memmovPtr is set to zero; the interpreter is used instead.
var memmovPtr uintptr
