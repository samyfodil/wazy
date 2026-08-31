package frontend

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"runtime"
	"strings"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

type (
	// loweringState is used to keep the state of lowering.
	loweringState struct {
		// values holds the values on the Wasm stack.
		values           []ssa.Value
		controlFrames    []controlFrame
		unreachable      bool
		unreachableDepth int
		tmpForBrTable    []uint32
		pc               int
	}
	controlFrame struct {
		kind controlFrameKind
		// originalStackLen holds the number of values on the Wasm stack
		// when start executing this control frame minus params for the block.
		originalStackLenWithoutParam int
		// blk is the loop header if this is loop, and is the else-block if this is an if frame.
		blk,
		// followingBlock is the basic block we enter if we reach "end" of block.
		followingBlock ssa.BasicBlock
		blockType *wasm.FunctionType
		// clonedArgs hold the arguments to Else block.
		clonedArgs ssa.Values
		// pendingEhIdx indexes into Compiler.pendingEhEntries for a
		// controlFrameKindTryTableWithCatch frame -- OpcodeEnd fills in
		// that pending entry's BodyBlkIDEnd once the try body has been
		// fully lowered. Unused (left zero) for all other frame kinds.
		pendingEhIdx int
	}

	controlFrameKind byte
)

// String implements fmt.Stringer for debugging.
func (l *loweringState) String() string {
	var str []string
	for _, v := range l.values {
		str = append(str, fmt.Sprintf("v%v", v.ID()))
	}
	var frames []string
	for i := range l.controlFrames {
		frames = append(frames, l.controlFrames[i].kind.String())
	}
	return fmt.Sprintf("\n\tunreachable=%v(depth=%d)\n\tstack: %s\n\tcontrol frames: %s",
		l.unreachable, l.unreachableDepth,
		strings.Join(str, ", "),
		strings.Join(frames, ", "),
	)
}

const (
	controlFrameKindFunction = iota + 1
	controlFrameKindLoop
	controlFrameKindIfWithElse
	controlFrameKindIfWithoutElse
	controlFrameKindBlock
	controlFrameKindTryTable
	controlFrameKindTryTableWithCatch
)

// String implements fmt.Stringer for debugging.
func (k controlFrameKind) String() string {
	switch k {
	case controlFrameKindFunction:
		return "function"
	case controlFrameKindLoop:
		return "loop"
	case controlFrameKindIfWithElse:
		return "if_with_else"
	case controlFrameKindIfWithoutElse:
		return "if_without_else"
	case controlFrameKindBlock:
		return "block"
	case controlFrameKindTryTable:
		return "try_table"
	case controlFrameKindTryTableWithCatch:
		return "try_table_with_catch"
	default:
		panic(k)
	}
}

// isLoop returns true if this is a loop frame.
func (ctrl *controlFrame) isLoop() bool {
	return ctrl.kind == controlFrameKindLoop
}

func (ctrl *controlFrame) isTryCatch() bool {
	return ctrl.kind == controlFrameKindTryTableWithCatch
}

// reset resets the state of loweringState for reuse.
func (l *loweringState) reset() {
	l.values = l.values[:0]
	l.controlFrames = l.controlFrames[:0]
	l.pc = 0
	l.unreachable = false
	l.unreachableDepth = 0
}

func (l *loweringState) peek() (ret ssa.Value) {
	tail := len(l.values) - 1
	return l.values[tail]
}

func (l *loweringState) pop() (ret ssa.Value) {
	tail := len(l.values) - 1
	ret = l.values[tail]
	l.values = l.values[:tail]
	return
}

func (l *loweringState) push(ret ssa.Value) {
	l.values = append(l.values, ret)
}

func (c *Compiler) nPeekDup(n int) ssa.Values {
	if n == 0 {
		return ssa.ValuesNil
	}

	l := c.state()
	tail := len(l.values)

	args := c.allocateVarLengthValues(n, l.values[tail-n:tail]...)
	return args
}

func (l *loweringState) ctrlPop() (ret controlFrame) {
	tail := len(l.controlFrames) - 1
	ret = l.controlFrames[tail]
	l.controlFrames = l.controlFrames[:tail]
	return
}

func (l *loweringState) ctrlPush(ret controlFrame) {
	l.controlFrames = append(l.controlFrames, ret)
}

func (l *loweringState) ctrlPeekAt(n int) (ret *controlFrame) {
	tail := len(l.controlFrames) - 1
	return &l.controlFrames[tail-n]
}

// lowerBody lowers the body of the Wasm function to the SSA form.
func (c *Compiler) lowerBody(entryBlk ssa.BasicBlock) {
	c.ssaBuilder.Seal(entryBlk)

	if c.needListener {
		c.callListenerBefore()
	}

	// Pushes the empty control frame which corresponds to the function return.
	c.loweringState.ctrlPush(controlFrame{
		kind:           controlFrameKindFunction,
		blockType:      c.wasmFunctionTyp,
		followingBlock: c.ssaBuilder.ReturnBlock(),
	})

	for c.loweringState.pc < len(c.wasmFunctionBody) {
		blkBeforeLowering := c.ssaBuilder.CurrentBlock()
		c.lowerCurrentOpcode()
		blkAfterLowering := c.ssaBuilder.CurrentBlock()
		if blkBeforeLowering != blkAfterLowering {
			// In Wasm, once a block exits, that means we've done compiling the block.
			// Therefore, we finalize the known bounds at the end of the block for the exiting block.
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(blkBeforeLowering.ID())
			// After that, we initialize the known bounds for the new compilation target block.
			c.initializeCurrentBlockKnownBounds()
		}
	}
}

func (c *Compiler) state() *loweringState {
	return &c.loweringState
}

