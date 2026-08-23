package frontend

import (
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func Test_Offsets(t *testing.T) {
	var memInstance wasm.MemoryInstance
	require.Equal(t, uint32(unsafe.Offsetof(memInstance.Buffer)), memoryInstanceBufOffset)
	capacityOffset, sizeOffset := wasm.MemoryInstanceNativeGrowOffsets()
	require.Equal(t, capacityOffset, memoryInstanceNativeGrowCapOffset)
	require.Equal(t, sizeOffset, memoryInstanceSizeOffset)
	var tableInstance wasm.TableInstance
	require.Equal(t, int(unsafe.Offsetof(tableInstance.References)), tableInstanceBaseAddressOffset)

	var dataInstance wasm.DataInstance
	var elementInstance wasm.ElementInstance

	require.Equal(t, int(unsafe.Sizeof(dataInstance)), elementOrDataInstanceSize)
	require.Equal(t, int(unsafe.Sizeof(elementInstance)), elementOrDataInstanceSize)
}
