package native

import (
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// maxCallEngineSize is the largest callEngine that still allocates out of Go's 1024-byte size class.
//
// The eight bytes of slack are the malloc header Go puts in front of a pointer-containing object of this
// size, which counts against the class: at 1024 the allocation rounds up to 1152 instead. A callEngine is
// allocated per call (see moduleEngine.NewFunction), so crossing that line costs 128 bytes on every single
// invocation -- which is exactly what happened when the GC collector first added its fields here (against
// the 1536 class this used to sit in), and showed up as +16% B/op on the base64 benchmarks before anything
// else about it did.
const maxCallEngineSize = 1024 - 8

func TestCallEngineFitsItsSizeClass(t *testing.T) {
	size := unsafe.Sizeof(callEngine{})
	require.True(t, size <= maxCallEngineSize,
		"callEngine is %d bytes, over the %d that still allocates from the 1024 size class: every call "+
			"would pay 128 bytes more. Put small fields in existing padding (executionContext has a hole "+
			"after exitCode) or reach the value another way.", size, maxCallEngineSize)
}
