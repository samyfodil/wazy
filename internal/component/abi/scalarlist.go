package abi

import (
	"encoding/binary"
	"fmt"
	"math"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// A list of a fixed-width primitive lifts to the Go slice of that primitive --
// []uint32 for list<u32>, []float64 for list<f64>, []byte for list<u8> -- rather
// than to a []Value holding one interface per element.
//
// The general shape costs a machine word per element on 64-bit, sixteen bytes
// to carry four, and every consumer of a numeric list converts it back to a
// typed slice anyway. The typed form is one allocation of exactly the data,
// laid out contiguously, and it is what a caller wanted in the first place.
//
// Only fixed-width primitives qualify: bool, the integers, the floats, and
// char. A list of strings or of records has no single Go element type, and
// stays []Value.
//
// The encodings here are deliberately identical to what loadPrimitive and
// storePrimitive produce element by element, and scalarlist_test.go asserts
// that against them for every type rather than trusting the reading -- a
// disagreement would not fail, it would hand a guest silently wrong numbers.

// scalarListLen bounds-checks a list of length elements of elemSize bytes at
// ptr and returns the byte span.
func scalarListSpan(mem []byte, ptr, length, elemSize uint32) ([]byte, error) {
	byteLen := length * elemSize
	// The multiply and the add both have to be checked: a length near 2^32
	// wraps, and a wrapped span would read somewhere else entirely.
	if elemSize != 0 && length > (1<<32-1)/elemSize {
		return nil, fmt.Errorf("loadListFromRange: list length %d overflows at %d bytes per element", length, elemSize)
	}
	if ptr+byteLen < ptr || uint32(len(mem)) < ptr+byteLen {
		return nil, fmt.Errorf("loadListFromRange: list buffer overflow at ptr=%d len=%d mem_len=%d", ptr, byteLen, len(mem))
	}
	return mem[ptr : ptr+byteLen], nil
}

// decodeScalarList reads length elements of elemSize bytes each through dec.
// dec sees exactly one element's bytes.
func decodeScalarList[T any](mem []byte, ptr, length, elemSize uint32, dec func([]byte) (T, error)) ([]T, error) {
	span, err := scalarListSpan(mem, ptr, length, elemSize)
	if err != nil {
		return nil, err
	}
	out := make([]T, length)
	for i := range out {
		v, err := dec(span[uint32(i)*elemSize:])
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// liftScalarList lifts a list of fixed-width primitives into the typed slice
// for that primitive. handled is false for an element type that has no such
// slice, which the caller lifts as []Value instead.
func liftScalarList(mem []byte, ptr, length uint32, prim string) (v Value, handled bool, err error) {
	switch prim {
	case "u8":
		// u8 needs no decode at all, so this is one copy. The bytes are
		// copied rather than aliased: mem is live guest memory and a lifted
		// value must not point into it.
		span, err := scalarListSpan(mem, ptr, length, 1)
		if err != nil {
			return nil, true, err
		}
		out := make([]byte, length)
		copy(out, span)
		return out, true, nil

	case "s8":
		out, err := decodeScalarList(mem, ptr, length, 1, func(b []byte) (int8, error) { return int8(b[0]), nil })
		return out, true, err

	case "bool":
		// Any non-zero byte is true, matching loadPrimitive.
		out, err := decodeScalarList(mem, ptr, length, 1, func(b []byte) (bool, error) { return b[0] != 0, nil })
		return out, true, err

	case "u16":
		out, err := decodeScalarList(mem, ptr, length, 2, func(b []byte) (uint16, error) {
			return binary.LittleEndian.Uint16(b), nil
		})
		return out, true, err

	case "s16":
		out, err := decodeScalarList(mem, ptr, length, 2, func(b []byte) (int16, error) {
			return int16(binary.LittleEndian.Uint16(b)), nil
		})
		return out, true, err

	case "u32":
		out, err := decodeScalarList(mem, ptr, length, 4, func(b []byte) (uint32, error) {
			return binary.LittleEndian.Uint32(b), nil
		})
		return out, true, err

	case "s32":
		out, err := decodeScalarList(mem, ptr, length, 4, func(b []byte) (int32, error) {
			return int32(binary.LittleEndian.Uint32(b)), nil
		})
		return out, true, err

	case "u64":
		out, err := decodeScalarList(mem, ptr, length, 8, func(b []byte) (uint64, error) {
			return binary.LittleEndian.Uint64(b), nil
		})
		return out, true, err

	case "s64":
		out, err := decodeScalarList(mem, ptr, length, 8, func(b []byte) (int64, error) {
			return int64(binary.LittleEndian.Uint64(b)), nil
		})
		return out, true, err

	case "f32":
		out, err := decodeScalarList(mem, ptr, length, 4, func(b []byte) (float32, error) {
			return math.Float32frombits(binary.LittleEndian.Uint32(b)), nil
		})
		return out, true, err

	case "f64":
		out, err := decodeScalarList(mem, ptr, length, 8, func(b []byte) (float64, error) {
			return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
		})
		return out, true, err

	case "char":
		// Validated per element exactly as loadPrimitive does: a list is not
		// a way to smuggle in an unpaired surrogate.
		out, err := decodeScalarList(mem, ptr, length, 4, func(b []byte) (rune, error) {
			i := binary.LittleEndian.Uint32(b)
			if i >= 0x110000 {
				return 0, fmt.Errorf("load char: value %d out of range", i)
			}
			if i >= 0xD800 && i <= 0xDFFF {
				return 0, fmt.Errorf("load char: surrogate half %d not allowed", i)
			}
			return rune(i), nil
		})
		return out, true, err
	}
	return nil, false, nil
}

// encodeScalarList writes each element of in through enc into a freshly
// allocated guest buffer, and returns its pointer and element count.
func encodeScalarList[T any](mem []byte, in []T, elemSize, align uint32, realloc Realloc, enc func([]byte, T) error) (uint32, uint32, error) {
	length := uint32(len(in))
	if elemSize != 0 && length > (1<<32-1)/elemSize {
		return 0, 0, fmt.Errorf("list length %d overflows at %d bytes per element", length, elemSize)
	}
	byteLen := length * elemSize

	ptr, err := realloc.Grow(0, 0, align, byteLen)
	if err != nil {
		return 0, 0, fmt.Errorf("realloc failed: %w", err)
	}
	if ptr+byteLen < ptr || uint32(len(mem)) < ptr+byteLen {
		return 0, 0, fmt.Errorf("allocated memory out of bounds: ptr=%d size=%d", ptr, byteLen)
	}
	span := mem[ptr : ptr+byteLen]
	for i, v := range in {
		if err := enc(span[uint32(i)*elemSize:], v); err != nil {
			return 0, 0, err
		}
	}
	return ptr, length, nil
}

// storeScalarList stores a typed slice as a list of the matching primitive.
// handled is false when v is not the typed slice for prim, which the caller
// then stores from its []Value shape instead -- both shapes stay accepted.
func storeScalarList(mem []byte, v Value, prim string, realloc Realloc) (ptr, n uint32, handled bool, err error) {
	switch prim {
	case "u8":
		b, ok := v.([]byte)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = allocStoreBytes(mem, b, realloc)
		return ptr, n, true, err

	case "s8":
		in, ok := v.([]int8)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 1, 1, realloc, func(b []byte, e int8) error {
			b[0] = byte(e)
			return nil
		})
		return ptr, n, true, err

	case "bool":
		in, ok := v.([]bool)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 1, 1, realloc, func(b []byte, e bool) error {
			b[0] = 0
			if e {
				b[0] = 1
			}
			return nil
		})
		return ptr, n, true, err

	case "u16":
		in, ok := v.([]uint16)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 2, 2, realloc, func(b []byte, e uint16) error {
			binary.LittleEndian.PutUint16(b, e)
			return nil
		})
		return ptr, n, true, err

	case "s16":
		in, ok := v.([]int16)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 2, 2, realloc, func(b []byte, e int16) error {
			binary.LittleEndian.PutUint16(b, uint16(e))
			return nil
		})
		return ptr, n, true, err

	case "u32":
		in, ok := v.([]uint32)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 4, 4, realloc, func(b []byte, e uint32) error {
			binary.LittleEndian.PutUint32(b, e)
			return nil
		})
		return ptr, n, true, err

	case "s32":
		in, ok := v.([]int32)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 4, 4, realloc, func(b []byte, e int32) error {
			binary.LittleEndian.PutUint32(b, uint32(e))
			return nil
		})
		return ptr, n, true, err

	case "u64":
		in, ok := v.([]uint64)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 8, 8, realloc, func(b []byte, e uint64) error {
			binary.LittleEndian.PutUint64(b, e)
			return nil
		})
		return ptr, n, true, err

	case "s64":
		in, ok := v.([]int64)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 8, 8, realloc, func(b []byte, e int64) error {
			binary.LittleEndian.PutUint64(b, uint64(e))
			return nil
		})
		return ptr, n, true, err

	case "f32":
		in, ok := v.([]float32)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 4, 4, realloc, func(b []byte, e float32) error {
			binary.LittleEndian.PutUint32(b, math.Float32bits(e))
			return nil
		})
		return ptr, n, true, err

	case "f64":
		in, ok := v.([]float64)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 8, 8, realloc, func(b []byte, e float64) error {
			binary.LittleEndian.PutUint64(b, math.Float64bits(e))
			return nil
		})
		return ptr, n, true, err

	case "char":
		in, ok := v.([]rune)
		if !ok {
			return 0, 0, false, nil
		}
		ptr, n, err = encodeScalarList(mem, in, 4, 4, realloc, func(b []byte, e rune) error {
			if e < 0 || e >= 0x110000 {
				return fmt.Errorf("store char: value %d out of range", e)
			}
			if e >= 0xD800 && e <= 0xDFFF {
				return fmt.Errorf("store char: surrogate half %d not allowed", e)
			}
			binary.LittleEndian.PutUint32(b, uint32(e))
			return nil
		})
		return ptr, n, true, err
	}
	return 0, 0, false, nil
}

// scalarPrim returns the primitive name of a list's element type, if the
// element is a primitive at all.
func scalarPrim(elemType bintype.TypeDesc) (string, bool) {
	p, ok := elemType.(bintype.PrimitiveDesc)
	if !ok {
		return "", false
	}
	return p.Prim, true
}
