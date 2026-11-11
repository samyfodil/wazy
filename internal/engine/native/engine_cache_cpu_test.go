//go:build (amd64 || arm64) && !tinygo

// This test sets platform.CpuFeatures, which is a variable only on amd64 and arm64.

package native

import (
	"crypto/sha256"
	"testing"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Test_fileCacheKey_cpuFeatures pins the property the whole on-disk cache relies
// on: two hosts that would compile the same module into different machine code
// must not share a cache entry. The JCC-erratum workaround is the case that
// motivated this -- it changes both function alignment and the emitted bytes,
// and unlike an instruction-set feature it is not detectable by inspecting the
// cached code -- but the assertion is over CpuFeatures as a whole, so any future
// flag inherits it.
func Test_fileCacheKey_cpuFeatures(t *testing.T) {
	saved := platform.CpuFeatures
	t.Cleanup(func() { platform.CpuFeatures = saved })

	m := &wasm.Module{}
	s := sha256.New()
	s.Write([]byte("hello world"))
	s.Sum(m.ID[:0])

	for _, flag := range []platform.CpuFeatureFlags{
		platform.CpuFeatureAmd64JCCErratum,
		platform.CpuFeatureAmd64SSE4_1,
		platform.CpuFeatureAmd64BMI1,
		platform.CpuFeatureAmd64ABM,
	} {
		platform.CpuFeatures = saved &^ flag
		without := fileCacheKey(m)
		platform.CpuFeatures = saved | flag
		with := fileCacheKey(m)
		require.NotEqual(t, without, with, "cache key does not depend on CPU feature %#x", uint64(flag))

		// And it is stable: the same flags always give the same key, otherwise
		// the cache would simply never hit and the check above would be vacuous.
		require.Equal(t, with, fileCacheKey(m))
	}
}
