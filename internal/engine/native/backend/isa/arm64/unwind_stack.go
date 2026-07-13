package arm64

import (
	"encoding/binary"
	"unsafe"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/wasmdebug"
)

// sliceHeader mirrors the layout of reflect.SliceHeader. It builds a []byte
// over a raw machine-stack address (a plain uintptr, not a Go-managed pointer)
// without importing reflect, and without a uintptr->unsafe.Pointer conversion
// that go vet's unsafeptr check would flag. The stack memory is not
// GC-managed, so Data stays a uintptr, exactly as the original code did.
type sliceHeader struct {
	Data uintptr
	Len  int
	Cap  int
}

// UnwindStack implements native.unwindStack.
func UnwindStack(sp, _, top uintptr, returnAddresses []uintptr) []uintptr {
	l := int(top - sp)

	var stackBuf []byte
	{
		hdr := (*sliceHeader)(unsafe.Pointer(&stackBuf))
		hdr.Data = sp
		hdr.Len = l
		hdr.Cap = l
	}

	for i := uint64(0); i < uint64(l); {
		//       (high address)
		//    +-----------------+
		//    |     .......     |
		//    |      ret Y      |  <----+
		//    |     .......     |       |
		//    |      ret 0      |       |
		//    |      arg X      |       |  size_of_arg_ret
		//    |     .......     |       |
		//    |      arg 1      |       |
		//    |      arg 0      |  <----+
		//    | size_of_arg_ret |
		//    |  ReturnAddress  |
		//    +-----------------+ <----+
		//    |   ...........   |      |
		//    |   spill slot M  |      |
		//    |   ............  |      |
		//    |   spill slot 2  |      |
		//    |   spill slot 1  |      | frame size
		//    |   spill slot 1  |      |
		//    |   clobbered N   |      |
		//    |   ............  |      |
		//    |   clobbered 0   | <----+
		//    |     xxxxxx      |  ;; unused space to make it 16-byte aligned.
		//    |   frame_size    |
		//    +-----------------+ <---- SP
		//       (low address)

		frameSize := binary.LittleEndian.Uint64(stackBuf[i:])
		i += frameSize +
			16 // frame size + aligned space.
		retAddr := binary.LittleEndian.Uint64(stackBuf[i:])
		i += 8 // ret addr.
		sizeOfArgRet := binary.LittleEndian.Uint64(stackBuf[i:])
		i += 8 + sizeOfArgRet
		returnAddresses = append(returnAddresses, uintptr(retAddr))
		if len(returnAddresses) == wasmdebug.MaxFrames {
			break
		}
	}
	return returnAddresses
}

// UnwindStackForThrow is the exception-handling variant of UnwindStack: for
// each frame, in addition to the return address, it recovers the SP that the
// *owning* function (the one whose code the return address points into)
// executes with. See nativeapi.ThrowFrame for why arm64 leaves FP zero.
//
// This mirrors UnwindStack's loop exactly (same frame_size-chased walk), the
// only difference being what's captured per iteration: after advancing `i`
// past the current frame's own frame_size/return-address/size_of_arg_ret
// fields, `i` (relative to `sp`) is precisely the SP of the frame that owns
// the just-read return address (see the ASCII diagram in UnwindStack) --
// because native frames are fixed-size, this is the exact SP that frame
// would have at *any* point in its body, including a landing pad reached via
// a direct transfer rather than a normal call return.
//
// Unlike UnwindStack, this has no wasmdebug.MaxFrames cap: silently giving
// up before reaching a matching (or the outermost) frame would wrongly
// report a catchable exception as uncaught.
func UnwindStackForThrow(sp, top uintptr, frames []nativeapi.ThrowFrame) []nativeapi.ThrowFrame {
	l := int(top - sp)

	var stackBuf []byte
	{
		hdr := (*sliceHeader)(unsafe.Pointer(&stackBuf))
		hdr.Data = sp
		hdr.Len = l
		hdr.Cap = l
	}

	for i := uint64(0); i < uint64(l); {
		frameSize := binary.LittleEndian.Uint64(stackBuf[i:])
		i += frameSize + 16 // frame size + aligned space.
		retAddr := binary.LittleEndian.Uint64(stackBuf[i:])
		i += 8 // ret addr.
		sizeOfArgRet := binary.LittleEndian.Uint64(stackBuf[i:])
		i += 8 + sizeOfArgRet

		// i is now the SP-relative offset of the next frame, i.e. exactly
		// the SP of the function that owns retAddr.
		frames = append(frames, nativeapi.ThrowFrame{
			ReturnAddress: uintptr(retAddr),
			SP:            sp + uintptr(i),
		})
	}
	return frames
}

// FirstReturnAddress returns, in O(1), the return address of the frame that
// owns the just-below-sp frame -- i.e. the address the function whose frame
// sits at sp will return to. It reads exactly the words UnwindStackForThrow
// reads on its first iteration (frame_size at sp, then the ReturnAddress
// slot one frame up), without walking the rest of the stack. Used to recover
// a try_table's enter-continuation from the still-live enter trampoline
// frame at try_table-enter time (see call_engine.go's firstReturnAddress and
// ExitCodeTryTableEnter).
func FirstReturnAddress(sp, top uintptr) uintptr {
	l := int(top - sp)

	var stackBuf []byte
	{
		hdr := (*sliceHeader)(unsafe.Pointer(&stackBuf))
		hdr.Data = sp
		hdr.Len = l
		hdr.Cap = l
	}

	frameSize := binary.LittleEndian.Uint64(stackBuf[0:])
	return uintptr(binary.LittleEndian.Uint64(stackBuf[frameSize+16:]))
}

// GoCallStackView implements native.goCallStackView.
func GoCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	//                  (high address)
	//              +-----------------+ <----+
	//              |   xxxxxxxxxxx   |      | ;; optional unused space to make it 16-byte aligned.
	//           ^  |  arg[N]/ret[M]  |      |
	// sliceSize |  |  ............   |      | sliceSize
	//           |  |  arg[1]/ret[1]  |      |
	//           v  |  arg[0]/ret[0]  | <----+
	//              |    sliceSize    |
	//              |   frame_size    |
	//              +-----------------+ <---- stackPointerBeforeGoCall
	//                 (low address)
	ptr := unsafe.Pointer(stackPointerBeforeGoCall)
	data := (*uint64)(unsafe.Add(ptr, 16)) // skips the (frame_size, sliceSize).
	size := *(*uint64)(unsafe.Add(ptr, 8))
	return unsafe.Slice(data, size)
}
