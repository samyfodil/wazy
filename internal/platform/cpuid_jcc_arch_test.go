//go:build (amd64 || arm64) && !tinygo

// This test sets CpuFeatures, which is a variable only on amd64 and arm64.

package platform

import (
	"runtime"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestJCCErratumWorkaroundEnabled_archGate asserts the workaround can never turn
// itself on outside amd64, even if the flag bit (which the arm64 flag namespace
// reuses for its own numbering) happened to be set.
func TestJCCErratumWorkaroundEnabled_archGate(t *testing.T) {
	saved := CpuFeatures
	t.Cleanup(func() { CpuFeatures = saved })

	CpuFeatures = saved | CpuFeatureAmd64JCCErratum
	require.Equal(t, runtime.GOARCH == "amd64", JCCErratumWorkaroundEnabled())

	CpuFeatures = saved &^ CpuFeatureAmd64JCCErratum
	require.False(t, JCCErratumWorkaroundEnabled())
}
