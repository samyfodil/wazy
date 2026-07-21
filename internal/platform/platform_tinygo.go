//go:build tinygo

package platform

// TinyGo does not support the native JIT engine: it lacks //go:linkname
// compatibility with the standard runtime and uses a different func-value layout.
const nativeCompilerAvailable = false
