#include "funcdata.h"
#include "textflag.h"

// entrypoint(preambleExecutable, functionExecutable *byte, executionContextPtr uintptr, moduleContextPtr *byte, paramResultPtr *uint64, goAllocatedStackSlicePtr uintptr
TEXT ·entrypoint(SB), NOSPLIT|NOFRAME, $0-48
	MOVQ preambleExecutable+0(FP), R11
	MOVQ functionExectuable+8(FP), R14
	MOVQ executionContextPtr+16(FP), AX       // First argument is passed in AX.
	MOVQ moduleContextPtr+24(FP), BX          // Second argument is passed in BX.
	MOVQ paramResultSlicePtr+32(FP), R12
	MOVQ goAllocatedStackSlicePtr+40(FP), R13
	JMP  R11

// afterGoFunctionCallEntrypoint(executable *byte, executionContextPtr uintptr, stackPointer, framePointer uintptr)
TEXT ·afterGoFunctionCallEntrypoint(SB), NOSPLIT|NOFRAME, $0-32
	MOVQ executable+0(FP), CX
	MOVQ executionContextPtr+8(FP), AX // First argument is passed in AX.

	// Save the stack pointer and frame pointer.
	MOVQ BP, 16(AX) // 16 == ExecutionContextOffsetOriginalFramePointer
	MOVQ SP, 24(AX) // 24 == ExecutionContextOffsetOriginalStackPointer

	// Then set the stack pointer and frame pointer to the values we got from the Go runtime.
	MOVQ framePointer+24(FP), BP

	// WARNING: do not update SP before BP, because the Go translates (FP) as (SP) + 8.
	MOVQ stackPointer+16(FP), SP

	JMP CX

// afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, stackPointer, framePointer, targetPC uintptr)
//
// Like afterGoFunctionCallEntrypoint above, except it additionally restores
// the callee-saved registers (RDX, R12-R15, XMM8-15 -- amd64's
// calleeSavedVRegs, backend/isa/amd64/abi_go_call.go) from
// wazevo.executionContext.savedRegisters before jumping to targetPC -- see
// the Go declaration's comment for why.
//
// This is inlined directly (fixed offsets matching
// registerSaveRestoreSlots-equivalent layout for amd64: each register gets
// its own 16-byte-aligned slot starting at
// ExecutionContextOffsetSavedRegistersBegin=96, see
// saveRegistersInExecutionContext/restoreRegistersInExecutionContext)
// rather than calling restoreFn as a separate leaf function: an internal
// CALL from this NOSPLIT/NOFRAME entrypoint (reached directly from Go via
// go:linkname, not through a normal Go call frame) corrupted AX before the
// final JMP in testing, for reasons not fully root-caused before this was
// written -- inlining sidesteps it entirely and is no less correct, since
// the offsets are fixed and shared with saveRegistersInExecutionContext by
// construction. restoreFn is accepted but unused; both ISAs now inline the
// restore (arm64 for a related SPWRITE-traceback reason), so the
// CompileThrowTransferRegisterRestore blob is currently unused by the
// transfer -- the argument and blob are kept only so the Go-level
// signature/plumbing stays uniform across ISAs.
TEXT ·afterThrowTransferEntrypoint(SB), NOSPLIT|NOFRAME, $0-40
	MOVQ executionContextPtr+8(FP), AX // First argument is passed in AX.
	MOVQ stackPointer+16(FP), R9
	MOVQ framePointer+24(FP), R10
	MOVQ targetPC+32(FP), R11

	// Save the stack pointer and frame pointer, exactly like
	// afterGoFunctionCallEntrypoint, so a subsequent Go exit from the
	// resumed code can still find its way back to the Go runtime's own stack.
	MOVQ BP, 16(AX)
	MOVQ SP, 24(AX)

	// Restore callee-saved registers from execCtx.savedRegisters (AX).
	MOVQ 96(AX), DX
	MOVQ 112(AX), R12
	MOVQ 128(AX), R13
	MOVQ 144(AX), R14
	MOVQ 160(AX), R15
	MOVOU 176(AX), X8
	MOVOU 192(AX), X9
	MOVOU 208(AX), X10
	MOVOU 224(AX), X11
	MOVOU 240(AX), X12
	MOVOU 256(AX), X13
	MOVOU 272(AX), X14
	MOVOU 288(AX), X15

	// Then set the frame/stack pointer to the recovered values (BP before
	// SP, matching afterGoFunctionCallEntrypoint's convention above).
	MOVQ R10, BP
	MOVQ R9, SP

	JMP R11
