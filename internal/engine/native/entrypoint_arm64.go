package native

import _ "unsafe"

// entrypointAsm is implemented by the backend.
//
//go:linkname entrypointAsm github.com/samyfodil/wazy/internal/engine/native/backend/isa/arm64.entrypoint
func entrypointAsm(preambleExecutable, functionExecutable *byte, executionContextPtr uintptr, moduleContextPtr *byte, paramResultStackPtr *uint64, goAllocatedStackSlicePtr uintptr)

// entrypointAsm is implemented by the backend.
//
//go:linkname afterGoFunctionCallEntrypointAsm github.com/samyfodil/wazy/internal/engine/native/backend/isa/arm64.afterGoFunctionCallEntrypoint
func afterGoFunctionCallEntrypointAsm(executable *byte, executionContextPtr uintptr, stackPointer, framePointer uintptr)

// rawAfterThrowTransferEntrypoint is implemented by the backend; see
// arm64.afterThrowTransferEntrypoint's doc comment.
//
//go:linkname rawAfterThrowTransferEntrypoint github.com/samyfodil/wazy/internal/engine/native/backend/isa/arm64.afterThrowTransferEntrypoint
func rawAfterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, stackPointer, targetPC uintptr)
