package riscv64

import (
	"encoding/binary"
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
)

// RISC-V needs no call trampoline islands.
//
// arm64 and amd64 emit a single 4/5-byte direct branch whose displacement runs
// out (+/-128MiB, +/-2GiB) on a large executable, so they place islands of
// longer sequences within reach. RISC-V has no single-instruction call at all:
// the standard `call` is already the two-instruction `auipc`+`jalr` pair, which
// reaches +/-2GiB -- the entire addressable range of a mmap'd code segment.
// Paying those 4 extra bytes on every direct call buys the removal of island
// placement, island search, and the out-of-range relocation fallback.
const (
	// callSequenceSize is the size of the auipc+jalr direct call sequence.
	callSequenceSize = 8

	// callTrampolineIslandInterval is unused (islandSize is reported as 0) but
	// must be non-zero to satisfy the engine's interval arithmetic.
	callTrampolineIslandInterval = 1 << 30

	// trampolineCallSize is likewise unused; kept at zero so any accidental
	// island allocation is empty rather than wrong.
	trampolineCallSize = 0
)

// ResolveRelocations implements backend.Machine.
//
// Every relocation site is the auipc of a call sequence. The pair
//
//	auipc rd, hi20
//	jalr  rd', lo12(rd)
//
// computes (pc_of_auipc + (hi20<<12)) + lo12, so the displacement is split with
// the usual +0x800 bias: jalr's immediate is sign-extended, and a negative low
// half must be compensated by rounding the high half up.
func (m *machine) ResolveRelocations(
	refToBinaryOffset []int,
	_ int,
	executable []byte,
	relocations []backend.RelocationInfo,
	_ []int,
) {
	for _, r := range relocations {
		instrOffset := r.Offset
		calleeFnOffset := refToBinaryOffset[r.FuncRef]
		diff := int64(calleeFnOffset) - instrOffset
		if diff < math.MinInt32 || diff > math.MaxInt32 {
			// auipc+jalr reaches +/-2GiB. An executable larger than that is
			// beyond what any backend here handles.
			panic("BUG: call displacement exceeds 2GiB")
		}

		hi, lo := splitImm32(int32(diff))

		// A tail call must not clobber the return address: it jumps with rd =
		// zero so the callee returns straight to *our* caller. A regular call
		// links through ra.
		linkReg := regNumberInEncoding[raReg]
		scratch := linkReg
		if r.IsTailCall {
			linkReg = regNumberInEncoding[zeroReg]
			// jalr's base register must survive auipc, and ra is about to be
			// left untouched, so use the reserved scratch instead.
			scratch = regNumberInEncoding[tmpReg]
		}

		binary.LittleEndian.PutUint32(executable[instrOffset:], encodeAuipc(scratch, hi))
		binary.LittleEndian.PutUint32(executable[instrOffset+4:], encodeJalr(linkReg, scratch, lo))
	}
}
