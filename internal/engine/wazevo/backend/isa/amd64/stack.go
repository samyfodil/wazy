package amd64

import (
	"encoding/binary"
	"unsafe"

	"github.com/samyfodil/wazy/internal/engine/wazevo/wazevoapi"
	"github.com/samyfodil/wazy/internal/wasmdebug"
)

// sliceHeader mirrors the layout of reflect.SliceHeader. It lets stackView
// build a []byte over a raw machine-stack address (a plain uintptr, not a
// Go-managed pointer) without importing reflect. We deliberately avoid
// unsafe.Slice((*byte)(unsafe.Pointer(rbp)), l) here: that form makes go vet's
// unsafeptr check flag the uintptr->unsafe.Pointer conversion, and it would
// change the backing array's GC visibility. The stack memory is not
// GC-managed, so Data stays a uintptr, exactly as the original code did.
type sliceHeader struct {
	Data uintptr
	Len  int
	Cap  int
}

func stackView(rbp, top uintptr) []byte {
	l := int(top - rbp)
	var stackBuf []byte
	hdr := (*sliceHeader)(unsafe.Pointer(&stackBuf))
	hdr.Data = rbp
	hdr.Len = l
	hdr.Cap = l
	return stackBuf
}

// UnwindStack implements wazevo.unwindStack.
func UnwindStack(_, rbp, top uintptr, returnAddresses []uintptr) []uintptr {
	stackBuf := stackView(rbp, top)

	for i := uint64(0); i < uint64(len(stackBuf)); {
		//       (high address)
		//    +-----------------+
		//    |     .......     |
		//    |      ret Y      |
		//    |     .......     |
		//    |      ret 0      |
		//    |      arg X      |
		//    |     .......     |
		//    |      arg 1      |
		//    |      arg 0      |
		//    |  ReturnAddress  |
		//    |   Caller_RBP    |
		//    +-----------------+ <---- Caller_RBP
		//    |   ...........   |
		//    |   clobbered  M  |
		//    |   ............  |
		//    |   clobbered  0  |
		//    |   spill slot N  |
		//    |   ............  |
		//    |   spill slot 0  |
		//    |  ReturnAddress  |
		//    |   Caller_RBP    |
		//    +-----------------+ <---- RBP
		//       (low address)

		callerRBP := binary.LittleEndian.Uint64(stackBuf[i:])
		retAddr := binary.LittleEndian.Uint64(stackBuf[i+8:])
		returnAddresses = append(returnAddresses, uintptr(retAddr))
		i = callerRBP - uint64(rbp)
		if len(returnAddresses) == wasmdebug.MaxFrames {
			break
		}
	}
	return returnAddresses
}

// UnwindStackForThrow is the exception-handling variant of UnwindStack: for
// each frame it additionally recovers FP (=RBP), which is directly available
// from the RBP chain. Unlike arm64, amd64 has no on-stack marker from which
// to recover a frame's SP directly during this walk (frames are chained
// purely via [Caller_RBP][ReturnAddress] pairs, with no frame_size word) --
// the caller (call_engine.go) computes SP = FP - FrameSize for whichever
// single frame matches an exception, using that function's FrameSize
// (backend.Machine.FrameSize) recorded at compile time. See
// wazevoapi.ThrowFrame.
//
// Unlike UnwindStack, this has no wasmdebug.MaxFrames cap: silently giving
// up before reaching a matching (or the outermost) frame would wrongly
// report a catchable exception as uncaught.
func UnwindStackForThrow(rbp, top uintptr, frames []wazevoapi.ThrowFrame) []wazevoapi.ThrowFrame {
	stackBuf := stackView(rbp, top)

	for i := uint64(0); i < uint64(len(stackBuf)); {
		callerRBP := binary.LittleEndian.Uint64(stackBuf[i:])
		retAddr := binary.LittleEndian.Uint64(stackBuf[i+8:])
		frames = append(frames, wazevoapi.ThrowFrame{
			ReturnAddress: uintptr(retAddr),
			FP:            uintptr(callerRBP),
		})
		i = callerRBP - uint64(rbp)
	}
	return frames
}

// FirstReturnAddress returns, in O(1), the return address of the frame
// whose callee's frame is chained from rbp -- i.e. the address the function
// at rbp will return to. On amd64 the RBP chain stores [Caller_RBP,
// ReturnAddress] at rbp, so the return address is the second word. Used to
// recover a try_table's enter-continuation from the still-live enter
// trampoline frame at try_table-enter time (see call_engine.go's
// firstReturnAddress and ExitCodeTryTableEnter); it reads the exact word
// UnwindStackForThrow would read for its first frame.
func FirstReturnAddress(rbp uintptr) uintptr {
	stackBuf := stackView(rbp, rbp+16)
	return uintptr(binary.LittleEndian.Uint64(stackBuf[8:]))
}

// GoCallStackView implements wazevo.goCallStackView.
func GoCallStackView(stackPointerBeforeGoCall *uint64) []uint64 {
	//                  (high address)
	//              +-----------------+ <----+
	//              |   xxxxxxxxxxx   |      | ;; optional unused space to make it 16-byte aligned.
	//           ^  |  arg[N]/ret[M]  |      |
	// sliceSize |  |  ............   |      | SizeInBytes/8
	//           |  |  arg[1]/ret[1]  |      |
	//           v  |  arg[0]/ret[0]  | <----+
	//              |   SizeInBytes   |
	//              +-----------------+ <---- stackPointerBeforeGoCall
	//                 (low address)
	data := unsafe.Add(unsafe.Pointer(stackPointerBeforeGoCall), 8)
	size := *stackPointerBeforeGoCall / 8
	return unsafe.Slice((*uint64)(data), size)
}

func AdjustClonedStack(oldRsp, oldTop, rsp, rbp, top uintptr) {
	diff := uint64(rsp - oldRsp)

	newBuf := stackView(rbp, top)
	for i := uint64(0); i < uint64(len(newBuf)); {
		//       (high address)
		//    +-----------------+
		//    |     .......     |
		//    |      ret Y      |
		//    |     .......     |
		//    |      ret 0      |
		//    |      arg X      |
		//    |     .......     |
		//    |      arg 1      |
		//    |      arg 0      |
		//    |  ReturnAddress  |
		//    |   Caller_RBP    |
		//    +-----------------+ <---- Caller_RBP
		//    |   ...........   |
		//    |   clobbered  M  |
		//    |   ............  |
		//    |   clobbered  0  |
		//    |   spill slot N  |
		//    |   ............  |
		//    |   spill slot 0  |
		//    |  ReturnAddress  |
		//    |   Caller_RBP    |
		//    +-----------------+ <---- RBP
		//       (low address)

		callerRBP := binary.LittleEndian.Uint64(newBuf[i:])
		if callerRBP == 0 {
			// End of stack.
			break
		}
		if i64 := int64(callerRBP); i64 < int64(oldRsp) || i64 >= int64(oldTop) {
			panic("BUG: callerRBP is out of range")
		}
		if int(callerRBP) < 0 {
			panic("BUG: callerRBP is negative")
		}
		adjustedCallerRBP := callerRBP + diff
		if int(adjustedCallerRBP) < 0 {
			panic("BUG: adjustedCallerRBP is negative")
		}
		binary.LittleEndian.PutUint64(newBuf[i:], adjustedCallerRBP)
		i = adjustedCallerRBP - uint64(rbp)
	}
}
