package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// decodeLimitsType returns the `limitsType` (min, max) decoded with the WebAssembly 1.0 (20191205) Binary Format.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#limits%E2%91%A6
//
// Extended in threads proposal: https://webassembly.github.io/threads/core/binary/types.html#limits
func decodeLimitsType(buf []byte, offset int) (min uint32, max *uint32, shared bool, newOffset int, err error) {
	flag, offset, err := readByte(buf, offset)
	if err != nil {
		return 0, nil, false, offset, fmt.Errorf("read leading byte: %v", err)
	}

	switch flag {
	case 0x00, 0x02:
		var n uint64
		min, n, err = leb128.LoadUint32(buf[offset:])
		if err != nil {
			return 0, nil, false, offset, fmt.Errorf("read min of limit: %v", err)
		}
		offset += int(n)
	case 0x01, 0x03:
		var n uint64
		min, n, err = leb128.LoadUint32(buf[offset:])
		if err != nil {
			return 0, nil, false, offset, fmt.Errorf("read min of limit: %v", err)
		}
		offset += int(n)

		var m uint32
		if m, n, err = leb128.LoadUint32(buf[offset:]); err != nil {
			return 0, nil, false, offset, fmt.Errorf("read max of limit: %v", err)
		}
		offset += int(n)
		max = &m
	default:
		return 0, nil, false, offset, fmt.Errorf("%v for limits: %#x not in (0x00, 0x01, 0x02, 0x03)", ErrInvalidByte, flag)
	}

	shared = flag == 0x02 || flag == 0x03

	return min, max, shared, offset, nil
}
