package amd64

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// Intel erratum SKX102, the "JCC erratum": on a Skylake-family core the uop
// cache (DSB) drops any 32-byte window of instruction bytes that holds a jump
// crossing a 32-byte boundary, or ending exactly on one, so that code is fed by
// the legacy decoder instead. Nothing is observable except the speed.
//
// The fix, the same one LLVM's -mbranches-within-32B-boundaries applies, is to
// insert NOPs before such a jump so it lands wholly inside one window. Whether
// to pay for it is platform.JCCErratumWorkaroundEnabled's call; everything here
// is only reached once that has said yes.
//
// Two invariants make padding against a *buffer* offset equivalent to padding
// against the final *address*:
//
//  1. engineRelocator.appendFunction aligns every compiled function to 32 bytes
//     when the workaround is on (16 otherwise), and
//  2. the executable itself is page-aligned (platform.MmapCodeSegment),
//
// so offset ≡ address (mod 32) for every byte of every function body.

// jccErratumWindow is the size of a DSB instruction window, in bytes.
const jccErratumWindow = 32

// jccBranchLen returns an upper bound on the encoded length of i when i is, or
// ends in, a jump that erratum SKX102 applies to, and 0 when it is not one.
//
// An upper bound is sound rather than merely convenient. If [off, off+nMax)
// lies inside one window and does not end on its boundary, then off+nMax is
// strictly below the next boundary, so for every nActual <= nMax the real end
// off+nActual is strictly between off and that boundary too: it can neither
// cross it nor land on it. Over-estimating therefore pads a little more often
// than strictly necessary and never less often -- which is the direction that
// matters, because an under-estimate would fail silently.
func jccBranchLen(i *instruction) int64 {
	switch i.kind {
	case jmp:
		switch i.op1.kind {
		case operandKindLabel, operandKindImm32:
			return 5 // E9 cd
		case operandKindReg:
			return 3 // [REX] FF /4
		case operandKindMem:
			return 8 // [REX] FF /4 ModRM [SIB] [disp32]
		}
	case jmpIf:
		return 6 // 0F 80+cc cd -- the only encoding lowerings emit.
	case call:
		return 5 // E8 cd
	case callIndirect:
		switch i.op1.kind {
		case operandKindReg:
			return 3 // [REX] FF /2
		case operandKindMem:
			return 8 // [REX] FF /2 ModRM [SIB] [disp32]
		}
	case tailCall:
		return 5 // E9 cd
	case tailCallIndirect:
		return 3 // [REX] FF /4
	case ret:
		return 1 // C3
	case exitSequence:
		// Two `movq disp(%execCtx), %r` restores followed by a RET; the RET is
		// the jump, and a one-byte instruction can only offend by ending on a
		// boundary. Keeping the whole sequence inside one window is sufficient
		// (and, at 9-11 bytes, affordable): by the argument above the RET's end
		// is then strictly below the boundary.
		return 2*execCtxLoad64Len(i.op1.reg().RealReg()) + 1
	}
	return 0
}

// execCtxLoad64Len is the exact encoded length of one of the two
// `movq disp(%base), %r` loads the exitSequence case of instruction.encode
// emits through encodeLoad64. Both displacements are non-zero and fit in a
// signed byte (TestExecCtxLoad64LenMatchesEncoder pins that, and pins this
// function against the encoder itself), so the encoding is always
// REX.W + 0x8B + ModRM [+ SIB] + disp8.
func execCtxLoad64Len(base regalloc.RealReg) int64 {
	n := int64(4) // REX.W, opcode, ModRM, disp8.
	if base == rsp || base == r12 {
		n++ // Those two encode ModRM.rm=100, which means "SIB follows".
	}
	return n
}

// jccErratumPad emits NOPs, if needed, so that the jump i is about to encode to
// neither crosses a 32-byte boundary nor ends on one. off must be the current
// end of the code buffer.
//
// When padding is needed the branch is moved to the next boundary, which is the
// least it can be moved: every intermediate offset has a larger residue mod 32
// than the current one, so none of them can fit a branch that does not fit now.
func jccErratumPad(c backend.Compiler, off int64, i *instruction) {
	n := jccBranchLen(i)
	if n == 0 {
		return
	}
	end := off + n
	if off/jccErratumWindow == (end-1)/jccErratumWindow && end%jccErratumWindow != 0 {
		return // Already wholly inside one window, and not ending on its edge.
	}
	emitNops(c, int(jccErratumWindow-off%jccErratumWindow))
}

// emitNops emits exactly n bytes of canonical multi-byte NOPs (Intel SDM
// Vol.2B, NOP: the recommended 1-to-9-byte forms), using as few instructions as
// possible so the padding costs decode slots rather than uops.
func emitNops(c backend.Compiler, n int) {
	for n > 0 {
		k := n
		if k > len(canonicalNops) {
			k = len(canonicalNops)
		}
		for _, b := range canonicalNops[k-1] {
			c.EmitByte(b)
		}
		n -= k
	}
}

// canonicalNops[k-1] is the recommended k-byte NOP encoding.
var canonicalNops = [9][]byte{
	{0x90},
	{0x66, 0x90},
	{0x0f, 0x1f, 0x00},
	{0x0f, 0x1f, 0x40, 0x00},
	{0x0f, 0x1f, 0x44, 0x00, 0x00},
	{0x66, 0x0f, 0x1f, 0x44, 0x00, 0x00},
	{0x0f, 0x1f, 0x80, 0x00, 0x00, 0x00, 0x00},
	{0x0f, 0x1f, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00},
	{0x66, 0x0f, 0x1f, 0x84, 0x00, 0x00, 0x00, 0x00, 0x00},
}

// InstrExtent is one instruction's placement in a function's code buffer.
// Since compiled functions are 32-byte aligned whenever the JCC-erratum
// workaround is on, Offset agrees with the instruction's final address modulo
// 32, which is all the erratum cares about.
type InstrExtent struct{ Offset, Length int64 }

// InstrExtentSink, when non-nil, is called once per machine.Encode with the
// extent of every instruction that Encode emitted for that function, in order,
// and the function's code buffer. It may be called concurrently, since
// functions compile in parallel, and it must retain neither argument.
//
// It exists so that a test can reconstruct exact instruction boundaries in
// compiled code and audit branch placement against the erratum without a
// disassembler, and -- crucially -- without reusing jccBranchLen's own notion
// of what a branch is, which is the thing under test. Production leaves it nil.
var InstrExtentSink func(extents []InstrExtent, code []byte)

// Compile-time assertion that the two displacements execCtxLoad64Len predicts
// an encoding for still fit in the signed byte it assumes. Converting a negative
// constant to uint does not compile, so growing either offset past 127 -- which
// would make encodeLoad64 emit a disp32 and silently leave the exit sequence's
// RET one window further along than jccBranchLen believes -- fails the build
// instead of the mitigation.
const (
	_ = uint(127 - nativeapi.ExecutionContextOffsetOriginalFramePointer)
	_ = uint(127 - nativeapi.ExecutionContextOffsetOriginalStackPointer)
)
