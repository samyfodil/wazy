package ssa

import (
	"slices"
)

func sortBlocks(blocks []*basicBlock) {
	slices.SortFunc(blocks, func(i, j *basicBlock) int {
		jIsReturn := j.ReturnBlock()
		iIsReturn := i.ReturnBlock()
		if iIsReturn && jIsReturn {
			return 0
		}
		if jIsReturn {
			return 1
		}
		if iIsReturn {
			return -1
		}
		iRoot, jRoot := i.rootInstr, j.rootInstr
		if iRoot == nil && jRoot == nil { // For testing.
			return 0
		}
		// A block that opens by yielding is the slow path of a preemption check,
		// which almost never runs: placed last, it leaves the check falling through.
		if iYields, jYields := i.yields(), j.yields(); iYields != jYields {
			if iYields {
				return 1
			}
			return -1
		}
		if jRoot == nil {
			return 1
		}
		if iRoot == nil {
			return -1
		}
		return i.rootInstr.id - j.rootInstr.id
	})
}
