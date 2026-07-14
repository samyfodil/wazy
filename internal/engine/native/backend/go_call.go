package backend

import "github.com/samyfodil/wazy/internal/engine/native/ssa"

// StackBoundsCheckMarginBytes (H7 in OPTIMIZATIONS.md) is a constant amount
// of extra slack that every wasm function prologue's stack-bounds check
// demands, beyond what the function itself (its own frame plus the deepest
// transient call it makes) actually needs. It costs nothing at runtime: the
// prologue check (insertStackBoundsCheck, both ISAs) is a pure comparison
// against native.executionContext.stackBottomPtr -- it does not change how
// much stack a frame physically consumes, only how early/how large the
// underlying stack buffer grows. Reserving MARGIN is implemented by biasing
// the stored stackBottomPtr check-limit up by MARGIN (see call_engine.go)
// rather than by adding MARGIN into every function's requiredStackSize()
// immediate; requiredStackSize() (isa/amd64/machine.go, isa/arm64/
// machine.go) therefore excludes MARGIN, which keeps that hot-path prologue
// immediate small. This just shifts *which side* of the check inequality
// carries MARGIN and *when* the existing stack-grow mechanism fires; it
// adds no instructions and no per-call cost to ordinary wasm execution.
//
// What it buys: CompileGoFunctionTrampoline, for a host-function signature
// whose entire Go argument/result slice (plus its small fixed bookkeeping
// overhead) fits within MARGIN, can skip its own stack-bounds check/
// grow-call sequence entirely -- see the goSliceSizeAligned/goCallStackSize
// comparisons against this constant at the CompileGoFunctionTrampoline call
// sites in isa/amd64/abi_go_call.go and isa/arm64/abi_go_call.go. That
// removes ~4 instructions (an add/sub, a cmp, a conditional branch, and the
// reverse add/sub) from the hot path of every such host call.
//
// Invariant that makes this safe (stated once here; both ISAs rely on it).
// MARGIN is reserved by biasing the stored stackBottomPtr check-limit up by
// MARGIN (set in call_engine.go), not by adding MARGIN into
// requiredStackSize() (isa/amd64/machine.go and isa/arm64/machine.go, which
// therefore exclude it) -- only which side of the check inequality below
// carries MARGIN has moved; the inequality itself, and everything the rest
// of this proof derives from it, is unchanged:
//
//  1. Every wasm function's prologue check verifies that
//     frameBase - stackBottomPtr >= maxRequiredStackSizeForCalls +
//     frameSize() + MARGIN + <fixed per-ISA bookkeeping, 32 bytes on both
//     amd64 and arm64: return-address + saved-frame-pointer/unwind slots>.
//     (frameBase is the value of RBP/SP right after the prologue has pushed
//     the caller's frame-pointer/return-address bookkeeping, i.e. exactly
//     where the check itself runs.)
//
//  2. native frames are fixed-size: once a function's prologue check
//     passes, its body only ever uses up to frameSize() bytes for its own
//     spill/clobbered-register area (a compile-time constant, allocated
//     once), plus up to maxRequiredStackSizeForCalls bytes transiently for
//     whichever single call it is currently making (outgoing stack args +
//     that callee's own entry bookkeeping). Nothing else in a wasm frame's
//     body consumes stack -- there is no dynamic (loop- or data-dependent)
//     stack growth within a single frame.
//
//  3. Consequently, at the point of any call this function makes --
//     whether to another wasm function or to a go-call trampoline -- the
//     callee's own frame base is guaranteed to sit at least MARGIN bytes
//     above stackBottomPtr, regardless of call depth. If the callee is
//     itself a wasm function, it independently re-establishes the very same
//     invariant via its own prologue check before making any further calls
//     of its own, so the property holds transitively no matter how deep the
//     call chain gets before a host call actually happens.
//
//     Derivation (amd64 constants; arm64 differs only in which fixed
//     constant plays which role, not in the shape of the argument): let
//     actualFrameBytes be a frame's real physical stack usage at the call
//     site (<= frameSize(), since clobbered int registers are pushed 8
//     bytes each while frameSize() conservatively charges 16), and let
//     stackSlotSize be the outgoing stack-argument bytes for this
//     particular call (<= maxRequiredStackSizeForCalls - 16, see
//     prepareCall in isa/{amd64,arm64}/machine.go). Then the callee's frame
//     base, measured from stackBottomPtr, is:
//     calleeBase - stackBottomPtr
//     = (callerFrameBase - stackBottomPtr) - actualFrameBytes - stackSlotSize - 16
//     >= requiredStackSize - frameSize() - stackSlotSize - 16   (check passed)
//     = maxRequiredStackSizeForCalls + MARGIN + 32 - stackSlotSize - 16 + (frameSize()-actualFrameBytes)
//     >= MARGIN + 32 + (frameSize()-actualFrameBytes)   (since maxRequiredStackSizeForCalls - stackSlotSize >= 16)
//     >= MARGIN.
//     Without MARGIN (i.e. today, pre-H7) this same derivation bottoms out
//     at exactly 32 bytes of guaranteed slack at any callee's entry --
//     enough for the callee's own prologue bookkeeping, but never enough to
//     let a callee skip its own check. Adding MARGIN is what turns that
//     bound into something a small trampoline can actually rely on.
//
//  4. The stack-grow path (nativeapi.ExitCodeGrowStack), on the rare
//     occasion some deeper frame's own prologue check does trip, allocates
//     a fresh (larger) buffer and relocates every live frame onto it
//     (cloneStack/adjustClonedStack). This doesn't threaten the invariant:
//     the invariant is a per-check property (each frame's prologue check
//     re-verifies it against the *current* stackBottomPtr/buffer every time
//     that frame is entered), not something that has to be preserved by the
//     relocation itself. A relocated frame's saved registers, return
//     addresses, and frame-pointer chain are adjusted to the new buffer by
//     adjustClonedStack, but MARGIN plays no part in that adjustment --
//     it only ever matters at the moment a prologue check (or an elided
//     go-call trampoline standing in for one) runs.
//
// MARGIN's value (512 bytes) is picked to cover the Go argument/result
// slice of the overwhelming majority of host function signatures: on amd64
// the trampoline's own check would otherwise ask for goSliceSizeAligned+8,
// so MARGIN=512 covers any signature whose (params, results) max, rounded
// up to 8-byte slots and 16-byte aligned, is <=504 bytes -- i.e. up to 62
// 8-byte parameters/results on the larger side of the signature (arm64's
// goCallStackSize+16 gives the same bound). Real host functions -- including
// the largest WASI syscalls such as fd_write/path_open, which have at most
// ~9 params -- use a small handful of slots, so 512 bytes covers them with
// a wide safety margin; only deliberately pathological, huge-arity
// signatures fall back to the full check (still correct, just not elided).
const StackBoundsCheckMarginBytes = 512

