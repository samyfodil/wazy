package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeExport(buf []byte, offset int, arena *stringArena, ret *wasm.Export) (int, error) {
	var err error
	if ret.Name, offset, err = decodeUTF8(buf, offset, arena, "export name"); err != nil {
		return offset, err
	}

	b, offset, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("error decoding export kind: %w", err)
	}

	ret.Type = b
	switch ret.Type {
	case wasm.ExternTypeFunc, wasm.ExternTypeTable, wasm.ExternTypeMemory, wasm.ExternTypeGlobal, wasm.ExternTypeTag:
		var n uint64
		if ret.Index, n, err = leb128.LoadUint32(buf[offset:]); err != nil {
			return offset, fmt.Errorf("error decoding export index: %w", err)
		}
		offset += int(n)
	default:
		return offset, fmt.Errorf("%w: invalid byte for exportdesc: %#x", ErrInvalidByte, b)
	}
	return offset, nil
}
