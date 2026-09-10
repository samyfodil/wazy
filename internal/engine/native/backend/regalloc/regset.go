package regalloc

import (
	"fmt"
	"math/bits"
	"strings"
)

// NewRegSet returns a new RegSet with the given registers.
func NewRegSet(regs ...RealReg) RegSet {
	var ret RegSet
	for _, r := range regs {
		ret = ret.add(r)
	}
	return ret
}

// maxRealRegs is the number of physical registers a backend may declare.
//
// It was 64, which suited amd64 (32) and arm64 (63) but not riscv64: 32
// integer, 32 float and 32 vector registers come to 96. The old limit did not
// merely truncate, it discarded silently -- add() returned the set unchanged
// for anything at or above 64 while the parallel lookup array indexed straight
// into a panic -- so a backend that crossed it got a register that was never
// recorded as in use and an out-of-range read some time later.
const maxRealRegs = 128

// RegSet represents a set of registers.
type RegSet [maxRealRegs / 64]uint64

func (rs RegSet) format(info *RegisterInfo) string { //nolint:unused
	var ret []string
	rs.Range(func(r RealReg) { ret = append(ret, info.RealRegName(r)) })
	return strings.Join(ret, ", ")
}

func (rs RegSet) has(r RealReg) bool {
	if r >= maxRealRegs {
		return false
	}
	return rs[r/64]&(1<<uint(r%64)) != 0
}

func (rs RegSet) add(r RealReg) RegSet {
	if r >= maxRealRegs {
		panic("BUG: RealReg out of range; raise maxRealRegs")
	}
	rs[r/64] |= 1 << uint(r%64)
	return rs
}

func (rs RegSet) Range(f func(allocatedRealReg RealReg)) {
	for w, m := range rs {
		for ; m != 0; m &= m - 1 {
			f(RealReg(w*64 + bits.TrailingZeros64(m)))
		}
	}
}

// regInUseSet maps each in-use RealReg to its vrState. `mask` mirrors occupancy
// (bit r set iff arr[r] != nil) so range_ visits only live registers via
// bits.TrailingZeros64 instead of scanning all 64 slots — range_ runs on the
// hot per-call-instruction and per-edge paths (C12).
type regInUseSet[I Instr, B Block[I], F Function[I, B]] struct {
	arr  [maxRealRegs]*vrState[I, B, F]
	mask RegSet
}

func newRegInUseSet[I Instr, B Block[I], F Function[I, B]]() regInUseSet[I, B, F] {
	var ret regInUseSet[I, B, F]
	ret.reset()
	return ret
}

func (rs *regInUseSet[I, B, F]) reset() {
	// Only the slots mask says are live can be non-nil, so clearing them one by one beats
	// memclr-ing 512 bytes of pointers (and its bulk write barrier) on every block.
	rs.mask.Range(func(r RealReg) { rs.arr[r] = nil })
	rs.mask = RegSet{}
}

// clearVRegs empties the set, unassigning the RealReg of every vrState it held.
func (rs *regInUseSet[I, B, F]) clearVRegs() {
	rs.mask.Range(func(r RealReg) {
		rs.arr[r].r = RealRegInvalid
		rs.arr[r] = nil
	})
	rs.mask = RegSet{}
}

func (rs *regInUseSet[I, B, F]) format(info *RegisterInfo) string { //nolint:unused
	var ret []string
	for i, vr := range rs.arr {
		if vr != nil {
			ret = append(ret, fmt.Sprintf("(%s->v%d)", info.RealRegName(RealReg(i)), vr.v.ID()))
		}
	}
	return strings.Join(ret, ", ")
}

func (rs *regInUseSet[I, B, F]) has(r RealReg) bool {
	return r < maxRealRegs && rs.arr[r] != nil
}

func (rs *regInUseSet[I, B, F]) get(r RealReg) *vrState[I, B, F] {
	return rs.arr[r]
}

func (rs *regInUseSet[I, B, F]) remove(r RealReg) {
	rs.arr[r] = nil
	rs.mask[r/64] &^= 1 << uint(r%64)
}

func (rs *regInUseSet[I, B, F]) add(r RealReg, vr *vrState[I, B, F]) {
	if r >= maxRealRegs {
		panic("BUG: RealReg out of range; raise maxRealRegs")
	}
	rs.arr[r] = vr
	rs.mask[r/64] |= 1 << uint(r%64)
}

func (rs *regInUseSet[I, B, F]) range_(f func(allocatedRealReg RealReg, vr *vrState[I, B, F])) {
	rs.mask.Range(func(r RealReg) { f(r, rs.arr[r]) })
}

// set returns the occupancy as a RegSet.
func (rs *regInUseSet[I, B, F]) set() RegSet { return rs.mask }
