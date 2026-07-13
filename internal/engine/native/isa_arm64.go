//go:build arm64

package native

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/isa/arm64"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

func newMachine() backend.Machine {
	return arm64.NewBackend()
}

// unwindStack is a function to unwind the stack, and appends return addresses to `returnAddresses` slice.
// The implementation must be aligned with the ABI/Calling convention.
func unwindStack(sp, fp, top uintptr, returnAddresses []uintptr) []uintptr {
	return arm64.UnwindStack(sp, fp, top, returnAddresses)
}

// unwindStackForThrow is the exception-handling variant of unwindStack; see
// nativeapi.ThrowFrame.
func unwindStackForThrow(sp, _, top uintptr, frames []nativeapi.ThrowFrame) []nativeapi.ThrowFrame {
	return arm64.UnwindStackForThrow(sp, top, frames)
}

// firstReturnAddress returns the return address of the frame at (sp, top) --
// on arm64 recovered in O(1) by chasing one frame_size word from sp. Used at
// try_table-enter time to capture the enter-continuation (activeCatchScope.resumePC);
// see call_engine.go's ExitCodeTryTableEnter.
func firstReturnAddress(sp, _, top uintptr) uintptr {
	return arm64.FirstReturnAddress(sp, top)
}

// resolveThrowTransferSPFP resolves the SP/FP afterThrowTransferEntrypoint
// needs to resume directly inside the frame identified by fr. On arm64,
// unwindStackForThrow already computed fr.SP directly (self-contained,
// per-frame frame_size chasing); FP is unused by arm64's transfer entrypoints
// (see abi_entry_arm64.s), so it's ignored here too.
func resolveThrowTransferSPFP(fr nativeapi.ThrowFrame, _ int64) (sp, fp uintptr) {
	return fr.SP, 0
}

// afterThrowTransferEntrypoint transfers control to a throw's matched landing
// pad, restoring callee-saved registers from execCtx.savedRegisters (via
// restoreFn) before jumping -- see (*callEngine).handleThrow and the
// backend.Machine.CompileThrowTransferRegisterRestore doc comment for why
// this, and not afterGoFunctionCallEntrypoint, is needed here. FP is unused
// on arm64 (see rawAfterThrowTransferEntrypoint in entrypoint_arm64.go).
func afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, sp, _, targetPC uintptr) {
	rawAfterThrowTransferEntrypoint(restoreFn, executionContextPtr, sp, targetPC)
}

// goCallStackView is a function to get a view of the stack before a Go call, which
// is the view of the stack allocated in CompileGoFunctionTrampoline.
func goCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	return arm64.GoCallStackView(stackPointerBeforeGoCall)
}

// adjustClonedStack is a function to adjust the stack after it is grown.
// More precisely, absolute addresses (frame pointers) in the stack must be adjusted.
func adjustClonedStack(oldsp, oldTop, sp, fp, top uintptr) {
	// TODO: currently, the frame pointers are not used, and saved old sps are relative to the current stack pointer,
	//  so no need to adjustment on arm64. However, when we make it absolute, which in my opinion is better perf-wise
	//  at the expense of slightly costly stack growth, we need to adjust the pushed frame pointers.
}
