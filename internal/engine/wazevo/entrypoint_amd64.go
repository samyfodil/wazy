package wazevo

import _ "unsafe"

// entrypoint is implemented by the backend.
//
//go:linkname entrypoint github.com/samyfodil/wazy/internal/engine/wazevo/backend/isa/amd64.entrypoint
func entrypoint(preambleExecutable, functionExecutable *byte, executionContextPtr uintptr, moduleContextPtr *byte, paramResultStackPtr *uint64, goAllocatedStackSlicePtr uintptr)

// entrypoint is implemented by the backend.
//
//go:linkname afterGoFunctionCallEntrypoint github.com/samyfodil/wazy/internal/engine/wazevo/backend/isa/amd64.afterGoFunctionCallEntrypoint
func afterGoFunctionCallEntrypoint(executable *byte, executionContextPtr uintptr, stackPointer, framePointer uintptr)

// rawAfterThrowTransferEntrypoint is implemented by the backend; see
// amd64.afterThrowTransferEntrypoint's doc comment.
//
//go:linkname rawAfterThrowTransferEntrypoint github.com/samyfodil/wazy/internal/engine/wazevo/backend/isa/amd64.afterThrowTransferEntrypoint
func rawAfterThrowTransferEntrypoint(restoreFn *byte, executionContextPtr uintptr, stackPointer, framePointer, targetPC uintptr)
