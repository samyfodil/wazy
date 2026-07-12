package leb128

import (
	"errors"
	"io"
)

const (
	maxVarintLen32 = 5
	maxVarintLen33 = maxVarintLen32
	maxVarintLen64 = 10

	int33Mask  int64 = 1 << 7
	int33Mask2       = ^int33Mask
	int33Mask3       = 1 << 6
	int33Mask4       = 8589934591 // 2^33-1
	int33Mask5       = 1 << 32
	int33Mask6       = int33Mask4 + 1 // 2^33

	int64Mask3 = 1 << 6
	int64Mask4 = ^0
)

var (
	errOverflow32 = errors.New("overflows a 32-bit integer")
	errOverflow33 = errors.New("overflows a 33-bit integer")
	errOverflow64 = errors.New("overflows a 64-bit integer")
)

// EncodeInt32 encodes the signed value into a buffer in LEB128 format
//
// See https://en.wikipedia.org/wiki/LEB128#Encode_signed_integer
func EncodeInt32(value int32) []byte {
	return EncodeInt64(int64(value))
}

// EncodeInt64 encodes the signed value into a buffer in LEB128 format
//
// See https://en.wikipedia.org/wiki/LEB128#Encode_signed_integer
func EncodeInt64(value int64) (buf []byte) {
	for {
		// Take 7 remaining low-order bits from the value into b.
		b := uint8(value & 0x7f)
		// Extract the sign bit.
		s := uint8(value & 0x40)
		value >>= 7

		// The encoding unsigned numbers is simpler as it only needs to check if the value is non-zero to tell if there
		// are more bits to encode. Signed is a little more complicated as you have to double-check the sign bit.
		// If either case, set the high-order bit to tell the reader there are more bytes in this int.
		if (value != -1 || s == 0) && (value != 0 || s != 0) {
			b |= 0x80
		}

		// Append b into the buffer
		buf = append(buf, b)
		if b&0x80 == 0 {
			break
		}
	}
	return buf
}

// EncodeUint32 encodes the value into a buffer in LEB128 format
//
// See https://en.wikipedia.org/wiki/LEB128#Encode_unsigned_integer
func EncodeUint32(value uint32) []byte {
	return EncodeUint64(uint64(value))
}

// EncodeUint64 encodes the value into a buffer in LEB128 format
//
// See https://en.wikipedia.org/wiki/LEB128#Encode_unsigned_integer
func EncodeUint64(value uint64) (buf []byte) {
	// This is effectively a do/while loop where we take 7 bits of the value and encode them until it is zero.
	for {
		// Take 7 remaining low-order bits from the value into b.
		b := uint8(value & 0x7f)
		value = value >> 7

		// If there are remaining bits, the value won't be zero: Set the high-
		// order bit to tell the reader there are more bytes in this uint.
		if value != 0 {
			b |= 0x80
		}

		// Append b into the buffer
		buf = append(buf, b)
		if b&0x80 == 0 {
			return buf
		}
	}
}

// DecodeUint32 reads a LEB128 encoded uint32 one byte at a time via r.
//
// This has the identical decode logic to LoadUint32, duplicated instead of shared through a closure so that
// neither path pays for an indirect call per byte.
func DecodeUint32(r io.ByteReader) (ret uint32, bytesRead uint64, err error) {
	// Derived from https://github.com/golang/go/blob/go1.24.0/src/encoding/binary/varint.go
	// with the modification on the overflow handling tailored for 32-bits.
	var s uint32
	for i := 0; i < maxVarintLen32; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		if b < 0x80 {
			// Unused bits must be all zero.
			if i == maxVarintLen32-1 && (b&0xf0) > 0 {
				return 0, 0, errOverflow32
			}
			return ret | uint32(b)<<s, uint64(i) + 1, nil
		}
		ret |= uint32(b&0x7f) << s
		s += 7
	}
	return 0, 0, errOverflow32
}