func (c *Compiler) lowerCurrentOpcode() {
	op := c.wasmFunctionBody[c.loweringState.pc]

	if c.needSourceOffsetInfo {
		c.ssaBuilder.SetCurrentSourceOffset(
			ssa.SourceOffset(c.loweringState.pc) + ssa.SourceOffset(c.wasmFunctionBodyOffsetInCodeSection),
		)
	}

	builder := c.ssaBuilder
	state := c.state()
	switch op {
	case wasm.OpcodeI32Const:
		c := c.readI32s()
		if state.unreachable {
			break
		}

		iconst := builder.AllocateInstruction().AsIconst32(uint32(c)).Insert(builder)
		value := iconst.Return()
		state.push(value)
	case wasm.OpcodeI64Const:
		c := c.readI64s()
		if state.unreachable {
			break
		}
		iconst := builder.AllocateInstruction().AsIconst64(uint64(c)).Insert(builder)
		value := iconst.Return()
		state.push(value)
	case wasm.OpcodeF32Const:
		f32 := c.readF32()
		if state.unreachable {
			break
		}
		f32const := builder.AllocateInstruction().
			AsF32const(f32).
			Insert(builder).
			Return()
		state.push(f32const)
	case wasm.OpcodeF64Const:
		f64 := c.readF64()
		if state.unreachable {
			break
		}
		f64const := builder.AllocateInstruction().
			AsF64const(f64).
			Insert(builder).
			Return()
		state.push(f64const)
	case wasm.OpcodeI32Add, wasm.OpcodeI64Add:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		iadd := builder.AllocateInstruction()
		iadd.AsIadd(x, y)
		builder.InsertInstruction(iadd)
		value := iadd.Return()
		state.push(value)
	case wasm.OpcodeI32Sub, wasm.OpcodeI64Sub:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsIsub(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeF32Add, wasm.OpcodeF64Add:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		iadd := builder.AllocateInstruction()
		iadd.AsFadd(x, y)
		builder.InsertInstruction(iadd)
		value := iadd.Return()
		state.push(value)
	case wasm.OpcodeI32Mul, wasm.OpcodeI64Mul:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		imul := builder.AllocateInstruction()
		imul.AsImul(x, y)
		builder.InsertInstruction(imul)
		value := imul.Return()
		state.push(value)
	case wasm.OpcodeF32Sub, wasm.OpcodeF64Sub:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsFsub(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeF32Mul, wasm.OpcodeF64Mul:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsFmul(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeF32Div, wasm.OpcodeF64Div:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsFdiv(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeF32Max, wasm.OpcodeF64Max:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsFmax(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeF32Min, wasm.OpcodeF64Min:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		isub := builder.AllocateInstruction()
		isub.AsFmin(x, y)
		builder.InsertInstruction(isub)
		value := isub.Return()
		state.push(value)
	case wasm.OpcodeI64Extend8S:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(true, 8, 64)
	case wasm.OpcodeI64Extend16S:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(true, 16, 64)
	case wasm.OpcodeI64Extend32S, wasm.OpcodeI64ExtendI32S:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(true, 32, 64)
	case wasm.OpcodeI64ExtendI32U:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(false, 32, 64)
	case wasm.OpcodeI32Extend8S:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(true, 8, 32)
	case wasm.OpcodeI32Extend16S:
		if state.unreachable {
			break
		}
		c.insertIntegerExtend(true, 16, 32)
	case wasm.OpcodeI32Eqz, wasm.OpcodeI64Eqz:
		if state.unreachable {
			break
		}
		x := state.pop()
		zero := builder.AllocateInstruction()
		if op == wasm.OpcodeI32Eqz {
			zero.AsIconst32(0)
		} else {
			zero.AsIconst64(0)
		}
		builder.InsertInstruction(zero)
		icmp := builder.AllocateInstruction().
			AsIcmp(x, zero.Return(), ssa.IntegerCmpCondEqual).
			Insert(builder).
			Return()
		state.push(icmp)
	case wasm.OpcodeI32Eq, wasm.OpcodeI64Eq:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondEqual)
	case wasm.OpcodeI32Ne, wasm.OpcodeI64Ne:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondNotEqual)
	case wasm.OpcodeI32LtS, wasm.OpcodeI64LtS:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondSignedLessThan)
	case wasm.OpcodeI32LtU, wasm.OpcodeI64LtU:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondUnsignedLessThan)
	case wasm.OpcodeI32GtS, wasm.OpcodeI64GtS:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondSignedGreaterThan)
	case wasm.OpcodeI32GtU, wasm.OpcodeI64GtU:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondUnsignedGreaterThan)
	case wasm.OpcodeI32LeS, wasm.OpcodeI64LeS:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondSignedLessThanOrEqual)
	case wasm.OpcodeI32LeU, wasm.OpcodeI64LeU:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondUnsignedLessThanOrEqual)
	case wasm.OpcodeI32GeS, wasm.OpcodeI64GeS:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondSignedGreaterThanOrEqual)
	case wasm.OpcodeI32GeU, wasm.OpcodeI64GeU:
		if state.unreachable {
			break
		}
		c.insertIcmp(ssa.IntegerCmpCondUnsignedGreaterThanOrEqual)

	case wasm.OpcodeF32Eq, wasm.OpcodeF64Eq:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondEqual)
	case wasm.OpcodeF32Ne, wasm.OpcodeF64Ne:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondNotEqual)
	case wasm.OpcodeF32Lt, wasm.OpcodeF64Lt:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondLessThan)
	case wasm.OpcodeF32Gt, wasm.OpcodeF64Gt:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondGreaterThan)
	case wasm.OpcodeF32Le, wasm.OpcodeF64Le:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondLessThanOrEqual)
	case wasm.OpcodeF32Ge, wasm.OpcodeF64Ge:
		if state.unreachable {
			break
		}
		c.insertFcmp(ssa.FloatCmpCondGreaterThanOrEqual)
	case wasm.OpcodeF32Neg, wasm.OpcodeF64Neg:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsFneg(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Sqrt, wasm.OpcodeF64Sqrt:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsSqrt(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Abs, wasm.OpcodeF64Abs:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsFabs(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Copysign, wasm.OpcodeF64Copysign:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		v := builder.AllocateInstruction().AsFcopysign(x, y).Insert(builder).Return()
		state.push(v)

	case wasm.OpcodeF32Ceil, wasm.OpcodeF64Ceil:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsCeil(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Floor, wasm.OpcodeF64Floor:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsFloor(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Trunc, wasm.OpcodeF64Trunc:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsTrunc(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeF32Nearest, wasm.OpcodeF64Nearest:
		if state.unreachable {
			break
		}
		x := state.pop()
		v := builder.AllocateInstruction().AsNearest(x).Insert(builder).Return()
		state.push(v)
	case wasm.OpcodeI64TruncF64S, wasm.OpcodeI64TruncF32S,
		wasm.OpcodeI32TruncF64S, wasm.OpcodeI32TruncF32S,
		wasm.OpcodeI64TruncF64U, wasm.OpcodeI64TruncF32U,
		wasm.OpcodeI32TruncF64U, wasm.OpcodeI32TruncF32U:
		if state.unreachable {
			break
		}
		ret := builder.AllocateInstruction().AsFcvtToInt(
			state.pop(),
			c.execCtxPtrValue,
			op == wasm.OpcodeI64TruncF64S || op == wasm.OpcodeI64TruncF32S || op == wasm.OpcodeI32TruncF32S || op == wasm.OpcodeI32TruncF64S,
			op == wasm.OpcodeI64TruncF64S || op == wasm.OpcodeI64TruncF32S || op == wasm.OpcodeI64TruncF64U || op == wasm.OpcodeI64TruncF32U,
			false,
		).Insert(builder).Return()
		state.push(ret)
	case wasm.OpcodeMiscPrefix:
		state.pc++
		// A misc opcode is encoded as an unsigned variable 32-bit integer.
		miscOpUint, num, err := leb128.LoadUint32(c.wasmFunctionBody[state.pc:])
		if err != nil {
			// In normal conditions this should never happen because the function has passed validation.
			panic(fmt.Sprintf("failed to read misc opcode: %v", err))
		}
		state.pc += int(num - 1)
		miscOp := wasm.OpcodeMisc(miscOpUint)
		switch miscOp {
		case wasm.OpcodeMiscI64TruncSatF64S, wasm.OpcodeMiscI64TruncSatF32S,
			wasm.OpcodeMiscI32TruncSatF64S, wasm.OpcodeMiscI32TruncSatF32S,
			wasm.OpcodeMiscI64TruncSatF64U, wasm.OpcodeMiscI64TruncSatF32U,
			wasm.OpcodeMiscI32TruncSatF64U, wasm.OpcodeMiscI32TruncSatF32U:
			if state.unreachable {
				break
			}
			ret := builder.AllocateInstruction().AsFcvtToInt(
				state.pop(),
				c.execCtxPtrValue,
				miscOp == wasm.OpcodeMiscI64TruncSatF64S || miscOp == wasm.OpcodeMiscI64TruncSatF32S || miscOp == wasm.OpcodeMiscI32TruncSatF32S || miscOp == wasm.OpcodeMiscI32TruncSatF64S,
				miscOp == wasm.OpcodeMiscI64TruncSatF64S || miscOp == wasm.OpcodeMiscI64TruncSatF32S || miscOp == wasm.OpcodeMiscI64TruncSatF64U || miscOp == wasm.OpcodeMiscI64TruncSatF32U,
				true,
			).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeMiscTableSize:
			tableIndex := c.readI32u()
			if state.unreachable {
				break
			}

			// Load the table.
			loadTableInstancePtr := builder.AllocateInstruction()
			loadTableInstancePtr.AsLoad(c.moduleCtxPtrValue, c.offset.TableOffset(int(tableIndex)).U32(), ssa.TypeI64)
			builder.InsertInstruction(loadTableInstancePtr)
			tableInstancePtr := loadTableInstancePtr.Return()

			// Load the table's length.
			loadTableLen := builder.AllocateInstruction().
				AsLoad(tableInstancePtr, tableInstanceLenOffset, ssa.TypeI32).
				Insert(builder)
			tableLen := loadTableLen.Return()
			if c.tableIsIndex64(tableIndex) {
				// table.size on a 64-bit table yields an i64.
				tableLen = builder.AllocateInstruction().AsUExtend(tableLen, 32, 64).Insert(builder).Return()
			}
			state.push(tableLen)

		case wasm.OpcodeMiscTableGrow:
			tableIndex := c.readI32u()
			if state.unreachable {
				break
			}

			c.storeCallerModuleContext()

			tableIndexVal := builder.AllocateInstruction().AsIconst32(tableIndex).Insert(builder).Return()

			num := state.pop()
			r := state.pop()

			// tableGrowSig takes and returns the entry count as i64 whatever the
			// table's index type, so a 32-bit table's is widened and narrowed
			// around the call. See memoryGrowSig for the rationale.
			index64 := c.tableIsIndex64(tableIndex)
			if !index64 {
				num = builder.AllocateInstruction().AsUExtend(num, 32, 64).Insert(builder).Return()
			}

			tableGrowPtr := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetTableGrowTrampolineAddress.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()

			args := c.allocateVarLengthValues(4, c.execCtxPtrValue, tableIndexVal, num, r)
			callGrowRet := builder.
				AllocateInstruction().
				AsCallIndirect(tableGrowPtr, &c.tableGrowSig, args).
				Insert(builder).Return()
			if !index64 {
				callGrowRet = builder.AllocateInstruction().AsIreduce(callGrowRet, ssa.TypeI32).Insert(builder).Return()
			}
			state.push(callGrowRet)

		case wasm.OpcodeMiscTableCopy:
			dstTableIndex := c.readI32u()
			srcTableIndex := c.readI32u()
			if state.unreachable {
				break
			}

			// table.copy x y : [at_x, at_y, min(at_x, at_y)] -> [], so the
			// length is i64 only when both tables are.
			dst64, src64 := c.tableIsIndex64(dstTableIndex), c.tableIsIndex64(srcTableIndex)
			copySize := c.zeroExtendIndex(state.pop(), dst64 && src64)
			srcOffset := c.zeroExtendIndex(state.pop(), src64)
			dstOffset := c.zeroExtendIndex(state.pop(), dst64)

			// Out of bounds check.
			dstTableInstancePtr := c.boundsCheckInTable(dstTableIndex, dstOffset, copySize, dst64)
			srcTableInstancePtr := c.boundsCheckInTable(srcTableIndex, srcOffset, copySize, src64)

			dstTableBaseAddr := c.loadTableBaseAddr(dstTableInstancePtr)
			srcTableBaseAddr := c.loadTableBaseAddr(srcTableInstancePtr)

			three := builder.AllocateInstruction().AsIconst64(3).Insert(builder).Return()

			dstOffsetInBytes := builder.AllocateInstruction().AsIshl(dstOffset, three).Insert(builder).Return()
			dstAddr := builder.AllocateInstruction().AsIadd(dstTableBaseAddr, dstOffsetInBytes).Insert(builder).Return()
			srcOffsetInBytes := builder.AllocateInstruction().AsIshl(srcOffset, three).Insert(builder).Return()
			srcAddr := builder.AllocateInstruction().AsIadd(srcTableBaseAddr, srcOffsetInBytes).Insert(builder).Return()

			copySizeInBytes := builder.AllocateInstruction().AsIshl(copySize, three).Insert(builder).Return()
			c.callMemmove(dstAddr, srcAddr, copySizeInBytes)

		case wasm.OpcodeMiscMemoryCopy:
			dstMemIndex := wasm.Index(c.readI32u())
			srcMemIndex := wasm.Index(c.readI32u())
			if state.unreachable {
				break
			}

			// memory.copy x y : [at_x, at_y, min(at_x, at_y)] -> [], so the
			// length is i64 only when both memories are.
			dst64, src64 := c.memoryIsIndex64(dstMemIndex), c.memoryIsIndex64(srcMemIndex)
			copySize := c.zeroExtendIndex(state.pop(), dst64 && src64)
			srcOffset := c.zeroExtendIndex(state.pop(), src64)
			dstOffset := c.zeroExtendIndex(state.pop(), dst64)

			// Out of bounds check.
			dstMemLen := c.getMemoryLenValue(dstMemIndex, false)
			srcMemLen := c.getMemoryLenValue(srcMemIndex, false)
			c.boundsCheckInMemory(dstMemLen, dstOffset, copySize, dst64)
			c.boundsCheckInMemory(srcMemLen, srcOffset, copySize, src64)

			dstMemBase := c.getMemoryBaseValue(dstMemIndex, false)
			srcMemBase := c.getMemoryBaseValue(srcMemIndex, false)
			dstAddr := builder.AllocateInstruction().AsIadd(dstMemBase, dstOffset).Insert(builder).Return()
			srcAddr := builder.AllocateInstruction().AsIadd(srcMemBase, srcOffset).Insert(builder).Return()

			c.callMemmove(dstAddr, srcAddr, copySize)

		case wasm.OpcodeMiscTableFill:
			tableIndex := c.readI32u()
			if state.unreachable {
				break
			}
			// table.fill x : [at, ref, at] -> []
			index64 := c.tableIsIndex64(tableIndex)
			fillSize := state.pop()
			value := state.pop()
			offset := state.pop()

			fillSizeExt := c.zeroExtendIndex(fillSize, index64)
			offsetExt := c.zeroExtendIndex(offset, index64)
			tableInstancePtr := c.boundsCheckInTable(tableIndex, offsetExt, fillSizeExt, index64)

			three := builder.AllocateInstruction().AsIconst64(3).Insert(builder).Return()
			offsetInBytes := builder.AllocateInstruction().AsIshl(offsetExt, three).Insert(builder).Return()
			fillSizeInBytes := builder.AllocateInstruction().AsIshl(fillSizeExt, three).Insert(builder).Return()

			// Calculate the base address of the table.
			tableBaseAddr := c.loadTableBaseAddr(tableInstancePtr)
			addr := builder.AllocateInstruction().AsIadd(tableBaseAddr, offsetInBytes).Insert(builder).Return()

			// Uses the copy trick for faster filling buffer like memory.fill, but in this case we copy 8 bytes at a time.
			// Tables are rarely huge, so ignore the 8KB maximum.
			// https://github.com/golang/go/blob/go1.24.0/src/slices/slices.go#L514-L517
			//
			// 	buf := memoryInst.Buffer[offset : offset+fillSize]
			// 	buf[0:8] = value
			// 	for i := 8; i < fillSize; i *= 2 { Begin with 8 bytes.
			// 		copy(buf[i:], buf[:i])
			// 	}

			// Prepare the loop and following block.
			beforeLoop := builder.AllocateBasicBlock()
			loopBlk := builder.AllocateBasicBlock()
			loopVar := loopBlk.AddParam(builder, ssa.TypeI64)
			followingBlk := builder.AllocateBasicBlock()

			// Insert the jump to the beforeLoop block; If the fillSize is zero, then jump to the following block to skip entire logics.
			zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
			ifFillSizeZero := builder.AllocateInstruction().AsIcmp(fillSizeExt, zero, ssa.IntegerCmpCondEqual).
				Insert(builder).Return()
			builder.AllocateInstruction().AsBrnz(ifFillSizeZero, ssa.ValuesNil, followingBlk).Insert(builder)
			c.insertJumpToBlock(ssa.ValuesNil, beforeLoop)

			// buf[0:8] = value
			builder.SetCurrentBlock(beforeLoop)
			builder.AllocateInstruction().AsStore(ssa.OpcodeStore, value, addr, 0).Insert(builder)
			eight := builder.AllocateInstruction().AsIconst64(8).Insert(builder).Return()
			c.insertJumpToBlock(c.allocateVarLengthValues(1, eight), loopBlk)

			builder.SetCurrentBlock(loopBlk)
			dstAddr := builder.AllocateInstruction().AsIadd(addr, loopVar).Insert(builder).Return()

			newLoopVar := builder.AllocateInstruction().AsIadd(loopVar, loopVar).Insert(builder).Return()
			newLoopVarLessThanFillSize := builder.AllocateInstruction().
				AsIcmp(newLoopVar, fillSizeInBytes, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()

			// On the last iteration, count must be fillSizeInBytes-loopVar.
			diff := builder.AllocateInstruction().AsIsub(fillSizeInBytes, loopVar).Insert(builder).Return()
			count := builder.AllocateInstruction().AsSelect(newLoopVarLessThanFillSize, loopVar, diff).Insert(builder).Return()

			c.callMemmove(dstAddr, addr, count)

			builder.AllocateInstruction().
				AsBrnz(newLoopVarLessThanFillSize, c.allocateVarLengthValues(1, newLoopVar), loopBlk).
				Insert(builder)

			c.insertJumpToBlock(ssa.ValuesNil, followingBlk)
			builder.SetCurrentBlock(followingBlk)

			builder.Seal(beforeLoop)
			builder.Seal(loopBlk)
			builder.Seal(followingBlk)

		case wasm.OpcodeMiscMemoryFill:
			memIndex := wasm.Index(c.readI32u())
			if state.unreachable {
				break
			}

			// memory.fill x : [at, i32, at] -> []
			index64 := c.memoryIsIndex64(memIndex)
			fillSizeOperand := state.pop()
			fillSize := c.zeroExtendIndex(fillSizeOperand, index64)
			value := state.pop()
			offset := c.zeroExtendIndex(state.pop(), index64)

			// Out of bounds check.
			c.boundsCheckInMemory(c.getMemoryLenValue(memIndex, false), offset, fillSize, index64)

			// Calculate the base address:
			addr := builder.AllocateInstruction().AsIadd(c.getMemoryBaseValue(memIndex, false), offset).Insert(builder).Return()

			if def := builder.InstructionOfValue(fillSizeOperand); def != nil && def.Constant() &&
				def.ConstantVal() <= memoryFillInlineMaxBytes {
				// A short constant fill is a handful of stores; producers emit
				// it for every small struct or array zeroing.
				c.inlineMemoryFill(addr, value, uint32(def.ConstantVal()))
				break
			}

			// Uses the copy trick for faster filling buffer, with a maximum chunk size of 8KB.
			// https://github.com/golang/go/blob/go1.24.0/src/bytes/bytes.go#L664-L673
			//
			// 	buf := memoryInst.Buffer[offset : offset+fillSize]
			// 	buf[0] = value
			// 	for i := 1; i < fillSize; {
			// 		chunk := ((i - 1) & 8191) + 1
			// 		copy(buf[i:], buf[:chunk])
			// 		i += chunk
			// 	}

			// Prepare the loop and following block.
			beforeLoop := builder.AllocateBasicBlock()
			loopBlk := builder.AllocateBasicBlock()
			loopVar := loopBlk.AddParam(builder, ssa.TypeI64)
			followingBlk := builder.AllocateBasicBlock()

			// Insert the jump to the beforeLoop block; If the fillSize is zero, then jump to the following block to skip entire logics.
			zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
			ifFillSizeZero := builder.AllocateInstruction().AsIcmp(fillSize, zero, ssa.IntegerCmpCondEqual).
				Insert(builder).Return()
			builder.AllocateInstruction().AsBrnz(ifFillSizeZero, ssa.ValuesNil, followingBlk).Insert(builder)
			c.insertJumpToBlock(ssa.ValuesNil, beforeLoop)

			// buf[0] = value
			builder.SetCurrentBlock(beforeLoop)
			builder.AllocateInstruction().AsStore(ssa.OpcodeIstore8, value, addr, 0).Insert(builder)
			one := builder.AllocateInstruction().AsIconst64(1).Insert(builder).Return()
			c.insertJumpToBlock(c.allocateVarLengthValues(1, one), loopBlk)

			builder.SetCurrentBlock(loopBlk)
			dstAddr := builder.AllocateInstruction().AsIadd(addr, loopVar).Insert(builder).Return()

			// chunk := ((i - 1) & 8191) + 1
			mask := builder.AllocateInstruction().AsIconst64(8191).Insert(builder).Return()
			tmp1 := builder.AllocateInstruction().AsIsub(loopVar, one).Insert(builder).Return()
			tmp2 := builder.AllocateInstruction().AsBand(tmp1, mask).Insert(builder).Return()
			chunk := builder.AllocateInstruction().AsIadd(tmp2, one).Insert(builder).Return()

			// i += chunk
			newLoopVar := builder.AllocateInstruction().AsIadd(loopVar, chunk).Insert(builder).Return()
			newLoopVarLessThanFillSize := builder.AllocateInstruction().
				AsIcmp(newLoopVar, fillSize, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()

			// count = min(chunk, fillSize-loopVar)
			diff := builder.AllocateInstruction().AsIsub(fillSize, loopVar).Insert(builder).Return()
			count := builder.AllocateInstruction().AsSelect(newLoopVarLessThanFillSize, chunk, diff).Insert(builder).Return()

			c.callMemmove(dstAddr, addr, count)

			builder.AllocateInstruction().
				AsBrnz(newLoopVarLessThanFillSize, c.allocateVarLengthValues(1, newLoopVar), loopBlk).
				Insert(builder)

			c.insertJumpToBlock(ssa.ValuesNil, followingBlk)
			builder.SetCurrentBlock(followingBlk)

			builder.Seal(beforeLoop)
			builder.Seal(loopBlk)
			builder.Seal(followingBlk)

		case wasm.OpcodeMiscMemoryInit:
			index := c.readI32u()
			memIndex := wasm.Index(c.readI32u())
			if state.unreachable {
				break
			}

			// memory.init x y : [at, i32, i32] -> [], so only the destination
			// offset follows the memory's index type.
			copySize := c.zeroExtendIndex(state.pop(), false)
			offsetInDataInstance := c.zeroExtendIndex(state.pop(), false)
			offsetInMemory := c.zeroExtendIndex(state.pop(), c.memoryIsIndex64(memIndex))

			dataInstPtr := c.dataOrElementInstanceAddr(index, c.offset.DataInstances1stElement)

			// Bounds check. The copy size is a zero-extended i32, so the sum can
			// only carry when the destination offset is a 64-bit memory's.
			c.boundsCheckInMemory(c.getMemoryLenValue(memIndex, false), offsetInMemory, copySize, c.memoryIsIndex64(memIndex))
			c.boundsCheckInDataOrElementInstance(dataInstPtr, offsetInDataInstance, copySize, nativeapi.ExitCodeMemoryOutOfBounds)

			dataInstBaseAddr := builder.AllocateInstruction().AsLoad(dataInstPtr, 0, ssa.TypeI64).Insert(builder).Return()
			srcAddr := builder.AllocateInstruction().AsIadd(dataInstBaseAddr, offsetInDataInstance).Insert(builder).Return()

			memBase := c.getMemoryBaseValue(memIndex, false)
			dstAddr := builder.AllocateInstruction().AsIadd(memBase, offsetInMemory).Insert(builder).Return()

			c.callMemmove(dstAddr, srcAddr, copySize)

		case wasm.OpcodeMiscTableInit:
			elemIndex := c.readI32u()
			tableIndex := c.readI32u()
			if state.unreachable {
				break
			}

			// table.init x y : [at, i32, i32] -> [], so only the destination
			// offset follows the table's index type.
			index64 := c.tableIsIndex64(tableIndex)
			copySize := c.zeroExtendIndex(state.pop(), false)
			offsetInElementInstance := c.zeroExtendIndex(state.pop(), false)
			offsetInTable := c.zeroExtendIndex(state.pop(), index64)

			elemInstPtr := c.dataOrElementInstanceAddr(elemIndex, c.offset.ElementInstances1stElement)

			// Bounds check. The copy size is a zero-extended i32, so the sum can
			// only carry when the destination offset is a 64-bit table's.
			tableInstancePtr := c.boundsCheckInTable(tableIndex, offsetInTable, copySize, index64)
			c.boundsCheckInDataOrElementInstance(elemInstPtr, offsetInElementInstance, copySize, nativeapi.ExitCodeTableOutOfBounds)

			three := builder.AllocateInstruction().AsIconst64(3).Insert(builder).Return()
			// Calculates the destination address in the table.
			tableOffsetInBytes := builder.AllocateInstruction().AsIshl(offsetInTable, three).Insert(builder).Return()
			tableBaseAddr := c.loadTableBaseAddr(tableInstancePtr)
			dstAddr := builder.AllocateInstruction().AsIadd(tableBaseAddr, tableOffsetInBytes).Insert(builder).Return()

			// Calculates the source address in the element instance.
			srcOffsetInBytes := builder.AllocateInstruction().AsIshl(offsetInElementInstance, three).Insert(builder).Return()
			elemInstBaseAddr := builder.AllocateInstruction().AsLoad(elemInstPtr, 0, ssa.TypeI64).Insert(builder).Return()
			srcAddr := builder.AllocateInstruction().AsIadd(elemInstBaseAddr, srcOffsetInBytes).Insert(builder).Return()

			copySizeInBytes := builder.AllocateInstruction().AsIshl(copySize, three).Insert(builder).Return()
			c.callMemmove(dstAddr, srcAddr, copySizeInBytes)

		case wasm.OpcodeMiscElemDrop:
			index := c.readI32u()
			if state.unreachable {
				break
			}

			c.dropDataOrElementInstance(index, c.offset.ElementInstances1stElement)

		case wasm.OpcodeMiscDataDrop:
			index := c.readI32u()
			if state.unreachable {
				break
			}
			c.dropDataOrElementInstance(index, c.offset.DataInstances1stElement)

		default:
			panic("Unknown MiscOp " + wasm.MiscInstructionName(miscOp))
		}

	case wasm.OpcodeI32ReinterpretF32:
		if state.unreachable {
			break
		}
		reinterpret := builder.AllocateInstruction().
			AsBitcast(state.pop(), ssa.TypeI32).
			Insert(builder).Return()
		state.push(reinterpret)

	case wasm.OpcodeI64ReinterpretF64:
		if state.unreachable {
			break
		}
		reinterpret := builder.AllocateInstruction().
			AsBitcast(state.pop(), ssa.TypeI64).
			Insert(builder).Return()
		state.push(reinterpret)

	case wasm.OpcodeF32ReinterpretI32:
		if state.unreachable {
			break
		}
		reinterpret := builder.AllocateInstruction().
			AsBitcast(state.pop(), ssa.TypeF32).
			Insert(builder).Return()
		state.push(reinterpret)

	case wasm.OpcodeF64ReinterpretI64:
		if state.unreachable {
			break
		}
		reinterpret := builder.AllocateInstruction().
			AsBitcast(state.pop(), ssa.TypeF64).
			Insert(builder).Return()
		state.push(reinterpret)

	case wasm.OpcodeI32DivS, wasm.OpcodeI64DivS:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		result := builder.AllocateInstruction().AsSDiv(x, y, c.execCtxPtrValue).Insert(builder).Return()
		state.push(result)

	case wasm.OpcodeI32DivU, wasm.OpcodeI64DivU:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		result := builder.AllocateInstruction().AsUDiv(x, y, c.execCtxPtrValue).Insert(builder).Return()
		state.push(result)

	case wasm.OpcodeI32RemS, wasm.OpcodeI64RemS:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		result := builder.AllocateInstruction().AsSRem(x, y, c.execCtxPtrValue).Insert(builder).Return()
		state.push(result)

	case wasm.OpcodeI32RemU, wasm.OpcodeI64RemU:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		result := builder.AllocateInstruction().AsURem(x, y, c.execCtxPtrValue).Insert(builder).Return()
		state.push(result)

	case wasm.OpcodeI32And, wasm.OpcodeI64And:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		and := builder.AllocateInstruction()
		and.AsBand(x, y)
		builder.InsertInstruction(and)
		value := and.Return()
		state.push(value)
	case wasm.OpcodeI32Or, wasm.OpcodeI64Or:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		or := builder.AllocateInstruction()
		or.AsBor(x, y)
		builder.InsertInstruction(or)
		value := or.Return()
		state.push(value)
	case wasm.OpcodeI32Xor, wasm.OpcodeI64Xor:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		xor := builder.AllocateInstruction()
		xor.AsBxor(x, y)
		builder.InsertInstruction(xor)
		value := xor.Return()
		state.push(value)
	case wasm.OpcodeI32Shl, wasm.OpcodeI64Shl:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		ishl := builder.AllocateInstruction()
		ishl.AsIshl(x, y)
		builder.InsertInstruction(ishl)
		value := ishl.Return()
		state.push(value)
	case wasm.OpcodeI32ShrU, wasm.OpcodeI64ShrU:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		ishl := builder.AllocateInstruction()
		ishl.AsUshr(x, y)
		builder.InsertInstruction(ishl)
		value := ishl.Return()
		state.push(value)
	case wasm.OpcodeI32ShrS, wasm.OpcodeI64ShrS:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		ishl := builder.AllocateInstruction()
		ishl.AsSshr(x, y)
		builder.InsertInstruction(ishl)
		value := ishl.Return()
		state.push(value)
	case wasm.OpcodeI32Rotl, wasm.OpcodeI64Rotl:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		rotl := builder.AllocateInstruction()
		rotl.AsRotl(x, y)
		builder.InsertInstruction(rotl)
		value := rotl.Return()
		state.push(value)
	case wasm.OpcodeI32Rotr, wasm.OpcodeI64Rotr:
		if state.unreachable {
			break
		}
		y, x := state.pop(), state.pop()
		rotr := builder.AllocateInstruction()
		rotr.AsRotr(x, y)
		builder.InsertInstruction(rotr)
		value := rotr.Return()
		state.push(value)
	case wasm.OpcodeI32Clz, wasm.OpcodeI64Clz:
		if state.unreachable {
			break
		}
		x := state.pop()
		clz := builder.AllocateInstruction()
		clz.AsClz(x)
		builder.InsertInstruction(clz)
		value := clz.Return()
		state.push(value)
	case wasm.OpcodeI32Ctz, wasm.OpcodeI64Ctz:
		if state.unreachable {
			break
		}
		x := state.pop()
		ctz := builder.AllocateInstruction()
		ctz.AsCtz(x)
		builder.InsertInstruction(ctz)
		value := ctz.Return()
		state.push(value)
	case wasm.OpcodeI32Popcnt, wasm.OpcodeI64Popcnt:
		if state.unreachable {
			break
		}
		x := state.pop()
		popcnt := builder.AllocateInstruction()
		popcnt.AsPopcnt(x)
		builder.InsertInstruction(popcnt)
		value := popcnt.Return()
		state.push(value)

	case wasm.OpcodeI32WrapI64:
		if state.unreachable {
			break
		}
		x := state.pop()
		wrap := builder.AllocateInstruction().AsIreduce(x, ssa.TypeI32).Insert(builder).Return()
		state.push(wrap)
	case wasm.OpcodeGlobalGet:
		index := c.readI32u()
		if state.unreachable {
			break
		}
		v := c.getWasmGlobalValue(index, false)
		state.push(v)
	case wasm.OpcodeGlobalSet:
		index := c.readI32u()
		if state.unreachable {
			break
		}
		v := state.pop()
		c.setWasmGlobalValue(index, v)
	case wasm.OpcodeLocalGet:
		index := c.readI32u()
		if state.unreachable {
			break
		}
		variable := c.localVariable(index)
		state.push(builder.MustFindValue(variable))

	case wasm.OpcodeLocalSet:
		index := c.readI32u()
		if state.unreachable {
			break
		}
		variable := c.localVariable(index)
		newValue := state.pop()
		builder.DefineVariableInCurrentBB(variable, newValue)
		if c.tryTableDepth > 0 {
			c.storeLocalToSaveArea(wasm.Index(index), newValue)
		}

	case wasm.OpcodeLocalTee:
		index := c.readI32u()
		if state.unreachable {
			break
		}
		variable := c.localVariable(index)
		newValue := state.peek()
		builder.DefineVariableInCurrentBB(variable, newValue)
		if c.tryTableDepth > 0 {
			c.storeLocalToSaveArea(wasm.Index(index), newValue)
		}

	case wasm.OpcodeSelect, wasm.OpcodeTypedSelect:
		if op == wasm.OpcodeTypedSelect {
			state.pc += 2 // ignores the type which is only needed during validation.
		}

		if state.unreachable {
			break
		}

		cond := state.pop()
		v2 := state.pop()
		v1 := state.pop()

		sl := builder.AllocateInstruction().
			AsSelect(cond, v1, v2).
			Insert(builder).
			Return()
		state.push(sl)

	case wasm.OpcodeMemorySize:
		memIndex := wasm.Index(c.readI32u())
		if state.unreachable {
			break
		}

		// The byte length itself, not just the resulting page count, must be
		// read as a full 64-bit value: a 65536-page (the legal maximum)
		// memory has a byte length of exactly 2^32, which a 32-bit load
		// truncates to zero. getMemoryLenValue already reads a full i64 (and,
		// for a shared memory, an atomic one -- required since another
		// thread can concurrently grow it); using it here (rather than a
		// hand-rolled duplicate load) also lets it and any load/store bounds
		// check on the same memory in this linear path share one read.
		memSizeInBytes := c.getMemoryLenValue(memIndex, false)

		amount := builder.AllocateInstruction().AsIconst64(uint64(wasm.MemoryPageSizeInBits)).Insert(builder).Return()
		memSize64 := builder.AllocateInstruction().
			AsUshr(memSizeInBytes, amount).
			Insert(builder).
			Return()
		if c.memoryIsIndex64(memIndex) {
			// memory.size on a 64-bit memory yields an i64, so the page count
			// is already the right width.
			state.push(memSize64)
			break
		}
		memSize := builder.AllocateInstruction().
			AsIreduce(memSize64, ssa.TypeI32).
			Insert(builder).
			Return()
		state.push(memSize)

	case wasm.OpcodeMemoryGrow:
		memIndex := wasm.Index(c.readI32u())
		if state.unreachable {
			break
		}

		pages := state.pop()
		if c.memoryIsIndex64(memIndex) {
			// A 64-bit memory's page delta spans a range the in-capacity fast
			// path's arithmetic cannot bound without extra overflow checks.
			// memory.grow is rare enough that the Go trampoline -- which
			// already handles the whole range in MemoryInstance.Grow64 -- is
			// the better trade.
			state.push(c.lowerMemoryGrowCall(memIndex, pages))
			c.reloadAllMemories()
			break
		}
		if memIndex >= c.m.ImportMemoryCount && !c.memoryShared[memIndex] {
			state.push(c.lowerLocalMemoryGrow(memIndex, pages))
			// A local memory has its own dedicated *wasm.MemoryInstance (see
			// buildMemory), so growing it can never move another index's
			// buffer; only this index's cache needs reloading.
			c.reloadMemoryBaseLen(memIndex)
		} else {
			state.push(c.lowerMemoryGrowCall(memIndex, pages))
			// An imported memory's *wasm.MemoryInstance can be aliased by
			// another imported index at runtime (this module is compiled once
			// and reused across instantiations with different import
			// bindings, so the compiler cannot know the import graph).
			// Conservatively reload every memory (globals can't be affected
			// by a memory grow, so reloadAfterCall's extra global reload
			// isn't needed here).
			c.reloadAllMemories()
		}

	case wasm.OpcodeI32Store,
		wasm.OpcodeI64Store,
		wasm.OpcodeF32Store,
		wasm.OpcodeF64Store,
		wasm.OpcodeI32Store8,
		wasm.OpcodeI32Store16,
		wasm.OpcodeI64Store8,
		wasm.OpcodeI64Store16,
		wasm.OpcodeI64Store32:

		_, offset, disp, memIndex := c.readMemArg()
		if state.unreachable {
			break
		}
		var opSize uint64
		var opcode ssa.Opcode
		switch op {
		case wasm.OpcodeI32Store, wasm.OpcodeF32Store:
			opcode = ssa.OpcodeStore
			opSize = 4
		case wasm.OpcodeI64Store, wasm.OpcodeF64Store:
			opcode = ssa.OpcodeStore
			opSize = 8
		case wasm.OpcodeI32Store8, wasm.OpcodeI64Store8:
			opcode = ssa.OpcodeIstore8
			opSize = 1
		case wasm.OpcodeI32Store16, wasm.OpcodeI64Store16:
			opcode = ssa.OpcodeIstore16
			opSize = 2
		case wasm.OpcodeI64Store32:
			opcode = ssa.OpcodeIstore32
			opSize = 4
		default:
			panic("BUG")
		}

		value := state.pop()
		baseAddr := state.pop()
		addr := c.memOpSetup(memIndex, baseAddr, offset, opSize)
		builder.AllocateInstruction().
			AsStore(opcode, value, addr, disp).
			Insert(builder)

	case wasm.OpcodeI32Load,
		wasm.OpcodeI64Load,
		wasm.OpcodeF32Load,
		wasm.OpcodeF64Load,
		wasm.OpcodeI32Load8S,
		wasm.OpcodeI32Load8U,
		wasm.OpcodeI32Load16S,
		wasm.OpcodeI32Load16U,
		wasm.OpcodeI64Load8S,
		wasm.OpcodeI64Load8U,
		wasm.OpcodeI64Load16S,
		wasm.OpcodeI64Load16U,
		wasm.OpcodeI64Load32S,
		wasm.OpcodeI64Load32U:
		_, offset, disp, memIndex := c.readMemArg()
		if state.unreachable {
			break
		}

		var opSize uint64
		switch op {
		case wasm.OpcodeI32Load, wasm.OpcodeF32Load:
			opSize = 4
		case wasm.OpcodeI64Load, wasm.OpcodeF64Load:
			opSize = 8
		case wasm.OpcodeI32Load8S, wasm.OpcodeI32Load8U:
			opSize = 1
		case wasm.OpcodeI32Load16S, wasm.OpcodeI32Load16U:
			opSize = 2
		case wasm.OpcodeI64Load8S, wasm.OpcodeI64Load8U:
			opSize = 1
		case wasm.OpcodeI64Load16S, wasm.OpcodeI64Load16U:
			opSize = 2
		case wasm.OpcodeI64Load32S, wasm.OpcodeI64Load32U:
			opSize = 4
		default:
			panic("BUG")
		}

		baseAddr := state.pop()
		addr := c.memOpSetup(memIndex, baseAddr, offset, opSize)
		load := builder.AllocateInstruction()
		switch op {
		case wasm.OpcodeI32Load:
			load.AsLoad(addr, disp, ssa.TypeI32)
		case wasm.OpcodeI64Load:
			load.AsLoad(addr, disp, ssa.TypeI64)
		case wasm.OpcodeF32Load:
			load.AsLoad(addr, disp, ssa.TypeF32)
		case wasm.OpcodeF64Load:
			load.AsLoad(addr, disp, ssa.TypeF64)
		case wasm.OpcodeI32Load8S:
			load.AsExtLoad(ssa.OpcodeSload8, addr, disp, false)
		case wasm.OpcodeI32Load8U:
			load.AsExtLoad(ssa.OpcodeUload8, addr, disp, false)
		case wasm.OpcodeI32Load16S:
			load.AsExtLoad(ssa.OpcodeSload16, addr, disp, false)
		case wasm.OpcodeI32Load16U:
			load.AsExtLoad(ssa.OpcodeUload16, addr, disp, false)
		case wasm.OpcodeI64Load8S:
			load.AsExtLoad(ssa.OpcodeSload8, addr, disp, true)
		case wasm.OpcodeI64Load8U:
			load.AsExtLoad(ssa.OpcodeUload8, addr, disp, true)
		case wasm.OpcodeI64Load16S:
			load.AsExtLoad(ssa.OpcodeSload16, addr, disp, true)
		case wasm.OpcodeI64Load16U:
			load.AsExtLoad(ssa.OpcodeUload16, addr, disp, true)
		case wasm.OpcodeI64Load32S:
			load.AsExtLoad(ssa.OpcodeSload32, addr, disp, true)
		case wasm.OpcodeI64Load32U:
			load.AsExtLoad(ssa.OpcodeUload32, addr, disp, true)
		default:
			panic("BUG")
		}
		builder.InsertInstruction(load)
		state.push(load.Return())
	case wasm.OpcodeBlock:
		// Note: we do not need to create a BB for this as that would always have only one predecessor
		// which is the current BB, and therefore it's always ok to merge them in any way.

		bt := c.readBlockType()

		if state.unreachable {
			state.unreachableDepth++
			break
		}

		followingBlk := builder.AllocateBasicBlock()
		c.addBlockParamsFromWasmTypes(bt.Results, followingBlk)

		state.ctrlPush(controlFrame{
			kind:                         controlFrameKindBlock,
			originalStackLenWithoutParam: len(state.values) - len(bt.Params),
			followingBlock:               followingBlk,
			blockType:                    bt,
		})
	case wasm.OpcodeLoop:
		bt := c.readBlockType()

		if state.unreachable {
			state.unreachableDepth++
			break
		}

		loopHeader, afterLoopBlock := builder.AllocateBasicBlock(), builder.AllocateBasicBlock()
		c.addBlockParamsFromWasmTypes(bt.Params, loopHeader)
		c.addBlockParamsFromWasmTypes(bt.Results, afterLoopBlock)

		originalLen := len(state.values) - len(bt.Params)
		state.ctrlPush(controlFrame{
			originalStackLenWithoutParam: originalLen,
			kind:                         controlFrameKindLoop,
			blk:                          loopHeader,
			followingBlock:               afterLoopBlock,
			blockType:                    bt,
		})

		args := c.allocateVarLengthValues(len(bt.Params), state.values[originalLen:]...)

		// The interrupt-check mask is loop-invariant, so load it once here in the
		// preheader (which dominates the loop header) rather than every iteration.
		var interruptMaskVal ssa.Value
		if c.ensureTermination && c.interruptCheckInterval != 0 {
			interruptMaskVal = builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetInterruptCheckMask.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()
		}

		// Insert the jump to the header of loop.
		br := builder.AllocateInstruction()
		br.AsJump(args, loopHeader)
		builder.InsertInstruction(br)

		c.switchTo(originalLen, loopHeader)

		if c.gcEnabled {
			// A loop header is where execution parks for a collection: a guest cannot run unboundedly
			// without passing one, so this is what bounds how long a collection waits. The poll itself is
			// a load and a rarely-taken branch; only the branch calls into Go.
			c.emitGCSafepoint(builder)
		}

		if c.ensureTermination {
			if c.interruptCheckInterval == 0 {
				// Check every iteration: a Go round-trip (also the scheduler/GC
				// yield point) at every loop header.
				c.emitCheckModuleExitCode(builder)
			} else {
				// Amortized checking: bump a counter in the execution context and
				// only do the Go round-trip when (counter & mask) == 0. The mask
				// (= interval-1) was hoisted to the preheader (interruptMaskVal) and
				// comes from the execution context at runtime rather than baked in,
				// so the yield frequency can be retuned per run/per loop without
				// recompiling. mask==0 (interval 1) degenerates to checking every
				// iteration.
				current := builder.AllocateInstruction().
					AsLoad(c.execCtxPtrValue,
						nativeapi.ExecutionContextOffsetInterruptCounter.U32(),
						ssa.TypeI64,
					).Insert(builder).Return()
				one := builder.AllocateInstruction().AsIconst64(1).Insert(builder).Return()
				next := builder.AllocateInstruction().AsIadd(current, one).Insert(builder).Return()
				builder.AllocateInstruction().
					AsStore(ssa.OpcodeStore, next, c.execCtxPtrValue,
						nativeapi.ExecutionContextOffsetInterruptCounter.U32()).
					Insert(builder)

				masked := builder.AllocateInstruction().AsBand(next, interruptMaskVal).Insert(builder).Return()
				zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
				cond := builder.AllocateInstruction().
					AsIcmp(masked, zero, ssa.IntegerCmpCondEqual).Insert(builder).Return()

				checkBlk := builder.AllocateBasicBlock()
				afterBlk := builder.AllocateBasicBlock()

				builder.AllocateInstruction().AsBrnz(cond, ssa.ValuesNil, checkBlk).Insert(builder)
				builder.AllocateInstruction().AsJump(ssa.ValuesNil, afterBlk).Insert(builder)

				builder.SetCurrentBlock(checkBlk)
				c.emitCheckModuleExitCode(builder)
				builder.AllocateInstruction().AsJump(ssa.ValuesNil, afterBlk).Insert(builder)
				builder.Seal(checkBlk)

				builder.SetCurrentBlock(afterBlk)
				builder.Seal(afterBlk)
			}
		}
	case wasm.OpcodeIf:
		bt := c.readBlockType()

		if state.unreachable {
			state.unreachableDepth++
			break
		}

		v := state.pop()
		thenBlk, elseBlk, followingBlk := builder.AllocateBasicBlock(), builder.AllocateBasicBlock(), builder.AllocateBasicBlock()

		// We do not make the Wasm-level block parameters as SSA-level block params for if-else blocks
		// since they won't be PHI and the definition is unique.

		// On the other hand, the following block after if-else-end will likely have
		// multiple definitions (one in Then and another in Else blocks).
		c.addBlockParamsFromWasmTypes(bt.Results, followingBlk)

		args := c.allocateVarLengthValues(len(bt.Params), state.values[len(state.values)-len(bt.Params):]...)

		// Insert the conditional jump to the Else block.
		brz := builder.AllocateInstruction()
		brz.AsBrz(v, ssa.ValuesNil, elseBlk)
		builder.InsertInstruction(brz)

		// Then, insert the jump to the Then block.
		br := builder.AllocateInstruction()
		br.AsJump(ssa.ValuesNil, thenBlk)
		builder.InsertInstruction(br)

		state.ctrlPush(controlFrame{
			kind:                         controlFrameKindIfWithoutElse,
			originalStackLenWithoutParam: len(state.values) - len(bt.Params),
			blk:                          elseBlk,
			followingBlock:               followingBlk,
			blockType:                    bt,
			clonedArgs:                   args,
		})

		builder.SetCurrentBlock(thenBlk)

		// Then and Else (if exists) have only one predecessor.
		builder.Seal(thenBlk)
		builder.Seal(elseBlk)
	case wasm.OpcodeElse:
		ifctrl := state.ctrlPeekAt(0)
		if unreachable := state.unreachable; unreachable && state.unreachableDepth > 0 {
			// If it is currently in unreachable and is a nested if,
			// we just remove the entire else block.
			break
		}

		ifctrl.kind = controlFrameKindIfWithElse
		if !state.unreachable {
			// If this Then block is currently reachable, we have to insert the branching to the following BB.
			followingBlk := ifctrl.followingBlock // == the BB after if-then-else.
			args := c.nPeekDup(len(ifctrl.blockType.Results))
			c.insertJumpToBlock(args, followingBlk)
		} else {
			state.unreachable = false
		}

		// Reset the stack so that we can correctly handle the else block.
		state.values = state.values[:ifctrl.originalStackLenWithoutParam]
		elseBlk := ifctrl.blk
		for _, arg := range ifctrl.clonedArgs.View() {
			state.push(arg)
		}

		builder.SetCurrentBlock(elseBlk)

	case wasm.OpcodeEnd:
		if state.unreachableDepth > 0 {
			state.unreachableDepth--
			break
		}

		ctrl := state.ctrlPop()
		followingBlk := ctrl.followingBlock

		unreachable := state.unreachable
		// Nothing branched out of this frame, so its continuation block would
		// only forward what is already here: stay in the current block instead,
		// and let dead-block elimination drop the one we never used. Restricted
		// to block and loop because an if's else arm still has to jump into
		// theirs, a try_table's continuation must stay outside the block-ID
		// range its handlers protect, and the function frame's is the return
		// block.
		fallThrough := !unreachable && followingBlk.Preds() == 0 &&
			(ctrl.kind == controlFrameKindBlock || ctrl.kind == controlFrameKindLoop)

		if !unreachable {
			// For try_table with catch clauses, emit the leave trampoline
			// before the jump to the following block. If there are no catch clauses,
			// skip since they never pushed a handler.
			if ctrl.isTryCatch() {
				c.emitTryTableLeave()
			}

			if !fallThrough {
				// Top n-th args will be used as a result of the current control frame.
				args := c.nPeekDup(len(ctrl.blockType.Results))

				// Insert the unconditional branch to the target.
				c.insertJumpToBlock(args, followingBlk)
			}
		} else { // recover from the unreachable state.
			state.unreachable = false
		}

		switch ctrl.kind {
		case controlFrameKindFunction:
			break // This is the very end of function.
		case controlFrameKindLoop:
			// Loop header block can be reached from any br/br_table contained in the loop,
			// so now that we've reached End of it, we can seal it.
			builder.Seal(ctrl.blk)
		case controlFrameKindIfWithoutElse:
			// If this is the end of Then block, we have to emit the empty Else block.
			elseBlk := ctrl.blk
			builder.SetCurrentBlock(elseBlk)
			c.insertJumpToBlock(ctrl.clonedArgs, followingBlk)
		case controlFrameKindTryTableWithCatch:
			if c.tryTableDepth > 0 {
				c.tryTableDepth--
			}
			// Finalize the pending EH entry's body block-ID range
			// regardless of reachability at this `end` (even if the body
			// became unreachable partway through, e.g. it always throws,
			// any of its blocks that DID get compiled must still be
			// covered by the side table).
			c.pendingEhEntries[ctrl.pendingEhIdx].BodyBlkIDEnd = ssa.BasicBlockID(c.ssaBuilder.BlockIDMax())
		}

		builder.Seal(followingBlk)

		if fallThrough {
			// Leave the frame's results on the stack exactly where the
			// continuation block's parameters would have put them.
			base := ctrl.originalStackLenWithoutParam
			results := len(ctrl.blockType.Results)
			copy(state.values[base:], state.values[len(state.values)-results:])
			state.values = state.values[:base+results]
			break
		}

		// Ready to start translating the following block.
		c.switchTo(ctrl.originalStackLenWithoutParam, followingBlk)

	case wasm.OpcodeBr:
		labelIndex := c.readI32u()
		if state.unreachable {
			break
		}

		c.emitTryTableLeaves(int(labelIndex))
		targetBlk, argNum := state.brTargetArgNumFor(labelIndex)
		args := c.nPeekDup(argNum)
		c.insertJumpToBlock(args, targetBlk)

		state.unreachable = true

	case wasm.OpcodeBrIf:
		labelIndex := c.readI32u()
		if state.unreachable {
			break
		}

		v := state.pop()

		targetBlk, argNum := state.brTargetArgNumFor(labelIndex)
		args := c.nPeekDup(argNum)
		var sealTargetBlk bool

		// If the branch exits any try_table frames, emit TryTableLeave
		// calls in a trampoline block that only runs on the taken path.
		if c.branchExitsTryTable(int(labelIndex)) {
			current := builder.CurrentBlock()
			trampolineBlk := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(trampolineBlk)
			c.emitTryTableLeaves(int(labelIndex))
			c.insertJumpToBlock(args, targetBlk)
			builder.SetCurrentBlock(current)
			targetBlk = trampolineBlk
			sealTargetBlk = true
			args = ssa.ValuesNil
		}

		if c.needListener && targetBlk.ReturnBlock() { // In this case, we have to call the listener before returning.
			// Save the currently active block.
			current := builder.CurrentBlock()

			// Allocate the trampoline block to the return where we call the listener.
			targetBlk = builder.AllocateBasicBlock()
			builder.SetCurrentBlock(targetBlk)
			sealTargetBlk = true

			c.callListenerAfter()

			instr := builder.AllocateInstruction()
			instr.AsReturn(args)
			builder.InsertInstruction(instr)

			args = ssa.ValuesNil

			// Revert the current block.
			builder.SetCurrentBlock(current)
		}

		// Insert the conditional jump to the target block.
		brnz := builder.AllocateInstruction()
		brnz.AsBrnz(v, args, targetBlk)
		builder.InsertInstruction(brnz)

		if sealTargetBlk {
			builder.Seal(targetBlk)
		}

		// Insert the unconditional jump to the Else block which corresponds to after br_if.
		elseBlk := builder.AllocateBasicBlock()
		c.insertJumpToBlock(ssa.ValuesNil, elseBlk)

		// Now start translating the instructions after br_if.
		builder.Seal(elseBlk) // Else of br_if has the current block as the only one successor.
		builder.SetCurrentBlock(elseBlk)

	case wasm.OpcodeBrTable:
		labels := state.tmpForBrTable[:0]
		labelCount := c.readI32u()
		for i := 0; i < int(labelCount); i++ {
			labels = append(labels, c.readI32u())
		}
		labels = append(labels, c.readI32u()) // default label.
		if state.unreachable {
			break
		}

		index := state.pop()
		if labelCount == 0 { // If this br_table is empty, we can just emit the unconditional jump.
			targetBlk, argNum := state.brTargetArgNumFor(labels[0])
			args := c.nPeekDup(argNum)
			c.insertJumpToBlock(args, targetBlk)
		} else {
			c.lowerBrTable(labels, index)
		}
		state.tmpForBrTable = labels // reuse the temporary slice for next use.
		state.unreachable = true

	case wasm.OpcodeNop:
	case wasm.OpcodeReturn:
		if state.unreachable {
			break
		}
		c.emitTryTableLeaves(len(state.controlFrames))
		if c.needListener {
			c.callListenerAfter()
		}

		c.lowerReturn(builder)
		state.unreachable = true

	case wasm.OpcodeUnreachable:
		if state.unreachable {
			break
		}
		exit := builder.AllocateInstruction()
		exit.AsExitWithCode(c.execCtxPtrValue, nativeapi.ExitCodeUnreachable)
		builder.InsertInstruction(exit)
		state.unreachable = true

	case wasm.OpcodeCallIndirect:
		typeIndex := c.readI32u()
		tableIndex := c.readI32u()
		if state.unreachable {
			break
		}
		c.lowerCallIndirect(typeIndex, tableIndex)

	case wasm.OpcodeCall:
		fnIndex := c.readI32u()
		if state.unreachable {
			break
		}
		c.lowerCall(fnIndex)

	case wasm.OpcodeDrop:
		if state.unreachable {
			break
		}
		_ = state.pop()
	case wasm.OpcodeF64ConvertI32S, wasm.OpcodeF64ConvertI64S, wasm.OpcodeF64ConvertI32U, wasm.OpcodeF64ConvertI64U:
		if state.unreachable {
			break
		}
		result := builder.AllocateInstruction().AsFcvtFromInt(
			state.pop(),
			op == wasm.OpcodeF64ConvertI32S || op == wasm.OpcodeF64ConvertI64S,
			true,
		).Insert(builder).Return()
		state.push(result)
	case wasm.OpcodeF32ConvertI32S, wasm.OpcodeF32ConvertI64S, wasm.OpcodeF32ConvertI32U, wasm.OpcodeF32ConvertI64U:
		if state.unreachable {
			break
		}
		result := builder.AllocateInstruction().AsFcvtFromInt(
			state.pop(),
			op == wasm.OpcodeF32ConvertI32S || op == wasm.OpcodeF32ConvertI64S,
			false,
		).Insert(builder).Return()
		state.push(result)
	case wasm.OpcodeF32DemoteF64:
		if state.unreachable {
			break
		}
		cvt := builder.AllocateInstruction()
		cvt.AsFdemote(state.pop())
		builder.InsertInstruction(cvt)
		state.push(cvt.Return())
	case wasm.OpcodeF64PromoteF32:
		if state.unreachable {
			break
		}
		cvt := builder.AllocateInstruction()
		cvt.AsFpromote(state.pop())
		builder.InsertInstruction(cvt)
		state.push(cvt.Return())

	case wasm.OpcodeVecPrefix:
		state.pc++
		vecOp, vecOpSize := wasm.ReadVecOpcode(c.wasmFunctionBody, state.pc)
		state.pc += vecOpSize - 1
		switch vecOp {
		case wasm.OpcodeVecV128Const:
			state.pc++
			lo := binary.LittleEndian.Uint64(c.wasmFunctionBody[state.pc:])
			state.pc += 8
			hi := binary.LittleEndian.Uint64(c.wasmFunctionBody[state.pc:])
			state.pc += 7
			if state.unreachable {
				break
			}
			ret := builder.AllocateInstruction().AsVconst(lo, hi).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Load:
			_, offset, disp, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, 16)
			load := builder.AllocateInstruction()
			load.AsLoad(addr, disp, ssa.TypeV128)
			builder.InsertInstruction(load)
			state.push(load.Return())
		case wasm.OpcodeVecV128Load8Lane, wasm.OpcodeVecV128Load16Lane, wasm.OpcodeVecV128Load32Lane:
			_, offset, disp, memIndex := c.readMemArg()
			state.pc++
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			var loadOp ssa.Opcode
			var opSize uint64
			switch vecOp {
			case wasm.OpcodeVecV128Load8Lane:
				loadOp, lane, opSize = ssa.OpcodeUload8, ssa.VecLaneI8x16, 1
			case wasm.OpcodeVecV128Load16Lane:
				loadOp, lane, opSize = ssa.OpcodeUload16, ssa.VecLaneI16x8, 2
			case wasm.OpcodeVecV128Load32Lane:
				loadOp, lane, opSize = ssa.OpcodeUload32, ssa.VecLaneI32x4, 4
			}
			laneIndex := c.wasmFunctionBody[state.pc]
			vector := state.pop()
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, opSize)
			load := builder.AllocateInstruction().
				AsExtLoad(loadOp, addr, disp, false).
				Insert(builder).Return()
			ret := builder.AllocateInstruction().
				AsInsertlane(vector, load, laneIndex, lane).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Load64Lane:
			_, offset, disp, memIndex := c.readMemArg()
			state.pc++
			if state.unreachable {
				break
			}
			laneIndex := c.wasmFunctionBody[state.pc]
			vector := state.pop()
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, 8)
			load := builder.AllocateInstruction().
				AsLoad(addr, disp, ssa.TypeI64).
				Insert(builder).Return()
			ret := builder.AllocateInstruction().
				AsInsertlane(vector, load, laneIndex, ssa.VecLaneI64x2).
				Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecV128Load32zero, wasm.OpcodeVecV128Load64zero:
			_, offset, disp, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			var scalarType ssa.Type
			switch vecOp {
			case wasm.OpcodeVecV128Load32zero:
				scalarType = ssa.TypeF32
			case wasm.OpcodeVecV128Load64zero:
				scalarType = ssa.TypeF64
			}

			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, uint64(scalarType.Size()))

			ret := builder.AllocateInstruction().
				AsVZeroExtLoad(addr, disp, scalarType).
				Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecV128Load8x8u, wasm.OpcodeVecV128Load8x8s,
			wasm.OpcodeVecV128Load16x4u, wasm.OpcodeVecV128Load16x4s,
			wasm.OpcodeVecV128Load32x2u, wasm.OpcodeVecV128Load32x2s:
			_, offset, disp, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			var signed bool
			switch vecOp {
			case wasm.OpcodeVecV128Load8x8s:
				signed = true
				fallthrough
			case wasm.OpcodeVecV128Load8x8u:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecV128Load16x4s:
				signed = true
				fallthrough
			case wasm.OpcodeVecV128Load16x4u:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecV128Load32x2s:
				signed = true
				fallthrough
			case wasm.OpcodeVecV128Load32x2u:
				lane = ssa.VecLaneI32x4
			}
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, 8)
			load := builder.AllocateInstruction().
				AsLoad(addr, disp, ssa.TypeF64).
				Insert(builder).Return()
			ret := builder.AllocateInstruction().
				AsWiden(load, lane, signed, true).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Load8Splat, wasm.OpcodeVecV128Load16Splat,
			wasm.OpcodeVecV128Load32Splat, wasm.OpcodeVecV128Load64Splat:
			_, offset, disp, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			var opSize uint64
			switch vecOp {
			case wasm.OpcodeVecV128Load8Splat:
				lane, opSize = ssa.VecLaneI8x16, 1
			case wasm.OpcodeVecV128Load16Splat:
				lane, opSize = ssa.VecLaneI16x8, 2
			case wasm.OpcodeVecV128Load32Splat:
				lane, opSize = ssa.VecLaneI32x4, 4
			case wasm.OpcodeVecV128Load64Splat:
				lane, opSize = ssa.VecLaneI64x2, 8
			}
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, opSize)
			ret := builder.AllocateInstruction().
				AsLoadSplat(addr, disp, lane).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Store:
			_, offset, disp, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}
			value := state.pop()
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, 16)
			builder.AllocateInstruction().
				AsStore(ssa.OpcodeStore, value, addr, disp).
				Insert(builder)
		case wasm.OpcodeVecV128Store8Lane, wasm.OpcodeVecV128Store16Lane,
			wasm.OpcodeVecV128Store32Lane, wasm.OpcodeVecV128Store64Lane:
			_, offset, disp, memIndex := c.readMemArg()
			state.pc++
			if state.unreachable {
				break
			}
			laneIndex := c.wasmFunctionBody[state.pc]
			var storeOp ssa.Opcode
			var lane ssa.VecLane
			var opSize uint64
			switch vecOp {
			case wasm.OpcodeVecV128Store8Lane:
				storeOp, lane, opSize = ssa.OpcodeIstore8, ssa.VecLaneI8x16, 1
			case wasm.OpcodeVecV128Store16Lane:
				storeOp, lane, opSize = ssa.OpcodeIstore16, ssa.VecLaneI16x8, 2
			case wasm.OpcodeVecV128Store32Lane:
				storeOp, lane, opSize = ssa.OpcodeIstore32, ssa.VecLaneI32x4, 4
			case wasm.OpcodeVecV128Store64Lane:
				storeOp, lane, opSize = ssa.OpcodeStore, ssa.VecLaneI64x2, 8
			}
			vector := state.pop()
			baseAddr := state.pop()
			addr := c.memOpSetup(memIndex, baseAddr, offset, opSize)
			value := builder.AllocateInstruction().
				AsExtractlane(vector, laneIndex, lane, false).
				Insert(builder).Return()
			builder.AllocateInstruction().
				AsStore(storeOp, value, addr, disp).
				Insert(builder)
		case wasm.OpcodeVecV128Not:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbnot(v1).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128And:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVband(v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128AndNot:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbandnot(v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Or:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbor(v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Xor:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbxor(v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128Bitselect:
			if state.unreachable {
				break
			}
			c := state.pop()
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbitselect(c, v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128AnyTrue:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVanyTrue(v1).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16AllTrue, wasm.OpcodeVecI16x8AllTrue, wasm.OpcodeVecI32x4AllTrue, wasm.OpcodeVecI64x2AllTrue:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16AllTrue:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8AllTrue:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4AllTrue:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2AllTrue:
				lane = ssa.VecLaneI64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVallTrue(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16BitMask, wasm.OpcodeVecI16x8BitMask, wasm.OpcodeVecI32x4BitMask, wasm.OpcodeVecI64x2BitMask:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16BitMask:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8BitMask:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4BitMask:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2BitMask:
				lane = ssa.VecLaneI64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVhighBits(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Abs, wasm.OpcodeVecI16x8Abs, wasm.OpcodeVecI32x4Abs, wasm.OpcodeVecI64x2Abs:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Abs:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Abs:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Abs:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Abs:
				lane = ssa.VecLaneI64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVIabs(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Neg, wasm.OpcodeVecI16x8Neg, wasm.OpcodeVecI32x4Neg, wasm.OpcodeVecI64x2Neg:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Neg:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Neg:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Neg:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Neg:
				lane = ssa.VecLaneI64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVIneg(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Popcnt:
			if state.unreachable {
				break
			}
			lane := ssa.VecLaneI8x16
			v1 := state.pop()

			ret := builder.AllocateInstruction().AsVIpopcnt(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Add, wasm.OpcodeVecI16x8Add, wasm.OpcodeVecI32x4Add, wasm.OpcodeVecI64x2Add:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Add:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Add:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Add:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Add:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVIadd(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16AddSatS, wasm.OpcodeVecI16x8AddSatS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16AddSatS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8AddSatS:
				lane = ssa.VecLaneI16x8
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVSaddSat(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16AddSatU, wasm.OpcodeVecI16x8AddSatU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16AddSatU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8AddSatU:
				lane = ssa.VecLaneI16x8
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVUaddSat(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16SubSatS, wasm.OpcodeVecI16x8SubSatS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16SubSatS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8SubSatS:
				lane = ssa.VecLaneI16x8
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVSsubSat(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16SubSatU, wasm.OpcodeVecI16x8SubSatU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16SubSatU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8SubSatU:
				lane = ssa.VecLaneI16x8
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVUsubSat(v1, v2, lane).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI8x16Sub, wasm.OpcodeVecI16x8Sub, wasm.OpcodeVecI32x4Sub, wasm.OpcodeVecI64x2Sub:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Sub:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Sub:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Sub:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Sub:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVIsub(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16MinS, wasm.OpcodeVecI16x8MinS, wasm.OpcodeVecI32x4MinS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16MinS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8MinS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4MinS:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVImin(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16MinU, wasm.OpcodeVecI16x8MinU, wasm.OpcodeVecI32x4MinU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16MinU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8MinU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4MinU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVUmin(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16MaxS, wasm.OpcodeVecI16x8MaxS, wasm.OpcodeVecI32x4MaxS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16MaxS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8MaxS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4MaxS:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVImax(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16MaxU, wasm.OpcodeVecI16x8MaxU, wasm.OpcodeVecI32x4MaxU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16MaxU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8MaxU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4MaxU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVUmax(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16AvgrU, wasm.OpcodeVecI16x8AvgrU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16AvgrU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8AvgrU:
				lane = ssa.VecLaneI16x8
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVAvgRound(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8Mul, wasm.OpcodeVecI32x4Mul, wasm.OpcodeVecI64x2Mul:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI16x8Mul:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Mul:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Mul:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVImul(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8Q15mulrSatS:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsSqmulRoundSat(v1, v2, ssa.VecLaneI16x8).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Eq, wasm.OpcodeVecI16x8Eq, wasm.OpcodeVecI32x4Eq, wasm.OpcodeVecI64x2Eq:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Eq:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Eq:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Eq:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Eq:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Ne, wasm.OpcodeVecI16x8Ne, wasm.OpcodeVecI32x4Ne, wasm.OpcodeVecI64x2Ne:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Ne:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Ne:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Ne:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Ne:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondNotEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16LtS, wasm.OpcodeVecI16x8LtS, wasm.OpcodeVecI32x4LtS, wasm.OpcodeVecI64x2LtS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16LtS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8LtS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4LtS:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2LtS:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondSignedLessThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16LtU, wasm.OpcodeVecI16x8LtU, wasm.OpcodeVecI32x4LtU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16LtU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8LtU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4LtU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondUnsignedLessThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16LeS, wasm.OpcodeVecI16x8LeS, wasm.OpcodeVecI32x4LeS, wasm.OpcodeVecI64x2LeS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16LeS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8LeS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4LeS:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2LeS:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondSignedLessThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16LeU, wasm.OpcodeVecI16x8LeU, wasm.OpcodeVecI32x4LeU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16LeU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8LeU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4LeU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondUnsignedLessThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16GtS, wasm.OpcodeVecI16x8GtS, wasm.OpcodeVecI32x4GtS, wasm.OpcodeVecI64x2GtS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16GtS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8GtS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4GtS:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2GtS:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondSignedGreaterThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16GtU, wasm.OpcodeVecI16x8GtU, wasm.OpcodeVecI32x4GtU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16GtU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8GtU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4GtU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondUnsignedGreaterThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16GeS, wasm.OpcodeVecI16x8GeS, wasm.OpcodeVecI32x4GeS, wasm.OpcodeVecI64x2GeS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16GeS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8GeS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4GeS:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2GeS:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondSignedGreaterThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16GeU, wasm.OpcodeVecI16x8GeU, wasm.OpcodeVecI32x4GeU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16GeU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8GeU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4GeU:
				lane = ssa.VecLaneI32x4
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVIcmp(v1, v2, ssa.IntegerCmpCondUnsignedGreaterThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Max, wasm.OpcodeVecF64x2Max:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Max:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Max:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFmax(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Abs, wasm.OpcodeVecF64x2Abs:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Abs:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Abs:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFabs(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Min, wasm.OpcodeVecF64x2Min:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Min:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Min:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFmin(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Neg, wasm.OpcodeVecF64x2Neg:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Neg:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Neg:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFneg(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Sqrt, wasm.OpcodeVecF64x2Sqrt:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Sqrt:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Sqrt:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVSqrt(v1, lane).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecF32x4Add, wasm.OpcodeVecF64x2Add:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Add:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Add:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFadd(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Sub, wasm.OpcodeVecF64x2Sub:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Sub:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Sub:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFsub(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Mul, wasm.OpcodeVecF64x2Mul:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Mul:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Mul:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFmul(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Div, wasm.OpcodeVecF64x2Div:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Div:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Div:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFdiv(v1, v2, lane).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI16x8ExtaddPairwiseI8x16S, wasm.OpcodeVecI16x8ExtaddPairwiseI8x16U:
			if state.unreachable {
				break
			}
			v := state.pop()
			signed := vecOp == wasm.OpcodeVecI16x8ExtaddPairwiseI8x16S
			ret := builder.AllocateInstruction().AsExtIaddPairwise(v, ssa.VecLaneI8x16, signed).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI32x4ExtaddPairwiseI16x8S, wasm.OpcodeVecI32x4ExtaddPairwiseI16x8U:
			if state.unreachable {
				break
			}
			v := state.pop()
			signed := vecOp == wasm.OpcodeVecI32x4ExtaddPairwiseI16x8S
			ret := builder.AllocateInstruction().AsExtIaddPairwise(v, ssa.VecLaneI16x8, signed).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI16x8ExtMulLowI8x16S, wasm.OpcodeVecI16x8ExtMulLowI8x16U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI8x16, ssa.VecLaneI16x8,
				vecOp == wasm.OpcodeVecI16x8ExtMulLowI8x16S, true)
			state.push(ret)

		case wasm.OpcodeVecI16x8ExtMulHighI8x16S, wasm.OpcodeVecI16x8ExtMulHighI8x16U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI8x16, ssa.VecLaneI16x8,
				vecOp == wasm.OpcodeVecI16x8ExtMulHighI8x16S, false)
			state.push(ret)

		case wasm.OpcodeVecI32x4ExtMulLowI16x8S, wasm.OpcodeVecI32x4ExtMulLowI16x8U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI16x8, ssa.VecLaneI32x4,
				vecOp == wasm.OpcodeVecI32x4ExtMulLowI16x8S, true)
			state.push(ret)

		case wasm.OpcodeVecI32x4ExtMulHighI16x8S, wasm.OpcodeVecI32x4ExtMulHighI16x8U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI16x8, ssa.VecLaneI32x4,
				vecOp == wasm.OpcodeVecI32x4ExtMulHighI16x8S, false)
			state.push(ret)
		case wasm.OpcodeVecI64x2ExtMulLowI32x4S, wasm.OpcodeVecI64x2ExtMulLowI32x4U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI32x4, ssa.VecLaneI64x2,
				vecOp == wasm.OpcodeVecI64x2ExtMulLowI32x4S, true)
			state.push(ret)

		case wasm.OpcodeVecI64x2ExtMulHighI32x4S, wasm.OpcodeVecI64x2ExtMulHighI32x4U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := c.lowerExtMul(
				v1, v2,
				ssa.VecLaneI32x4, ssa.VecLaneI64x2,
				vecOp == wasm.OpcodeVecI64x2ExtMulHighI32x4S, false)
			state.push(ret)

		case wasm.OpcodeVecI32x4DotI16x8S:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()

			ret := builder.AllocateInstruction().AsWideningPairwiseDotProductS(v1, v2).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecF32x4Eq, wasm.OpcodeVecF64x2Eq:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Eq:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Eq:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Ne, wasm.OpcodeVecF64x2Ne:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Ne:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Ne:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondNotEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Lt, wasm.OpcodeVecF64x2Lt:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Lt:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Lt:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondLessThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Le, wasm.OpcodeVecF64x2Le:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Le:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Le:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondLessThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Gt, wasm.OpcodeVecF64x2Gt:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Gt:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Gt:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondGreaterThan, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Ge, wasm.OpcodeVecF64x2Ge:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Ge:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Ge:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcmp(v1, v2, ssa.FloatCmpCondGreaterThanOrEqual, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Ceil, wasm.OpcodeVecF64x2Ceil:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Ceil:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Ceil:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVCeil(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Floor, wasm.OpcodeVecF64x2Floor:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Floor:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Floor:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVFloor(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Trunc, wasm.OpcodeVecF64x2Trunc:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Trunc:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Trunc:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVTrunc(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Nearest, wasm.OpcodeVecF64x2Nearest:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Nearest:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Nearest:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVNearest(v1, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Pmin, wasm.OpcodeVecF64x2Pmin:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Pmin:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Pmin:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVMinPseudo(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4Pmax, wasm.OpcodeVecF64x2Pmax:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecF32x4Pmax:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Pmax:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVMaxPseudo(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4TruncSatF32x4S, wasm.OpcodeVecI32x4TruncSatF32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcvtToIntSat(v1, ssa.VecLaneF32x4, vecOp == wasm.OpcodeVecI32x4TruncSatF32x4S).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4TruncSatF64x2SZero, wasm.OpcodeVecI32x4TruncSatF64x2UZero:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcvtToIntSat(v1, ssa.VecLaneF64x2, vecOp == wasm.OpcodeVecI32x4TruncSatF64x2SZero).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4ConvertI32x4S, wasm.OpcodeVecF32x4ConvertI32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcvtFromInt(v1, ssa.VecLaneF32x4, vecOp == wasm.OpcodeVecF32x4ConvertI32x4S).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF64x2ConvertLowI32x4S, wasm.OpcodeVecF64x2ConvertLowI32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			if runtime.GOARCH == "arm64" {
				// TODO: this is weird. fix.
				v1 = builder.AllocateInstruction().
					AsWiden(v1, ssa.VecLaneI32x4, vecOp == wasm.OpcodeVecF64x2ConvertLowI32x4S, true).Insert(builder).Return()
			}
			ret := builder.AllocateInstruction().
				AsVFcvtFromInt(v1, ssa.VecLaneF64x2, vecOp == wasm.OpcodeVecF64x2ConvertLowI32x4S).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16NarrowI16x8S, wasm.OpcodeVecI8x16NarrowI16x8U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsNarrow(v1, v2, ssa.VecLaneI16x8, vecOp == wasm.OpcodeVecI8x16NarrowI16x8S).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8NarrowI32x4S, wasm.OpcodeVecI16x8NarrowI32x4U:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsNarrow(v1, v2, ssa.VecLaneI32x4, vecOp == wasm.OpcodeVecI16x8NarrowI32x4S).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8ExtendLowI8x16S, wasm.OpcodeVecI16x8ExtendLowI8x16U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI8x16, vecOp == wasm.OpcodeVecI16x8ExtendLowI8x16S, true).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8ExtendHighI8x16S, wasm.OpcodeVecI16x8ExtendHighI8x16U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI8x16, vecOp == wasm.OpcodeVecI16x8ExtendHighI8x16S, false).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4ExtendLowI16x8S, wasm.OpcodeVecI32x4ExtendLowI16x8U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI16x8, vecOp == wasm.OpcodeVecI32x4ExtendLowI16x8S, true).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4ExtendHighI16x8S, wasm.OpcodeVecI32x4ExtendHighI16x8U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI16x8, vecOp == wasm.OpcodeVecI32x4ExtendHighI16x8S, false).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI64x2ExtendLowI32x4S, wasm.OpcodeVecI64x2ExtendLowI32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI32x4, vecOp == wasm.OpcodeVecI64x2ExtendLowI32x4S, true).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI64x2ExtendHighI32x4S, wasm.OpcodeVecI64x2ExtendHighI32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsWiden(v1, ssa.VecLaneI32x4, vecOp == wasm.OpcodeVecI64x2ExtendHighI32x4S, false).
				Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecF64x2PromoteLowF32x4Zero:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsFvpromoteLow(v1, ssa.VecLaneF32x4).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4DemoteF64x2Zero:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsFvdemote(v1, ssa.VecLaneF64x2).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16Shl, wasm.OpcodeVecI16x8Shl, wasm.OpcodeVecI32x4Shl, wasm.OpcodeVecI64x2Shl:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Shl:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Shl:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Shl:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Shl:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVIshl(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16ShrS, wasm.OpcodeVecI16x8ShrS, wasm.OpcodeVecI32x4ShrS, wasm.OpcodeVecI64x2ShrS:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16ShrS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8ShrS:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4ShrS:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2ShrS:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVSshr(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16ShrU, wasm.OpcodeVecI16x8ShrU, wasm.OpcodeVecI32x4ShrU, wasm.OpcodeVecI64x2ShrU:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16ShrU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8ShrU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4ShrU:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2ShrU:
				lane = ssa.VecLaneI64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVUshr(v1, v2, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI8x16ExtractLaneS, wasm.OpcodeVecI16x8ExtractLaneS:
			state.pc++
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16ExtractLaneS:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8ExtractLaneS:
				lane = ssa.VecLaneI16x8
			}
			v1 := state.pop()
			index := c.wasmFunctionBody[state.pc]
			ext := builder.AllocateInstruction().AsExtractlane(v1, index, lane, true).Insert(builder).Return()
			state.push(ext)
		case wasm.OpcodeVecI8x16ExtractLaneU, wasm.OpcodeVecI16x8ExtractLaneU,
			wasm.OpcodeVecI32x4ExtractLane, wasm.OpcodeVecI64x2ExtractLane,
			wasm.OpcodeVecF32x4ExtractLane, wasm.OpcodeVecF64x2ExtractLane:
			state.pc++ // Skip the immediate value.
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16ExtractLaneU:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8ExtractLaneU:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4ExtractLane:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2ExtractLane:
				lane = ssa.VecLaneI64x2
			case wasm.OpcodeVecF32x4ExtractLane:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2ExtractLane:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			index := c.wasmFunctionBody[state.pc]
			ext := builder.AllocateInstruction().AsExtractlane(v1, index, lane, false).Insert(builder).Return()
			state.push(ext)
		case wasm.OpcodeVecI8x16ReplaceLane, wasm.OpcodeVecI16x8ReplaceLane,
			wasm.OpcodeVecI32x4ReplaceLane, wasm.OpcodeVecI64x2ReplaceLane,
			wasm.OpcodeVecF32x4ReplaceLane, wasm.OpcodeVecF64x2ReplaceLane:
			state.pc++
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16ReplaceLane:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8ReplaceLane:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4ReplaceLane:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2ReplaceLane:
				lane = ssa.VecLaneI64x2
			case wasm.OpcodeVecF32x4ReplaceLane:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2ReplaceLane:
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			index := c.wasmFunctionBody[state.pc]
			ret := builder.AllocateInstruction().AsInsertlane(v1, v2, index, lane).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecV128i8x16Shuffle:
			state.pc++
			laneIndexes := c.wasmFunctionBody[state.pc : state.pc+16]
			state.pc += 15
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsShuffle(v1, v2, laneIndexes).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI8x16Swizzle:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsSwizzle(v1, v2, ssa.VecLaneI8x16).Insert(builder).Return()
			state.push(ret)

		case wasm.OpcodeVecI8x16Splat,
			wasm.OpcodeVecI16x8Splat,
			wasm.OpcodeVecI32x4Splat,
			wasm.OpcodeVecI64x2Splat,
			wasm.OpcodeVecF32x4Splat,
			wasm.OpcodeVecF64x2Splat:
			if state.unreachable {
				break
			}
			var lane ssa.VecLane
			switch vecOp {
			case wasm.OpcodeVecI8x16Splat:
				lane = ssa.VecLaneI8x16
			case wasm.OpcodeVecI16x8Splat:
				lane = ssa.VecLaneI16x8
			case wasm.OpcodeVecI32x4Splat:
				lane = ssa.VecLaneI32x4
			case wasm.OpcodeVecI64x2Splat:
				lane = ssa.VecLaneI64x2
			case wasm.OpcodeVecF32x4Splat:
				lane = ssa.VecLaneF32x4
			case wasm.OpcodeVecF64x2Splat:
				lane = ssa.VecLaneF64x2
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsSplat(v1, lane).Insert(builder).Return()
			state.push(ret)

		// Relaxed SIMD. Each of these picks one of the results the proposal
		// permits and sticks to it on every engine and architecture, so most
		// reuse the non-relaxed instruction outright. See RATIONALE.md.
		case wasm.OpcodeVecI8x16RelaxedSwizzle:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsSwizzle(v1, v2, ssa.VecLaneI8x16).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4RelaxedTruncF32x4S, wasm.OpcodeVecI32x4RelaxedTruncF32x4U:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcvtToIntSat(v1, ssa.VecLaneF32x4, vecOp == wasm.OpcodeVecI32x4RelaxedTruncF32x4S).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI32x4RelaxedTruncF64x2SZero, wasm.OpcodeVecI32x4RelaxedTruncF64x2UZero:
			if state.unreachable {
				break
			}
			v1 := state.pop()
			ret := builder.AllocateInstruction().
				AsVFcvtToIntSat(v1, ssa.VecLaneF64x2, vecOp == wasm.OpcodeVecI32x4RelaxedTruncF64x2SZero).
				Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4RelaxedMadd, wasm.OpcodeVecF32x4RelaxedNmadd,
			wasm.OpcodeVecF64x2RelaxedMadd, wasm.OpcodeVecF64x2RelaxedNmadd:
			if state.unreachable {
				break
			}
			lane := ssa.VecLaneF32x4
			if vecOp == wasm.OpcodeVecF64x2RelaxedMadd || vecOp == wasm.OpcodeVecF64x2RelaxedNmadd {
				lane = ssa.VecLaneF64x2
			}
			v3 := state.pop()
			v2 := state.pop()
			v1 := state.pop()
			// The multiply and the add round separately: contracting them into a
			// fused multiply-add would make the result depend on the host.
			product := builder.AllocateInstruction().AsVFmul(v1, v2, lane).Insert(builder).Return()
			sum := builder.AllocateInstruction()
			if vecOp == wasm.OpcodeVecF32x4RelaxedNmadd || vecOp == wasm.OpcodeVecF64x2RelaxedNmadd {
				// -(a*b) + c is exactly c - a*b in IEEE 754, which saves a negate.
				sum.AsVFsub(v3, product, lane)
			} else {
				sum.AsVFadd(product, v3, lane)
			}
			state.push(sum.Insert(builder).Return())
		case wasm.OpcodeVecI8x16RelaxedLaneselect, wasm.OpcodeVecI16x8RelaxedLaneselect,
			wasm.OpcodeVecI32x4RelaxedLaneselect, wasm.OpcodeVecI64x2RelaxedLaneselect:
			if state.unreachable {
				break
			}
			c := state.pop()
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsVbitselect(c, v1, v2).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecF32x4RelaxedMin, wasm.OpcodeVecF64x2RelaxedMin,
			wasm.OpcodeVecF32x4RelaxedMax, wasm.OpcodeVecF64x2RelaxedMax:
			if state.unreachable {
				break
			}
			lane := ssa.VecLaneF32x4
			if vecOp == wasm.OpcodeVecF64x2RelaxedMin || vecOp == wasm.OpcodeVecF64x2RelaxedMax {
				lane = ssa.VecLaneF64x2
			}
			v2 := state.pop()
			v1 := state.pop()
			instr := builder.AllocateInstruction()
			if vecOp == wasm.OpcodeVecF32x4RelaxedMin || vecOp == wasm.OpcodeVecF64x2RelaxedMin {
				instr.AsVFmin(v1, v2, lane)
			} else {
				instr.AsVFmax(v1, v2, lane)
			}
			state.push(instr.Insert(builder).Return())
		case wasm.OpcodeVecI16x8RelaxedQ15mulrS:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			ret := builder.AllocateInstruction().AsSqmulRoundSat(v1, v2, ssa.VecLaneI16x8).Insert(builder).Return()
			state.push(ret)
		case wasm.OpcodeVecI16x8RelaxedDotI8x16I7x16S:
			if state.unreachable {
				break
			}
			v2 := state.pop()
			v1 := state.pop()
			state.push(relaxedDotI8x16(builder, v1, v2))
		case wasm.OpcodeVecI32x4RelaxedDotI8x16I7x16AddS:
			if state.unreachable {
				break
			}
			v3 := state.pop()
			v2 := state.pop()
			v1 := state.pop()
			dot := relaxedDotI8x16(builder, v1, v2)
			widened := builder.AllocateInstruction().
				AsExtIaddPairwise(dot, ssa.VecLaneI16x8, true).Insert(builder).Return()
			ret := builder.AllocateInstruction().
				AsVIadd(widened, v3, ssa.VecLaneI32x4).Insert(builder).Return()
			state.push(ret)

		default:
			panic("TODO: unsupported vector instruction: " + wasm.VectorInstructionName(vecOp))
		}
	case wasm.OpcodeAtomicPrefix:
		state.pc++
		atomicOp := c.wasmFunctionBody[state.pc]
		switch atomicOp {
		case wasm.OpcodeAtomicMemoryWait32, wasm.OpcodeAtomicMemoryWait64:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			c.storeCallerModuleContext()

			var opSize uint64
			var trampoline nativeapi.Offset
			var sig *ssa.Signature
			switch atomicOp {
			case wasm.OpcodeAtomicMemoryWait32:
				opSize = 4
				trampoline = nativeapi.ExecutionContextOffsetMemoryWait32TrampolineAddress
				sig = &c.memoryWait32Sig
			case wasm.OpcodeAtomicMemoryWait64:
				opSize = 8
				trampoline = nativeapi.ExecutionContextOffsetMemoryWait64TrampolineAddress
				sig = &c.memoryWait64Sig
			}

			timeout := state.pop()
			exp := state.pop()
			baseAddr := state.pop()
			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, opSize)

			memoryWaitPtr := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					trampoline.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()

			memIndexVal := builder.AllocateInstruction().AsIconst32(uint32(memIndex)).Insert(builder).Return()
			args := c.allocateVarLengthValues(5, c.execCtxPtrValue, memIndexVal, timeout, exp, addr)
			memoryWaitRet := builder.AllocateInstruction().
				AsCallIndirect(memoryWaitPtr, sig, args).
				Insert(builder).Return()
			state.push(memoryWaitRet)
		case wasm.OpcodeAtomicMemoryNotify:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			c.storeCallerModuleContext()
			count := state.pop()
			baseAddr := state.pop()
			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, 4)

			memoryNotifyPtr := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetMemoryNotifyTrampolineAddress.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()
			memIndexVal := builder.AllocateInstruction().AsIconst32(uint32(memIndex)).Insert(builder).Return()
			args := c.allocateVarLengthValues(4, c.execCtxPtrValue, memIndexVal, count, addr)
			memoryNotifyRet := builder.AllocateInstruction().
				AsCallIndirect(memoryNotifyPtr, &c.memoryNotifySig, args).
				Insert(builder).Return()
			state.push(memoryNotifyRet)
		case wasm.OpcodeAtomicI32Load, wasm.OpcodeAtomicI64Load, wasm.OpcodeAtomicI32Load8U, wasm.OpcodeAtomicI32Load16U, wasm.OpcodeAtomicI64Load8U, wasm.OpcodeAtomicI64Load16U, wasm.OpcodeAtomicI64Load32U:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			baseAddr := state.pop()

			var size uint64
			switch atomicOp {
			case wasm.OpcodeAtomicI64Load:
				size = 8
			case wasm.OpcodeAtomicI32Load, wasm.OpcodeAtomicI64Load32U:
				size = 4
			case wasm.OpcodeAtomicI32Load16U, wasm.OpcodeAtomicI64Load16U:
				size = 2
			case wasm.OpcodeAtomicI32Load8U, wasm.OpcodeAtomicI64Load8U:
				size = 1
			}

			var typ ssa.Type
			switch atomicOp {
			case wasm.OpcodeAtomicI64Load, wasm.OpcodeAtomicI64Load32U, wasm.OpcodeAtomicI64Load16U, wasm.OpcodeAtomicI64Load8U:
				typ = ssa.TypeI64
			case wasm.OpcodeAtomicI32Load, wasm.OpcodeAtomicI32Load16U, wasm.OpcodeAtomicI32Load8U:
				typ = ssa.TypeI32
			}

			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, size)
			res := builder.AllocateInstruction().AsAtomicLoad(addr, size, typ).Insert(builder).Return()
			state.push(res)
		case wasm.OpcodeAtomicI32Store, wasm.OpcodeAtomicI64Store, wasm.OpcodeAtomicI32Store8, wasm.OpcodeAtomicI32Store16, wasm.OpcodeAtomicI64Store8, wasm.OpcodeAtomicI64Store16, wasm.OpcodeAtomicI64Store32:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			val := state.pop()
			baseAddr := state.pop()

			var size uint64
			switch atomicOp {
			case wasm.OpcodeAtomicI64Store:
				size = 8
			case wasm.OpcodeAtomicI32Store, wasm.OpcodeAtomicI64Store32:
				size = 4
			case wasm.OpcodeAtomicI32Store16, wasm.OpcodeAtomicI64Store16:
				size = 2
			case wasm.OpcodeAtomicI32Store8, wasm.OpcodeAtomicI64Store8:
				size = 1
			}

			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, size)
			builder.AllocateInstruction().AsAtomicStore(addr, val, size).Insert(builder)
		case wasm.OpcodeAtomicI32RmwAdd, wasm.OpcodeAtomicI64RmwAdd, wasm.OpcodeAtomicI32Rmw8AddU, wasm.OpcodeAtomicI32Rmw16AddU, wasm.OpcodeAtomicI64Rmw8AddU, wasm.OpcodeAtomicI64Rmw16AddU, wasm.OpcodeAtomicI64Rmw32AddU,
			wasm.OpcodeAtomicI32RmwSub, wasm.OpcodeAtomicI64RmwSub, wasm.OpcodeAtomicI32Rmw8SubU, wasm.OpcodeAtomicI32Rmw16SubU, wasm.OpcodeAtomicI64Rmw8SubU, wasm.OpcodeAtomicI64Rmw16SubU, wasm.OpcodeAtomicI64Rmw32SubU,
			wasm.OpcodeAtomicI32RmwAnd, wasm.OpcodeAtomicI64RmwAnd, wasm.OpcodeAtomicI32Rmw8AndU, wasm.OpcodeAtomicI32Rmw16AndU, wasm.OpcodeAtomicI64Rmw8AndU, wasm.OpcodeAtomicI64Rmw16AndU, wasm.OpcodeAtomicI64Rmw32AndU,
			wasm.OpcodeAtomicI32RmwOr, wasm.OpcodeAtomicI64RmwOr, wasm.OpcodeAtomicI32Rmw8OrU, wasm.OpcodeAtomicI32Rmw16OrU, wasm.OpcodeAtomicI64Rmw8OrU, wasm.OpcodeAtomicI64Rmw16OrU, wasm.OpcodeAtomicI64Rmw32OrU,
			wasm.OpcodeAtomicI32RmwXor, wasm.OpcodeAtomicI64RmwXor, wasm.OpcodeAtomicI32Rmw8XorU, wasm.OpcodeAtomicI32Rmw16XorU, wasm.OpcodeAtomicI64Rmw8XorU, wasm.OpcodeAtomicI64Rmw16XorU, wasm.OpcodeAtomicI64Rmw32XorU,
			wasm.OpcodeAtomicI32RmwXchg, wasm.OpcodeAtomicI64RmwXchg, wasm.OpcodeAtomicI32Rmw8XchgU, wasm.OpcodeAtomicI32Rmw16XchgU, wasm.OpcodeAtomicI64Rmw8XchgU, wasm.OpcodeAtomicI64Rmw16XchgU, wasm.OpcodeAtomicI64Rmw32XchgU:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			val := state.pop()
			baseAddr := state.pop()

			var rmwOp ssa.AtomicRmwOp
			var size uint64
			switch atomicOp {
			case wasm.OpcodeAtomicI32RmwAdd, wasm.OpcodeAtomicI64RmwAdd, wasm.OpcodeAtomicI32Rmw8AddU, wasm.OpcodeAtomicI32Rmw16AddU, wasm.OpcodeAtomicI64Rmw8AddU, wasm.OpcodeAtomicI64Rmw16AddU, wasm.OpcodeAtomicI64Rmw32AddU:
				rmwOp = ssa.AtomicRmwOpAdd
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwAdd:
					size = 8
				case wasm.OpcodeAtomicI32RmwAdd, wasm.OpcodeAtomicI64Rmw32AddU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16AddU, wasm.OpcodeAtomicI64Rmw16AddU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8AddU, wasm.OpcodeAtomicI64Rmw8AddU:
					size = 1
				}
			case wasm.OpcodeAtomicI32RmwSub, wasm.OpcodeAtomicI64RmwSub, wasm.OpcodeAtomicI32Rmw8SubU, wasm.OpcodeAtomicI32Rmw16SubU, wasm.OpcodeAtomicI64Rmw8SubU, wasm.OpcodeAtomicI64Rmw16SubU, wasm.OpcodeAtomicI64Rmw32SubU:
				rmwOp = ssa.AtomicRmwOpSub
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwSub:
					size = 8
				case wasm.OpcodeAtomicI32RmwSub, wasm.OpcodeAtomicI64Rmw32SubU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16SubU, wasm.OpcodeAtomicI64Rmw16SubU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8SubU, wasm.OpcodeAtomicI64Rmw8SubU:
					size = 1
				}
			case wasm.OpcodeAtomicI32RmwAnd, wasm.OpcodeAtomicI64RmwAnd, wasm.OpcodeAtomicI32Rmw8AndU, wasm.OpcodeAtomicI32Rmw16AndU, wasm.OpcodeAtomicI64Rmw8AndU, wasm.OpcodeAtomicI64Rmw16AndU, wasm.OpcodeAtomicI64Rmw32AndU:
				rmwOp = ssa.AtomicRmwOpAnd
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwAnd:
					size = 8
				case wasm.OpcodeAtomicI32RmwAnd, wasm.OpcodeAtomicI64Rmw32AndU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16AndU, wasm.OpcodeAtomicI64Rmw16AndU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8AndU, wasm.OpcodeAtomicI64Rmw8AndU:
					size = 1
				}
			case wasm.OpcodeAtomicI32RmwOr, wasm.OpcodeAtomicI64RmwOr, wasm.OpcodeAtomicI32Rmw8OrU, wasm.OpcodeAtomicI32Rmw16OrU, wasm.OpcodeAtomicI64Rmw8OrU, wasm.OpcodeAtomicI64Rmw16OrU, wasm.OpcodeAtomicI64Rmw32OrU:
				rmwOp = ssa.AtomicRmwOpOr
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwOr:
					size = 8
				case wasm.OpcodeAtomicI32RmwOr, wasm.OpcodeAtomicI64Rmw32OrU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16OrU, wasm.OpcodeAtomicI64Rmw16OrU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8OrU, wasm.OpcodeAtomicI64Rmw8OrU:
					size = 1
				}
			case wasm.OpcodeAtomicI32RmwXor, wasm.OpcodeAtomicI64RmwXor, wasm.OpcodeAtomicI32Rmw8XorU, wasm.OpcodeAtomicI32Rmw16XorU, wasm.OpcodeAtomicI64Rmw8XorU, wasm.OpcodeAtomicI64Rmw16XorU, wasm.OpcodeAtomicI64Rmw32XorU:
				rmwOp = ssa.AtomicRmwOpXor
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwXor:
					size = 8
				case wasm.OpcodeAtomicI32RmwXor, wasm.OpcodeAtomicI64Rmw32XorU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16XorU, wasm.OpcodeAtomicI64Rmw16XorU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8XorU, wasm.OpcodeAtomicI64Rmw8XorU:
					size = 1
				}
			case wasm.OpcodeAtomicI32RmwXchg, wasm.OpcodeAtomicI64RmwXchg, wasm.OpcodeAtomicI32Rmw8XchgU, wasm.OpcodeAtomicI32Rmw16XchgU, wasm.OpcodeAtomicI64Rmw8XchgU, wasm.OpcodeAtomicI64Rmw16XchgU, wasm.OpcodeAtomicI64Rmw32XchgU:
				rmwOp = ssa.AtomicRmwOpXchg
				switch atomicOp {
				case wasm.OpcodeAtomicI64RmwXchg:
					size = 8
				case wasm.OpcodeAtomicI32RmwXchg, wasm.OpcodeAtomicI64Rmw32XchgU:
					size = 4
				case wasm.OpcodeAtomicI32Rmw16XchgU, wasm.OpcodeAtomicI64Rmw16XchgU:
					size = 2
				case wasm.OpcodeAtomicI32Rmw8XchgU, wasm.OpcodeAtomicI64Rmw8XchgU:
					size = 1
				}
			}

			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, size)
			res := builder.AllocateInstruction().AsAtomicRmw(rmwOp, addr, val, size).Insert(builder).Return()
			state.push(res)
		case wasm.OpcodeAtomicI32RmwCmpxchg, wasm.OpcodeAtomicI64RmwCmpxchg, wasm.OpcodeAtomicI32Rmw8CmpxchgU, wasm.OpcodeAtomicI32Rmw16CmpxchgU, wasm.OpcodeAtomicI64Rmw8CmpxchgU, wasm.OpcodeAtomicI64Rmw16CmpxchgU, wasm.OpcodeAtomicI64Rmw32CmpxchgU:
			_, offset, _, memIndex := c.readMemArg()
			if state.unreachable {
				break
			}

			repl := state.pop()
			exp := state.pop()
			baseAddr := state.pop()

			var size uint64
			switch atomicOp {
			case wasm.OpcodeAtomicI64RmwCmpxchg:
				size = 8
			case wasm.OpcodeAtomicI32RmwCmpxchg, wasm.OpcodeAtomicI64Rmw32CmpxchgU:
				size = 4
			case wasm.OpcodeAtomicI32Rmw16CmpxchgU, wasm.OpcodeAtomicI64Rmw16CmpxchgU:
				size = 2
			case wasm.OpcodeAtomicI32Rmw8CmpxchgU, wasm.OpcodeAtomicI64Rmw8CmpxchgU:
				size = 1
			}
			addr := c.atomicMemOpSetup(memIndex, baseAddr, offset, size)
			res := builder.AllocateInstruction().AsAtomicCas(addr, exp, repl, size).Insert(builder).Return()
			state.push(res)
		case wasm.OpcodeAtomicFence:
			order := c.readByte()
			if state.unreachable {
				break
			}
			if c.needMemory {
				builder.AllocateInstruction().AsFence(order).Insert(builder)
			}
		default:
			panic("TODO: unsupported atomic instruction: " + wasm.AtomicInstructionName(atomicOp))
		}
	case wasm.OpcodeRefEq:
		if state.unreachable {
			break
		}
		b, a := state.pop(), state.pop()
		state.push(c.gcResultI32(c.callGC(wasm.GCRefEq, a, b)))

	case wasm.OpcodeGCPrefix:
		gcOp := c.wasmFunctionBody[c.loweringState.pc+1]
		c.loweringState.pc++
		c.lowerGCInstruction(gcOp)

	case wasm.OpcodeRefFunc:
		funcIndex := c.readI32u()
		if state.unreachable {
			break
		}

		c.storeCallerModuleContext()

		funcIndexVal := builder.AllocateInstruction().AsIconst32(funcIndex).Insert(builder).Return()

		refFuncPtr := builder.AllocateInstruction().
			AsLoad(c.execCtxPtrValue,
				nativeapi.ExecutionContextOffsetRefFuncTrampolineAddress.U32(),
				ssa.TypeI64,
			).Insert(builder).Return()

		args := c.allocateVarLengthValues(2, c.execCtxPtrValue, funcIndexVal)
		refFuncRet := builder.
			AllocateInstruction().
			AsCallIndirect(refFuncPtr, &c.refFuncSig, args).
			Insert(builder).Return()
		state.push(refFuncRet)

	case wasm.OpcodeRefNull:
		switch reftype := c.wasmFunctionBody[c.loweringState.pc+1]; wasm.ValueType(reftype) {
		case wasm.ValueTypeFuncref, wasm.ValueTypeExternref, wasm.ValueTypeExnref:
			c.loweringState.pc++
		default:
			c.readI32u()
		}
		if state.unreachable {
			break
		}
		ret := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
		state.push(ret)
	case wasm.OpcodeRefIsNull:
		if state.unreachable {
			break
		}
		r := state.pop()
		zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder)
		icmp := builder.AllocateInstruction().
			AsIcmp(r, zero.Return(), ssa.IntegerCmpCondEqual).
			Insert(builder).
			Return()
		state.push(icmp)
	case wasm.OpcodeTableSet:
		tableIndex := c.readI32u()
		if state.unreachable {
			break
		}
		r := state.pop()
		targetOffsetInTable := state.pop()

		elementAddr := c.lowerAccessTableWithBoundsCheck(tableIndex, targetOffsetInTable)
		builder.AllocateInstruction().AsStore(ssa.OpcodeStore, r, elementAddr, 0).Insert(builder)

	case wasm.OpcodeTableGet:
		tableIndex := c.readI32u()
		if state.unreachable {
			break
		}
		targetOffsetInTable := state.pop()
		elementAddr := c.lowerAccessTableWithBoundsCheck(tableIndex, targetOffsetInTable)
		loaded := builder.AllocateInstruction().AsLoad(elementAddr, 0, ssa.TypeI64).Insert(builder).Return()
		state.push(loaded)

	case wasm.OpcodeTailCallReturnCallIndirect:
		typeIndex := c.readI32u()
		tableIndex := c.readI32u()
		if state.unreachable {
			break
		}
		// Per spec, return_call leaves the current frame, so all enclosing
		// try_table handlers must be popped before the tail call.
		c.emitTryTableLeaves(len(c.state().controlFrames))
		_, _ = typeIndex, tableIndex
		c.lowerTailCallReturnCallIndirect(typeIndex, tableIndex)
		state.unreachable = true

	case wasm.OpcodeTailCallReturnCall:
		fnIndex := c.readI32u()
		if state.unreachable {
			break
		}
		// Per spec, return_call leaves the current frame, so all enclosing
		// try_table handlers must be popped before the tail call.
		c.emitTryTableLeaves(len(c.state().controlFrames))
		c.lowerTailCallReturnCall(fnIndex)
		state.unreachable = true

	case wasm.OpcodeThrow:
		tagIndex := c.readI32u()
		if state.unreachable {
			break
		}
		tagType := c.resolveTagType(tagIndex)
		// Pop the tag's param values from the stack.
		var throwParams []ssa.Value
		if tagType != nil {
			throwParams = make([]ssa.Value, len(tagType.Params))
			for i := len(tagType.Params) - 1; i >= 0; i-- {
				throwParams[i] = state.pop()
			}
		}

		c.storeCallerModuleContext()

		tagIdxVal := builder.AllocateInstruction().AsIconst64(uint64(tagIndex)).Insert(builder).Return()

		// We need to store the throwParams in the exception and then throw it.
		// However, each exception might have a variable number of parameters,
		// so we let Go allocate the reference on the heap.
		// The Go side allocates the Exception object (Params sized to nParams)
		// and stores the pointer to the backing-array into execCtx.exceptionParamsPtr.
		throwAllocPtr := builder.AllocateInstruction().
			AsLoad(c.execCtxPtrValue,
				nativeapi.ExecutionContextOffsetThrowAllocTrampolineAddress.U32(),
				ssa.TypeI64,
			).Insert(builder).Return()
		throwAllocArgs := c.allocateVarLengthValues(2, c.execCtxPtrValue, tagIdxVal)
		exnref := builder.AllocateInstruction().
			AsCallIndirect(throwAllocPtr, &c.throwAllocSig, throwAllocArgs).
			Insert(builder).Return()

		// Reload memory pointers invalidated by the Go call.
		c.reloadAfterCall()

		// We can now store each param directly into Exception.Params using the pointer
		// stored into execCtx.exceptionParamsPtr.
		if len(throwParams) > 0 {
			paramsPtr := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetExceptionParamsPtr.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()
			for i, v := range throwParams {
				switch v.Type() {
				case ssa.TypeF32:
					v = builder.AllocateInstruction().AsBitcast(v, ssa.TypeI32).Insert(builder).Return()
				case ssa.TypeF64:
					v = builder.AllocateInstruction().AsBitcast(v, ssa.TypeI64).Insert(builder).Return()
				}
				builder.AllocateInstruction().
					AsStore(ssa.OpcodeStore, v, paramsPtr, uint32(i)*8).
					Insert(builder)
			}
		}

		// We return again control to Go to search and dispatch to a matching catch clause.
		c.emitThrow(exnref)
		state.unreachable = true

	case wasm.OpcodeThrowRef:
		if state.unreachable {
			break
		}
		exnref := state.pop()
		// Check for null exnref.
		zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
		isNull := builder.AllocateInstruction()
		isNull.AsIcmp(exnref, zero, ssa.IntegerCmpCondEqual)
		builder.InsertInstruction(isNull)
		exitIfNull := builder.AllocateInstruction()
		exitIfNull.AsExitIfTrueWithCode(c.execCtxPtrValue, isNull.Return(), nativeapi.ExitCodeNullReference)
		builder.InsertInstruction(exitIfNull)

		c.storeCallerModuleContext()

		c.emitThrow(exnref)
		state.unreachable = true

	case wasm.OpcodeTryTable:
		bt := c.readBlockType()

		if state.unreachable {
			state.unreachableDepth++
			// Still need to skip the catch clause bytes in the unreachable case.
			c.skipTryTableCatchClauses()
			break
		}

		// Parse catch clauses.
		c.loweringState.pc++
		catchCount, catchNum, _ := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
		c.loweringState.pc += int(catchNum) - 1

		var catchClauses []catchClause
		for i := uint32(0); i < catchCount; i++ {
			c.loweringState.pc++
			kind := c.wasmFunctionBody[c.loweringState.pc]
			var tagIdx uint32
			switch kind {
			case wasm.CatchKindCatch, wasm.CatchKindCatchRef:
				c.loweringState.pc++
				var n uint64
				tagIdx, n, _ = leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
				c.loweringState.pc += int(n) - 1
			case wasm.CatchKindCatchAll, wasm.CatchKindCatchAllRef:
				// No tagIdx for catch_all variants.
			}
			c.loweringState.pc++
			labelIdx, n, _ := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
			c.loweringState.pc += int(n) - 1
			catchClauses = append(catchClauses, catchClause{kind: kind, tagIndex: tagIdx, labelIdx: labelIdx})
		}

		// Register try_table metadata and get the try_table ID.
		var clauseInstances []nativeapi.CatchClauseInstance
		for _, cc := range catchClauses {
			clauseInstances = append(clauseInstances, nativeapi.CatchClauseInstance{
				Kind:     cc.kind,
				TagIndex: cc.tagIndex,
			})
		}
		numLocals := len(c.wasmFunctionTyp.Params) + len(c.wasmFunctionLocalTypes)
		// The operand floor is whatever is on the value stack below this
		// try_table's own block params -- identical computation to
		// originalStackLenWithoutParam below, just captured here (before
		// bodyBlk/handler blocks are built) so it can travel with the
		// try-table metadata. See TryTableInfo.FloorSize's doc comment.
		floorSize := len(state.values) - len(bt.Params)
		tryTableID := c.tryTableMetadata.Append(nativeapi.TryTableInfo{
			CatchClauses: clauseInstances,
			NumLocals:    numLocals,
			ReuseLocals:  c.tryTableDepth > 0,
			FloorSize:    floorSize,
		})

		// Allocate the following block (after try_table end).
		followingBlk := builder.AllocateBasicBlock()
		c.addBlockParamsFromWasmTypes(bt.Results, followingBlk)

		// bodyBlk is allocated *after* the handler blocks below (when there
		// are catch clauses) so that [bodyBlk.ID(), BlockIDMax() at this
		// try's `end`) is a block-ID range uncontaminated by handler-block
		// IDs -- see EhPendingEntry's doc comment.
		var bodyBlk ssa.BasicBlock
		var pendingEhIdx int

		if len(catchClauses) > 0 {
			// Store the caller module context so the dispatch loop can find the module.
			c.storeCallerModuleContext()

			// For each catch clause, create a handler block that loads exception
			// params and jumps to the wasm target label.
			// NOTE: catch clause label indices do NOT include the try_table itself
			// (the try_table is pushed onto the control stack after the catch clauses
			// are processed, per the spec). So we resolve labels BEFORE pushing.
			varPool := builder.VarLengthPool()
			targets := varPool.Allocate(len(catchClauses) + 1) // +1 for bodyBlk

			currentBlk := builder.CurrentBlock()
			pendingClauses := make([]EhPendingClause, 0, len(catchClauses))
			for _, cc := range catchClauses {
				handlerBlk := builder.AllocateBasicBlock()
				builder.SetCurrentBlock(handlerBlk)
				c.reloadAfterCall()
				c.reloadLocalsFromSaveArea()

				// Resolve the wasm target label.
				targetBlk, _ := state.brTargetArgNumFor(cc.labelIdx)

				// Load exception params and jump to wasm target.
				var brArgs []ssa.Value
				switch cc.kind {
				case wasm.CatchKindCatch:
					if tagType := c.resolveTagType(cc.tagIndex); tagType != nil {
						brArgs = c.loadExceptionParams(tagType)
					}
				case wasm.CatchKindCatchRef:
					if tagType := c.resolveTagType(cc.tagIndex); tagType != nil {
						brArgs = c.loadExceptionParams(tagType)
					}
					brArgs = append(brArgs, c.loadExnRef())
				case wasm.CatchKindCatchAll:
					// No values.
				case wasm.CatchKindCatchAllRef:
					brArgs = append(brArgs, c.loadExnRef())
				}

				// Pop any enclosing try_table handlers that the jump crosses.
				c.emitTryTableLeaves(int(cc.labelIdx))

				jmpArgs := c.allocateVarLengthValues(len(brArgs), brArgs...)
				c.insertJumpToBlock(jmpArgs, targetBlk)

				targets = targets.Append(varPool, ssa.Value(handlerBlk.ID()))
				pendingClauses = append(pendingClauses, EhPendingClause{
					Kind: cc.kind, TagIndex: cc.tagIndex, HandlerBlkID: handlerBlk.ID(),
				})
			}

			// Now allocate the body block (see the comment above) and
			// record the pending exception side-table entry for this
			// try_table. BodyBlkIDEnd is filled in at this try's own
			// OpcodeEnd, once its whole body (including nested constructs)
			// has been lowered.
			bodyBlk = builder.AllocateBasicBlock()
			pendingEhIdx = len(c.pendingEhEntries)
			c.pendingEhEntries = append(c.pendingEhEntries, EhPendingEntry{
				BodyBlkIDStart: bodyBlk.ID(),
				Clauses:        pendingClauses,
				TryTableID:     tryTableID,
			})

			// Last target is the body block (default for clauseIdx == -1 / out of range).
			targets = targets.Append(varPool, ssa.Value(bodyBlk.ID()))

			// Back to the original block: call the try_table enter trampoline,
			// then dispatch on the caught clause index.
			builder.SetCurrentBlock(currentBlk)
			encodedExitCode := uint64(nativeapi.ExitCodeTryTableEnter | nativeapi.ExitCode(tryTableID<<8))

			// Load trampoline address from execCtx.
			enterPtr := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetTryTableEnterTrampolineAddress.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()

			// Call the trampoline: (execCtx, encodedExitCode) -> ().
			exitCodeVal := builder.AllocateInstruction().AsIconst64(encodedExitCode).Insert(builder).Return()
			args := c.allocateVarLengthValues(2, c.execCtxPtrValue, exitCodeVal)
			builder.AllocateInstruction().
				AsCallIndirect(enterPtr, &c.tryTableEnterSig, args).
				Insert(builder)

			// Load the caught clause index written by the dispatch loop.
			clauseIdx := builder.AllocateInstruction().
				AsLoad(c.execCtxPtrValue,
					nativeapi.ExecutionContextOffsetCaughtExceptionClauseIdx.U32(),
					ssa.TypeI64,
				).Insert(builder).Return()

			// Dispatch to handler blocks or body block via br_table.
			brTable := builder.AllocateInstruction()
			brTable.AsBrTable(clauseIdx, targets)
			builder.InsertInstruction(brTable)

			// Seal handler blocks after BrTable is inserted (so predecessors are registered).
			for _, targetID := range targets.View() {
				blk := builder.BasicBlock(ssa.BasicBlockID(targetID))
				if !blk.Sealed() {
					builder.Seal(blk)
				}
			}
		} else {
			// No catch clauses — try_table acts as a plain block.
			// Jump directly to body without entering exception handling.
			bodyBlk = builder.AllocateBasicBlock()
			c.insertJumpToBlock(ssa.ValuesNil, bodyBlk)
		}

		if !bodyBlk.Sealed() {
			builder.Seal(bodyBlk)
		}
		builder.SetCurrentBlock(bodyBlk)
		if len(catchClauses) > 0 {
			// Body block is entered after the trampoline call, so we need to reload.
			c.reloadAfterCall()
			// Initialize the locals save area so handlers can read
			// correct values after a stack restore.
			c.storeAllLocalsToSaveArea()
			c.tryTableDepth++
		}

		// Push the try_table control frame AFTER resolving catch labels.
		kind := controlFrameKind(controlFrameKindTryTable)
		if len(catchClauses) > 0 {
			kind = controlFrameKindTryTableWithCatch
		}
		state.ctrlPush(controlFrame{
			kind:                         kind,
			pendingEhIdx:                 pendingEhIdx,
			originalStackLenWithoutParam: len(state.values) - len(bt.Params),
			followingBlock:               followingBlk,
			blockType:                    bt,
		})

	case wasm.OpcodeRefAsNonNull:
		if state.unreachable {
			break
		}
		r := state.pop()
		zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder)
		checkNull := builder.AllocateInstruction().
			AsIcmp(r, zero.Return(), ssa.IntegerCmpCondEqual).
			Insert(builder).Return()
		exitIfNull := builder.AllocateInstruction()
		exitIfNull.AsExitIfTrueWithCode(c.execCtxPtrValue, checkNull, nativeapi.ExitCodeNullReference)
		builder.InsertInstruction(exitIfNull)
		state.push(r)

	case wasm.OpcodeBrOnNull:
		labelIndex := c.readI32u()
		if state.unreachable {
			break
		}

		r := state.pop()
		zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder)
		isNull := builder.AllocateInstruction().
			AsIcmp(r, zero.Return(), ssa.IntegerCmpCondEqual).
			Insert(builder).Return()

		targetBlk, argNum := state.brTargetArgNumFor(labelIndex)
		args := c.nPeekDup(argNum)
		var sealTargetBlk bool

		if c.branchExitsTryTable(int(labelIndex)) {
			current := builder.CurrentBlock()
			trampolineBlk := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(trampolineBlk)
			c.emitTryTableLeaves(int(labelIndex))
			c.insertJumpToBlock(args, targetBlk)
			builder.SetCurrentBlock(current)
			targetBlk = trampolineBlk
			sealTargetBlk = true
			args = ssa.ValuesNil
		}

		if c.needListener && targetBlk.ReturnBlock() {
			current := builder.CurrentBlock()
			targetBlk = builder.AllocateBasicBlock()
			builder.SetCurrentBlock(targetBlk)
			sealTargetBlk = true
			c.callListenerAfter()
			instr := builder.AllocateInstruction()
			instr.AsReturn(args)
			builder.InsertInstruction(instr)
			args = ssa.ValuesNil
			builder.SetCurrentBlock(current)
		}

		brnz := builder.AllocateInstruction()
		brnz.AsBrnz(isNull, args, targetBlk)
		builder.InsertInstruction(brnz)

		if sealTargetBlk {
			builder.Seal(targetBlk)
		}

		// Fall-through: ref is non-null, push it back.
		elseBlk := builder.AllocateBasicBlock()
		c.insertJumpToBlock(ssa.ValuesNil, elseBlk)
		builder.Seal(elseBlk)
		builder.SetCurrentBlock(elseBlk)
		state.push(r)

	case wasm.OpcodeBrOnNonNull:
		labelIndex := c.readI32u()
		if state.unreachable {
			break
		}

		r := state.pop()
		zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder)
		isNonNull := builder.AllocateInstruction().
			AsIcmp(r, zero.Return(), ssa.IntegerCmpCondNotEqual).
			Insert(builder).Return()

		// When non-null, branch to label with args + the non-null ref.
		targetBlk, argNum := state.brTargetArgNumFor(labelIndex)
		// The branch delivers argNum-1 values from the stack plus the ref.
		// The ref is the last value delivered to the label target.
		args := c.nPeekDup(argNum - 1)
		args = args.Append(builder.VarLengthPool(), r)
		var sealTargetBlk bool

		if c.branchExitsTryTable(int(labelIndex)) {
			current := builder.CurrentBlock()
			trampolineBlk := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(trampolineBlk)
			c.emitTryTableLeaves(int(labelIndex))
			c.insertJumpToBlock(args, targetBlk)
			builder.SetCurrentBlock(current)
			targetBlk = trampolineBlk
			sealTargetBlk = true
			args = ssa.ValuesNil
		}

		if c.needListener && targetBlk.ReturnBlock() {
			current := builder.CurrentBlock()
			targetBlk = builder.AllocateBasicBlock()
			builder.SetCurrentBlock(targetBlk)
			sealTargetBlk = true
			c.callListenerAfter()
			instr := builder.AllocateInstruction()
			instr.AsReturn(args)
			builder.InsertInstruction(instr)
			args = ssa.ValuesNil
			builder.SetCurrentBlock(current)
		}

		brnz := builder.AllocateInstruction()
		brnz.AsBrnz(isNonNull, args, targetBlk)
		builder.InsertInstruction(brnz)

		if sealTargetBlk {
			builder.Seal(targetBlk)
		}

		// Fall-through: ref is null, nothing extra pushed.
		elseBlk := builder.AllocateBasicBlock()
		c.insertJumpToBlock(ssa.ValuesNil, elseBlk)
		builder.Seal(elseBlk)
		builder.SetCurrentBlock(elseBlk)

	case wasm.OpcodeCallRef:
		typeIndex := c.readI32u()
		if state.unreachable {
			break
		}
		c.lowerCallRef(typeIndex)

	case wasm.OpcodeReturnCallRef:
		typeIndex := c.readI32u()
		if state.unreachable {
			break
		}
		c.emitTryTableLeaves(len(c.state().controlFrames))
		c.lowerTailCallReturnCallRef(typeIndex)
		state.unreachable = true

	default:
		panic("TODO: unsupported in native yet: " + wasm.InstructionName(op))
	}

	if nativeapi.FrontEndLoggingEnabled {
		fmt.Println("--------- Translated " + wasm.InstructionName(op) + " --------")
		fmt.Println("state: " + c.loweringState.String())
		fmt.Println(c.formatBuilder())
		fmt.Println("--------------------------")
	}
	c.loweringState.pc++
}

func (c *Compiler) lowerReturn(builder ssa.Builder) {
	results := c.nPeekDup(c.results())
	instr := builder.AllocateInstruction()

	instr.AsReturn(results)
	builder.InsertInstruction(instr)
}

func (c *Compiler) lowerExtMul(v1, v2 ssa.Value, from, to ssa.VecLane, signed, low bool) ssa.Value {
	// TODO: The sequence `Widen; Widen; VIMul` can be substituted for a single instruction on some ISAs.
	builder := c.ssaBuilder

	v1lo := builder.AllocateInstruction().AsWiden(v1, from, signed, low).Insert(builder).Return()
	v2lo := builder.AllocateInstruction().AsWiden(v2, from, signed, low).Insert(builder).Return()

	return builder.AllocateInstruction().AsVImul(v1lo, v2lo, to).Insert(builder).Return()
}

const (
	tableInstanceBaseAddressOffset = 0
	tableInstanceLenOffset         = tableInstanceBaseAddressOffset + 8
)

func (c *Compiler) lowerAccessTableWithBoundsCheck(tableIndex uint32, elementOffsetInTable ssa.Value) (elementAddress ssa.Value) {
	builder := c.ssaBuilder

	// Load the table.
	loadTableInstancePtr := builder.AllocateInstruction()
	loadTableInstancePtr.AsLoad(c.moduleCtxPtrValue, c.offset.TableOffset(int(tableIndex)).U32(), ssa.TypeI64)
	builder.InsertInstruction(loadTableInstancePtr)
	tableInstancePtr := loadTableInstancePtr.Return()

	// Load the table's length.
	loadTableLen := builder.AllocateInstruction()
	loadTableLen.AsLoad(tableInstancePtr, tableInstanceLenOffset, ssa.TypeI32)
	builder.InsertInstruction(loadTableLen)
	tableLen := loadTableLen.Return()

	// A 64-bit table's index operand is i64, so the comparison has to happen at
	// that width. A table never holds more than 2^32-1 entries whatever its
	// index type, so widening the length is enough: any index past that
	// compares greater and traps.
	if c.tableIsIndex64(tableIndex) {
		tableLen = builder.AllocateInstruction().AsUExtend(tableLen, 32, 64).Insert(builder).Return()
	}

	// Compare the length and the target, and trap if out of bounds.
	checkOOB := builder.AllocateInstruction()
	checkOOB.AsIcmp(elementOffsetInTable, tableLen, ssa.IntegerCmpCondUnsignedGreaterThanOrEqual)
	builder.InsertInstruction(checkOOB)
	exitIfOOB := builder.AllocateInstruction()
	exitIfOOB.AsExitIfTrueWithCode(c.execCtxPtrValue, checkOOB.Return(), nativeapi.ExitCodeTableOutOfBounds)
	builder.InsertInstruction(exitIfOOB)

	// Get the base address of wasm.TableInstance.References.
	loadTableBaseAddress := builder.AllocateInstruction()
	loadTableBaseAddress.AsLoad(tableInstancePtr, tableInstanceBaseAddressOffset, ssa.TypeI64)
	builder.InsertInstruction(loadTableBaseAddress)
	tableBase := loadTableBaseAddress.Return()

	// Calculate the address of the target function. First we need to multiply targetOffsetInTable by 8 (pointer size).
	multiplyBy8 := builder.AllocateInstruction()
	three := builder.AllocateInstruction()
	three.AsIconst64(3)
	builder.InsertInstruction(three)
	multiplyBy8.AsIshl(elementOffsetInTable, three.Return())
	builder.InsertInstruction(multiplyBy8)
	targetOffsetInTableMultipliedBy8 := multiplyBy8.Return()

	// Then add the multiplied value to the base which results in the address of the target function (*native.functionInstance)
	calcElementAddressInTable := builder.AllocateInstruction()
	calcElementAddressInTable.AsIadd(tableBase, targetOffsetInTableMultipliedBy8)
	builder.InsertInstruction(calcElementAddressInTable)
	return calcElementAddressInTable.Return()
}

func (c *Compiler) prepareCall(fnIndex uint32) (isIndirect bool, sig *ssa.Signature, args ssa.Values, funcRefOrPtrValue uint64) {
	builder := c.ssaBuilder
	state := c.state()
	var typIndex wasm.Index
	if fnIndex < c.m.ImportFunctionCount {
		// Before transfer the control to the callee, we have to store the current module's moduleContextPtr
		// into execContext.callerModuleContextPtr in case when the callee is a Go function.
		c.storeCallerModuleContext()
		var fi int
		for i := range c.m.ImportSection {
			imp := &c.m.ImportSection[i]
			if imp.Type == wasm.ExternTypeFunc {
				if fi == int(fnIndex) {
					typIndex = imp.DescFunc
					break
				}
				fi++
			}
		}
	} else {
		typIndex = c.m.FunctionSection[fnIndex-c.m.ImportFunctionCount]
	}
	typ := &c.m.TypeSection[typIndex]

	argN := len(typ.Params)
	tail := len(state.values) - argN
	vs := state.values[tail:]
	state.values = state.values[:tail]
	args = c.allocateVarLengthValues(2+len(vs), c.execCtxPtrValue)

	sig = c.signatures[typ]
	if fnIndex >= c.m.ImportFunctionCount {
		args = args.Append(builder.VarLengthPool(), c.moduleCtxPtrValue) // This case the callee module is itself.
		args = args.Append(builder.VarLengthPool(), vs...)
		return false, sig, args, uint64(FunctionIndexToFuncRef(fnIndex))
	} else {
		// This case we have to read the address of the imported function from the module context.
		moduleCtx := c.moduleCtxPtrValue
		loadFuncPtr, loadModuleCtxPtr := builder.AllocateInstruction(), builder.AllocateInstruction()
		funcPtrOffset, moduleCtxPtrOffset, _ := c.offset.ImportedFunctionOffset(fnIndex)
		loadFuncPtr.AsLoad(moduleCtx, funcPtrOffset.U32(), ssa.TypeI64)
		loadModuleCtxPtr.AsLoad(moduleCtx, moduleCtxPtrOffset.U32(), ssa.TypeI64)
		builder.InsertInstruction(loadFuncPtr)
		builder.InsertInstruction(loadModuleCtxPtr)

		args = args.Append(builder.VarLengthPool(), loadModuleCtxPtr.Return())
		args = args.Append(builder.VarLengthPool(), vs...)

		return true, sig, args, uint64(loadFuncPtr.Return())
	}
}

func (c *Compiler) lowerCall(fnIndex uint32) {
	builder := c.ssaBuilder
	state := c.state()
	isIndirect, sig, args, funcRefOrPtrValue := c.prepareCall(fnIndex)

	call := builder.AllocateInstruction()
	if isIndirect {
		call.AsCallIndirect(ssa.Value(funcRefOrPtrValue), sig, args)
	} else {
		call.AsCall(ssa.FuncRef(funcRefOrPtrValue), sig, args)
	}
	builder.InsertInstruction(call)

	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
}

func (c *Compiler) prepareCallIndirect(typeIndex, tableIndex uint32) (ssa.Value, *wasm.FunctionType, ssa.Values) {
	builder := c.ssaBuilder
	state := c.state()

	elementOffsetInTable := state.pop()
	functionInstancePtrAddress := c.lowerAccessTableWithBoundsCheck(tableIndex, elementOffsetInTable)
	loadFunctionInstancePtr := builder.AllocateInstruction()
	loadFunctionInstancePtr.AsLoad(functionInstancePtrAddress, 0, ssa.TypeI64)
	builder.InsertInstruction(loadFunctionInstancePtr)
	functionInstancePtr := loadFunctionInstancePtr.Return()

	// Check if it is not the null pointer.
	zero := builder.AllocateInstruction()
	zero.AsIconst64(0)
	builder.InsertInstruction(zero)
	checkNull := builder.AllocateInstruction()
	checkNull.AsIcmp(functionInstancePtr, zero.Return(), ssa.IntegerCmpCondEqual)
	builder.InsertInstruction(checkNull)
	exitIfNull := builder.AllocateInstruction()
	exitIfNull.AsExitIfTrueWithCode(c.execCtxPtrValue, checkNull.Return(), nativeapi.ExitCodeIndirectCallNullPointer)
	builder.InsertInstruction(exitIfNull)

	// We need to do the type check. First, load the target function instance's typeID.
	loadTypeID := builder.AllocateInstruction()
	loadTypeID.AsLoad(functionInstancePtr, nativeapi.FunctionInstanceTypeIDOffset, ssa.TypeI32)
	builder.InsertInstruction(loadTypeID)
	actualTypeID := loadTypeID.Return()

	// Next, we load the expected TypeID:
	loadTypeIDsBegin := builder.AllocateInstruction()
	loadTypeIDsBegin.AsLoad(c.moduleCtxPtrValue, c.offset.TypeIDs1stElement.U32(), ssa.TypeI64)
	builder.InsertInstruction(loadTypeIDsBegin)
	typeIDsBegin := loadTypeIDsBegin.Return()

	loadExpectedTypeID := builder.AllocateInstruction()
	loadExpectedTypeID.AsLoad(typeIDsBegin, uint32(typeIndex)*4 /* size of wasm.FunctionTypeID */, ssa.TypeI32)
	builder.InsertInstruction(loadExpectedTypeID)
	expectedTypeID := loadExpectedTypeID.Return()

	// Check if the type ID matches. Under the GC proposal a callee whose type is a *subtype* of the declared
	// one also matches, and deciding that needs store-wide type identity which compiled code cannot reach --
	// so a module that declares any subtype relation asks Go instead. Every module that declares none, which
	// is every pre-GC module, keeps the inline compare it always had.
	if c.declaresSubtypes {
		c.callGC(wasm.GCCheckIndirectCall,
			builder.AllocateInstruction().AsUExtend(actualTypeID, 32, 64).Insert(builder).Return(),
			builder.AllocateInstruction().AsUExtend(expectedTypeID, 32, 64).Insert(builder).Return(),
		)
	} else {
		checkTypeID := builder.AllocateInstruction()
		checkTypeID.AsIcmp(actualTypeID, expectedTypeID, ssa.IntegerCmpCondNotEqual)
		builder.InsertInstruction(checkTypeID)
		exitIfNotMatch := builder.AllocateInstruction()
		exitIfNotMatch.AsExitIfTrueWithCode(c.execCtxPtrValue, checkTypeID.Return(), nativeapi.ExitCodeIndirectCallTypeMismatch)
		builder.InsertInstruction(exitIfNotMatch)
	}

	// Now ready to call the function. Load the executable and moduleContextOpaquePtr from the function instance.
	loadExecutablePtr := builder.AllocateInstruction()
	loadExecutablePtr.AsLoad(functionInstancePtr, nativeapi.FunctionInstanceExecutableOffset, ssa.TypeI64)
	builder.InsertInstruction(loadExecutablePtr)
	executablePtr := loadExecutablePtr.Return()
	loadModuleContextOpaquePtr := builder.AllocateInstruction()
	loadModuleContextOpaquePtr.AsLoad(functionInstancePtr, nativeapi.FunctionInstanceModuleContextOpaquePtrOffset, ssa.TypeI64)
	builder.InsertInstruction(loadModuleContextOpaquePtr)
	moduleContextOpaquePtr := loadModuleContextOpaquePtr.Return()

	typ := &c.m.TypeSection[typeIndex]
	tail := len(state.values) - len(typ.Params)
	vs := state.values[tail:]
	state.values = state.values[:tail]
	args := c.allocateVarLengthValues(2+len(vs), c.execCtxPtrValue, moduleContextOpaquePtr)
	args = args.Append(builder.VarLengthPool(), vs...)

	// Before transfer the control to the callee, we have to store the current module's moduleContextPtr
	// into execContext.callerModuleContextPtr in case when the callee is a Go function.
	c.storeCallerModuleContext()

	return executablePtr, typ, args
}

func (c *Compiler) lowerCallIndirect(typeIndex, tableIndex uint32) {
	builder := c.ssaBuilder
	state := c.state()
	executablePtr, typ, args := c.prepareCallIndirect(typeIndex, tableIndex)

	call := builder.AllocateInstruction()
	call.AsCallIndirect(executablePtr, c.signatures[typ], args)
	builder.InsertInstruction(call)

	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
}

func (c *Compiler) lowerTailCallReturnCall(fnIndex uint32) {
	isIndirect, sig, args, funcRefOrPtrValue := c.prepareCall(fnIndex)
	builder := c.ssaBuilder
	state := c.state()

	call := builder.AllocateInstruction()
	if isIndirect {
		call.AsTailCallReturnCallIndirect(ssa.Value(funcRefOrPtrValue), sig, args)
	} else {
		call.AsTailCallReturnCall(ssa.FuncRef(funcRefOrPtrValue), sig, args)
	}
	builder.InsertInstruction(call)

	// In a proper tail call, the following code is unreachable since execution
	// transfers to the callee. However, sometimes the backend might need to fall back to
	// a regular call, so we include return handling and let the backend delete it
	// when redundant.
	// For details, see internal/engine/RATIONALE.md
	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
	c.lowerReturn(builder)
}

func (c *Compiler) lowerTailCallReturnCallIndirect(typeIndex, tableIndex uint32) {
	builder := c.ssaBuilder
	state := c.state()
	executablePtr, typ, args := c.prepareCallIndirect(typeIndex, tableIndex)

	call := builder.AllocateInstruction()
	call.AsTailCallReturnCallIndirect(executablePtr, c.signatures[typ], args)
	builder.InsertInstruction(call)

	// In a proper tail call, the following code is unreachable since execution
	// transfers to the callee. However, sometimes the backend might need to fall back to
	// a regular call, so we include return handling and let the backend delete it
	// when redundant.
	// For details, see internal/engine/RATIONALE.md
	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
	c.lowerReturn(builder)
}

func (c *Compiler) prepareCallRef(typeIndex uint32) (ssa.Value, *wasm.FunctionType, ssa.Values) {
	builder := c.ssaBuilder
	state := c.state()

	functionInstancePtr := state.pop()

	// Check if it is not the null pointer.
	zero := builder.AllocateInstruction()
	zero.AsIconst64(0)
	builder.InsertInstruction(zero)
	checkNull := builder.AllocateInstruction()
	checkNull.AsIcmp(functionInstancePtr, zero.Return(), ssa.IntegerCmpCondEqual)
	builder.InsertInstruction(checkNull)
	exitIfNull := builder.AllocateInstruction()
	exitIfNull.AsExitIfTrueWithCode(c.execCtxPtrValue, checkNull.Return(), nativeapi.ExitCodeNullReference)
	builder.InsertInstruction(exitIfNull)

	// Load the executable and moduleContextOpaquePtr from the function instance.
	loadExecutablePtr := builder.AllocateInstruction()
	loadExecutablePtr.AsLoad(functionInstancePtr, nativeapi.FunctionInstanceExecutableOffset, ssa.TypeI64)
	builder.InsertInstruction(loadExecutablePtr)
	executablePtr := loadExecutablePtr.Return()
	loadModuleContextOpaquePtr := builder.AllocateInstruction()
	loadModuleContextOpaquePtr.AsLoad(functionInstancePtr, nativeapi.FunctionInstanceModuleContextOpaquePtrOffset, ssa.TypeI64)
	builder.InsertInstruction(loadModuleContextOpaquePtr)
	moduleContextOpaquePtr := loadModuleContextOpaquePtr.Return()

	typ := &c.m.TypeSection[typeIndex]
	tail := len(state.values) - len(typ.Params)
	vs := state.values[tail:]
	state.values = state.values[:tail]
	args := c.allocateVarLengthValues(2+len(vs), c.execCtxPtrValue, moduleContextOpaquePtr)
	args = args.Append(builder.VarLengthPool(), vs...)

	c.storeCallerModuleContext()

	return executablePtr, typ, args
}

func (c *Compiler) lowerCallRef(typeIndex uint32) {
	builder := c.ssaBuilder
	state := c.state()
	executablePtr, typ, args := c.prepareCallRef(typeIndex)

	call := builder.AllocateInstruction()
	call.AsCallIndirect(executablePtr, c.signatures[typ], args)
	builder.InsertInstruction(call)

	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
}

func (c *Compiler) lowerTailCallReturnCallRef(typeIndex uint32) {
	builder := c.ssaBuilder
	state := c.state()
	executablePtr, typ, args := c.prepareCallRef(typeIndex)

	call := builder.AllocateInstruction()
	call.AsTailCallReturnCallIndirect(executablePtr, c.signatures[typ], args)
	builder.InsertInstruction(call)

	// In a proper tail call, the following code is unreachable since execution
	// transfers to the callee. However, sometimes the backend might need to fall back to
	// a regular call, so we include return handling and let the backend delete it
	// when redundant.
	// For details, see internal/engine/RATIONALE.md
	first, rest := call.Returns()
	if first.Valid() {
		state.push(first)
	}
	for _, v := range rest {
		state.push(v)
	}

	c.reloadAfterCall()
	c.lowerReturn(builder)
}

// emitCheckModuleExitCode emits the indirect call to the check-module-exit-code
// trampoline. This is a Go round-trip: besides observing a pending
// WithCloseOnContextDone cancellation, it is the only scheduler/GC yield point
// in an otherwise non-preemptible compiled loop, so it must stay a Go call.
func (c *Compiler) emitCheckModuleExitCode(builder ssa.Builder) {
	checkModuleExitCodePtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetCheckModuleExitCodeTrampolineAddress.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()

	args := c.allocateVarLengthValues(1, c.execCtxPtrValue)
	builder.AllocateInstruction().
		AsCallIndirect(checkModuleExitCodePtr, &c.checkModuleExitCodeSig, args).
		Insert(builder)
}

// memOpSetup inserts the bounds check and calculates the address of the memory operation (loads/stores).
// constUpperBoundU32 returns the value of v as an unsigned 32-bit constant if v
// is a constant instruction, mirroring the 32-bit-address assumption memOpSetup
// makes elsewhere.
func (c *Compiler) constUpperBoundU32(v ssa.Value) (uint64, bool) {
	if d := c.ssaBuilder.InstructionOfValue(v); d != nil && d.Constant() {
		return uint64(uint32(d.ConstantVal())), true
	}
	return 0, false
}

// staticUpperBoundU32 returns a static inclusive upper bound for the unsigned
// 32-bit value v. Keep this deliberately narrow: add only instruction shapes
// whose bound follows directly from WebAssembly integer semantics.
func (c *Compiler) staticUpperBoundU32(v ssa.Value) (uint64, bool) {
	return c.staticUpperBoundU32Depth(v, 0)
}

func (c *Compiler) staticUpperBoundU32Depth(v ssa.Value, depth int) (uint64, bool) {
	if depth > 4 {
		return 0, false
	}
	def := c.ssaBuilder.InstructionOfValue(v)
	if def == nil {
		return 0, false
	}
	if def.Constant() {
		return uint64(uint32(def.ConstantVal())), true
	}

	x, y := def.Arg2()
	switch def.Opcode() {
	case ssa.OpcodeBand:
		xBound, xOK := c.staticUpperBoundU32Depth(x, depth+1)
		yBound, yOK := c.staticUpperBoundU32Depth(y, depth+1)
		if xOK && yOK {
			return min(xBound, yBound), true
		} else if xOK {
			return xBound, true
		}
		return yBound, yOK
	case ssa.OpcodeUrem:
		if divisor, ok := c.constUpperBoundU32(y); ok && divisor != 0 {
			return divisor - 1, true
		}
	case ssa.OpcodeIadd:
		xBound, xOK := c.staticUpperBoundU32Depth(x, depth+1)
		yBound, yOK := c.staticUpperBoundU32Depth(y, depth+1)
		if xOK && yOK && xBound+yBound <= uint64(^uint32(0)) {
			return xBound + yBound, true
		}
	case ssa.OpcodeIshl:
		xBound, xOK := c.staticUpperBoundU32Depth(x, depth+1)
		shift, shiftOK := c.constUpperBoundU32(y)
		if xOK && shiftOK {
			shift &= 31 // WebAssembly masks i32 shift counts to five bits.
			if xBound <= uint64(^uint32(0))>>shift {
				return xBound << shift, true
			}
		}
	}
	return 0, false
}

// recordRangeSafeBound materializes the absolute address for a bounds-check-
// elided access (if not already computed) and records the known-safe bound so
// subsequent accesses off the same base reuse it.
func (c *Compiler) recordRangeSafeBound(memIndex wasm.Index, baseAddr ssa.Value, baseAddrID ssa.ValueID, ceil uint64, address ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	if !address.Valid() {
		memBase := c.getMemoryBaseValue(memIndex, false)
		extBaseAddr := builder.AllocateInstruction().AsUExtend(baseAddr, 32, 64).Insert(builder).Return()
		address = builder.AllocateInstruction().AsIadd(memBase, extBaseAddr).Insert(builder).Return()
	}
	c.recordKnownSafeBound(baseAddrID, ceil, address)
	return address
}

func (c *Compiler) memOpSetup(memIndex wasm.Index, baseAddr ssa.Value, constOffset, operationSizeInBytes uint64) (address ssa.Value) {
	if c.memoryIsIndex64(memIndex) {
		return c.memOpSetup64(memIndex, baseAddr, constOffset, operationSizeInBytes)
	}
	address = ssa.ValueInvalid
	builder := c.ssaBuilder

	baseAddrID := baseAddr.ID()
	ceil := constOffset + operationSizeInBytes

	// The linear-path known-safe-bounds cache (knownSafeBounds) is keyed only
	// by baseAddrID, with no memory dimension in its cross-block merge -- see
	// the multiMemory doc comment. Skip it outright for multi-memory modules;
	// the dominance-based pass in ssa/pass.go (which IS memory-aware) still
	// applies as a post-lowering optimization.
	if !c.multiMemory {
		if known := c.getKnownSafeBound(baseAddrID); known.valid() {
			// We reuse the calculated absolute address even if the bound is not known to be safe.
			address = known.absoluteAddr
			if ceil <= known.bound {
				if !address.Valid() {
					// This means that, the bound is known to be safe, but the memory base might have changed.
					// So, we re-calculate the address.
					memBase := c.getMemoryBaseValue(memIndex, false)
					extBaseAddr := builder.AllocateInstruction().
						AsUExtend(baseAddr, 32, 64).
						Insert(builder).
						Return()
					address = builder.AllocateInstruction().
						AsIadd(memBase, extBaseAddr).Insert(builder).Return()
					known.absoluteAddr = address // Update the absolute address for the subsequent memory access.
				}
				return
			}
		}
	}

	memoryMinSizeInBytes := c.memoryMinSizeInBytes[memIndex]

	// A constant base address whose access end lies within the memory's
	// minimum size can never be out of bounds: memories only ever grow, so
	// the declared minimum is a static lower bound on the current length.
	if !c.multiMemory {
		if def := builder.InstructionOfValue(baseAddr); def != nil && def.Constant() {
			if uint64(uint32(def.ConstantVal()))+ceil <= memoryMinSizeInBytes {
				if !address.Valid() {
					memBase := c.getMemoryBaseValue(memIndex, false)
					extBaseAddr := builder.AllocateInstruction().
						AsUExtend(baseAddr, 32, 64).
						Insert(builder).
						Return()
					address = builder.AllocateInstruction().
						AsIadd(memBase, extBaseAddr).Insert(builder).Return()
				}
				c.recordKnownSafeBound(baseAddrID, ceil, address)
				return
			}
		}

		// A statically bounded base whose maximum access end lies within the
		// memory's minimum can never be out of bounds. This includes x & C (at most
		// C), x % C for a non-zero constant C (at most C-1), and non-wrapping add or
		// constant-shift address composition. Mirrors the constant case's 32-bit-
		// address assumption.
		if upperBound, ok := c.staticUpperBoundU32(baseAddr); ok && upperBound+ceil <= memoryMinSizeInBytes {
			address = c.recordRangeSafeBound(memIndex, baseAddr, baseAddrID, ceil, address)
			return
		}
	}

	ceilConst := builder.AllocateInstruction()
	ceilConst.AsIconst64(ceil)
	builder.InsertInstruction(ceilConst)

	// We calculate the offset in 64-bit space.
	extBaseAddr := builder.AllocateInstruction().
		AsUExtend(baseAddr, 32, 64).
		Insert(builder).
		Return()

	// Note: memLen is already zero extended to 64-bit space at the load time.
	memLen := c.getMemoryLenValue(memIndex, false)

	// baseAddrPlusCeil = baseAddr + ceil
	baseAddrPlusCeil := builder.AllocateInstruction()
	baseAddrPlusCeil.AsIadd(extBaseAddr, ceilConst.Return())
	builder.InsertInstruction(baseAddrPlusCeil)

	// Check for out of bounds memory access: `memLen >= baseAddrPlusCeil`.
	cmp := builder.AllocateInstruction()
	cmp.AsIcmp(memLen, baseAddrPlusCeil.Return(), ssa.IntegerCmpCondUnsignedLessThan)
	builder.InsertInstruction(cmp)
	exitIfNZ := builder.AllocateInstruction()
	exitIfNZ.AsExitIfTrueWithCode(c.execCtxPtrValue, cmp.Return(), nativeapi.ExitCodeMemoryOutOfBounds)
	builder.InsertInstruction(exitIfNZ)

	// Load the value from memBase + extBaseAddr.
	if address == ssa.ValueInvalid { // Reuse the value if the memBase is already calculated at this point.
		memBase := c.getMemoryBaseValue(memIndex, false)
		address = builder.AllocateInstruction().
			AsIadd(memBase, extBaseAddr).Insert(builder).Return()
	}

	// Record the bound ceil for this baseAddr is known to be safe for the subsequent memory access in the same block.
	if !c.multiMemory {
		c.recordKnownSafeBound(baseAddrID, ceil, address)
	}
	return
}

// memOpSetup64 is memOpSetup for a memory with an i64 index type. Two things
// separate it from the 32-bit form:
//
//   - The address operand is already 64 bits wide, so there is no zero
//     extension, and adding the memarg offset to it can carry out of a uint64.
//     A carry is itself an out-of-bounds access -- no buffer is 2^64 bytes long
//     -- so it has to be detected rather than silently wrapping into a small,
//     in-bounds-looking address.
//   - The memarg offset can exceed any machine displacement, so it is folded
//     into the returned address instead of being left to the load/store
//     instruction's addressing mode. readMemArg reports a zero displacement for
//     these accesses to match.
//
// The bounds-check-elision caches memOpSetup consults do not apply here: they
// are all keyed on 32-bit address arithmetic. The dominance-based pass in
// ssa/pass.go likewise only recognizes the 32-bit shape, so a 64-bit memory
// simply keeps every check.
func (c *Compiler) memOpSetup64(memIndex wasm.Index, baseAddr ssa.Value, constOffset, operationSizeInBytes uint64) ssa.Value {
	builder := c.ssaBuilder

	// ceil is the first byte past the access, relative to baseAddr. If it
	// overflows a uint64 the access is out of bounds for every possible
	// baseAddr, and saturating expresses exactly that: baseAddr+MaxUint64
	// either carries (baseAddr >= 1) or leaves MaxUint64, which no memory
	// length can reach.
	ceil, carry := bits.Add64(constOffset, operationSizeInBytes, 0)
	if carry != 0 {
		ceil = math.MaxUint64
	}
	ceilConst := builder.AllocateInstruction().AsIconst64(ceil).Insert(builder).Return()
	memLen := c.getMemoryLenValue(memIndex, false)

	var oob ssa.Value
	if ceil <= c.memoryMinSizeInBytes[memIndex] {
		// A memory never shrinks below its declared minimum, so memLen >= ceil
		// always holds and memLen-ceil cannot underflow. Comparing against that
		// adjusted limit costs the same as the 32-bit form's add-and-compare
		// and needs no separate overflow test.
		limit := builder.AllocateInstruction().AsIsub(memLen, ceilConst).Insert(builder).Return()
		oob = builder.AllocateInstruction().
			AsIcmp(baseAddr, limit, ssa.IntegerCmpCondUnsignedGreaterThan).Insert(builder).Return()
	} else {
		end := builder.AllocateInstruction().AsIadd(baseAddr, ceilConst).Insert(builder).Return()
		// ceil > 0 always (the access is at least one byte), so the sum carried
		// out of the uint64 exactly when it came out below ceil.
		overflowed := builder.AllocateInstruction().
			AsIcmp(end, ceilConst, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()
		past := builder.AllocateInstruction().
			AsIcmp(memLen, end, ssa.IntegerCmpCondUnsignedLessThan).Insert(builder).Return()
		orInstr := builder.AllocateInstruction()
		orInstr.AsBor(overflowed, past)
		oob = orInstr.Insert(builder).Return()
	}
	builder.AllocateInstruction().
		AsExitIfTrueWithCode(c.execCtxPtrValue, oob, nativeapi.ExitCodeMemoryOutOfBounds).
		Insert(builder)

	memBase := c.getMemoryBaseValue(memIndex, false)
	address := builder.AllocateInstruction().AsIadd(memBase, baseAddr).Insert(builder).Return()
	if constOffset != 0 {
		offsetConst := builder.AllocateInstruction().AsIconst64(constOffset).Insert(builder).Return()
		address = builder.AllocateInstruction().AsIadd(address, offsetConst).Insert(builder).Return()
	}
	return address
}

// atomicMemOpSetup inserts the bounds check and calculates the address of the memory operation (loads/stores), including
// the constant offset and performs an alignment check on the final address.
func (c *Compiler) atomicMemOpSetup(memIndex wasm.Index, baseAddr ssa.Value, constOffset, operationSizeInBytes uint64) (address ssa.Value) {
	builder := c.ssaBuilder

	addrWithoutOffset := c.memOpSetup(memIndex, baseAddr, constOffset, operationSizeInBytes)
	var addr ssa.Value
	// memOpSetup64 has already folded the offset into the address it returns,
	// since a 64-bit memory's offset can exceed a machine displacement.
	if constOffset == 0 || c.memoryIsIndex64(memIndex) {
		addr = addrWithoutOffset
	} else {
		offset := builder.AllocateInstruction().AsIconst64(constOffset).Insert(builder).Return()
		addr = builder.AllocateInstruction().AsIadd(addrWithoutOffset, offset).Insert(builder).Return()
	}

	c.memAlignmentCheck(addr, operationSizeInBytes)

	return addr
}

func (c *Compiler) memAlignmentCheck(addr ssa.Value, operationSizeInBytes uint64) {
	if operationSizeInBytes == 1 {
		return // No alignment restrictions when accessing a byte
	}
	var checkBits uint64
	switch operationSizeInBytes {
	case 2:
		checkBits = 0b1
	case 4:
		checkBits = 0b11
	case 8:
		checkBits = 0b111
	}

	builder := c.ssaBuilder

	mask := builder.AllocateInstruction().AsIconst64(checkBits).Insert(builder).Return()
	masked := builder.AllocateInstruction().AsBand(addr, mask).Insert(builder).Return()
	zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
	cmp := builder.AllocateInstruction().AsIcmp(masked, zero, ssa.IntegerCmpCondNotEqual).Insert(builder).Return()
	builder.AllocateInstruction().AsExitIfTrueWithCode(c.execCtxPtrValue, cmp, nativeapi.ExitCodeUnalignedAtomic).Insert(builder)
}

// memoryFillInlineMaxBytes is the largest compile-time-constant memory.fill
// size lowered as straight-line stores instead of the copy-doubling memmove
// loop. The ceiling is code size: at 128 bytes this is 16 eight-byte stores,
// and past that the loop -- one Go-runtime memmove call, whatever the size --
// is the better trade.
const memoryFillInlineMaxBytes = 128

// inlineMemoryFill writes size bytes of value's low byte at addr with plain
// stores. Its caller has already emitted the bounds check, so the
// trap-before-any-write ordering of the memmove loop it replaces is kept, as is
// the zero-size case: bounds are still checked, and nothing is written.
func (c *Compiler) inlineMemoryFill(addr, value ssa.Value, size uint32) {
	if size == 0 {
		return
	}
	builder := c.ssaBuilder

	// One 8-byte store covers 8 bytes only if the byte is splatted across the
	// whole word first; multiplying by 0x01..01 is the cheapest way there.
	var splat ssa.Value
	if def := builder.InstructionOfValue(value); def != nil && def.Constant() {
		splat = builder.AllocateInstruction().
			AsIconst64((def.ConstantVal() & 0xff) * 0x0101010101010101).Insert(builder).Return()
	} else {
		mask := builder.AllocateInstruction().AsIconst32(0xff).Insert(builder).Return()
		lowByte := builder.AllocateInstruction().AsBand(value, mask).Insert(builder).Return()
		wide := builder.AllocateInstruction().AsUExtend(lowByte, 32, 64).Insert(builder).Return()
		factor := builder.AllocateInstruction().AsIconst64(0x0101010101010101).Insert(builder).Return()
		splat = builder.AllocateInstruction().AsImul(wide, factor).Insert(builder).Return()
	}

	store := func(op ssa.Opcode, offset uint32) {
		builder.AllocateInstruction().AsStore(op, splat, addr, offset).Insert(builder)
	}
	switch {
	case size >= 8:
		// The last store overlaps the previous one when size is not a multiple
		// of 8, which is cheaper than stepping down through narrower stores.
		var off uint32
		for ; off+8 <= size; off += 8 {
			store(ssa.OpcodeStore, off)
		}
		if off < size {
			store(ssa.OpcodeStore, size-8)
		}
	case size >= 4:
		store(ssa.OpcodeIstore32, 0)
		if size > 4 {
			store(ssa.OpcodeIstore32, size-4)
		}
	case size >= 2:
		store(ssa.OpcodeIstore16, 0)
		if size > 2 {
			store(ssa.OpcodeIstore16, size-2)
		}
	default:
		store(ssa.OpcodeIstore8, 0)
	}
}

func (c *Compiler) callMemmove(dst, src, size ssa.Value) {
	args := c.allocateVarLengthValues(3, dst, src, size)
	if size.Type() != ssa.TypeI64 {
		panic("TODO: memmove size must be i64")
	}

	builder := c.ssaBuilder
	memmovePtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetMemmoveAddress.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()
	builder.AllocateInstruction().AsCallGoRuntimeMemmove(memmovePtr, &c.memmoveSig, args).Insert(builder)
}

func (c *Compiler) reloadAfterCall() {
	// Note that when these are not used in the following instructions, they will be optimized out.
	// So in any ways, we define them!

	// After calling any function, memory buffers might have changed.
	c.reloadAllMemories()

	// Also, any mutable Global can change.
	for _, index := range c.mutableGlobalVariablesIndexes {
		_ = c.getWasmGlobalValue(index, true)
	}
}

// reloadAllMemories re-defines the cached base/length of every non-shared
// memory. If a memory is shared, we don't need to reload its base and length
// as the base will never change. (c.memoryShared is empty when the module has
// no memory, so this is a no-op in that case.)
func (c *Compiler) reloadAllMemories() {
	if c.needMemory {
		for i := range c.memoryShared {
			if !c.memoryShared[i] {
				c.reloadMemoryBaseLen(wasm.Index(i))
			}
		}
	}
}

func (c *Compiler) reloadMemoryBaseLen(memIndex wasm.Index) {
	_ = c.getMemoryBaseValue(memIndex, true)
	_ = c.getMemoryLenValue(memIndex, true)

	// This function being called means that the memory base might have changed.
	// Therefore, we need to clear the absolute addresses recorded in the known safe bounds
	// because we cache the absolute address of the memory access per each base offset.
	c.resetAbsoluteAddressInSafeBounds()
}

// lowerMemoryGrowCall calls the memory.grow trampoline. Its page delta and
// result are i64 whatever the memory's index type (see memoryGrowSig), so a
// 32-bit memory's are widened and narrowed around the call. Those two
// instructions sit in a block that is about to allocate a whole memory, which
// is why memory.grow does not carry a second trampoline just to avoid them.
func (c *Compiler) lowerMemoryGrowCall(memIndex wasm.Index, pages ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	index64 := c.memoryIsIndex64(memIndex)
	if !index64 {
		pages = builder.AllocateInstruction().AsUExtend(pages, 32, 64).Insert(builder).Return()
	}
	c.storeCallerModuleContext()
	memoryGrowPtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetMemoryGrowTrampolineAddress.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()
	memIndexVal := builder.AllocateInstruction().AsIconst32(uint32(memIndex)).Insert(builder).Return()
	args := c.allocateVarLengthValues(3, c.execCtxPtrValue, memIndexVal, pages)
	ret := builder.AllocateInstruction().
		AsCallIndirect(memoryGrowPtr, &c.memoryGrowSig, args).
		Insert(builder).Return()
	if index64 {
		return ret
	}
	return builder.AllocateInstruction().AsIreduce(ret, ssa.TypeI32).Insert(builder).Return()
}

// lowerLocalMemoryGrow keeps growth within an ordinary memory's reserved
// capacity in native code. Growth that needs allocation, and all custom
// allocator growth (nativeGrowCap == 0), uses the existing Go trampoline.
func (c *Compiler) lowerLocalMemoryGrow(memIndex wasm.Index, pages ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	checkCapacityBlk := builder.AllocateBasicBlock()
	fastBlk := builder.AllocateBasicBlock()
	slowBlk := builder.AllocateBasicBlock()
	failBlk := builder.AllocateBasicBlock()
	followingBlk := builder.AllocateBasicBlock()
	result := followingBlk.AddParam(builder, ssa.TypeI32)

	currentLen := c.getMemoryLenValue(memIndex, false)
	pageShift := builder.AllocateInstruction().AsIconst64(wasm.MemoryPageSizeInBits).Insert(builder).Return()
	currentPages64 := builder.AllocateInstruction().AsUshr(currentLen, pageShift).Insert(builder).Return()
	currentPages := builder.AllocateInstruction().AsIreduce(currentPages64, ssa.TypeI32).Insert(builder).Return()
	pages64 := builder.AllocateInstruction().AsUExtend(pages, 32, 64).Insert(builder).Return()
	newPages := builder.AllocateInstruction().AsIadd(currentPages64, pages64).Insert(builder).Return()
	maxPages := builder.AllocateInstruction().AsIconst64(uint64(c.memoryMaxPages[memIndex])).Insert(builder).Return()
	overMax := builder.AllocateInstruction().
		AsIcmp(newPages, maxPages, ssa.IntegerCmpCondUnsignedGreaterThan).Insert(builder).Return()
	builder.AllocateInstruction().AsBrnz(overMax, ssa.ValuesNil, failBlk).Insert(builder)
	builder.AllocateInstruction().AsJump(ssa.ValuesNil, checkCapacityBlk).Insert(builder)

	builder.SetCurrentBlock(checkCapacityBlk)
	// This local memory's *wasm.MemoryInstance is mirrored into its own
	// opaque-context record (see ModuleContextOffsetData.MemoriesBegin), so a
	// single constant-offset load off moduleCtxPtrValue reaches it directly --
	// no need to chase ModuleInstance -> Memories slice data pointer -> element.
	memoryInstance := builder.AllocateInstruction().
		AsLoad(c.moduleCtxPtrValue, c.offset.LocalMemoryInstancePtrOffset(int(memIndex)).U32(), ssa.TypeI64).
		Insert(builder).Return()
	capacity := builder.AllocateInstruction().
		AsLoad(memoryInstance, memoryInstanceNativeGrowCapOffset, ssa.TypeI64).
		Insert(builder).Return()
	newLen := builder.AllocateInstruction().AsIshl(newPages, pageShift).Insert(builder).Return()
	withinCapacity := builder.AllocateInstruction().
		AsIcmp(capacity, newLen, ssa.IntegerCmpCondUnsignedGreaterThanOrEqual).Insert(builder).Return()
	builder.AllocateInstruction().AsBrnz(withinCapacity, ssa.ValuesNil, fastBlk).Insert(builder)
	builder.AllocateInstruction().AsJump(ssa.ValuesNil, slowBlk).Insert(builder)
	builder.Seal(checkCapacityBlk)

	builder.SetCurrentBlock(fastBlk)
	// Keep both logical-size views synchronized. The backing allocation and Go
	// slice header are immutable on this path.
	builder.AllocateInstruction().
		AsStore(ssa.OpcodeStore, newLen, c.moduleCtxPtrValue, (c.offset.MemoryOffset(int(memIndex)) + 8).U32()).
		Insert(builder)
	builder.AllocateInstruction().
		AsStore(ssa.OpcodeStore, newLen, memoryInstance, memoryInstanceSizeOffset).
		Insert(builder)
	builder.AllocateInstruction().
		AsJump(c.allocateVarLengthValues(1, currentPages), followingBlk).Insert(builder)
	builder.Seal(fastBlk)

	builder.SetCurrentBlock(slowBlk)
	slowResult := c.lowerMemoryGrowCall(memIndex, pages)
	builder.AllocateInstruction().
		AsJump(c.allocateVarLengthValues(1, slowResult), followingBlk).Insert(builder)
	builder.Seal(slowBlk)

	builder.SetCurrentBlock(failBlk)
	failed := builder.AllocateInstruction().AsIconst32(0xffffffff).Insert(builder).Return()
	builder.AllocateInstruction().
		AsJump(c.allocateVarLengthValues(1, failed), followingBlk).Insert(builder)
	builder.Seal(failBlk)

	builder.SetCurrentBlock(followingBlk)
	builder.Seal(followingBlk)
	return result
}

func (c *Compiler) setWasmGlobalValue(index wasm.Index, v ssa.Value) {
	variable := c.globalVariables[index]
	opaqueOffset := c.offset.GlobalInstanceOffset(index)

	builder := c.ssaBuilder
	if index < c.m.ImportGlobalCount {
		loadGlobalInstPtr := builder.AllocateInstruction()
		loadGlobalInstPtr.AsLoad(c.moduleCtxPtrValue, uint32(opaqueOffset), ssa.TypeI64)
		builder.InsertInstruction(loadGlobalInstPtr)

		store := builder.AllocateInstruction()
		store.AsStore(ssa.OpcodeStore, v, loadGlobalInstPtr.Return(), uint32(0))
		builder.InsertInstruction(store)

	} else {
		store := builder.AllocateInstruction()
		store.AsStore(ssa.OpcodeStore, v, c.moduleCtxPtrValue, uint32(opaqueOffset))
		builder.InsertInstruction(store)
	}

	// The value has changed to `v`, so we record it.
	builder.DefineVariableInCurrentBB(variable, v)
}

func (c *Compiler) getWasmGlobalValue(index wasm.Index, forceLoad bool) ssa.Value {
	variable := c.globalVariables[index]
	typ := c.globalVariablesTypes[index]
	opaqueOffset := c.offset.GlobalInstanceOffset(index)

	builder := c.ssaBuilder
	if !forceLoad {
		if v := builder.FindValueInLinearPath(variable); v.Valid() {
			return v
		}
	}

	var load *ssa.Instruction
	if index < c.m.ImportGlobalCount {
		loadGlobalInstPtr := builder.AllocateInstruction()
		loadGlobalInstPtr.AsLoad(c.moduleCtxPtrValue, uint32(opaqueOffset), ssa.TypeI64)
		builder.InsertInstruction(loadGlobalInstPtr)
		load = builder.AllocateInstruction().
			AsLoad(loadGlobalInstPtr.Return(), uint32(0), typ)
	} else {
		load = builder.AllocateInstruction().
			AsLoad(c.moduleCtxPtrValue, uint32(opaqueOffset), typ)
	}

	v := load.Insert(builder).Return()
	builder.DefineVariableInCurrentBB(variable, v)
	return v
}

var memoryInstanceBufOffset = wasm.MemoryInstanceBufferOffset()

var memoryInstanceNativeGrowCapOffset, memoryInstanceSizeOffset = wasm.MemoryInstanceNativeGrowOffsets()

func (c *Compiler) getMemoryBaseValue(memIndex wasm.Index, forceReload bool) ssa.Value {
	builder := c.ssaBuilder
	variable := c.memoryBaseVariables[memIndex]
	if !forceReload {
		if v := builder.FindValueInLinearPath(variable); v.Valid() {
			return v
		}
	}

	opaqueOffset := c.offset.MemoryOffset(int(memIndex))

	var ret ssa.Value
	if memIndex < c.m.ImportMemoryCount {
		loadMemInstPtr := builder.AllocateInstruction()
		loadMemInstPtr.AsLoad(c.moduleCtxPtrValue, opaqueOffset.U32(), ssa.TypeI64)
		builder.InsertInstruction(loadMemInstPtr)
		memInstPtr := loadMemInstPtr.Return()

		loadBufPtr := builder.AllocateInstruction()
		loadBufPtr.AsLoad(memInstPtr, memoryInstanceBufOffset, ssa.TypeI64)
		builder.InsertInstruction(loadBufPtr)
		ret = loadBufPtr.Return()
	} else {
		load := builder.AllocateInstruction()
		load.AsLoad(c.moduleCtxPtrValue, opaqueOffset.U32(), ssa.TypeI64)
		builder.InsertInstruction(load)
		ret = load.Return()
	}

	builder.DefineVariableInCurrentBB(variable, ret)
	return ret
}

func (c *Compiler) getMemoryLenValue(memIndex wasm.Index, forceReload bool) ssa.Value {
	variable := c.memoryLenVariables[memIndex]
	shared := c.memoryShared[memIndex]
	builder := c.ssaBuilder
	if !forceReload && !shared {
		if v := builder.FindValueInLinearPath(variable); v.Valid() {
			return v
		}
	}

	opaqueOffset := c.offset.MemoryOffset(int(memIndex))

	var ret ssa.Value
	if memIndex < c.m.ImportMemoryCount {
		loadMemInstPtr := builder.AllocateInstruction()
		loadMemInstPtr.AsLoad(c.moduleCtxPtrValue, opaqueOffset.U32(), ssa.TypeI64)
		builder.InsertInstruction(loadMemInstPtr)
		memInstPtr := loadMemInstPtr.Return()

		loadBufSizePtr := builder.AllocateInstruction()
		if shared {
			sizeOffset := builder.AllocateInstruction().AsIconst64(uint64(memoryInstanceSizeOffset)).Insert(builder).Return()
			addr := builder.AllocateInstruction().AsIadd(memInstPtr, sizeOffset).Insert(builder).Return()
			loadBufSizePtr.AsAtomicLoad(addr, 8, ssa.TypeI64)
		} else {
			loadBufSizePtr.AsLoad(memInstPtr, memoryInstanceSizeOffset, ssa.TypeI64)
		}
		builder.InsertInstruction(loadBufSizePtr)

		ret = loadBufSizePtr.Return()
	} else {
		load := builder.AllocateInstruction()
		if shared {
			lenOffset := builder.AllocateInstruction().AsIconst64(uint64(opaqueOffset) + 8).Insert(builder).Return()
			addr := builder.AllocateInstruction().AsIadd(c.moduleCtxPtrValue, lenOffset).Insert(builder).Return()
			load.AsAtomicLoad(addr, 8, ssa.TypeI64)
		} else {
			// Must be a full 64-bit load, not AsExtLoad(Uload32, ...): a
			// 65536-page (the legal maximum) memory has a byte length of
			// exactly 2^32, which a 32-bit load truncates to zero, making
			// every access on such a memory incorrectly trap as out-of-bounds.
			load.AsLoad(c.moduleCtxPtrValue, (opaqueOffset + 8).U32(), ssa.TypeI64)
		}
		builder.InsertInstruction(load)
		ret = load.Return()
	}

	builder.DefineVariableInCurrentBB(variable, ret)
	return ret
}

func (c *Compiler) insertIcmp(cond ssa.IntegerCmpCond) {
	state, builder := c.state(), c.ssaBuilder
	y, x := state.pop(), state.pop()
	cmp := builder.AllocateInstruction()
	cmp.AsIcmp(x, y, cond)
	builder.InsertInstruction(cmp)
	value := cmp.Return()
	state.push(value)
}

func (c *Compiler) insertFcmp(cond ssa.FloatCmpCond) {
	state, builder := c.state(), c.ssaBuilder
	y, x := state.pop(), state.pop()
	cmp := builder.AllocateInstruction()
	cmp.AsFcmp(x, y, cond)
	builder.InsertInstruction(cmp)
	value := cmp.Return()
	state.push(value)
}

// storeCallerModuleContext stores the current module's moduleContextPtr into execContext.callerModuleContextPtr.
func (c *Compiler) storeCallerModuleContext() {
	builder := c.ssaBuilder
	execCtx := c.execCtxPtrValue
	store := builder.AllocateInstruction()
	store.AsStore(ssa.OpcodeStore,
		c.moduleCtxPtrValue, execCtx, nativeapi.ExecutionContextOffsetCallerModuleContextPtr.U32())
	builder.InsertInstruction(store)
}

// resolveTagType returns the FunctionType for the tag at the given module-local index.
func (c *Compiler) resolveTagType(tagIndex uint32) *wasm.FunctionType {
	if tagIndex < c.m.ImportTagCount {
		cur := uint32(0)
		for i := range c.m.ImportSection {
			imp := &c.m.ImportSection[i]
			if imp.Type != wasm.ExternTypeTag {
				continue
			}
			if tagIndex == cur {
				return &c.m.TypeSection[imp.DescTag]
			}
			cur++
		}
	} else {
		tagSectionIdx := tagIndex - c.m.ImportTagCount
		if tagSectionIdx < uint32(len(c.m.TagSection)) {
			typeIdx := c.m.TagSection[tagSectionIdx].Type
			return &c.m.TypeSection[typeIdx]
		}
	}
	return nil
}

// emitThrow emits a call to the shared throw trampoline with the given exnref,
// followed by an unreachable exit (throw never returns).
func (c *Compiler) emitThrow(exnref ssa.Value) {
	builder := c.ssaBuilder
	throwPtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetThrowTrampolineAddress.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()
	throwArgs := c.allocateVarLengthValues(2, c.execCtxPtrValue, exnref)
	builder.AllocateInstruction().
		AsCallIndirect(throwPtr, &c.throwSig, throwArgs).
		Insert(builder)

	exit := builder.AllocateInstruction()
	exit.AsExitWithCode(c.execCtxPtrValue, nativeapi.ExitCodeUnreachable)
	builder.InsertInstruction(exit)
}

// loadLocalsSaveAreaPtr emits a load of the locals save area pointer from execCtx.
func (c *Compiler) loadLocalsSaveAreaPtr() ssa.Value {
	return c.ssaBuilder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetLocalsSaveAreaPtr.U32(),
			ssa.TypeI64).
		Insert(c.ssaBuilder).Return()
}

// storeLocalToSaveArea emits a store of the given local value to the
// heap-allocated locals save area.
func (c *Compiler) storeLocalToSaveArea(localIdx wasm.Index, val ssa.Value) {
	ptr := c.loadLocalsSaveAreaPtr()
	store := c.ssaBuilder.AllocateInstruction()
	store.AsStore(ssa.OpcodeStore, val, ptr, uint32(localIdx)*16)
	c.ssaBuilder.InsertInstruction(store)
}

// reloadLocalsFromSaveArea loads all locals from the heap-allocated save area
// and redefines the SSA variables, so handler blocks see throw-time values.
func (c *Compiler) reloadLocalsFromSaveArea() {
	builder := c.ssaBuilder
	ptr := c.loadLocalsSaveAreaPtr()
	numParams := len(c.wasmFunctionTyp.Params)
	numLocals := numParams + len(c.wasmFunctionLocalTypes)
	for i := 0; i < numLocals; i++ {
		localIdx := wasm.Index(i)
		var wasmType wasm.ValueType
		if i < numParams {
			wasmType = c.wasmFunctionTyp.Params[i]
		} else {
			wasmType = c.wasmFunctionLocalTypes[i-numParams]
		}
		ssaType := WasmTypeToSSAType(wasmType)
		load := builder.AllocateInstruction()
		load.AsLoad(ptr, uint32(localIdx)*16, ssaType)
		builder.InsertInstruction(load)
		variable := c.localVariable(localIdx)
		builder.DefineVariableInCurrentBB(variable, load.Return())
	}
}

// storeAllLocalsToSaveArea stores all locals to the save area at once.
func (c *Compiler) storeAllLocalsToSaveArea() {
	builder := c.ssaBuilder
	ptr := c.loadLocalsSaveAreaPtr()
	numParams := len(c.wasmFunctionTyp.Params)
	numLocals := numParams + len(c.wasmFunctionLocalTypes)
	for i := 0; i < numLocals; i++ {
		localIdx := wasm.Index(i)
		variable := c.localVariable(localIdx)
		val := builder.MustFindValue(variable)
		store := builder.AllocateInstruction()
		store.AsStore(ssa.OpcodeStore, val, ptr, uint32(localIdx)*16)
		builder.InsertInstruction(store)
	}
}

// emitTryTableLeave emits a trampoline call to pop the try handler in the dispatch loop.
func (c *Compiler) emitTryTableLeave() {
	builder := c.ssaBuilder
	c.storeCallerModuleContext()

	leavePtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetTryTableLeaveTrampolineAddress.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()

	args := c.allocateVarLengthValues(1, c.execCtxPtrValue)
	builder.AllocateInstruction().
		AsCallIndirect(leavePtr, &c.tryTableLeaveSig, args).
		Insert(builder)
}

// branchExitsTryTable returns true if a branch to the given depth would
// exit at least one try_table frame that has catch clauses.
func (c *Compiler) branchExitsTryTable(depth int) bool {
	state := c.state()
	tail := len(state.controlFrames) - 1
	for i := 0; i < depth; i++ {
		if state.controlFrames[tail-i].isTryCatch() {
			return true
		}
	}
	// A br to a non-loop target also exits that frame.
	if depth <= tail {
		cf := &state.controlFrames[tail-depth]
		if !cf.isLoop() && cf.isTryCatch() {
			return true
		}
	}
	return false
}

// emitTryTableLeaves emits TryTableLeave calls for try_table frames
// with catch clauses that would be exited by a branch to the given depth.
func (c *Compiler) emitTryTableLeaves(depth int) {
	state := c.state()
	tail := len(state.controlFrames) - 1
	for i := 0; i < depth; i++ {
		if state.controlFrames[tail-i].isTryCatch() {
			c.emitTryTableLeave()
		}
	}
	// A br to a non-loop target also exits that frame.
	if depth <= tail {
		cf := &state.controlFrames[tail-depth]
		if !cf.isLoop() && cf.isTryCatch() {
			c.emitTryTableLeave()
		}
	}
}

// catchClause holds a parsed catch clause from a try_table instruction.
type catchClause struct {
	kind     byte
	tagIndex uint32
	labelIdx uint32
}

// loadExceptionParams loads the exception params from the caught Exception's
// Params slice. The dispatch loop sets execCtx.exceptionParamsPtr to the
// slice's backing-array pointer after matching a handler. We load that pointer
// and then read each param from [ptr + i*8], mirroring the stores emitted by
// the throw lowering. Float params were bitcast to integers at the throw site,
// so we load as integer and bitcast back to the original type.
func (c *Compiler) loadExceptionParams(tagType *wasm.FunctionType) []ssa.Value {
	if len(tagType.Params) == 0 {
		return nil
	}
	builder := c.ssaBuilder

	// Load the pointer to the caught Exception's Params backing array.
	paramsPtr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetExceptionParamsPtr.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()

	var values []ssa.Value
	for i, vt := range tagType.Params {
		offset := uint32(i) * 8
		ssaType := WasmTypeToSSAType(vt)
		switch ssaType {
		case ssa.TypeF32:
			// Stored as i32 at throw site; bitcast back to f32.
			raw := builder.AllocateInstruction().
				AsLoad(paramsPtr, offset, ssa.TypeI32).
				Insert(builder).Return()
			val := builder.AllocateInstruction().AsBitcast(raw, ssa.TypeF32).Insert(builder).Return()
			values = append(values, val)
		case ssa.TypeF64:
			// Stored as i64 at throw site; bitcast back to f64.
			raw := builder.AllocateInstruction().
				AsLoad(paramsPtr, offset, ssa.TypeI64).
				Insert(builder).Return()
			val := builder.AllocateInstruction().AsBitcast(raw, ssa.TypeF64).Insert(builder).Return()
			values = append(values, val)
		default:
			val := builder.AllocateInstruction().
				AsLoad(paramsPtr, offset, ssaType).
				Insert(builder).Return()
			values = append(values, val)
		}
	}
	return values
}

// loadExnRef loads the exnref (pointer to Exception) from the executionContext.
// The dispatch loop writes it to exceptionRef after matching a handler.
func (c *Compiler) loadExnRef() ssa.Value {
	builder := c.ssaBuilder
	return builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue,
			nativeapi.ExecutionContextOffsetExceptionRef.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()
}

// skipTryTableCatchClauses advances the bytecode PC past the catch clauses
// of a try_table instruction. This is used both in reachable and unreachable states.
func (c *Compiler) skipTryTableCatchClauses() {
	c.loweringState.pc++
	catchCount, catchNum, _ := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
	c.loweringState.pc += int(catchNum) - 1
	for i := uint32(0); i < catchCount; i++ {
		c.loweringState.pc++
		kind := c.wasmFunctionBody[c.loweringState.pc]
		switch kind {
		case wasm.CatchKindCatch, wasm.CatchKindCatchRef:
			// Read tag index.
			c.loweringState.pc++
			_, n, _ := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
			c.loweringState.pc += int(n) - 1
			// Read label index.
			c.loweringState.pc++
			_, n, _ = leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
			c.loweringState.pc += int(n) - 1
		case wasm.CatchKindCatchAll, wasm.CatchKindCatchAllRef:
			// Read label index.
			c.loweringState.pc++
			_, n, _ := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc:])
			c.loweringState.pc += int(n) - 1
		}
	}
}

func (c *Compiler) readByte() byte {
	v := c.wasmFunctionBody[c.loweringState.pc+1]
	c.loweringState.pc++
	return v
}

func (c *Compiler) readI32u() uint32 {
	v, n, err := leb128.LoadUint32(c.wasmFunctionBody[c.loweringState.pc+1:])
	if err != nil {
		panic(err) // shouldn't be reached since compilation comes after validation.
	}
	c.loweringState.pc += int(n)
	return v
}

func (c *Compiler) readI32s() int32 {
	v, n, err := leb128.LoadInt32(c.wasmFunctionBody[c.loweringState.pc+1:])
	if err != nil {
		panic(err) // shouldn't be reached since compilation comes after validation.
	}
	c.loweringState.pc += int(n)
	return v
}

// readI33s reads the signed 33-bit LEB128 a heap type immediate is encoded as.
func (c *Compiler) readI33s() int64 {
	v, n, err := leb128.LoadInt33AsInt64(c.wasmFunctionBody[c.loweringState.pc+1:])
	if err != nil {
		panic(err) // shouldn't be reached since compilation comes after validation.
	}
	c.loweringState.pc += int(n)
	return v
}

// encodeRefTarget turns a decoded heap type immediate into the descriptor wasm.RunGCCheck expects.
func encodeRefTarget(heapType int64, nullable bool) (uint64, bool) {
	if heapType < 0 {
		abstract, ok := wasm.AbstractHeapTypeValueType(heapType)
		if !ok {
			return 0, false
		}
		return wasm.EncodeRefTarget(uint32(abstract.Kind()), nullable, false), true
	}
	return wasm.EncodeRefTarget(uint32(heapType), nullable, true), true
}

// callGC emits a call to the trampoline behind every GC runtime operation, returning its result. See
// wasm.RunGC for what the operands mean per mode; unused ones are passed as zero.
//
// ponytail: a Go round trip per GC instruction. The answer needs store-wide type identity and the managed
// heap, neither of which compiled code can reach today, and no GC instruction is on a hot path yet -- so this
// buys the whole proposal for one trampoline. Inlining the common shapes (a struct field load, an i31 test) is
// the upgrade if a profile ever asks for it.
func (c *Compiler) callGC(mode uint64, operands ...ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	c.storeCallerModuleContext()
	ptr := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue, nativeapi.ExecutionContextOffsetGCCheckTrampolineAddress.U32(), ssa.TypeI64).
		Insert(builder).Return()
	// RunGC takes a fixed five operands whatever the mode, so pad the unused tail with zeroes.
	vs := make([]ssa.Value, 0, 7)
	vs = append(vs, c.execCtxPtrValue, builder.AllocateInstruction().AsIconst64(mode).Insert(builder).Return())
	vs = append(vs, operands...)
	for len(vs) < 7 {
		vs = append(vs, builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return())
	}
	args := c.allocateVarLengthValues(7, vs...)
	return builder.AllocateInstruction().AsCallIndirect(ptr, &c.gcSig, args).Insert(builder).Return()
}

func (c *Compiler) readI64s() int64 {
	v, n, err := leb128.LoadInt64(c.wasmFunctionBody[c.loweringState.pc+1:])
	if err != nil {
		panic(err) // shouldn't be reached since compilation comes after validation.
	}
	c.loweringState.pc += int(n)
	return v
}

func (c *Compiler) readF32() float32 {
	v := math.Float32frombits(binary.LittleEndian.Uint32(c.wasmFunctionBody[c.loweringState.pc+1:]))
	c.loweringState.pc += 4
	return v
}

func (c *Compiler) readF64() float64 {
	v := math.Float64frombits(binary.LittleEndian.Uint64(c.wasmFunctionBody[c.loweringState.pc+1:]))
	c.loweringState.pc += 8
	return v
}

// readBlockType reads the block type from the current position of the bytecode reader.
func (c *Compiler) readBlockType() *wasm.FunctionType {
	state := c.state()

	c.br.Reset(c.wasmFunctionBody[state.pc+1:])
	bt, num, err := wasm.DecodeBlockType(c.m.TypeSection, c.br, api.CoreFeaturesV2)
	if err != nil {
		panic(err) // shouldn't be reached since compilation comes after validation.
	}
	state.pc += int(num)

	return bt
}

// memArgMultiMemoryFlag is bit 6 of the memarg align LEB128, reserved by the
// multi-memory proposal to signal that a memidx LEB128 immediately follows.
// See https://webassembly.github.io/multi-memory/core/binary/instructions.html
const memArgMultiMemoryFlag = 0x40

// readMemArg decodes a memarg. Besides the full offset immediate it returns the
// displacement the load/store instruction itself should carry: for a 32-bit
// memory that is the offset, left to the addressing mode, but a 64-bit memory's
// offset can exceed any machine displacement, so memOpSetup folds it into the
// address instead and the instruction carries none.
func (c *Compiler) readMemArg() (align uint32, offset uint64, disp uint32, memIndex wasm.Index) {
	state := c.state()

	rawAlign, num, err := leb128.LoadUint32(c.wasmFunctionBody[state.pc+1:])
	if err != nil {
		panic(fmt.Errorf("read memory align: %v", err))
	}
	state.pc += int(num)

	if rawAlign&memArgMultiMemoryFlag != 0 {
		align = rawAlign &^ memArgMultiMemoryFlag
		var idx uint32
		idx, num, err = leb128.LoadUint32(c.wasmFunctionBody[state.pc+1:])
		if err != nil {
			panic(fmt.Errorf("read memory index: %v", err))
		}
		memIndex = wasm.Index(idx)
		state.pc += int(num)
	} else {
		align = rawAlign
	}

	// Always a u64, even though wasm.readMemArg only widens the field to that
	// when api.CoreFeatureMemory64 is enabled: this body has already been
	// validated, so an encoding the narrower form would have rejected cannot
	// reach here, and every encoding it accepts decodes identically -- same
	// value, same byte count -- as a u64.
	offset, num, err = leb128.LoadUint64(c.wasmFunctionBody[state.pc+1:])
	if err != nil {
		panic(fmt.Errorf("read memory offset: %v", err))
	}

	state.pc += int(num)
	if !c.memoryIsIndex64(memIndex) {
		disp = uint32(offset)
	}
	return align, offset, disp, memIndex
}

// memoryIsIndex64 reports whether the memory at memIndex has an i64 index type.
func (c *Compiler) memoryIsIndex64(memIndex wasm.Index) bool {
	return c.anyMemory64 && int(memIndex) < len(c.memoryIndex64) && c.memoryIndex64[memIndex]
}

// tableIsIndex64 reports whether the table at tableIndex has an i64 index type.
func (c *Compiler) tableIsIndex64(tableIndex uint32) bool {
	return c.anyTable64 && int(tableIndex) < len(c.tableIndex64) && c.tableIndex64[tableIndex]
}

// insertJumpToBlock inserts a jump instruction to the given block in the current block.
func (c *Compiler) insertJumpToBlock(args ssa.Values, targetBlk ssa.BasicBlock) {
	if targetBlk.ReturnBlock() {
		if c.needListener {
			c.callListenerAfter()
		}
	}

	builder := c.ssaBuilder
	jmp := builder.AllocateInstruction()
	jmp.AsJump(args, targetBlk)
	builder.InsertInstruction(jmp)
}

func (c *Compiler) insertIntegerExtend(signed bool, from, to byte) {
	state := c.state()
	builder := c.ssaBuilder
	v := state.pop()
	extend := builder.AllocateInstruction()
	if signed {
		extend.AsSExtend(v, from, to)
	} else {
		extend.AsUExtend(v, from, to)
	}
	builder.InsertInstruction(extend)
	value := extend.Return()
	state.push(value)
}

func (c *Compiler) switchTo(originalStackLen int, targetBlk ssa.BasicBlock) {
	if targetBlk.Preds() == 0 {
		c.loweringState.unreachable = true
	}

	// Now we should adjust the stack and start translating the continuation block.
	c.loweringState.values = c.loweringState.values[:originalStackLen]

	c.ssaBuilder.SetCurrentBlock(targetBlk)

	// At this point, blocks params consist only of the Wasm-level parameters,
	// (since it's added only when we are trying to resolve variable *inside* this block).
	for i := 0; i < targetBlk.Params(); i++ {
		value := targetBlk.Param(i)
		c.loweringState.push(value)
	}
}

// results returns the number of results of the current function.
func (c *Compiler) results() int {
	return len(c.wasmFunctionTyp.Results)
}

func (c *Compiler) lowerBrTable(labels []uint32, index ssa.Value) {
	state := c.state()
	builder := c.ssaBuilder

	f := state.ctrlPeekAt(int(labels[0]))
	var numArgs int
	if f.isLoop() {
		numArgs = len(f.blockType.Params)
	} else {
		numArgs = len(f.blockType.Results)
	}

	varPool := builder.VarLengthPool()
	trampolineBlockIDs := varPool.Allocate(len(labels))

	// We need trampoline blocks since depending on the target block structure, we might end up inserting moves before jumps,
	// which cannot be done with br_table. Instead, we can do such per-block moves in the trampoline blocks.
	// At the linking phase (very end of the backend), we can remove the unnecessary jumps, and therefore no runtime overhead.
	currentBlk := builder.CurrentBlock()
	for _, l := range labels {
		// Args are always on the top of the stack. Note that we should not share the args slice
		// among the jump instructions since the args are modified during passes (e.g. redundant phi elimination).
		args := c.nPeekDup(numArgs)
		targetBlk, _ := state.brTargetArgNumFor(l)
		trampoline := builder.AllocateBasicBlock()
		builder.SetCurrentBlock(trampoline)
		c.emitTryTableLeaves(int(l))
		c.insertJumpToBlock(args, targetBlk)
		trampolineBlockIDs = trampolineBlockIDs.Append(builder.VarLengthPool(), ssa.Value(trampoline.ID()))
	}
	builder.SetCurrentBlock(currentBlk)

	// If the target block has no arguments, we can just jump to the target block.
	brTable := builder.AllocateInstruction()
	brTable.AsBrTable(index, trampolineBlockIDs)
	builder.InsertInstruction(brTable)

	for _, trampolineID := range trampolineBlockIDs.View() {
		builder.Seal(builder.BasicBlock(ssa.BasicBlockID(trampolineID)))
	}
}

func (l *loweringState) brTargetArgNumFor(labelIndex uint32) (targetBlk ssa.BasicBlock, argNum int) {
	targetFrame := l.ctrlPeekAt(int(labelIndex))
	if targetFrame.isLoop() {
		targetBlk, argNum = targetFrame.blk, len(targetFrame.blockType.Params)
	} else {
		targetBlk, argNum = targetFrame.followingBlock, len(targetFrame.blockType.Results)
	}
	return
}

func (c *Compiler) callListenerBefore() {
	c.storeCallerModuleContext()

	builder := c.ssaBuilder
	beforeListeners1stElement := builder.AllocateInstruction().
		AsLoad(c.moduleCtxPtrValue,
			c.offset.BeforeListenerTrampolines1stElement.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()

	beforeListenerPtr := builder.AllocateInstruction().
		AsLoad(beforeListeners1stElement, uint32(c.wasmFunctionTypeIndex)*8 /* 8 bytes per index */, ssa.TypeI64).Insert(builder).Return()

	entry := builder.EntryBlock()
	ps := entry.Params()

	args := c.allocateVarLengthValues(ps, c.execCtxPtrValue,
		builder.AllocateInstruction().AsIconst32(c.wasmLocalFunctionIndex).Insert(builder).Return())
	for i := 2; i < ps; i++ {
		args = args.Append(builder.VarLengthPool(), entry.Param(i))
	}

	beforeSig := c.listenerSignatures[c.wasmFunctionTyp][0]
	builder.AllocateInstruction().
		AsCallIndirect(beforeListenerPtr, beforeSig, args).
		Insert(builder)
}

func (c *Compiler) callListenerAfter() {
	c.storeCallerModuleContext()

	builder := c.ssaBuilder
	afterListeners1stElement := builder.AllocateInstruction().
		AsLoad(c.moduleCtxPtrValue,
			c.offset.AfterListenerTrampolines1stElement.U32(),
			ssa.TypeI64,
		).Insert(builder).Return()

	afterListenerPtr := builder.AllocateInstruction().
		AsLoad(afterListeners1stElement,
			uint32(c.wasmFunctionTypeIndex)*8 /* 8 bytes per index */, ssa.TypeI64).
		Insert(builder).
		Return()

	afterSig := c.listenerSignatures[c.wasmFunctionTyp][1]
	args := c.allocateVarLengthValues(
		c.results()+2,
		c.execCtxPtrValue,
		builder.AllocateInstruction().AsIconst32(c.wasmLocalFunctionIndex).Insert(builder).Return(),
	)

	l := c.state()
	tail := len(l.values)
	args = args.Append(c.ssaBuilder.VarLengthPool(), l.values[tail-c.results():tail]...)
	builder.AllocateInstruction().
		AsCallIndirect(afterListenerPtr, afterSig, args).
		Insert(builder)
}

const (
	elementOrDataInstanceLenOffset = 8
	elementOrDataInstanceSize      = 24
)

// dropInstance inserts instructions to drop the element/data instance specified by the given index.
func (c *Compiler) dropDataOrElementInstance(index uint32, firstItemOffset nativeapi.Offset) {
	builder := c.ssaBuilder
	instPtr := c.dataOrElementInstanceAddr(index, firstItemOffset)

	zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()

	// Clear the instance.
	builder.AllocateInstruction().AsStore(ssa.OpcodeStore, zero, instPtr, 0).Insert(builder)
	builder.AllocateInstruction().AsStore(ssa.OpcodeStore, zero, instPtr, elementOrDataInstanceLenOffset).Insert(builder)
	builder.AllocateInstruction().AsStore(ssa.OpcodeStore, zero, instPtr, elementOrDataInstanceLenOffset+8).Insert(builder)
}

func (c *Compiler) dataOrElementInstanceAddr(index uint32, firstItemOffset nativeapi.Offset) ssa.Value {
	builder := c.ssaBuilder

	_1stItemPtr := builder.
		AllocateInstruction().
		AsLoad(c.moduleCtxPtrValue, firstItemOffset.U32(), ssa.TypeI64).
		Insert(builder).Return()

	// Each data/element instance is a slice, so we need to multiply index by 16 to get the offset of the target instance.
	index = index * elementOrDataInstanceSize
	indexExt := builder.AllocateInstruction().AsIconst64(uint64(index)).Insert(builder).Return()
	// Then, add the offset to the address of the instance.
	instPtr := builder.AllocateInstruction().AsIadd(_1stItemPtr, indexExt).Insert(builder).Return()
	return instPtr
}

func (c *Compiler) boundsCheckInDataOrElementInstance(instPtr, offsetInInstance, copySize ssa.Value, exitCode nativeapi.ExitCode) {
	builder := c.ssaBuilder
	dataInstLen := builder.AllocateInstruction().
		AsLoad(instPtr, elementOrDataInstanceLenOffset, ssa.TypeI64).
		Insert(builder).Return()
	ceil := builder.AllocateInstruction().AsIadd(offsetInInstance, copySize).Insert(builder).Return()
	cmp := builder.AllocateInstruction().
		AsIcmp(dataInstLen, ceil, ssa.IntegerCmpCondUnsignedLessThan).
		Insert(builder).
		Return()
	builder.AllocateInstruction().
		AsExitIfTrueWithCode(c.execCtxPtrValue, cmp, exitCode).
		Insert(builder)
}

// boundsCheckInTable traps unless the size entries at offset lie within the
// table, and returns a pointer to the table instance.
//
// checkOverflow adds the carry test offset+size needs when either operand can
// span the whole uint64 range, which only a 64-bit table's can.
func (c *Compiler) boundsCheckInTable(tableIndex uint32, offset, size ssa.Value, checkOverflow bool) (tableInstancePtr ssa.Value) {
	builder := c.ssaBuilder
	dstCeil := builder.AllocateInstruction().AsIadd(offset, size).Insert(builder).Return()

	// Load the table.
	tableInstancePtr = builder.AllocateInstruction().
		AsLoad(c.moduleCtxPtrValue, c.offset.TableOffset(int(tableIndex)).U32(), ssa.TypeI64).
		Insert(builder).Return()

	// Load the table's length.
	tableLen := builder.AllocateInstruction().
		AsLoad(tableInstancePtr, tableInstanceLenOffset, ssa.TypeI32).Insert(builder).Return()
	tableLenExt := builder.AllocateInstruction().AsUExtend(tableLen, 32, 64).Insert(builder).Return()

	// Compare the length and the target, and trap if out of bounds.
	checkOOB := builder.AllocateInstruction()
	checkOOB.AsIcmp(tableLenExt, dstCeil, ssa.IntegerCmpCondUnsignedLessThan)
	builder.InsertInstruction(checkOOB)
	oob := checkOOB.Return()
	if checkOverflow {
		overflowed := builder.AllocateInstruction().
			AsIcmp(dstCeil, size, ssa.IntegerCmpCondUnsignedLessThan).
			Insert(builder).Return()
		or := builder.AllocateInstruction()
		or.AsBor(oob, overflowed)
		oob = or.Insert(builder).Return()
	}
	exitIfOOB := builder.AllocateInstruction()
	exitIfOOB.AsExitIfTrueWithCode(c.execCtxPtrValue, oob, nativeapi.ExitCodeTableOutOfBounds)
	builder.InsertInstruction(exitIfOOB)
	return
}

func (c *Compiler) loadTableBaseAddr(tableInstancePtr ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	loadTableBaseAddress := builder.
		AllocateInstruction().
		AsLoad(tableInstancePtr, tableInstanceBaseAddressOffset, ssa.TypeI64).
		Insert(builder)
	return loadTableBaseAddress.Return()
}

// boundsCheckInMemory traps unless the size bytes at offset lie within memLen.
//
// checkOverflow adds the carry test that offset+size needs when either operand
// can span the whole uint64 range, which only a 64-bit memory's can: the sum
// would otherwise wrap around to a small, in-bounds-looking value. A 32-bit
// memory's operands are both zero-extended from i32, so their sum is at most
// 2^33 and the extra test would be dead weight.
func (c *Compiler) boundsCheckInMemory(memLen, offset, size ssa.Value, checkOverflow bool) {
	builder := c.ssaBuilder
	ceil := builder.AllocateInstruction().AsIadd(offset, size).Insert(builder).Return()
	cmp := builder.AllocateInstruction().
		AsIcmp(memLen, ceil, ssa.IntegerCmpCondUnsignedLessThan).
		Insert(builder).
		Return()
	if checkOverflow {
		overflowed := builder.AllocateInstruction().
			AsIcmp(ceil, size, ssa.IntegerCmpCondUnsignedLessThan).
			Insert(builder).
			Return()
		or := builder.AllocateInstruction()
		or.AsBor(cmp, overflowed)
		cmp = or.Insert(builder).Return()
	}
	builder.AllocateInstruction().
		AsExitIfTrueWithCode(c.execCtxPtrValue, cmp, nativeapi.ExitCodeMemoryOutOfBounds).
		Insert(builder)
}

// zeroExtendIndex zero-extends an i32 bulk-memory operand to the 64 bits the
// address arithmetic runs in. An operand of a 64-bit memory is already i64 and
// is returned untouched.
func (c *Compiler) zeroExtendIndex(v ssa.Value, alreadyI64 bool) ssa.Value {
	if alreadyI64 {
		return v
	}
	builder := c.ssaBuilder
	return builder.AllocateInstruction().AsUExtend(v, 32, 64).Insert(builder).Return()
}

// relaxedDotI8x16 emits i16x8.relaxed_dot_i8x16_i7x16_s: it reads both operands
// as signed i8, multiplies them lane-wise and sums each adjacent pair into a
// saturated i16 lane. Widening to i16 first lets the existing pairwise dot do
// the multiply and the horizontal add, so no backend gains an instruction.
func relaxedDotI8x16(builder ssa.Builder, v1, v2 ssa.Value) ssa.Value {
	dot := func(low bool) ssa.Value {
		x := builder.AllocateInstruction().AsWiden(v1, ssa.VecLaneI8x16, true, low).Insert(builder).Return()
		y := builder.AllocateInstruction().AsWiden(v2, ssa.VecLaneI8x16, true, low).Insert(builder).Return()
		return builder.AllocateInstruction().AsWideningPairwiseDotProductS(x, y).Insert(builder).Return()
	}
	return builder.AllocateInstruction().
		AsNarrow(dot(true), dot(false), ssa.VecLaneI32x4, true).Insert(builder).Return()
}

// lowerGCInstruction lowers one instruction of the GC proposal. Every one of them becomes a call to the shared
// trampoline (see callGC), so the work here is reading the immediates and getting the operands into and out of
// the i64s that trampoline speaks. c.loweringState.pc is at the GC opcode on entry.
func (c *Compiler) lowerGCInstruction(gcOp wasm.OpcodeGC) {
	state := c.state()

	switch gcOp {
	case wasm.OpcodeGCRefTest, wasm.OpcodeGCRefTestNull, wasm.OpcodeGCRefCast, wasm.OpcodeGCRefCastNull:
		nullable := gcOp == wasm.OpcodeGCRefTestNull || gcOp == wasm.OpcodeGCRefCastNull
		heapType := c.readI33s()
		if state.unreachable {
			return
		}
		target, ok := encodeRefTarget(heapType, nullable)
		if !ok {
			panic("BUG: validation should have rejected the heap type")
		}
		isCast := gcOp == wasm.OpcodeGCRefCast || gcOp == wasm.OpcodeGCRefCastNull
		mode := wasm.GCCheckRefTest
		if isCast {
			mode = wasm.GCCheckRefCast
		}
		// ref.cast keeps its operand: a successful cast only narrows the static type, and a failed one
		// traps inside the trampoline.
		ref := state.pop()
		result := c.callGC(mode, ref, c.gcConst(target))
		if isCast {
			state.push(ref)
		} else {
			state.push(c.gcResultI32(result))
		}

	case wasm.OpcodeGCBrOnCast, wasm.OpcodeGCBrOnCastFail:
		c.lowerBrOnCast(gcOp)

	case wasm.OpcodeGCAnyConvertExtern, wasm.OpcodeGCExternConvertAny:
		// Both are bijections on the same underlying value, and every reference is the same opaque word
		// here, so there is nothing to emit.

	case wasm.OpcodeGCStructNew, wasm.OpcodeGCStructNewDefault:
		typeIndex := c.readI32u()
		if state.unreachable {
			return
		}
		t := &c.m.TypeSection[typeIndex]
		var values []ssa.Value
		if gcOp == wasm.OpcodeGCStructNew {
			values = make([]ssa.Value, len(t.Fields))
			for i := len(values) - 1; i >= 0; i-- {
				values[i] = state.pop()
			}
		}
		// Allocating defaulted and then storing is how the variadic forms avoid needing a scratch area
		// across the trampoline. RunGC's own struct.new takes its fields from one, which only the
		// interpreter can hand it.
		ref := c.callGC(wasm.GCStructNewDefault, c.gcConst(uint64(typeIndex)))
		for i, v := range values {
			c.gcStoreWords(ref, wasm.GCStructSet, c.gcConst(uint64(t.FieldSlots[i])), v)
		}
		state.push(ref)

	case wasm.OpcodeGCStructGet, wasm.OpcodeGCStructGetS, wasm.OpcodeGCStructGetU:
		typeIndex, fieldIndex := c.readI32u(), c.readI32u()
		if state.unreachable {
			return
		}
		signed := uint64(0)
		if gcOp == wasm.OpcodeGCStructGetS {
			signed = 1
		}
		t := &c.m.TypeSection[typeIndex]
		slot := c.gcConst(uint64(t.FieldSlots[fieldIndex]))
		ref := state.pop()
		vt := unpackedFieldType(t, fieldIndex)
		if vt == wasm.ValueTypeV128 {
			lo := c.callGC(wasm.GCStructGet, ref, slot, c.gcConst(signed))
			hi := c.callGC(wasm.GCStructGet, ref, c.gcConst(uint64(t.FieldSlots[fieldIndex]+1)), c.gcConst(signed))
			state.push(c.gcV128From(lo, hi))
			return
		}
		state.push(c.gcFromI64(c.callGC(wasm.GCStructGet, ref, slot, c.gcConst(signed)), vt))

	case wasm.OpcodeGCStructSet:
		typeIndex, fieldIndex := c.readI32u(), c.readI32u()
		if state.unreachable {
			return
		}
		v, ref := state.pop(), state.pop()
		slot := uint64(c.m.TypeSection[typeIndex].FieldSlots[fieldIndex])
		c.gcStoreWords(ref, wasm.GCStructSet, c.gcConst(slot), v)

	case wasm.OpcodeGCArrayNew, wasm.OpcodeGCArrayNewDefault:
		typeIndex := c.readI32u()
		if state.unreachable {
			return
		}
		length := state.pop()
		if gcOp == wasm.OpcodeGCArrayNewDefault {
			state.push(c.callGC(wasm.GCArrayNewDefault, c.gcConst(uint64(typeIndex)), c.gcToI64(length)))
			return
		}
		lo, hi := c.gcSplitWords(state.pop())
		state.push(c.callGC(wasm.GCArrayNew, c.gcConst(uint64(typeIndex)), c.gcToI64(length), lo, hi))

	case wasm.OpcodeGCArrayNewFixed:
		typeIndex, count := c.readI32u(), c.readI32u()
		if state.unreachable {
			return
		}
		values := make([]ssa.Value, count)
		for i := int(count) - 1; i >= 0; i-- {
			values[i] = state.pop()
		}
		ref := c.callGC(wasm.GCArrayNewDefault, c.gcConst(uint64(typeIndex)), c.gcConst(uint64(count)))
		for i, v := range values {
			c.gcStoreElement(ref, c.gcConst(uint64(i)), v)
		}
		state.push(ref)

	case wasm.OpcodeGCArrayNewData, wasm.OpcodeGCArrayNewElem:
		typeIndex, segment := c.readI32u(), c.readI32u()
		if state.unreachable {
			return
		}
		mode := uint64(wasm.GCArrayNewData)
		if gcOp == wasm.OpcodeGCArrayNewElem {
			mode = wasm.GCArrayNewElem
		}
		length, offset := state.pop(), state.pop()
		state.push(c.callGC(mode, c.gcConst(uint64(typeIndex)), c.gcConst(uint64(segment)),
			c.gcToI64(offset), c.gcToI64(length)))

	case wasm.OpcodeGCArrayGet, wasm.OpcodeGCArrayGetS, wasm.OpcodeGCArrayGetU:
		typeIndex := c.readI32u()
		if state.unreachable {
			return
		}
		signed := uint64(0)
		if gcOp == wasm.OpcodeGCArrayGetS {
			signed = 1
		}
		index, ref := state.pop(), state.pop()
		idx := c.gcToI64(index)
		vt := unpackedFieldType(&c.m.TypeSection[typeIndex], 0)
		if vt == wasm.ValueTypeV128 {
			lo := c.callGC(wasm.GCArrayGet, ref, idx, c.gcConst(signed))
			hi := c.callGC(wasm.GCArrayGet, ref, idx, c.gcConst(signed|2))
			state.push(c.gcV128From(lo, hi))
			return
		}
		state.push(c.gcFromI64(c.callGC(wasm.GCArrayGet, ref, idx, c.gcConst(signed)), vt))

	case wasm.OpcodeGCArraySet:
		c.readI32u()
		if state.unreachable {
			return
		}
		v, index, ref := state.pop(), state.pop(), state.pop()
		c.gcStoreElement(ref, c.gcToI64(index), v)

	case wasm.OpcodeGCArrayLen:
		if state.unreachable {
			return
		}
		state.push(c.gcResultI32(c.callGC(wasm.GCArrayLen, state.pop())))

	case wasm.OpcodeGCArrayFill:
		c.readI32u()
		if state.unreachable {
			return
		}
		length, v, index, ref := state.pop(), state.pop(), state.pop(), state.pop()
		lo, hi := c.gcSplitWords(v)
		c.callGC(wasm.GCArrayFill, ref, c.gcToI64(index), lo, c.gcToI64(length), hi)

	case wasm.OpcodeGCArrayCopy:
		c.readI32u()
		c.readI32u()
		if state.unreachable {
			return
		}
		length, srcIndex, src, dstIndex, dst := state.pop(), state.pop(), state.pop(), state.pop(), state.pop()
		c.callGC(wasm.GCArrayCopy, dst, c.gcToI64(dstIndex), src, c.gcToI64(srcIndex), c.gcToI64(length))

	case wasm.OpcodeGCArrayInitData, wasm.OpcodeGCArrayInitElem:
		c.readI32u()
		segment := c.readI32u()
		if state.unreachable {
			return
		}
		mode := uint64(wasm.GCArrayInitData)
		if gcOp == wasm.OpcodeGCArrayInitElem {
			mode = wasm.GCArrayInitElem
		}
		length, offset, dstIndex, ref := state.pop(), state.pop(), state.pop(), state.pop()
		c.callGC(mode, ref, c.gcToI64(dstIndex), c.gcConst(uint64(segment)), c.gcToI64(offset), c.gcToI64(length))

	case wasm.OpcodeGCRefI31:
		if state.unreachable {
			return
		}
		state.push(c.callGC(wasm.GCRefI31, c.gcToI64(state.pop())))

	case wasm.OpcodeGCI31GetS, wasm.OpcodeGCI31GetU:
		if state.unreachable {
			return
		}
		mode := uint64(wasm.GCI31GetS)
		if gcOp == wasm.OpcodeGCI31GetU {
			mode = wasm.GCI31GetU
		}
		state.push(c.gcResultI32(c.callGC(mode, state.pop())))

	default:
		panic("TODO: unsupported GC instruction: " + wasm.GCInstructionName(gcOp))
	}
}

// lowerBrOnCast lowers br_on_cast and br_on_cast_fail as a peeking test plus a conditional branch, exactly as
// the interpreter does: the reference stays on the stack for whichever path is taken.
func (c *Compiler) lowerBrOnCast(gcOp wasm.OpcodeGC) {
	builder := c.ssaBuilder
	state := c.state()

	flags := c.wasmFunctionBody[c.loweringState.pc+1]
	c.loweringState.pc++
	labelIndex := c.readI32u()
	// The first heap type is what the operand already is, which only validation cares about.
	c.readI33s()
	castHeapType := c.readI33s()
	if state.unreachable {
		return
	}
	target, ok := encodeRefTarget(castHeapType, flags&2 != 0)
	if !ok {
		panic("BUG: validation should have rejected the heap type")
	}

	ref := state.pop()
	matched := c.callGC(wasm.GCCheckRefTest, ref, c.gcConst(target))
	state.push(ref)

	// br_on_cast branches when the test succeeds, br_on_cast_fail when it fails.
	cond := ssa.IntegerCmpCondNotEqual
	if gcOp == wasm.OpcodeGCBrOnCastFail {
		cond = ssa.IntegerCmpCondEqual
	}
	zero := builder.AllocateInstruction().AsIconst64(0).Insert(builder).Return()
	takeBranch := builder.AllocateInstruction().AsIcmp(matched, zero, cond).Insert(builder).Return()

	targetBlk, argNum := state.brTargetArgNumFor(labelIndex)
	args := c.nPeekDup(argNum)
	var sealTargetBlk bool

	if c.branchExitsTryTable(int(labelIndex)) {
		current := builder.CurrentBlock()
		trampolineBlk := builder.AllocateBasicBlock()
		builder.SetCurrentBlock(trampolineBlk)
		c.emitTryTableLeaves(int(labelIndex))
		c.insertJumpToBlock(args, targetBlk)
		builder.SetCurrentBlock(current)
		targetBlk = trampolineBlk
		sealTargetBlk = true
		args = ssa.ValuesNil
	}

	if c.needListener && targetBlk.ReturnBlock() {
		current := builder.CurrentBlock()
		targetBlk = builder.AllocateBasicBlock()
		builder.SetCurrentBlock(targetBlk)
		sealTargetBlk = true
		c.callListenerAfter()
		instr := builder.AllocateInstruction()
		instr.AsReturn(args)
		builder.InsertInstruction(instr)
		args = ssa.ValuesNil
		builder.SetCurrentBlock(current)
	}

	brnz := builder.AllocateInstruction()
	brnz.AsBrnz(takeBranch, args, targetBlk)
	builder.InsertInstruction(brnz)
	if sealTargetBlk {
		builder.Seal(targetBlk)
	}

	elseBlk := builder.AllocateBasicBlock()
	c.insertJumpToBlock(ssa.ValuesNil, elseBlk)
	builder.Seal(elseBlk)
	builder.SetCurrentBlock(elseBlk)
}

// unpackedFieldType is the value type a struct field or array element appears as on the operand stack.
func unpackedFieldType(t *wasm.FunctionType, fieldIndex uint32) wasm.ValueType {
	st := t.Fields[fieldIndex].Type
	switch st {
	case wasm.ValueTypeI8, wasm.ValueTypeI16:
		return wasm.ValueTypeI32
	}
	return st
}

func (c *Compiler) gcConst(v uint64) ssa.Value {
	return c.ssaBuilder.AllocateInstruction().AsIconst64(v).Insert(c.ssaBuilder).Return()
}

// gcToI64 widens an operand to the i64 the GC trampoline speaks, bit-for-bit.
func (c *Compiler) gcToI64(v ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	switch v.Type() {
	case ssa.TypeI64:
		return v
	case ssa.TypeI32:
		return builder.AllocateInstruction().AsUExtend(v, 32, 64).Insert(builder).Return()
	case ssa.TypeF32:
		bits := builder.AllocateInstruction().AsBitcast(v, ssa.TypeI32).Insert(builder).Return()
		return builder.AllocateInstruction().AsUExtend(bits, 32, 64).Insert(builder).Return()
	case ssa.TypeF64:
		return builder.AllocateInstruction().AsBitcast(v, ssa.TypeI64).Insert(builder).Return()
	}
	panic("BUG: a vector must go through gcSplitWords, not gcToI64")
}

// gcFromI64 narrows a trampoline result back to the wasm type the instruction pushes.
func (c *Compiler) gcFromI64(v ssa.Value, vt wasm.ValueType) ssa.Value {
	builder := c.ssaBuilder
	switch vt {
	case wasm.ValueTypeI32:
		return c.gcResultI32(v)
	case wasm.ValueTypeF32:
		bits := c.gcResultI32(v)
		return builder.AllocateInstruction().AsBitcast(bits, ssa.TypeF32).Insert(builder).Return()
	case wasm.ValueTypeF64:
		return builder.AllocateInstruction().AsBitcast(v, ssa.TypeF64).Insert(builder).Return()
	}
	// i64 and every reference type are already the right width; a vector is reassembled by gcV128From.
	return v
}

func (c *Compiler) gcResultI32(v ssa.Value) ssa.Value {
	return c.ssaBuilder.AllocateInstruction().AsIreduce(v, ssa.TypeI32).Insert(c.ssaBuilder).Return()
}

// gcSplitWords takes a value apart into the words RunGC moves it in: two for a vector, one for everything
// else, whose high word is zero.
func (c *Compiler) gcSplitWords(v ssa.Value) (lo, hi ssa.Value) {
	if v.Type() != ssa.TypeV128 {
		return c.gcToI64(v), c.gcConst(0)
	}
	builder := c.ssaBuilder
	lo = builder.AllocateInstruction().
		AsExtractlane(v, 0, ssa.VecLaneI64x2, false).Insert(builder).Return()
	hi = builder.AllocateInstruction().
		AsExtractlane(v, 1, ssa.VecLaneI64x2, false).Insert(builder).Return()
	return lo, hi
}

// gcV128From reassembles a vector from the two words RunGC returned it in.
func (c *Compiler) gcV128From(lo, hi ssa.Value) ssa.Value {
	builder := c.ssaBuilder
	zero := builder.AllocateInstruction().AsVconst(0, 0).Insert(builder).Return()
	withLo := builder.AllocateInstruction().
		AsInsertlane(zero, lo, 0, ssa.VecLaneI64x2).Insert(builder).Return()
	return builder.AllocateInstruction().
		AsInsertlane(withLo, hi, 1, ssa.VecLaneI64x2).Insert(builder).Return()
}

// gcStoreWords writes a value into a struct at the word slot names, taking two calls for a vector.
func (c *Compiler) gcStoreWords(ref ssa.Value, mode uint64, slot ssa.Value, v ssa.Value) {
	if v.Type() != ssa.TypeV128 {
		c.callGC(mode, ref, slot, c.gcToI64(v))
		return
	}
	lo, hi := c.gcSplitWords(v)
	c.callGC(mode, ref, slot, lo)
	next := c.ssaBuilder.AllocateInstruction().
		AsIadd(slot, c.gcConst(1)).Insert(c.ssaBuilder).Return()
	c.callGC(mode, ref, next, hi)
}

// gcStoreElement writes a value into an array element, taking two calls for a vector: the second selects the
// element's high word through the flags operand.
func (c *Compiler) gcStoreElement(ref, index, v ssa.Value) {
	if v.Type() != ssa.TypeV128 {
		c.callGC(wasm.GCArraySet, ref, index, c.gcToI64(v))
		return
	}
	lo, hi := c.gcSplitWords(v)
	c.callGC(wasm.GCArraySet, ref, index, lo)
	c.callGC(wasm.GCArraySet, ref, index, hi, c.gcConst(2))
}

// materializeGCRoots spills every live wasm value -- the locals and the operand stack -- into the region the
// call engine reserves at the bottom of the wasm stack, so a collection finds an exact root set in its ordinary
// conservative scan rather than trying to work out where the backend put them.
//
// Only i64-shaped values are spilled, which is every reference plus some integers that are not; the collector
// is conservative about what a word means, so including them costs nothing but a failed lookup. What it cannot
// do is *miss* one, and inferring liveness from the machine state is exactly what does not survive a change of
// register allocator: arm64 has enough registers to keep a loop's reference in one across the safepoint call,
// where it appears in neither the wasm stack nor the saved registers.
//
// The region is addressed off the stack limit, which compiled code already holds at a fixed offset in the
// execution context. Everything below that limit by more than the go-call margin is unreachable to generated
// code and to the trampoline, so a spill stays put until the collector has read it. Both are compile-time
// constants, so this is one subtract and n stores -- inside the park block, which is entered only when a
// collection is actually waiting.
func (c *Compiler) materializeGCRoots(builder ssa.Builder) {
	var vals []ssa.Value
	push := func(v ssa.Value) {
		if v.Valid() && v.Type() == ssa.TypeI64 {
			vals = append(vals, v)
		}
	}
	// Only this function's locals: wasmLocalToVariable is reused across functions and keeps whatever the
	// longest one before it needed, so its length is not the local count.
	locals := len(c.wasmFunctionTyp.Params) + len(c.wasmFunctionLocalTypes)
	for i := 0; i < locals && i < len(c.wasmLocalToVariable); i++ {
		push(builder.MustFindValue(c.wasmLocalToVariable[i]))
	}
	for _, v := range c.state().values {
		push(v)
	}
	if len(vals) == 0 {
		return
	}
	if len(vals) > c.maxGCRoots {
		c.maxGCRoots = len(vals)
	}

	limit := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue, nativeapi.ExecutionContextOffsetStackBottomPtr.U32(), ssa.TypeI64).
		Insert(builder).Return()
	// The count sits one margin below the limit and the spilled words below that, so this safepoint's n
	// words start n+1 words down. The scan reads the count first, which is what keeps a shorter safepoint
	// from retaining what a longer one before it left behind.
	back := builder.AllocateInstruction().
		AsIconst64(uint64(backend.StackBoundsCheckMarginBytes + 8*(len(vals)+1))).Insert(builder).Return()
	base := builder.AllocateInstruction().AsIsub(limit, back).Insert(builder).Return()
	for i, v := range vals {
		builder.AllocateInstruction().
			AsStore(ssa.OpcodeStore, v, base, uint32(i*8)).Insert(builder)
	}
	count := builder.AllocateInstruction().AsIconst64(uint64(len(vals))).Insert(builder).Return()
	builder.AllocateInstruction().
		AsStore(ssa.OpcodeStore, count, base, uint32(8*len(vals))).Insert(builder)
}

// emitGCSafepoint emits the loop-header poll of the collector's pause flag. The flag is a word in the
// execution context that the collector writes, so the common path is one load and one not-taken branch.
func (c *Compiler) emitGCSafepoint(builder ssa.Builder) {
	pause := builder.AllocateInstruction().
		AsLoad(c.execCtxPtrValue, nativeapi.ExecutionContextOffsetGCPause.U32(), ssa.TypeI32).
		Insert(builder).Return()
	zero := builder.AllocateInstruction().AsIconst32(0).Insert(builder).Return()
	paused := builder.AllocateInstruction().
		AsIcmp(pause, zero, ssa.IntegerCmpCondNotEqual).Insert(builder).Return()

	parkBlk := builder.AllocateBasicBlock()
	afterBlk := builder.AllocateBasicBlock()
	builder.AllocateInstruction().AsBrnz(paused, ssa.ValuesNil, parkBlk).Insert(builder)
	builder.AllocateInstruction().AsJump(ssa.ValuesNil, afterBlk).Insert(builder)

	builder.SetCurrentBlock(parkBlk)
	c.materializeGCRoots(builder)
	c.callGC(wasm.GCSafepoint)
	builder.AllocateInstruction().AsJump(ssa.ValuesNil, afterBlk).Insert(builder)
	builder.Seal(parkBlk)

	builder.SetCurrentBlock(afterBlk)
	builder.Seal(afterBlk)
}
