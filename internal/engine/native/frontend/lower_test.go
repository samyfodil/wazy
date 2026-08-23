package frontend

import (
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func Test_Offsets(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		// These constants describe layouts as the generated machine code sees
		// them, and the native compiler only runs on amd64 and arm64. The
		// package still builds on a 32-bit platform, where a slice header is
		// twelve bytes rather than twenty-four, so the sizes legitimately
		// differ from what the compiler would emit.
		t.Skip("the native compiler does not run on a 32-bit platform")
	}
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
