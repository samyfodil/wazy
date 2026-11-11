package platform

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestAmd64CompilerSupports(t *testing.T) {
	const all = CpuFeatureAmd64SSE4_1 | CpuFeatureAmd64SSE4_2 | CpuFeatureAmd64POPCNT
	require.True(t, amd64CompilerSupports(all))
	require.True(t, amd64CompilerSupports(all|CpuFeatureAmd64BMI1|CpuFeatureAmd64ABM))
	for _, missing := range []CpuFeatureFlags{CpuFeatureAmd64SSE4_1, CpuFeatureAmd64SSE4_2, CpuFeatureAmd64POPCNT} {
		require.False(t, amd64CompilerSupports(all&^missing))
	}
}
