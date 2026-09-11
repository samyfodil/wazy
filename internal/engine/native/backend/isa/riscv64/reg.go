package riscv64

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
)

// RISC-V RV64G registers.
//
// Constants are declared in *encoding* order (x0..x31, f0..f31) so
// regNumberInEncoding is the identity minus the RealRegInvalid bias, but they
// are *printed* with their ABI names (zero/ra/sp/a0/t0/...) because that is
// what every RISC-V disassembler emits and what the encoder golden tests
// compare against.
//
// See the RISC-V ELF psABI (LP64D):
// https://github.com/riscv-non-isa/riscv-elf-psabi-doc/blob/master/riscv-cc.adoc

const (
	// Integer registers. RV64 has no sub-register widths to distinguish: a
	// 32-bit ("word") operation is a different opcode on the same register,
	// so from the allocator's perspective there is exactly one class.

	x0  = regalloc.RealRegInvalid + 1 + iota // zero: hardwired zero, reads 0, writes discarded.
	x1                                       // ra: return address, written by jal/jalr.
	x2                                       // sp: stack pointer.
	x3                                       // gp: global pointer. Reserved by the psABI.
	x4                                       // tp: thread pointer. Reserved by the psABI.
	x5                                       // t0
	x6                                       // t1
	x7                                       // t2
	x8                                       // s0/fp
	x9                                       // s1
	x10                                      // a0
	x11                                      // a1
	x12                                      // a2
	x13                                      // a3
	x14                                      // a4
	x15                                      // a5
	x16                                      // a6
	x17                                      // a7
	x18                                      // s2
	x19                                      // s3
	x20                                      // s4
	x21                                      // s5
	x22                                      // s6
	x23                                      // s7
	x24                                      // s8
	x25                                      // s9
	x26                                      // s10
	x27                                      // s11: Go's goroutine pointer (g). Never allocated.
	x28                                      // t3
	x29                                      // t4
	x30                                      // t5: reserved, tmpReg2.
	x31                                      // t6: reserved, tmpReg.

	// Floating point registers. As with the integer file, f32/f64 are the
	// same register from the allocator's perspective; the opcode carries the
	// width.

	f0
	f1
	f2
	f3
	f4
	f5
	f6
	f7
	f8
	f9
	f10
	f11
	f12
	f13
	f14
	f15
	f16
	f17
	f18
	f19
	f20
	f21
	f22
	f23
	f24
	f25
	f26
	f27
	f28
	f29
	f30
	f31 // reserved, fpTmpReg.

	// RVV vector registers. These are a *separate* file from f0-f31: an
	// f-register is 64 bits and cannot hold a v128, so wasm's vector values
	// live here and get their own allocation class (regalloc.RegTypeVec).
	//
	// Wasm's v128 is a fixed 128 bits while VLEN is an implementation choice
	// known only at run time, so the lowering never assumes a width: it sets
	// `vsetivli zero, N, eN, m1` before each operation, which pins exactly 16
	// bytes (16 e8, 8 e16, 4 e32 or 2 e64 elements) at LMUL=1 whatever VLEN
	// is, provided VLEN >= 128. That minimum is what the platform gate checks.
	//
	// v0 is excluded from allocation: it is the architecturally fixed mask
	// register, so any masked operation clobbers it.
	v0
	v1
	v2
	v3
	v4
	v5
	v6
	v7
	v8
	v9
	v10
	v11
	v12
	v13
	v14
	v15
	v16
	v17
	v18
	v19
	v20
	v21
	v22
	v23
	v24
	v25
	v26
	v27
	v28
	v29
	v30
	v31 // reserved, vecTmpReg.
)

// Reserved registers.
//
// RISC-V needs two integer scratch registers where arm64 needs one, for two
// structural reasons: there is no register+register addressing mode (a
// base+index address must be materialized with a shift and an add), and no
// instruction takes an immediate wider than 12 bits (any larger constant costs
// lui+addi into a register). A single scratch would be live across both halves
// of those sequences.
const (
	zeroReg = x0
	raReg   = x1
	spReg   = x2
	tmpReg  = x31
	tmpReg2 = x30
	// tmpReg3 exists for one job: the byte and halfword atomics. RISC-V has no
	// sub-word AMO, so those become an LR/SC loop that must hold the aligned
	// address, the bit shift, the positioned mask and the value being
	// assembled all at once -- more than two scratch registers can carry, and
	// the loop is expanded at encode time, where nothing can be allocated.
	tmpReg3    = x29
	fpTmpReg   = f31
	vecMaskReg = v0
	vecTmpReg  = v31
)

