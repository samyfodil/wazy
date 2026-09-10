//go:build riscv64

#include "funcdata.h"
#include "textflag.h"

// See CompileEntryPreamble for what this hands over and where.
//
// Register choices worth noting: X5 (t0) carries the preamble's address rather
// than X31, because the Go assembler reserves X31 as its own temporary; and
// X27 is never touched, because that is g.
TEXT ·entrypoint(SB), NOSPLIT|NOFRAME, $0-48
	MOV preambleExecutable+0(FP), X5       // t0: jump target
	MOV functionExecutable+8(FP), X22      // s6
	MOV executionContextPtr+16(FP), X10    // a0
	MOV moduleContextPtr+24(FP), X11       // a1
	MOV paramResultSlicePtr+32(FP), X18    // s2
	MOV goAllocatedStackSlicePtr+40(FP), X20 // s4
	JMP (X5)

// afterGoFunctionCallEntrypoint re-enters compiled code after a Go call, e.g.
// once the wasm stack has been grown.
//
// Unlike arm64, SP is an ordinary register on RISC-V, so Go's stack pointer is
// stored straight into the execution context with no scratch in between.
TEXT ·afterGoFunctionCallEntrypoint(SB), NOSPLIT|NOFRAME, $0-32
	MOV goCallReturnAddress+0(FP), X19     // s3: jump target
	MOV executionContextPtr+8(FP), X10     // a0
	MOV stackPointer+16(FP), X18           // s2

	// Save Go's frame pointer (X8), stack pointer (X2) and return address
	// (X1) into native.executionContext, so a later exit can restore them.
	MOV X8, 16(X10)   // ExecutionContextOffsetOriginalFramePointer
	MOV X2, 24(X10)   // ExecutionContextOffsetOriginalStackPointer
	MOV X1, 32(X10)   // ExecutionContextOffsetGoReturnAddress

	// Switch to the wasm stack and resume.
	MOV X18, X2
	JMP (X19)

// afterThrowTransferEntrypoint is afterGoFunctionCallEntrypoint plus a restore
// of the wasm callee-saved registers from
// native.executionContext.savedRegisters, before jumping to targetPC.
//
// targetPC is the matched try_table's enter-continuation, not a raw landing
// pad: resuming there re-establishes execCtx and dispatches through the
// compiled br_table, so the transfer arrives in the handler with exactly the
// register state the non-throwing path would have left. See
// (*callEngine).handleThrow.
//
// The restore is inlined rather than reached by calling the
// CompileThrowTransferRegisterRestore blob, for the same reason as on arm64: a
// call would leave this SP-writing NOSPLIT function on the stack as a
// non-innermost frame, which the Go runtime's traceback refuses to unwind
// through if a signal arrives while the blob runs. restoreFn is accepted and
// unused, keeping the Go-level signature uniform across backends.
//
// The offsets below match registerSaveRestoreSlots in abi_go_call.go:
// ExecutionContextOffsetSavedRegistersBegin is 96, and each slot is 8 bytes
// because every RV64G register is 64 bits -- there is no 128-bit vector file
// to widen them the way arm64's v-registers do.
TEXT ·afterThrowTransferEntrypoint(SB), NOSPLIT|NOFRAME, $0-32
	MOV executionContextPtr+8(FP), X10     // a0
	MOV stackPointer+16(FP), X6            // t1
	MOV targetPC+24(FP), X7                // t2

	MOV X8, 16(X10)
	MOV X2, 24(X10)
	MOV X1, 32(X10)

	// Restore the wasm callee-saved integer registers. None of these is
	// X10/X6/X7, so the arguments above survive, and X27 (g) is deliberately
	// absent: it belongs to the Go runtime and compiled code never clobbers it.
	MOV 96(X10), X8    // s0
	MOV 104(X10), X9   // s1
	MOV 112(X10), X18  // s2
	MOV 120(X10), X19  // s3
	MOV 128(X10), X20  // s4
	MOV 136(X10), X21  // s5
	MOV 144(X10), X22  // s6
	MOV 152(X10), X23  // s7
	MOV 160(X10), X24  // s8
	MOV 168(X10), X25  // s9
	MOV 176(X10), X26  // s10

	// ...and the callee-saved float registers, fs0-fs11.
	MOVD 184(X10), F8
	MOVD 192(X10), F9
	MOVD 200(X10), F18
	MOVD 208(X10), F19
	MOVD 216(X10), F20
	MOVD 224(X10), F21
	MOVD 232(X10), F22
	MOVD 240(X10), F23
	MOVD 248(X10), F24
	MOVD 256(X10), F25
	MOVD 264(X10), F26
	MOVD 272(X10), F27

	// Switch to the catching frame's stack and jump to its enter-continuation.
	MOV X6, X2
	JMP (X7)
