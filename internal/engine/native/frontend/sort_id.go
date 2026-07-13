package frontend

import (
	"slices"

	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

func sortSSAValueIDs(IDs []ssa.ValueID) {
	slices.SortFunc(IDs, func(i, j ssa.ValueID) int {
		return int(i) - int(j)
	})
}
