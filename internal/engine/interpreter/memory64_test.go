package interpreter

import (
	"testing"

	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// Test_peekIndex64 covers the immediate-peeking wasmOpcodeSignature does to
// learn the index type of the memory or table an instruction names, before
// readMemoryArg has consumed the immediates. A truncated body cannot reach
// these in practice -- the function has already been validated -- so they fall
// back to the i32 form and leave the error to the real decode, and that is what
// is checked here.
func Test_peekIndex64(t *testing.T) {
	c := &compiler{
		anyMemory64: true, memory64: []bool{false, true},
		anyTable64: true, table64: []bool{false, true},
	}

	t.Run("a plain memarg names memory zero", func(t *testing.T) {
		c.body = []byte{wasm.OpcodeI32Load, 0x02, 0x00}
		require.False(t, c.peekMemArgIndex64(0))
	})
	t.Run("the multi-memory flag bit selects the named memory", func(t *testing.T) {
		c.body = append([]byte{wasm.OpcodeI32Load}, leb128.EncodeUint32(2|memArgMultiMemoryFlag)...)
		c.body = append(c.body, 0x01, 0x00) // memory index 1, offset 0
		require.True(t, c.peekMemArgIndex64(0))
	})
	t.Run("skip walks past a sub-opcode", func(t *testing.T) {
		c.body = append([]byte{wasm.OpcodeAtomicPrefix, wasm.OpcodeAtomicI32Load},
			leb128.EncodeUint32(2|memArgMultiMemoryFlag)...)
		c.body = append(c.body, 0x01, 0x00)
		require.True(t, c.peekMemArgIndex64(1))
		require.False(t, c.peekMemArgIndex64(0))
	})
	t.Run("a module with no 64-bit memory skips the peek entirely", func(t *testing.T) {
		c32 := &compiler{body: []byte{wasm.OpcodeI32Load, 0x02, 0x00}}
		require.False(t, c32.peekMemArgIndex64(0))
	})
	t.Run("a truncated memarg falls back to the i32 form", func(t *testing.T) {
		c.body = []byte{wasm.OpcodeI32Load} // no align byte
		require.False(t, c.peekMemArgIndex64(0))
		// An align byte with the flag bit set, but no memory index after it.
		c.body = append([]byte{wasm.OpcodeI32Load}, leb128.EncodeUint32(2|memArgMultiMemoryFlag)...)
		require.False(t, c.peekMemArgIndex64(0))
	})
	t.Run("a bare memory index immediate", func(t *testing.T) {
		c.body = []byte{wasm.OpcodeMemorySize, 0x01}
		index64, read := c.peekMemIndex64(0)
		require.True(t, index64)
		require.Equal(t, uint64(1), read)

		c.body = []byte{wasm.OpcodeMemorySize} // truncated
		index64, read = c.peekMemIndex64(0)
		require.False(t, index64)
		require.Equal(t, uint64(0), read)
	})
	t.Run("a bare table index immediate", func(t *testing.T) {
		c.body = []byte{wasm.OpcodeTableGet, 0x01}
		index64, read := c.peekTableIndex64(0)
		require.True(t, index64)
		require.Equal(t, uint64(1), read)

		c.body = []byte{wasm.OpcodeTableGet} // truncated
		index64, read = c.peekTableIndex64(0)
		require.False(t, index64)
		require.Equal(t, uint64(0), read)
	})
	t.Run("an index past the end of the module's memories or tables", func(t *testing.T) {
		require.False(t, c.memoryIsIndex64(2))
		require.False(t, c.tableIsIndex64(2))
	})
}

// Test_bulkMemorySignature covers the operand types of memory.init, memory.copy
// and memory.fill, which follow the index types of the memories the
// instruction's immediates name.
func Test_bulkMemorySignature(t *testing.T) {
	c := &compiler{anyMemory64: true, memory64: []bool{false, true}}
	body := func(miscOp byte, immediates ...byte) []byte {
		return append([]byte{wasm.OpcodeMiscPrefix, miscOp}, immediates...)
	}

	c.body = body(wasm.OpcodeMiscMemoryFill, 0x01)
	require.Equal(t, signature_I64I32I64_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryFill))
	c.body = body(wasm.OpcodeMiscMemoryFill, 0x00)
	require.Equal(t, signature_I32I32I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryFill))

	c.body = body(wasm.OpcodeMiscMemoryInit, 0x00, 0x01)
	require.Equal(t, signature_I64I32I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryInit))
	c.body = body(wasm.OpcodeMiscMemoryInit, 0x00, 0x00)
	require.Equal(t, signature_I32I32I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryInit))

	// memory.copy's length operand takes the narrower of the two index types.
	c.body = body(wasm.OpcodeMiscMemoryCopy, 0x01, 0x01)
	require.Equal(t, signature_I64I64I64_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryCopy))
	c.body = body(wasm.OpcodeMiscMemoryCopy, 0x01, 0x00)
	require.Equal(t, signature_I64I32I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryCopy))
	c.body = body(wasm.OpcodeMiscMemoryCopy, 0x00, 0x01)
	require.Equal(t, signature_I32I64I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryCopy))
	c.body = body(wasm.OpcodeMiscMemoryCopy, 0x00, 0x00)
	require.Equal(t, signature_I32I32I32_None, c.bulkMemorySignature(wasm.OpcodeMiscMemoryCopy))

	// A module with no 64-bit memory never peeks.
	c32 := &compiler{body: body(wasm.OpcodeMiscMemoryCopy, 0x00, 0x00)}
	require.Equal(t, signature_I32I32I32_None, c32.bulkMemorySignature(wasm.OpcodeMiscMemoryCopy))
}

// Test_bulkTableSignature is Test_bulkMemorySignature for table.init and
// table.copy.
func Test_bulkTableSignature(t *testing.T) {
	c := &compiler{anyTable64: true, table64: []bool{false, true}}
	body := func(miscOp byte, immediates ...byte) []byte {
		return append([]byte{wasm.OpcodeMiscPrefix, miscOp}, immediates...)
	}

	c.body = body(wasm.OpcodeMiscTableInit, 0x00, 0x01)
	require.Equal(t, signature_I64I32I32_None, c.bulkTableSignature(wasm.OpcodeMiscTableInit))
	c.body = body(wasm.OpcodeMiscTableInit, 0x00, 0x00)
	require.Equal(t, signature_I32I32I32_None, c.bulkTableSignature(wasm.OpcodeMiscTableInit))

	c.body = body(wasm.OpcodeMiscTableCopy, 0x01, 0x01)
	require.Equal(t, signature_I64I64I64_None, c.bulkTableSignature(wasm.OpcodeMiscTableCopy))
	c.body = body(wasm.OpcodeMiscTableCopy, 0x01, 0x00)
	require.Equal(t, signature_I64I32I32_None, c.bulkTableSignature(wasm.OpcodeMiscTableCopy))
	c.body = body(wasm.OpcodeMiscTableCopy, 0x00, 0x01)
	require.Equal(t, signature_I32I64I32_None, c.bulkTableSignature(wasm.OpcodeMiscTableCopy))

	c32 := &compiler{body: body(wasm.OpcodeMiscTableCopy, 0x00, 0x00)}
	require.Equal(t, signature_I32I32I32_None, c32.bulkTableSignature(wasm.OpcodeMiscTableCopy))
}
