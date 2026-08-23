package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
)

// Limit flag bits. Bit 0 selects the two-field (min and max) form, bit 1 marks
// the limits shared (threads proposal) and bit 2 marks the index type i64
// instead of i32 (memory64 proposal).
//
// See https://webassembly.github.io/threads/core/binary/types.html#limits and
// https://webassembly.github.io/memory64/core/binary/types.html#limits
const (
	limitsFlagHasMax  = 0x01
	limitsFlagShared  = 0x02
	limitsFlagIndex64 = 0x04
	limitsFlagsAll    = limitsFlagHasMax | limitsFlagShared | limitsFlagIndex64
)

// decodeLimitsType returns the `limitsType` (min, max) decoded with the WebAssembly 1.0 (20191205) Binary Format.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#limits%E2%91%A6
//
// Extended in threads proposal: https://webassembly.github.io/threads/core/binary/types.html#limits
// Extended in memory64 proposal: https://webassembly.github.io/memory64/core/binary/types.html#limits
func decodeLimitsType(buf []byte, offset int, enabledFeatures api.CoreFeatures) (min uint64, max *uint64, shared, index64 bool, newOffset int, err error) {
	flag, offset, err := readByte(buf, offset)
	if err != nil {
		return 0, nil, false, false, offset, fmt.Errorf("read leading byte: %v", err)
	}

	if flag&^limitsFlagsAll != 0 {
		return 0, nil, false, false, offset, fmt.Errorf("%v for limits: %#x not in (0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07)", ErrInvalidByte, flag)
	}

	index64 = flag&limitsFlagIndex64 != 0
	if index64 {
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureMemory64); err != nil {
			return 0, nil, false, false, offset, fmt.Errorf("i64 index type in limits: %w", err)
		}
	}

	// A 32-bit index type keeps its u32 LEB128 bounds, so an encoding that
	// overflows 32 bits stays malformed rather than silently becoming a huge
	// (and later rejected) limit.
	load := func(b []byte) (uint64, uint64, error) {
		if index64 {
			return leb128.LoadUint64(b)
		}
		v, n, err := leb128.LoadUint32(b)
		return uint64(v), n, err
	}

	var n uint64
	if min, n, err = load(buf[offset:]); err != nil {
		return 0, nil, false, false, offset, fmt.Errorf("read min of limit: %v", err)
	}
	offset += int(n)

	if flag&limitsFlagHasMax != 0 {
		var m uint64
		if m, n, err = load(buf[offset:]); err != nil {
			return 0, nil, false, false, offset, fmt.Errorf("read max of limit: %v", err)
		}
		offset += int(n)
		max = &m
	}

	shared = flag&limitsFlagShared != 0

	return min, max, shared, index64, offset, nil
}