var (
	x0VReg  = regalloc.FromRealReg(x0, regalloc.RegTypeInt)
	x1VReg  = regalloc.FromRealReg(x1, regalloc.RegTypeInt)
	x2VReg  = regalloc.FromRealReg(x2, regalloc.RegTypeInt)
	x3VReg  = regalloc.FromRealReg(x3, regalloc.RegTypeInt)
	x4VReg  = regalloc.FromRealReg(x4, regalloc.RegTypeInt)
	x5VReg  = regalloc.FromRealReg(x5, regalloc.RegTypeInt)
	x6VReg  = regalloc.FromRealReg(x6, regalloc.RegTypeInt)
	x7VReg  = regalloc.FromRealReg(x7, regalloc.RegTypeInt)
	x8VReg  = regalloc.FromRealReg(x8, regalloc.RegTypeInt)
	x9VReg  = regalloc.FromRealReg(x9, regalloc.RegTypeInt)
	x10VReg = regalloc.FromRealReg(x10, regalloc.RegTypeInt)
	x11VReg = regalloc.FromRealReg(x11, regalloc.RegTypeInt)
	x12VReg = regalloc.FromRealReg(x12, regalloc.RegTypeInt)
	x13VReg = regalloc.FromRealReg(x13, regalloc.RegTypeInt)
	x14VReg = regalloc.FromRealReg(x14, regalloc.RegTypeInt)
	x15VReg = regalloc.FromRealReg(x15, regalloc.RegTypeInt)
	x16VReg = regalloc.FromRealReg(x16, regalloc.RegTypeInt)
	x17VReg = regalloc.FromRealReg(x17, regalloc.RegTypeInt)
	x18VReg = regalloc.FromRealReg(x18, regalloc.RegTypeInt)
	x19VReg = regalloc.FromRealReg(x19, regalloc.RegTypeInt)
	x20VReg = regalloc.FromRealReg(x20, regalloc.RegTypeInt)
	x21VReg = regalloc.FromRealReg(x21, regalloc.RegTypeInt)
	x22VReg = regalloc.FromRealReg(x22, regalloc.RegTypeInt)
	x23VReg = regalloc.FromRealReg(x23, regalloc.RegTypeInt)
	x24VReg = regalloc.FromRealReg(x24, regalloc.RegTypeInt)
	x25VReg = regalloc.FromRealReg(x25, regalloc.RegTypeInt)
	x26VReg = regalloc.FromRealReg(x26, regalloc.RegTypeInt)
	x27VReg = regalloc.FromRealReg(x27, regalloc.RegTypeInt)
	x28VReg = regalloc.FromRealReg(x28, regalloc.RegTypeInt)
	x29VReg = regalloc.FromRealReg(x29, regalloc.RegTypeInt)
	x30VReg = regalloc.FromRealReg(x30, regalloc.RegTypeInt)
	x31VReg = regalloc.FromRealReg(x31, regalloc.RegTypeInt)

	f0VReg  = regalloc.FromRealReg(f0, regalloc.RegTypeFloat)
	f1VReg  = regalloc.FromRealReg(f1, regalloc.RegTypeFloat)
	f2VReg  = regalloc.FromRealReg(f2, regalloc.RegTypeFloat)
	f3VReg  = regalloc.FromRealReg(f3, regalloc.RegTypeFloat)
	f4VReg  = regalloc.FromRealReg(f4, regalloc.RegTypeFloat)
	f5VReg  = regalloc.FromRealReg(f5, regalloc.RegTypeFloat)
	f6VReg  = regalloc.FromRealReg(f6, regalloc.RegTypeFloat)
	f7VReg  = regalloc.FromRealReg(f7, regalloc.RegTypeFloat)
	f8VReg  = regalloc.FromRealReg(f8, regalloc.RegTypeFloat)
	f9VReg  = regalloc.FromRealReg(f9, regalloc.RegTypeFloat)
	f10VReg = regalloc.FromRealReg(f10, regalloc.RegTypeFloat)
	f11VReg = regalloc.FromRealReg(f11, regalloc.RegTypeFloat)
	f12VReg = regalloc.FromRealReg(f12, regalloc.RegTypeFloat)
	f13VReg = regalloc.FromRealReg(f13, regalloc.RegTypeFloat)
	f14VReg = regalloc.FromRealReg(f14, regalloc.RegTypeFloat)
	f15VReg = regalloc.FromRealReg(f15, regalloc.RegTypeFloat)
	f16VReg = regalloc.FromRealReg(f16, regalloc.RegTypeFloat)
	f17VReg = regalloc.FromRealReg(f17, regalloc.RegTypeFloat)
	f18VReg = regalloc.FromRealReg(f18, regalloc.RegTypeFloat)
	f19VReg = regalloc.FromRealReg(f19, regalloc.RegTypeFloat)
	f20VReg = regalloc.FromRealReg(f20, regalloc.RegTypeFloat)
	f21VReg = regalloc.FromRealReg(f21, regalloc.RegTypeFloat)
	f22VReg = regalloc.FromRealReg(f22, regalloc.RegTypeFloat)
	f23VReg = regalloc.FromRealReg(f23, regalloc.RegTypeFloat)
	f24VReg = regalloc.FromRealReg(f24, regalloc.RegTypeFloat)
	f25VReg = regalloc.FromRealReg(f25, regalloc.RegTypeFloat)
	f26VReg = regalloc.FromRealReg(f26, regalloc.RegTypeFloat)
	f27VReg = regalloc.FromRealReg(f27, regalloc.RegTypeFloat)
	f28VReg = regalloc.FromRealReg(f28, regalloc.RegTypeFloat)
	f29VReg = regalloc.FromRealReg(f29, regalloc.RegTypeFloat)
	f30VReg = regalloc.FromRealReg(f30, regalloc.RegTypeFloat)
	f31VReg = regalloc.FromRealReg(f31, regalloc.RegTypeFloat)

	v0VReg  = regalloc.FromRealReg(v0, regalloc.RegTypeVec)
	v1VReg  = regalloc.FromRealReg(v1, regalloc.RegTypeVec)
	v2VReg  = regalloc.FromRealReg(v2, regalloc.RegTypeVec)
	v3VReg  = regalloc.FromRealReg(v3, regalloc.RegTypeVec)
	v4VReg  = regalloc.FromRealReg(v4, regalloc.RegTypeVec)
	v5VReg  = regalloc.FromRealReg(v5, regalloc.RegTypeVec)
	v6VReg  = regalloc.FromRealReg(v6, regalloc.RegTypeVec)
	v7VReg  = regalloc.FromRealReg(v7, regalloc.RegTypeVec)
	v8VReg  = regalloc.FromRealReg(v8, regalloc.RegTypeVec)
	v9VReg  = regalloc.FromRealReg(v9, regalloc.RegTypeVec)
	v10VReg = regalloc.FromRealReg(v10, regalloc.RegTypeVec)
	v11VReg = regalloc.FromRealReg(v11, regalloc.RegTypeVec)
	v12VReg = regalloc.FromRealReg(v12, regalloc.RegTypeVec)
	v13VReg = regalloc.FromRealReg(v13, regalloc.RegTypeVec)
	v14VReg = regalloc.FromRealReg(v14, regalloc.RegTypeVec)
	v15VReg = regalloc.FromRealReg(v15, regalloc.RegTypeVec)
	v16VReg = regalloc.FromRealReg(v16, regalloc.RegTypeVec)
	v17VReg = regalloc.FromRealReg(v17, regalloc.RegTypeVec)
	v18VReg = regalloc.FromRealReg(v18, regalloc.RegTypeVec)
	v19VReg = regalloc.FromRealReg(v19, regalloc.RegTypeVec)
	v20VReg = regalloc.FromRealReg(v20, regalloc.RegTypeVec)
	v21VReg = regalloc.FromRealReg(v21, regalloc.RegTypeVec)
	v22VReg = regalloc.FromRealReg(v22, regalloc.RegTypeVec)
	v23VReg = regalloc.FromRealReg(v23, regalloc.RegTypeVec)
	v24VReg = regalloc.FromRealReg(v24, regalloc.RegTypeVec)
	v25VReg = regalloc.FromRealReg(v25, regalloc.RegTypeVec)
	v26VReg = regalloc.FromRealReg(v26, regalloc.RegTypeVec)
	v27VReg = regalloc.FromRealReg(v27, regalloc.RegTypeVec)
	v28VReg = regalloc.FromRealReg(v28, regalloc.RegTypeVec)
	v29VReg = regalloc.FromRealReg(v29, regalloc.RegTypeVec)
	v30VReg = regalloc.FromRealReg(v30, regalloc.RegTypeVec)
	v31VReg = regalloc.FromRealReg(v31, regalloc.RegTypeVec)

	zeroVReg    = x0VReg
	raVReg      = x1VReg
	spVReg      = x2VReg
	tmpRegVReg  = x31VReg
	tmpReg2VReg = x30VReg
)

