package binaryencoding

import (
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func encodeDataSegment(d *wasm.DataSegment) (ret []byte) {
	switch {
	case d.Passive:
		ret = append(ret, leb128.EncodeInt32(1)...)
	case d.MemoryIndex != 0:
		ret = append(ret, leb128.EncodeInt32(2)...) // active segment with an explicit memory index
		ret = append(ret, leb128.EncodeUint32(d.MemoryIndex)...)
		ret = append(ret, encodeConstantExpression(d.OffsetExpression)...)
	default:
		ret = append(ret, leb128.EncodeInt32(0)...) // active segment
		ret = append(ret, encodeConstantExpression(d.OffsetExpression)...)
	}
	ret = append(ret, leb128.EncodeUint32(uint32(len(d.Init)))...)
	ret = append(ret, d.Init...)
	return
}