// LeafStackCheckHeadroomBytes is slack kept below MARGIN when eliding a leaf
// function's prologue stack-bounds check (both ISAs). A leaf makes no calls of
// any kind (so maxRequiredStackSizeForCalls stays 0) and thus its whole frame
// sits within the MARGIN its caller already reserved below it (invariant point
// 3 above). This headroom additionally covers the return-address push of any
// Go-exiting trampoline (interrupt check, memory.grow, ...) the leaf might still
// invoke -- those exit to Go rather than recursing on the wasm stack, so they
// consume only a few bytes of it.
const LeafStackCheckHeadroomBytes = 128

// GoFunctionCallRequiredStackSize returns the size of the stack required for the Go function call.
// argBegin is the index of the first argument in the signature which is not either execution context or module context.
func GoFunctionCallRequiredStackSize(sig *ssa.Signature, argBegin int) (ret, retUnaligned int64) {
	var paramNeededInBytes, resultNeededInBytes int64
	for _, p := range sig.Params[argBegin:] {
		s := int64(p.Size())
		if s < 8 {
			s = 8 // We use uint64 for all basic types, except SIMD v128.
		}
		paramNeededInBytes += s
	}
	for _, r := range sig.Results {
		s := int64(r.Size())
		if s < 8 {
			s = 8 // We use uint64 for all basic types, except SIMD v128.
		}
		resultNeededInBytes += s
	}

	if paramNeededInBytes > resultNeededInBytes {
		ret = paramNeededInBytes
	} else {
		ret = resultNeededInBytes
	}
	retUnaligned = ret
	// Align to 16 bytes.
	ret = (ret + 15) &^ 15
	return
}
