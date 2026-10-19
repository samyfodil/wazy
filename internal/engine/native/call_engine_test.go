package native

import (
	"encoding/binary"
	"reflect"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestCallEngine_init(t *testing.T) {
	// init() no longer eagerly allocates the wasm stack in the default
	// (StackGuardCheckEnabled == false) build -- that's now acquired from
	// the engine's stack pool by callWithStack itself, once per top-level
	// call (see stack_pool.go and callWithStack's doc comments) -- so a
	// bare callEngine{} (no parent chain to reach an *engine through) can
	// no longer be used to exercise that part. What init() still does
	// unconditionally is wire up execCtxPtr.
	c := &callEngine{}
	c.init()
	require.Equal(t, uintptr(unsafe.Pointer(&c.execCtx)), c.execCtxPtr)
}

func TestCallEngine_growStack(t *testing.T) {
	t.Run("stack overflow", func(t *testing.T) {
		c := &callEngine{stack: make([]byte, callStackCeiling+1)}
		_, _, err := c.growStack()
		require.Error(t, err)
	})

	t.Run("ok", func(t *testing.T) {
		s := make([]byte, 32)
		for i := range s {
			s[i] = byte(i)
		}
		c := &callEngine{
			stack:    s,
			eng:      &engine{}, // growStack now sources the new buffer from the pool
			stackTop: uintptr(unsafe.Pointer(&s[15])),
			execCtx: executionContext{
				// stackGrowRequiredSize is large enough here (rather than
				// the toy value this test used pre-H7-followup) so that
				// newLen comfortably exceeds
				// backend.StackBoundsCheckMarginBytes: growStack indexes
				// the new buffer at that offset when setting
				// stackBottomPtr (see production invariant that the real
				// stack buffer is always >= stackPoolBaseSize (10240) >>
				// MARGIN, so this indexing never goes out of range).
				stackGrowRequiredSize:    1000,
				stackPointerBeforeGoCall: (*uint64)(unsafe.Pointer(&s[10])),
				framePointerBeforeGoCall: uintptr(unsafe.Pointer(&s[14])),
			},
		}
		newSP, newFp, err := c.growStack()
		require.NoError(t, err)
		// growStack now acquires the new buffer from the shared stack pool
		// (stack_pool.go) rather than make()-ing it at exactly newLen, so it is
		// the smallest pool class >= newLen (here class 0 = stackPoolBaseSize) --
		// the extra headroom is harmless. Assert the invariant (>= newLen), not
		// an exact length.
		require.True(t, len(c.stack) >= 1000+32*2+16)

		require.True(t, c.stackTop%16 == 0)
		require.Equal(t, &c.stack[backend.StackBoundsCheckMarginBytes], c.execCtx.stackBottomPtr)

		var view []byte
		{
			//nolint:staticcheck
			sh := (*reflect.SliceHeader)(unsafe.Pointer(&view))
			sh.Data = newSP
			sh.Len = 5
			sh.Cap = 5
		}
		require.Equal(t, []byte{10, 11, 12, 13, 14}, view)
		require.True(t, newSP >= uintptr(unsafe.Pointer(c.execCtx.stackBottomPtr)))
		require.True(t, newSP <= c.stackTop)
		require.Equal(t, newFp-newSP, uintptr(4))
	})

	// The new buffer gets the root region the call started with, even after the engine's has grown: one
	// placed for the larger size could land on frames the call already has.
	t.Run("keeps the root region", func(t *testing.T) {
		eng := &engine{}
		eng.noteGCRoots(3) // 3 words and the header, rounded up: 32 bytes.
		c := &callEngine{parent: &moduleEngine{gcRootsReserve: &eng.gcRootsReserve}, eng: eng}
		c.stack = make([]byte, c.requiredInitialStackSize(c.gcRootsReserve()))
		c.stackTop = alignedStackTop(c.stack)
		c.execCtx.stackBottomPtr = c.stackCheckLimitPtr(0, c.gcRootsReserve())
		c.execCtx.stackGrowRequiredSize = 64
		top := int(c.stackTop - uintptr(unsafe.Pointer(&c.stack[0])))
		c.execCtx.stackPointerBeforeGoCall = (*uint64)(unsafe.Pointer(&c.stack[top-32]))
		c.execCtx.framePointerBeforeGoCall = uintptr(unsafe.Pointer(&c.stack[top-16]))
		require.Equal(t, 32, c.rootsReserveInUse())

		eng.noteGCRoots(100)
		_, _, err := c.growStack()
		require.NoError(t, err)
		require.Equal(t, 32, c.rootsReserveInUse())
		require.Equal(t, &c.stack[32+backend.StackBoundsCheckMarginBytes], c.execCtx.stackBottomPtr)
		// The header says how many words the region holds, and that none are spilled yet.
		require.Equal(t, uint64(3)<<32, binary.LittleEndian.Uint64(c.stack[24:]))
	})
}

func TestCallEngine_requiredInitialStackSize(t *testing.T) {
	c := &callEngine{}
	require.Equal(t, 10240, c.requiredInitialStackSize(0))
	c.sizeOfParamResultSlice = 10
	require.Equal(t, 10240, c.requiredInitialStackSize(0))
	c.sizeOfParamResultSlice = 1000
	require.Equal(t, 1000*16+32+16, c.requiredInitialStackSize(0))
	// The root region comes out of the buffer, and leaves the guest at least as much again.
	require.Equal(t, 1000*16+32+16+64, c.requiredInitialStackSize(64))
	c.sizeOfParamResultSlice = 0
	require.Equal(t, 2*(8000+512), c.requiredInitialStackSize(8000))
}
