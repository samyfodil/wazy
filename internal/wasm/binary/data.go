package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// dataSegmentPrefix represents three types of data segments.
//
// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#data-section
type dataSegmentPrefix = uint32

const (
	// dataSegmentPrefixActive is the prefix for the version 1.0 compatible data segment, which is classified as "active" in 2.0.
	dataSegmentPrefixActive dataSegmentPrefix = 0x0
	// dataSegmentPrefixPassive prefixes the "passive" data segment as in version 2.0 specification.
	dataSegmentPrefixPassive dataSegmentPrefix = 0x1
	// dataSegmentPrefixActiveWithMemoryIndex is the active prefix with memory index encoded which is defined for futur use as of 2.0.
	dataSegmentPrefixActiveWithMemoryIndex dataSegmentPrefix = 0x2
)

func decodeDataSegment(buf []byte, offset int, enabledFeatures api.CoreFeatures, ret *wasm.DataSegment) (newOffset int, err error) {
	dataSegmentPrefx, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		err = fmt.Errorf("read data segment prefix: %w", err)
		return offset, err
	}
	offset += int(n)

	if dataSegmentPrefx != dataSegmentPrefixActive {
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureBulkMemoryOperations); err != nil {
			err = fmt.Errorf("non-zero prefix for data segment is invalid as %w", err)
			return offset, err
		}
	}

	switch dataSegmentPrefx {
	case dataSegmentPrefixActive,
		dataSegmentPrefixActiveWithMemoryIndex:
		// Active data segment as in
		// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#data-section
		if dataSegmentPrefx == 0x2 {
			d, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return offset, fmt.Errorf("read memory index: %v", err)
			}
			offset += int(n)
			if d != 0 {
				return offset, fmt.Errorf("memory index must be zero but was %d", d)
			}
		}

		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &ret.OffsetExpression)
		if err != nil {
			return offset, fmt.Errorf("read offset expression: %v", err)
		}
	case dataSegmentPrefixPassive:
		// Passive data segment doesn't need const expr nor memory index encoded.
		// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#data-section
		ret.Passive = true
	default:
		err = fmt.Errorf("invalid data segment prefix: 0x%x", dataSegmentPrefx)
		return offset, err
	}

	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		err = fmt.Errorf("get the size of vector: %v", err)
		return offset, err
	}
	offset += int(n)

	data, newOffset, err := readBytes(buf, offset, int(vs))
	if err != nil {
		return offset, fmt.Errorf("read bytes for init: %v", err)
	}
	ret.Init = make([]byte, vs)
	copy(ret.Init, data)
	return newOffset, nil
}
