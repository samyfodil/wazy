//go:build riscv64

package native

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/isa/riscv64"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

func newMachine() backend.Machine {
	return riscv64.NewBackend()
}

// unwindStack unwinds the stack, appending return addresses to the slice. The
// implementation must agree with the frame layout the prologue builds.
func unwindStack(sp, fp, top uintptr, returnAddresses []uintptr) []uintptr {
	return riscv64.UnwindStack(sp, fp, top, returnAddresses)
}

// unwindStackForThrow is the exception-handling variant of unwindStack; see
// nativeapi.ThrowFrame.
func unwindStackForThrow(sp, _, top uintptr, frames []nativeapi.ThrowFrame) []nativeapi.ThrowFrame {
	return riscv64.UnwindStackForThrow(sp, top, frames)
}

// firstReturnAddress returns the return address of the frame at (sp, top),
// recovered in O(1) by chasing one frame_size word from sp. Used at
// try_table-enter time to capture the enter-continuation.
func firstReturnAddress(sp, _, top uintptr) uintptr {
	return riscv64.FirstReturnAddress(sp, top)
}

// resolveThrowTransferSPFP resolves the SP/FP afterThrowTransferEntrypoint
// needs to resume inside the frame identified by fr. unwindStackForThrow
// already computed fr.SP by chasing frame_size per frame, and there is no
// frame-pointer chain to recover, so FP is unused here as on arm64.
func resolveThrowTransferSPFP(fr nativeapi.ThrowFrame, _ int64) (sp, fp uintptr) {
	return fr.SP, 0
}

// afterThrowTransferEntrypoint transfers control to a throw's matched landing
// pad, restoring callee-saved registers from execCtx.savedRegisters first. The
// restore is inlined in abi_entry_riscv64.s rather than reached through
// restoreFn; see that file for why.
func afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, sp, _, targetPC uintptr) {
	rawAfterThrowTransferEntrypoint(restoreFn, executionContextPtr, sp, targetPC)
}

// goCallStackView returns a view of the stack laid out by
// CompileGoFunctionTrampoline before a Go call.
func goCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	return riscv64.GoCallStackView(stackPointerBeforeGoCall)
}

// adjustClonedStack adjusts absolute addresses in the stack after it grows.
// Nothing to do: like arm64, this backend stores no absolute frame pointers --
// the unwinder chases relative frame_size words instead.
func adjustClonedStack(oldsp, oldTop, sp, fp, top uintptr) {}
