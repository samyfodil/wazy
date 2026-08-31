package interpreter

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestInstructionName ensures that all the operation Kind's stringer is well-defined.
func TestOperationKind_String(t *testing.T) {
	for k := operationKind(0); k < operationKindEnd; k++ {
		require.NotEqual(t, "", k.String())
	}
}

// Test_unionOperation_String ensures that UnionOperation's stringer is well-defined for all supported OpKinds.
func Test_unionOperation_String(t *testing.T) {
	op := unionOperation{}
	for k := operationKind(0); k < operationKindEnd; k++ {
		op.Kind = k
		require.NotEqual(t, "", op.String())
	}
}

func TestLabel(t *testing.T) {
	for k := labelKind(0); k < labelKindNum; k++ {
		label := newLabel(k, 12345)
		require.Equal(t, k, label.Kind())
		require.Equal(t, 12345, label.FrameID())
	}
}

// Test_inclusiveRange_AsU64 pins the packing callEngine.drop relies on: 0 for a no-op range,
// a value below 1<<32 for a plain truncation of the top N values, and a non-zero upper half
// for everything else. It also checks the round trip through inclusiveRangeFromU64.
func Test_inclusiveRange_AsU64(t *testing.T) {
	for _, tc := range []struct {
		r   inclusiveRange
		raw uint64
	}{
		{r: nopinclusiveRange, raw: 0},
		{r: inclusiveRange{Start: 0, End: 0}, raw: 1},
		{r: inclusiveRange{Start: 0, End: 1}, raw: 2},
		{r: inclusiveRange{Start: 0, End: 9}, raw: 10},
		{r: inclusiveRange{Start: 1, End: 1}, raw: 1<<32 | 1},
		{r: inclusiveRange{Start: 2, End: 5}, raw: 2<<32 | 5},
	} {
		raw := tc.r.AsU64()
		require.Equal(t, tc.raw, raw)
		require.Equal(t, tc.r, inclusiveRangeFromU64(raw))
		// Only a range that keeps values above the dropped ones may set the upper half.
		require.Equal(t, tc.r.Start > 0, raw>>32 != 0)
	}
}
