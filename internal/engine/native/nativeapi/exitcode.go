package nativeapi

// ExitCode is an exit code of an execution of a function.
type ExitCode uint32

const (
	ExitCodeOK ExitCode = iota
	ExitCodeGrowStack
	ExitCodeGrowMemory
	ExitCodeUnreachable
	ExitCodeMemoryOutOfBounds
	// ExitCodeCallGoModuleFunction is an exit code for a call to an api.GoModuleFunction.
	ExitCodeCallGoModuleFunction
	// ExitCodeCallGoFunction is an exit code for a call to an api.GoFunction.
	ExitCodeCallGoFunction
	ExitCodeTableOutOfBounds
	ExitCodeIndirectCallNullPointer
	ExitCodeIndirectCallTypeMismatch
	ExitCodeIntegerDivisionByZero
	ExitCodeIntegerOverflow
	ExitCodeInvalidConversionToInteger
	// ExitCodeCheckModuleExitCode is how a preemption check returns to Go when the runtime asked
	// the goroutine to stop (see ssa.OpcodePreemptRequested): Go yields there, and unwinds the
	// call instead of resuming it if the module was closed or the call's context is done.
	ExitCodeCheckModuleExitCode
	ExitCodeCallListenerBefore
	ExitCodeCallListenerAfter
	ExitCodeCallGoModuleFunctionWithListener
	ExitCodeCallGoFunctionWithListener
	ExitCodeTableGrow
	ExitCodeRefFunc
	ExitCodeMemoryWait32
	ExitCodeMemoryWait64
	ExitCodeMemoryNotify
	ExitCodeUnalignedAtomic
	// ExitCodeThrowAlloc is the first phase of wasm throw: Go allocates the
	// Exception heap object (with Params sized to the tag's param count) and
	// writes its Params data pointer to execCtx.exceptionParamsPtr.
	// Compiled code then stores params directly into the Exception.Params slice,
	// followed by ExitCodeThrow to search for a matching handler.
	ExitCodeThrowAlloc
	// ExitCodeThrow is the shared throw/throw_ref exit code.
	// The exnref is passed on the stack. The handler searches for a
	// matching catch clause and restores the stack checkpoint.
	ExitCodeThrow
	// ExitCodeNullReference is an exit code for a null reference trap (throw_ref with null exnref).
	ExitCodeNullReference
	// ExitCodeGCCheck is the shared exit code for the GC proposal's runtime type checks: ref.test,
	// ref.cast, and the subtype-aware call_indirect check. Which one is meant is the trampoline's mode
	// operand; see gcCheckMode.
	ExitCodeGCCheck
	exitCodeMax
)

const ExitCodeMask = 0xff

// String implements fmt.Stringer.
func (e ExitCode) String() string {
	switch e {
	case ExitCodeOK:
		return "ok"
	case ExitCodeGrowStack:
		return "grow_stack"
	case ExitCodeCallGoModuleFunction:
		return "call_go_module_function"
	case ExitCodeCallGoFunction:
		return "call_go_function"
	case ExitCodeUnreachable:
		return "unreachable"
	case ExitCodeMemoryOutOfBounds:
		return "memory_out_of_bounds"
	case ExitCodeUnalignedAtomic:
		return "unaligned_atomic"
	case ExitCodeTableOutOfBounds:
		return "table_out_of_bounds"
	case ExitCodeIndirectCallNullPointer:
		return "indirect_call_null_pointer"
	case ExitCodeIndirectCallTypeMismatch:
		return "indirect_call_type_mismatch"
	case ExitCodeIntegerDivisionByZero:
		return "integer_division_by_zero"
	case ExitCodeIntegerOverflow:
		return "integer_overflow"
	case ExitCodeInvalidConversionToInteger:
		return "invalid_conversion_to_integer"
	case ExitCodeCheckModuleExitCode:
		return "check_module_exit_code"
	case ExitCodeCallListenerBefore:
		return "call_listener_before"
	case ExitCodeCallListenerAfter:
		return "call_listener_after"
	case ExitCodeCallGoModuleFunctionWithListener:
		return "call_go_module_function_with_listener"
	case ExitCodeCallGoFunctionWithListener:
		return "call_go_function_with_listener"
	case ExitCodeGrowMemory:
		return "grow_memory"
	case ExitCodeTableGrow:
		return "table_grow"
	case ExitCodeRefFunc:
		return "ref_func"
	case ExitCodeMemoryWait32:
		return "memory_wait32"
	case ExitCodeMemoryWait64:
		return "memory_wait64"
	case ExitCodeMemoryNotify:
		return "memory_notify"
	case ExitCodeThrowAlloc:
		return "throw_alloc"
	case ExitCodeThrow:
		return "throw"
	case ExitCodeNullReference:
		return "null_reference"
	case ExitCodeGCCheck:
		return "gc_check"
	}
	panic("TODO")
}

func ExitCodeCallGoModuleFunctionWithIndex(index int, withListener bool) ExitCode {
	if withListener {
		return ExitCodeCallGoModuleFunctionWithListener | ExitCode(index<<8)
	}
	return ExitCodeCallGoModuleFunction | ExitCode(index<<8)
}

func ExitCodeCallGoFunctionWithIndex(index int, withListener bool) ExitCode {
	if withListener {
		return ExitCodeCallGoFunctionWithListener | ExitCode(index<<8)
	}
	return ExitCodeCallGoFunction | ExitCode(index<<8)
}

func GoFunctionIndexFromExitCode(exitCode ExitCode) int {
	return int(exitCode >> 8)
}
