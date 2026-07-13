//go:build !(amd64 || arm64)

package native

import (
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

func newMachine() backend.Machine {
	panic("unsupported architecture")
}

// unwindStack is a function to unwind the stack, and appends return addresses to `returnAddresses` slice.
// The implementation must be aligned with the ABI/Calling convention.
func unwindStack(sp, fp, top uintptr, returnAddresses []uintptr) []uintptr {
	panic("unsupported architecture")
}

// unwindStackForThrow is the exception-handling variant of unwindStack; see
// nativeapi.ThrowFrame.
func unwindStackForThrow(sp, fp, top uintptr, frames []nativeapi.ThrowFrame) []nativeapi.ThrowFrame {
	panic("unsupported architecture")
}

// firstReturnAddress captures a frame's return address for the try_table
// enter-continuation; see the arm64/amd64 implementations.
func firstReturnAddress(sp, fp, top uintptr) uintptr {
	panic("unsupported architecture")
}

// resolveThrowTransferSPFP resolves the SP/FP for a throw-time control
// transfer; see the arm64/amd64 implementations.
func resolveThrowTransferSPFP(fr nativeapi.ThrowFrame, frameSize int64) (sp, fp uintptr) {
	panic("unsupported architecture")
}

// afterThrowTransferEntrypoint transfers control to a throw's matched
// landing pad; see the arm64/amd64 implementations.
func afterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, sp, fp, targetPC uintptr) {
	panic("unsupported architecture")
}

// goCallStackView is a function to get a view of the stack before a Go call, which
// is the view of the stack allocated in CompileGoFunctionTrampoline.
func goCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	panic("unsupported architecture")
}

// adjustClonedStack is a function to adjust the stack after it is grown.
// More precisely, absolute addresses (frame pointers) in the stack must be adjusted.
func adjustClonedStack(oldsp, oldTop, sp, fp, top uintptr) {
	panic("unsupported architecture")
}
