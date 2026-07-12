//go:build amd64

package wazevo

import (
	"github.com/samyfodil/wazy/internal/engine/wazevo/backend"
	"github.com/samyfodil/wazy/internal/engine/wazevo/backend/isa/amd64"
	"github.com/samyfodil/wazy/internal/engine/wazevo/wazevoapi"
)

func newMachine() backend.Machine {
	return amd64.NewBackend()
}

// unwindStack is a function to unwind the stack, and appends return addresses to `returnAddresses` slice.
// The implementation must be aligned with the ABI/Calling convention.
func unwindStack(sp, fp, top uintptr, returnAddresses []uintptr) []uintptr {
	return amd64.UnwindStack(sp, fp, top, returnAddresses)
}

// unwindStackForThrow is the exception-handling variant of unwindStack; see
// wazevoapi.ThrowFrame.
func unwindStackForThrow(_, fp, top uintptr, frames []wazevoapi.ThrowFrame) []wazevoapi.ThrowFrame {
	return amd64.UnwindStackForThrow(fp, top, frames)
}

// firstReturnAddress returns the return address of the frame at (sp, fp) --
// on amd64 recovered in O(1) from the RBP chain at fp. Used at
// try_table-enter time to capture the enter-continuation (activeCatchScope.resumePC);
// see call_engine.go's ExitCodeTryTableEnter.
func firstReturnAddress(_, fp, _ uintptr) uintptr {
	return amd64.FirstReturnAddress(fp)
}

// resolveThrowTransferSPFP resolves the SP/FP afterGoFunctionCallEntrypoint
// needs to resume directly inside the frame identified by fr. Frame 0 (the
// throw site) already has both known exactly (they're what a Go exit always
// records, set directly by the caller into fr.SP/fr.FP, so fr.SP != 0);
// every other frame only carries FP (=RBP) from the RBP-chain walk, so SP is
// computed as FP - frameSize, using the owning function's FrameSize recorded
// at compile time (backend.Machine.FrameSize / compiledModule.functionFrameSizes) --
// see UnwindStackForThrow's doc comment for why amd64 cannot recover SP
// directly during the walk itself.
func resolveThrowTransferSPFP(fr wazevoapi.ThrowFrame, frameSize int64) (sp, fp uintptr) {
	if fr.SP != 0 {
		return fr.SP, fr.FP
	}
	return fr.FP - uintptr(frameSize), fr.FP
}

// afterThrowTransferEntrypoint transfers control to a throw's matched landing
// pad, restoring callee-saved registers from execCtx.savedRegisters (via
// restoreFn) before jumping -- see (*callEngine).handleThrow and the
// backend.Machine.CompileThrowTransferRegisterRestore doc comment for why
// this, and not afterGoFunctionCallEntrypoint, is needed here.
func afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, sp, fp, targetPC uintptr) {
	rawAfterThrowTransferEntrypoint(restoreFn, executionContextPtr, sp, fp, targetPC)
}

// goCallStackView is a function to get a view of the stack before a Go call, which
// is the view of the stack allocated in CompileGoFunctionTrampoline.
func goCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	return amd64.GoCallStackView(stackPointerBeforeGoCall)
}

// adjustClonedStack is a function to adjust the stack after it is grown.
// More precisely, absolute addresses (frame pointers) in the stack must be adjusted.
func adjustClonedStack(oldsp, oldTop, sp, fp, top uintptr) {
	amd64.AdjustClonedStack(oldsp, oldTop, sp, fp, top)
}
