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

// RegSet represents a set of registers.
type RegSet uint64

func (rs RegSet) format(info *RegisterInfo) string { //nolint:unused
	var ret []string
	for i := 0; i < 64; i++ {
		if rs&(1<<uint(i)) != 0 {
			ret = append(ret, info.RealRegName(RealReg(i)))
		}
	}
	return strings.Join(ret, ", ")
}

func (rs RegSet) has(r RealReg) bool {
	return rs&(1<<uint(r)) != 0
}

func (rs RegSet) add(r RealReg) RegSet {
	if r >= 64 {
		return rs
	}
	return rs | 1<<uint(r)
}

func (rs RegSet) Range(f func(allocatedRealReg RealReg)) {
	for i := 0; i < 64; i++ {
		if rs&(1<<uint(i)) != 0 {
			f(RealReg(i))
		}
	}
}

// regInUseSet maps each in-use RealReg to its vrState. `mask` mirrors occupancy
// (bit r set iff arr[r] != nil) so range_ visits only live registers via
// bits.TrailingZeros64 instead of scanning all 64 slots — range_ runs on the
// hot per-call-instruction and per-edge paths (C12).
type regInUseSet[I Instr, B Block[I], F Function[I, B]] struct {
	arr  [64]*vrState[I, B, F]
	mask uint64
}

func newRegInUseSet[I Instr, B Block[I], F Function[I, B]]() regInUseSet[I, B, F] {
	var ret regInUseSet[I, B, F]
	ret.reset()
	return ret
}

func (rs *regInUseSet[I, B, F]) reset() {
	clear(rs.arr[:])
	rs.mask = 0
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
	return r < 64 && rs.arr[r] != nil
}

func (rs *regInUseSet[I, B, F]) get(r RealReg) *vrState[I, B, F] {
	return rs.arr[r]
}

func (rs *regInUseSet[I, B, F]) remove(r RealReg) {
	rs.arr[r] = nil
	rs.mask &^= 1 << r
}

func (rs *regInUseSet[I, B, F]) add(r RealReg, vr *vrState[I, B, F]) {
	if r >= 64 {
		return
	}
	rs.arr[r] = vr
	rs.mask |= 1 << r
}

func (rs *regInUseSet[I, B, F]) range_(f func(allocatedRealReg RealReg, vr *vrState[I, B, F])) {
	for m := rs.mask; m != 0; m &= m - 1 {
		r := bits.TrailingZeros64(m)
		f(RealReg(r), rs.arr[r])
	}
}
