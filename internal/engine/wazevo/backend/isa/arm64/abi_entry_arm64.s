//go:build arm64

#include "funcdata.h"
#include "textflag.h"

// See the comments on EmitGoEntryPreamble for what this function is supposed to do.
TEXT ·entrypoint(SB), NOSPLIT|NOFRAME, $0-48
	MOVD preambleExecutable+0(FP), R27
	MOVD functionExectuable+8(FP), R24
	MOVD executionContextPtr+16(FP), R0
	MOVD moduleContextPtr+24(FP), R1
	MOVD paramResultSlicePtr+32(FP), R19
	MOVD goAllocatedStackSlicePtr+40(FP), R26
	JMP  (R27)

TEXT ·afterGoFunctionCallEntrypoint(SB), NOSPLIT|NOFRAME, $0-32
	MOVD goCallReturnAddress+0(FP), R20
	MOVD executionContextPtr+8(FP), R0
	MOVD stackPointer+16(FP), R19

	// Save the current FP(R29), SP and LR(R30) into the wazevo.executionContext (stored in R0).
	MOVD R29, 16(R0) // Store FP(R29) into [RO, #ExecutionContextOffsets.OriginalFramePointer]
	MOVD RSP, R27    // Move SP to R27 (temporary register) since SP cannot be stored directly in str instructions.
	MOVD R27, 24(R0) // Store R27 into [RO, #ExecutionContextOffsets.OriginalFramePointer]
	MOVD R30, 32(R0) // Store R30 into [R0, #ExecutionContextOffsets.GoReturnAddress]

	// Load the new stack pointer (which sits somewhere in Go-allocated stack) into SP.
	MOVD R19, RSP
	JMP  (R20)

// afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, stackPointer, targetPC uintptr)
//
// Like afterGoFunctionCallEntrypoint above, except it additionally restores
// the callee-saved registers (x19-x26, x28, v18-v31 -- calleeSavedRegistersSorted,
// backend/isa/arm64/abi_go_call.go) from wazevo.executionContext.savedRegisters
// before jumping to targetPC. targetPC is the matched try_table's
// enter-continuation (the compiled reload + br_table right after the enter
// trampoline call), NOT a raw landing pad: resuming there re-establishes
// execCtx (the reload the handler inherits, e.g. `ldr x8,[sp,#16]` on arm64)
// and dispatches through the compiled br_table, so the transfer lands in the
// handler with exactly the register/reload state the non-throwing path
// leaves -- see (*callEngine).handleThrow and activeCatchScope.resumePC for
// why resuming at the raw handler instead corrupted execCtx on arm64.
//
// The register restore is inlined here (fixed offsets matching
// registerSaveRestoreSlots' layout -- ExecutionContextOffsetSavedRegistersBegin=96,
// int pairs 8 bytes apart, the x28 singleton at 160, then the 128-bit
// v-registers 16 bytes apart starting at 176) rather than reached via BL to
// the CompileThrowTransferRegisterRestore blob: a BL would leave this
// SP-writing NOSPLIT entrypoint on the stack as a non-innermost frame, which
// the Go runtime's traceback refuses to unwind through ("unexpected SPWRITE
// function") if a signal fires while the blob runs. Inlining keeps this a
// single straight-line SP-writing entrypoint, innermost-only exactly like
// afterGoFunctionCallEntrypoint. restoreFn is accepted but unused, kept so
// the Go-level signature stays uniform with amd64 (which inlines for the
// same reason).
TEXT ·afterThrowTransferEntrypoint(SB), NOSPLIT|NOFRAME, $0-32
	MOVD executionContextPtr+8(FP), R0
	MOVD stackPointer+16(FP), R10
	MOVD targetPC+24(FP), R11

	// Save the current (Go) FP(R29), SP and LR(R30) into the
	// wazevo.executionContext (R0), exactly like afterGoFunctionCallEntrypoint,
	// so a subsequent Go exit from the resumed code can still find its way
	// back to the Go runtime's own stack.
	MOVD R29, 16(R0)
	MOVD RSP, R27
	MOVD R27, 24(R0)
	MOVD R30, 32(R0)

	// Restore the wasm callee-saved registers from execCtx.savedRegisters
	// (R0). None of the restored registers is R0/R10/R11, so the arguments
	// survive; x28 is named g in the Go assembler. Kept last-ish so the g
	// (R28)-clobbered window before entering wasm is minimal.
	LDP   96(R0), (R19, R20)
	LDP   112(R0), (R21, R22)
	LDP   128(R0), (R23, R24)
	LDP   144(R0), (R25, R26)
	FLDPQ 176(R0), (F18, F19)
	FLDPQ 208(R0), (F20, F21)
	FLDPQ 240(R0), (F22, F23)
	FLDPQ 272(R0), (F24, F25)
	FLDPQ 304(R0), (F26, F27)
	FLDPQ 336(R0), (F28, F29)
	FLDPQ 368(R0), (F30, F31)
	MOVD  160(R0), g

	// Switch to the catching frame's stack and jump to its enter-continuation.
	MOVD R10, RSP
	JMP  (R11)