var regNames = [...]string{
	x0: "zero", x1: "ra", x2: "sp", x3: "gp", x4: "tp",
	x5: "t0", x6: "t1", x7: "t2",
	x8: "s0", x9: "s1",
	x10: "a0", x11: "a1", x12: "a2", x13: "a3", x14: "a4", x15: "a5", x16: "a6", x17: "a7",
	x18: "s2", x19: "s3", x20: "s4", x21: "s5", x22: "s6", x23: "s7",
	x24: "s8", x25: "s9", x26: "s10", x27: "s11",
	x28: "t3", x29: "t4", x30: "t5", x31: "t6",

	f0: "ft0", f1: "ft1", f2: "ft2", f3: "ft3", f4: "ft4", f5: "ft5", f6: "ft6", f7: "ft7",
	f8: "fs0", f9: "fs1",
	f10: "fa0", f11: "fa1", f12: "fa2", f13: "fa3", f14: "fa4", f15: "fa5", f16: "fa6", f17: "fa7",
	f18: "fs2", f19: "fs3", f20: "fs4", f21: "fs5", f22: "fs6", f23: "fs7",
	f24: "fs8", f25: "fs9", f26: "fs10", f27: "fs11",
	f28: "ft8", f29: "ft9", f30: "ft10", f31: "ft11",

	v0: "v0", v1: "v1", v2: "v2", v3: "v3", v4: "v4", v5: "v5", v6: "v6", v7: "v7",
	v8: "v8", v9: "v9", v10: "v10", v11: "v11", v12: "v12", v13: "v13", v14: "v14", v15: "v15",
	v16: "v16", v17: "v17", v18: "v18", v19: "v19", v20: "v20", v21: "v21", v22: "v22", v23: "v23",
	v24: "v24", v25: "v25", v26: "v26", v27: "v27", v28: "v28", v29: "v29", v30: "v30", v31: "v31",
}

// formatVReg renders a register for Format(). Virtual (pre-regalloc)
// registers get a trailing '?', matching the other backends.
func formatVReg(r regalloc.VReg) string {
	if r.IsRealReg() {
		return regNames[r.RealReg()]
	}
	switch r.RegType() {
	case regalloc.RegTypeInt:
		return fmt.Sprintf("x%d?", r.ID())
	case regalloc.RegTypeFloat:
		return fmt.Sprintf("f%d?", r.ID())
	case regalloc.RegTypeVec:
		return fmt.Sprintf("v%d?", r.ID())
	default:
		panic(fmt.Sprintf("BUG: invalid register type: %d for %s", r.RegType(), r))
	}
}

// regNumberInEncoding maps a RealReg to the 5-bit field the instruction
// encoding carries. Both files are numbered 0..31 independently.
var regNumberInEncoding = func() (ret [v31 + 1]uint32) {
	for i := 0; i < 32; i++ {
		ret[int(x0)+i] = uint32(i)
		ret[int(f0)+i] = uint32(i)
		ret[int(v0)+i] = uint32(i)
	}
	return
}()
