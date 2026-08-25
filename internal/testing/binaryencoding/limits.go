package binaryencoding

import (
	"github.com/samyfodil/wazy/internal/leb128"
)

// EncodeLimitsType returns the `limitsType` (min, max) encoded in WebAssembly 1.0 (20191205) Binary Format.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#limits%E2%91%A6
//
// Extended in threads proposal: https://webassembly.github.io/threads/core/binary/types.html#limits
// Extended in memory64 proposal: https://webassembly.github.io/memory64/core/binary/types.html#limits
func EncodeLimitsType(min uint64, max *uint64, shared, index64 bool) []byte {
	var flag uint32
	if max != nil {
		flag |= 0x01
	}
	if shared {
		flag |= 0x02
	}
	// A 32-bit index type keeps its u32 encoding of min and max; only the
	// 64-bit one widens them to u64.
	encode := func(v uint64) []byte { return leb128.EncodeUint32(uint32(v)) }
	if index64 {
		flag |= 0x04
		encode = leb128.EncodeUint64
	}

	ret := append(leb128.EncodeUint32(flag), encode(min)...)
	if max != nil {
		ret = append(ret, encode(*max)...)
	}
	return ret
}
