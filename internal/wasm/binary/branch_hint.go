package binary

import (
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// branchHintSectionName is the custom section that carries branch hints. See wasm.BranchHint.
const branchHintSectionName = "metadata.code.branch_hint"

// funcBranchHints is one function's entry in the branch hint section. Until decodeCode rebases them, the
// offsets count from the start of the function's locals declaration, as the section gives them.
type funcBranchHints struct {
	funcIndex wasm.Index
	hints     []wasm.BranchHint
}

// decodeBranchHintSection decodes the branch hint section, or returns nil for one that is malformed or out of
// order. It is a custom section, and a module whose hints are wrong must still run.
func decodeBranchHintSection(data []byte) []funcBranchHints {
	pos := 0
	next := func() (uint32, bool) {
		v, n, err := leb128.LoadUint32(data[pos:])
		pos += int(n)
		return v, err == nil
	}
	count, ok := next()
	if !ok || uint64(count) > uint64(len(data)-pos) {
		return nil
	}
	ret := make([]funcBranchHints, 0, count)
	for i := uint32(0); i < count; i++ {
		funcIndex, ok1 := next()
		num, ok2 := next()
		if !ok1 || !ok2 || uint64(num) > uint64(len(data)-pos) || (i > 0 && funcIndex <= ret[i-1].funcIndex) {
			return nil
		}
		hints := make([]wasm.BranchHint, num)
		for j := range hints {
			offset, ok1 := next()
			size, ok2 := next()
			value, ok3 := next()
			if !ok1 || !ok2 || !ok3 || size != 1 || value > 1 || (j > 0 && offset <= hints[j-1].Offset) {
				return nil
			}
			hints[j] = wasm.BranchHint{Offset: offset, Likely: value == 1}
		}
		ret = append(ret, funcBranchHints{funcIndex: funcIndex, hints: hints})
	}
	if pos != len(data) {
		return nil
	}
	return ret
}

// rebaseBranchHints turns hints whose offsets count from the start of a function's locals declaration into
// ones that count from the start of its body, localsLen bytes later. A hint that names no if or br_if is
// dropped: the spec requires one there, and lowering only looks hints up at those instructions anyway.
func rebaseBranchHints(hints []wasm.BranchHint, localsLen uint32, body []byte) []wasm.BranchHint {
	kept := hints[:0]
	for _, h := range hints {
		if h.Offset < localsLen {
			continue
		}
		off := h.Offset - localsLen
		if uint64(off) >= uint64(len(body)) || (body[off] != wasm.OpcodeIf && body[off] != wasm.OpcodeBrIf) {
			continue
		}
		kept = append(kept, wasm.BranchHint{Offset: off, Likely: h.Likely})
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
