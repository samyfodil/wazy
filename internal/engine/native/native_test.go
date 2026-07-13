package native

import (
	"context"
	"os"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

var ctx = context.Background()

func TestMain(m *testing.M) {
	if !platform.CompilerSupported() {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestNewEngine(t *testing.T) {
	e := NewEngine(ctx, api.CoreFeaturesV1, nil)
	require.NotNil(t, e)
}

func TestEngine_CompiledModuleCount(t *testing.T) {
	e, ok := NewEngine(ctx, api.CoreFeaturesV1, nil).(*engine)
	require.True(t, ok)
	require.Equal(t, uint32(0), e.CompiledModuleCount())
	e.compiledModules[wasm.ModuleID{}] = &compiledModuleWithCount{compiledModule: &compiledModule{}, refCount: 1}
	require.Equal(t, uint32(1), e.CompiledModuleCount())
}

func TestEngine_DeleteCompiledModule(t *testing.T) {
	e, ok := NewEngine(ctx, api.CoreFeaturesV1, nil).(*engine)
	require.True(t, ok)
	id := wasm.ModuleID{0xaa}
	cm := &compiledModule{executables: &executables{executable: make([]byte, 1)}}
	cm2, err := e.addCompiledModule(&wasm.Module{ID: id}, cm)
	require.NoError(t, err)
	require.Same(t, cm, cm2)

	require.Equal(t, uint32(1), e.CompiledModuleCount())
	e.DeleteCompiledModule(&wasm.Module{ID: id})
	require.Equal(t, uint32(0), e.CompiledModuleCount())
}

func Test_ExecutionContextOffsets(t *testing.T) {
	var execCtx executionContext
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.exitCode)), nativeapi.ExecutionContextOffsetExitCodeOffset)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.callerModuleContextPtr)), nativeapi.ExecutionContextOffsetCallerModuleContextPtr)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.originalFramePointer)), nativeapi.ExecutionContextOffsetOriginalFramePointer)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.originalStackPointer)), nativeapi.ExecutionContextOffsetOriginalStackPointer)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.goReturnAddress)), nativeapi.ExecutionContextOffsetGoReturnAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.goCallReturnAddress)), nativeapi.ExecutionContextOffsetGoCallReturnAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.stackPointerBeforeGoCall)), nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.stackGrowRequiredSize)), nativeapi.ExecutionContextOffsetStackGrowRequiredSize)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.memoryGrowTrampolineAddress)), nativeapi.ExecutionContextOffsetMemoryGrowTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.stackGrowCallTrampolineAddress)), nativeapi.ExecutionContextOffsetStackGrowCallTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.checkModuleExitCodeTrampolineAddress)), nativeapi.ExecutionContextOffsetCheckModuleExitCodeTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.savedRegisters))%16, nativeapi.Offset(0),
		"SavedRegistersBegin must be aligned to 16 bytes")
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.savedRegisters)), nativeapi.ExecutionContextOffsetSavedRegistersBegin)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.goFunctionCallCalleeModuleContextOpaque)), nativeapi.ExecutionContextOffsetGoFunctionCallCalleeModuleContextOpaque)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.tableGrowTrampolineAddress)), nativeapi.ExecutionContextOffsetTableGrowTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.refFuncTrampolineAddress)), nativeapi.ExecutionContextOffsetRefFuncTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.memmoveAddress)), nativeapi.ExecutionContextOffsetMemmoveAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.framePointerBeforeGoCall)), nativeapi.ExecutionContextOffsetFramePointerBeforeGoCall)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.memoryWait32TrampolineAddress)), nativeapi.ExecutionContextOffsetMemoryWait32TrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.memoryWait64TrampolineAddress)), nativeapi.ExecutionContextOffsetMemoryWait64TrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.memoryNotifyTrampolineAddress)), nativeapi.ExecutionContextOffsetMemoryNotifyTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.throwTrampolineAddress)), nativeapi.ExecutionContextOffsetThrowTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.tryTableEnterTrampolineAddress)), nativeapi.ExecutionContextOffsetTryTableEnterTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.tryTableLeaveTrampolineAddress)), nativeapi.ExecutionContextOffsetTryTableLeaveTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.throwAllocTrampolineAddress)), nativeapi.ExecutionContextOffsetThrowAllocTrampolineAddress)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.exceptionPtr)), nativeapi.ExecutionContextOffsetExceptionPtr)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.exceptionParamsPtr)), nativeapi.ExecutionContextOffsetExceptionParamsPtr)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.caughtExceptionClauseIdx)), nativeapi.ExecutionContextOffsetCaughtExceptionClauseIdx)
	require.Equal(t, nativeapi.Offset(unsafe.Offsetof(execCtx.localsSaveAreaPtr)), nativeapi.ExecutionContextOffsetLocalsSaveAreaPtr)
}
