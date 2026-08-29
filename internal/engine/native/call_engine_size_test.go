package native

import (
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// maxCallEngineSize is the largest callEngine that still allocates out of Go's 1536-byte size class.
//
// The eight bytes of slack are the malloc header Go puts in front of a pointer-containing object of this
// size, which counts against the class: at 1536 the allocation rounds up to 1792 instead. A callEngine is
// allocated per call (see moduleEngine.NewFunction), so crossing that line costs 256 bytes on every single
// invocation -- which is exactly what happened when the GC collector first added its fields here, and showed
// up as +16% B/op on the base64 benchmarks before anything else about it did.
const maxCallEngineSize = 1536 - 8

func TestCallEngineFitsItsSizeClass(t *testing.T) {
	size := unsafe.Sizeof(callEngine{})
	require.True(t, size <= maxCallEngineSize,
		"callEngine is %d bytes, over the %d that still allocates from the 1536 size class: every call "+
			"would pay 256 bytes more. Put small fields in existing padding (executionContext has a hole "+
			"after exitCode) or reach the value another way.", size, maxCallEngineSize)
}
