package native

import (
	_ "unsafe" // for go:linkname
)

// runtime_entersyscall and runtime_exitsyscall mark this goroutine as being in a
// syscall, which detaches its P. That lets the Go runtime's sysmon retake the P
// after ~10ms and schedule other goroutines on it even though this one is off in
// a long-running native loop that never yields.
//
// The same trick gvisor uses. runtime/proc.go's comment on entersyscallblock
// records the runtime team's commitment not to change these entry points'
// signatures, which is what makes linknaming them safe to depend on.

//go:linkname runtime_entersyscall runtime.entersyscall
func runtime_entersyscall()

//go:linkname runtime_exitsyscall runtime.exitsyscall
func runtime_exitsyscall()

type (
	// entrypointFn enters compiled code at a function's entry preamble.
	entrypointFn func(preambleExecutable, functionExecutable *byte, executionContextPtr uintptr, moduleContextPtr *byte, paramResultStackPtr *uint64, goAllocatedStackSlicePtr uintptr)
	// afterGoFunctionCallEntrypointFn re-enters compiled code after a Go-side
	// dispatcher iteration: a host call, a stack grow, the module-closed check.
	afterGoFunctionCallEntrypointFn func(executable *byte, executionContextPtr uintptr, stackPointer, framePointer uintptr)
	// afterThrowTransferEntrypointFn re-enters compiled code at a throw's matched
	// landing pad; see (*callEngine).handleThrow.
	afterThrowTransferEntrypointFn func(restoreFn *byte, executionContextPtr uintptr, sp, fp, targetPC uintptr)
)

// entrypoints returns the three ways into compiled code for a module compiled
// with the given ensureTermination.
//
// Only ensureTermination needs the syscall bracketing. Its checks read
// ModuleInstance.Closed directly instead of calling back into Go, so the
// watchdog goroutine that sets that flag has to be able to run while wasm is
// running -- which it cannot if this goroutine is holding the only P. With
// ensureTermination off nothing has to run concurrently with wasm, and the
// bracketing would be pure overhead on every call and every host-call return.
func entrypoints(ensureTermination bool) (entrypointFn, afterGoFunctionCallEntrypointFn, afterThrowTransferEntrypointFn) {
	if ensureTermination {
		return entrypointInSyscall, afterGoFunctionCallEntrypointInSyscall, afterThrowTransferEntrypointInSyscall
	}
	return entrypointAsm, afterGoFunctionCallEntrypointAsm, afterThrowTransferEntrypointAsm
}

// The three *InSyscall wrappers are their Asm counterparts bracketed with
// entersyscall/exitsyscall. They are nosplit because entersyscall sets
// throwsplit: the stack must not grow between the two calls.

//go:nosplit
func entrypointInSyscall(preambleExecutable, functionExecutable *byte, executionContextPtr uintptr, moduleContextPtr *byte, paramResultStackPtr *uint64, goAllocatedStackSlicePtr uintptr) {
	runtime_entersyscall()
	entrypointAsm(preambleExecutable, functionExecutable, executionContextPtr, moduleContextPtr, paramResultStackPtr, goAllocatedStackSlicePtr)
	runtime_exitsyscall()
}

//go:nosplit
func afterGoFunctionCallEntrypointInSyscall(executable *byte, executionContextPtr uintptr, stackPointer, framePointer uintptr) {
	runtime_entersyscall()
	afterGoFunctionCallEntrypointAsm(executable, executionContextPtr, stackPointer, framePointer)
	runtime_exitsyscall()
}

//go:nosplit
func afterThrowTransferEntrypointInSyscall(restoreFn *byte, executionContextPtr uintptr, sp, fp, targetPC uintptr) {
	runtime_entersyscall()
	afterThrowTransferEntrypointAsm(restoreFn, executionContextPtr, sp, fp, targetPC)
	runtime_exitsyscall()
}
