package binary

import (
	"fmt"
	"io"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/ieee754"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeConstantExpression(buf []byte, offset int, enabledFeatures api.CoreFeatures, ret *wasm.ConstantExpression) (int, error) {
	startOffset := offset
	for {
		opcode, o, err := readByte(buf, offset)
		if err != nil {
			return offset, fmt.Errorf("read const expression opcode: %v", err)
		}
		offset = o
		switch opcode {
		case wasm.OpcodeI32Const:
			// Treat constants as signed as their interpretation is not yet known per /RATIONALE.md
			var n uint64
			_, n, err = leb128.LoadInt32(buf[offset:])
			offset += int(n)
		case wasm.OpcodeI32Add, wasm.OpcodeI32Sub, wasm.OpcodeI32Mul:
			// No immediate to read.
			if !enabledFeatures.IsEnabled(api.CoreFeatureExtendedConst) {
				return offset, fmt.Errorf("%v is not supported in a constant expression as feature \"extended-const\" is disabled", wasm.InstructionName(opcode))
			}
		case wasm.OpcodeI64Const:
			// Treat constants as signed as their interpretation is not yet known per /RATIONALE.md
			var n uint64
			_, n, err = leb128.LoadInt64(buf[offset:])
			offset += int(n)
		case wasm.OpcodeI64Add, wasm.OpcodeI64Sub, wasm.OpcodeI64Mul:
			// No immediate to read.
			if !enabledFeatures.IsEnabled(api.CoreFeatureExtendedConst) {
				return offset, fmt.Errorf("%v is not supported in a constant expression as feature \"extended-const\" is disabled", wasm.InstructionName(opcode))
			}
		case wasm.OpcodeF32Const:
			var tmp [4]byte
			data, o, e := readBytes(buf, offset, 4)
			if e != nil {
				return offset, fmt.Errorf("read f32 constant: %v", e)
			}
			copy(tmp[:], data)
			offset = o
			_, err = ieee754.DecodeFloat32(tmp[:])
		case wasm.OpcodeF64Const:
			var tmp [8]byte
			data, o, e := readBytes(buf, offset, 8)
			if e != nil {
				return offset, fmt.Errorf("read f64 constant: %v", e)
			}
			copy(tmp[:], data)
			offset = o
			_, err = ieee754.DecodeFloat64(tmp[:])
		case wasm.OpcodeGlobalGet:
			var n uint64
			_, n, err = leb128.LoadUint32(buf[offset:])
			offset += int(n)
		case wasm.OpcodeRefNull:
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureBulkMemoryOperations); err != nil {
				return offset, fmt.Errorf("ref.null is not supported as %w", err)
			}
			b, o, err := readByte(buf, offset)
			reftype := wasm.ValueType(b)
			if err != nil {
				return offset, fmt.Errorf("read reference type for ref.null: %w", err)
			}
			switch reftype {
			case wasm.RefTypeFuncref, wasm.RefTypeExternref, wasm.ValueTypeExnref:
				// Valid abstract heap type.
				offset = o
			default:
				// Could be a concrete type index; re-decode the same byte(s) (still at `offset`, since we
				// haven't advanced past it) as LEB128, rather than the reader-based "unread the byte" dance.
				var n uint64
				_, n, err = leb128.LoadUint32(buf[offset:])
				if err != nil {
					return offset, fmt.Errorf("invalid type for ref.null: 0x%x", reftype)
				}
				offset += int(n)
			}
		case wasm.OpcodeRefFunc:
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureBulkMemoryOperations); err != nil {
				return offset, fmt.Errorf("ref.func is not supported as %w", err)
			}
			// Parsing index.
			var n uint64
			_, n, err = leb128.LoadUint32(buf[offset:])
			offset += int(n)
		case wasm.OpcodeVecPrefix:
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureSIMD); err != nil {
				return offset, fmt.Errorf("vector instructions are not supported as %w", err)
			}
			vecOpcode, o, err := readByte(buf, offset)
			if err != nil {
				return offset, fmt.Errorf("read vector instruction opcode suffix: %w", err)
			}
			offset = o

			if vecOpcode != wasm.OpcodeVecV128Const {
				return offset, fmt.Errorf("invalid vector opcode for const expression: %#x", vecOpcode)
			}

			// Mirrors the original r.Read(make([]byte, 16)) semantics exactly (not io.ReadFull): copy as many
			// bytes as are available, up to 16, and only error with "needs 16 bytes but was N" when a partial
			// (but non-zero) read occurs; io.EOF only when nothing at all remains.
			avail := len(buf) - offset
			if avail <= 0 {
				return offset, fmt.Errorf("read vector const instruction immediates: %w", io.EOF)
			}
			n := avail
			if n > 16 {
				n = 16
			}
			offset += n
			if n != 16 {
				return offset, fmt.Errorf("read vector const instruction immediates: needs 16 bytes but was %d bytes", n)
			}
		case wasm.OpcodeEnd:
			data := make([]byte, offset-startOffset)
			copy(data, buf[startOffset:offset])
			ret.Data = data
			return offset, nil
		default:
			return offset, fmt.Errorf("%v for const expression op code: %#x", ErrInvalidByte, opcode)
		}

		if err != nil {
			return offset, fmt.Errorf("read value: %v", err)
		}
	}
}
