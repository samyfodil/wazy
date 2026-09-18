package native

import (
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestModuleClosedPtrAliasesClosed pins the layout assumption behind
// execCtx.moduleClosedPtr: compiled code reads ModuleInstance.Closed as a plain
// uint64 through that pointer, which only works while atomic.Uint64's value word
// sits at offset 0 (the noCopy/align64 markers ahead of it are zero-sized).
//
// If the standard library ever puts something with a size in front of it, the
// generated code would poll the wrong word and WithCloseOnContextDone would stop
// interrupting anything. That failure is silent at runtime, so it is pinned here.
func TestModuleClosedPtrAliasesClosed(t *testing.T) {
	var m wasm.ModuleInstance
	p := (*uint64)(unsafe.Pointer(&m.Closed))

	require.Equal(t, uint64(0), *p)

	m.Closed.Store(0x1234)
	require.Equal(t, uint64(0x1234), *p,
		"moduleClosedPtr does not alias ModuleInstance.Closed's value word: compiled code would poll the wrong address")

	*p = 0
	require.Equal(t, uint64(0), m.Closed.Load())
}