// LoadUint32 is the same as DecodeUint32 but reads directly out of buf via index, rather than through the
// io.ByteReader interface.
func LoadUint32(buf []byte) (ret uint32, bytesRead uint64, err error) {
	bufLen := len(buf)
	var s uint32
	for i := 0; i < maxVarintLen32; i++ {
		if i >= bufLen {
			return 0, 0, io.EOF
		}
		b := buf[i]
		if b < 0x80 {
			// Unused bits must be all zero.
			if i == maxVarintLen32-1 && (b&0xf0) > 0 {
				return 0, 0, errOverflow32
			}
			return ret | uint32(b)<<s, uint64(i) + 1, nil
		}
		ret |= uint32(b&0x7f) << s
		s += 7
	}
	return 0, 0, errOverflow32
}

// LoadUint64 is the same as LoadUint32, but for 64-bit results.
func LoadUint64(buf []byte) (ret uint64, bytesRead uint64, err error) {
	bufLen := len(buf)
	if bufLen == 0 {
		return 0, 0, io.EOF
	}

	// Derived from https://github.com/golang/go/blob/go1.24.0/src/encoding/binary/varint.go
	var s uint64
	for i := 0; i < maxVarintLen64; i++ {
		if i >= bufLen {
			return 0, 0, io.EOF
		}
		b := buf[i]
		if b < 0x80 {
			// Unused bits (non first bit) must all be zero.
			if i == maxVarintLen64-1 && b > 1 {
				return 0, 0, errOverflow64
			}
			return ret | uint64(b)<<s, uint64(i) + 1, nil
		}
		ret |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0, errOverflow64
}

// DecodeInt32 reads a LEB128 encoded int32 one byte at a time via r.
func DecodeInt32(r io.ByteReader) (ret int32, bytesRead uint64, err error) {
	var shift int
	for {
		b, e := r.ReadByte()
		if e != nil {
			return 0, 0, e
		}
		ret |= (int32(b) & 0x7f) << shift
		shift += 7
		bytesRead++
		if b&0x80 == 0 {
			if shift < 32 && (b&0x40) != 0 {
				ret |= ^int32(0) << shift
			}
			if of := int32OverflowCheck(b, bytesRead, ret); of {
				return 0, 0, errOverflow32
			}
			return ret, bytesRead, nil
		}
	}
}

// LoadInt32 is the same as DecodeInt32 but reads directly out of buf via index, rather than through the
// io.ByteReader interface.
func LoadInt32(buf []byte) (ret int32, bytesRead uint64, err error) {
	var shift int
	bufLen := len(buf)
	for i := 0; ; i++ {
		if i >= bufLen {
			return 0, 0, io.EOF
		}
		b := buf[i]
		ret |= (int32(b) & 0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			bytesRead = uint64(i) + 1
			if shift < 32 && (b&0x40) != 0 {
				ret |= ^int32(0) << shift
			}
			if of := int32OverflowCheck(b, bytesRead, ret); of {
				return 0, 0, errOverflow32
			}
			return ret, bytesRead, nil
		}
	}
}

// int32OverflowCheck folds the redundant terminal-byte overflow checks that used to be three cascaded branches
// (one that could never be false when the other two applied) into a single mask comparison against the final
// byte, done only once bytesRead reaches maxVarintLen32.
func int32OverflowCheck(finalByte byte, bytesRead uint64, ret int32) bool {
	if bytesRead > maxVarintLen32 {
		return true
	}
	if bytesRead == maxVarintLen32 {
		// The top 3 bits of the final byte are beyond the 32-bit range: they must agree with the sign of ret.
		const unusedBits = 0b00110000
		unused := finalByte & unusedBits
		want := byte(0)
		if ret < 0 {
			want = unusedBits
		}
		return unused != want
	}
	return false
}

// DecodeInt33AsInt64 is a special cased decoder for wasm.BlockType which is encoded as a positive signed integer, yet
// still needs to fit the 32-bit range of allowed indices. Hence, this is 33, not 32-bit!
//
// See https://webassembly.github.io/spec/core/binary/instructions.html#control-instructions
func DecodeInt33AsInt64(r io.ByteReader) (ret int64, bytesRead uint64, err error) {
	var shift int
	var b int64
	for shift < 35 {
		rb, e := r.ReadByte()
		if e != nil {
			return 0, 0, e
		}
		b = int64(rb)
		ret |= (b & int33Mask2) << shift
		shift += 7
		bytesRead++
		if b&int33Mask == 0 {
			break
		}
	}
	return int33Finish(b, shift, bytesRead, ret)
}

// LoadInt33AsInt64 is the same as DecodeInt33AsInt64 but reads directly out of buf via index, rather than
// through the io.ByteReader interface.
func LoadInt33AsInt64(buf []byte) (ret int64, bytesRead uint64, err error) {
	var shift int
	var b int64
	bufLen := len(buf)
	i := 0
	for shift < 35 {
		if i >= bufLen {
			return 0, 0, io.EOF
		}
		b = int64(buf[i])
		i++
		ret |= (b & int33Mask2) << shift
		shift += 7
		if b&int33Mask == 0 {
			break
		}
	}
	return int33Finish(b, shift, uint64(i), ret)
}

// int33Finish applies the sign extension and overflow checks shared by DecodeInt33AsInt64 and LoadInt33AsInt64
// once the raw 35-bit accumulation loop above has produced ret, shift and bytesRead from the final byte b.
func int33Finish(b int64, shift int, bytesRead uint64, ret int64) (int64, uint64, error) {
	// fixme: can be optimized
	if shift < 33 && (b&int33Mask3) == int33Mask3 {
		ret |= int33Mask4 << shift
	}
	ret = ret & int33Mask4

	// if 33rd bit == 1, we translate it as a corresponding signed-33bit minus value
	if ret&int33Mask5 > 0 {
		ret = ret - int33Mask6
	}

	// Over flow checks: the loop above can run at most maxVarintLen33 times (bounded by shift<35), so bytesRead
	// can never exceed maxVarintLen33; the > case is kept for defensive symmetry with the 32/64-bit variants.
	if bytesRead > maxVarintLen33 {
		return 0, 0, errOverflow33
	}
	if bytesRead == maxVarintLen33 {
		// The top 2 bits of the final byte are beyond the 33-bit range: they must agree with the sign of ret.
		const unusedBits = 0b00100000
		unused := b & unusedBits
		want := int64(0)
		if ret < 0 {
			want = unusedBits
		}
		if unused != want {
			return 0, 0, errOverflow33
		}
	}
	return ret, bytesRead, nil
}

// DecodeInt64 reads a LEB128 encoded int64 one byte at a time via r.
func DecodeInt64(r io.ByteReader) (ret int64, bytesRead uint64, err error) {
	var shift int
	for {
		b, e := r.ReadByte()
		if e != nil {
			return 0, 0, e
		}
		ret |= (int64(b) & 0x7f) << shift
		shift += 7
		bytesRead++
		if b&0x80 == 0 {
			if shift < 64 && (b&int64Mask3) == int64Mask3 {
				ret |= int64Mask4 << shift
			}
			if of := int64OverflowCheck(b, bytesRead, ret); of {
				return 0, 0, errOverflow64
			}
			return ret, bytesRead, nil
		}
	}
}

// LoadInt64 is the same as DecodeInt64 but reads directly out of buf via index, rather than through the
// io.ByteReader interface.
func LoadInt64(buf []byte) (ret int64, bytesRead uint64, err error) {
	var shift int
	bufLen := len(buf)
	for i := 0; ; i++ {
		if i >= bufLen {
			return 0, 0, io.EOF
		}
		b := buf[i]
		ret |= (int64(b) & 0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			bytesRead = uint64(i) + 1
			if shift < 64 && (b&int64Mask3) == int64Mask3 {
				ret |= int64Mask4 << shift
			}
			if of := int64OverflowCheck(b, bytesRead, ret); of {
				return 0, 0, errOverflow64
			}
			return ret, bytesRead, nil
		}
	}
}

// int64OverflowCheck is the 64-bit equivalent of int32OverflowCheck.
func int64OverflowCheck(finalByte byte, bytesRead uint64, ret int64) bool {
	if bytesRead > maxVarintLen64 {
		return true
	}
	if bytesRead == maxVarintLen64 {
		// The unused high bits of the final byte are beyond the 64-bit range: they must agree with the sign of ret.
		const unusedBits = 0b00111110
		unused := finalByte & unusedBits
		want := byte(0)
		if ret < 0 {
			want = unusedBits
		}
		return unused != want
	}
	return false
}
