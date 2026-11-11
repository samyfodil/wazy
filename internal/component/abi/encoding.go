package abi

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Encoding is a canonical option's string-encoding: how a string sits in
// linear memory. The zero value is UTF8, the default when a canon names none.
type Encoding uint8

const (
	UTF8 Encoding = iota
	UTF16
	// Latin1UTF16 stores each string as Latin-1 when every code point fits a
	// byte, and as UTF-16 otherwise, setting utf16Tag in the length to say
	// which.
	Latin1UTF16
)

// utf16Tag marks a Latin1UTF16 string's code-unit count as UTF-16: the top
// bit of a 32-bit length.
const utf16Tag = 1 << 31

// MaxStringByteLength and MaxListByteLength are the reference's
// MAX_STRING_BYTE_LENGTH and MAX_LIST_BYTE_LENGTH: the most a lifted string or
// list may span in memory.
const (
	MaxStringByteLength = 1<<28 - 1
	MaxListByteLength   = 1<<28 - 1
)

// String reports e the way the text format spells it.
func (e Encoding) String() string {
	switch e {
	case UTF16:
		return "utf16"
	case Latin1UTF16:
		return "latin1+utf16"
	default:
		return "utf8"
	}
}

// Traps the specification's test suites pin the wording of. The suites take
// it from Wasmtime, which words some traps differently in its fused adapters
// than on its host path; a wazy message carries both wordings where they
// differ, since one code path serves both here.
func errUnaligned(what string, ptr, align uint32) error {
	return fmt.Errorf("wasm trap: unaligned pointer: %s %d is not a multiple of %d", what, ptr, align)
}

func errStringBounds(ptr uint32, byteLen uint64, memLen int) error {
	return fmt.Errorf("wasm trap: string content out-of-bounds: string pointer/length out of bounds of memory (ptr=%d byte_length=%d mem_len=%d)", ptr, byteLen, memLen)
}

func errListBounds(ptr uint32, byteLen uint64, memLen int) error {
	return fmt.Errorf("wasm trap: list content out-of-bounds: list pointer/length out of bounds of memory (ptr=%d byte_length=%d mem_len=%d)", ptr, byteLen, memLen)
}

func errInvalidChar(v uint32) error {
	return fmt.Errorf("wasm trap: invalid `char` bit pattern: %#x", v)
}

func errDiscriminant(kind string, disc uint32, n int) error {
	return fmt.Errorf("wasm trap: invalid variant discriminant: %s case %d of %d", kind, disc, n)
}

// checkRealloc is the reference's LiftLowerContext.reallocate checks on what
// realloc returned: aligned, then inside memory. what names the allocation
// ("string", "list", or "" for anything else) for the fused-adapter wording.
//
// mem MUST be the view refreshed after the allocation (see GrowMem): checked
// against a pre-grow snapshot, a perfectly valid pointer is rejected. The sum
// is formed in 64 bits, so a high pointer or a huge size cannot wrap past the
// check, and a zero-size allocation at ptr == len(mem) stays legal.
func checkRealloc(mem []byte, ptr, align, size uint32, what string) error {
	if align != 0 && ptr%align != 0 {
		return fmt.Errorf("wasm trap: unaligned pointer: realloc return: result not aligned (ptr=%d alignment=%d)", ptr, align)
	}
	if uint64(ptr)+uint64(size) > uint64(len(mem)) {
		if what != "" {
			return fmt.Errorf("wasm trap: %s content out-of-bounds: realloc return: beyond end of memory (ptr=%d size=%d mem_len=%d)", what, ptr, size, len(mem))
		}
		return fmt.Errorf("wasm trap: realloc return: beyond end of memory (ptr=%d size=%d mem_len=%d)", ptr, size, len(mem))
	}
	return nil
}

// allocate is LiftLowerContext.reallocate: grow through realloc, then check
// what it returned. It returns the memory view valid after the growth.
func (r Realloc) allocate(mem []byte, old, oldSize, align, size uint32, what string) (uint32, []byte, error) {
	ptr, mem, err := r.GrowMem(mem, old, oldSize, align, size)
	if err != nil {
		return 0, mem, fmt.Errorf("realloc failed: %w", err)
	}
	if err := checkRealloc(mem, ptr, align, size, what); err != nil {
		return 0, mem, err
	}
	return ptr, mem, nil
}

// loadStringFromRange is load_string_from_range: the string a (pointer,
// tagged code units) pair names, in enc.
func loadStringFromRange(mem []byte, ptr, tagged uint32, enc Encoding) (string, error) {
	var align uint32 = 1
	byteLen := uint64(tagged)
	asUTF16 := false
	switch enc {
	case UTF16:
		align, byteLen, asUTF16 = 2, 2*uint64(tagged), true
	case Latin1UTF16:
		align = 2
		if tagged&utf16Tag != 0 {
			byteLen, asUTF16 = 2*uint64(tagged^utf16Tag), true
		}
	}
	if byteLen > MaxStringByteLength {
		return "", errStringBounds(ptr, byteLen, len(mem))
	}
	if ptr%align != 0 {
		return "", errUnaligned("string pointer", ptr, align)
	}
	if uint64(ptr)+byteLen > uint64(len(mem)) {
		return "", errStringBounds(ptr, byteLen, len(mem))
	}
	b := mem[ptr : uint64(ptr)+byteLen]
	switch {
	case asUTF16:
		return decodeUTF16(b)
	case enc == Latin1UTF16:
		return decodeLatin1(b), nil
	default:
		if err := validateUTF8(b); err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// validateUTF8 traps on malformed UTF-8 the way a strict decode does, telling
// a sequence cut off by the end of the string apart from an invalid one.
func validateUTF8(b []byte) error {
	if utf8.Valid(b) {
		return nil
	}
	// b is invalid, so the walk ends at an invalid rune.
	for i := 0; ; {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			if !utf8.FullRune(b[i:]) {
				return fmt.Errorf("wasm trap: invalid utf-8: incomplete utf-8 byte sequence from index %d", i)
			}
			return fmt.Errorf("wasm trap: invalid utf-8 sequence of 1 bytes from index %d", i)
		}
		i += size
	}
}

// decodeUTF16 decodes little-endian UTF-16, trapping on an unpaired
// surrogate as a strict decode does.
func decodeUTF16(b []byte) (string, error) {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	for i := 0; i < len(units); i++ {
		u := units[i]
		switch {
		case u < 0xd800 || u > 0xdfff:
		case u <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff:
			i++
		default:
			return "", fmt.Errorf("wasm trap: invalid utf-16: unpaired surrogate %#x at code unit %d", u, i)
		}
	}
	return string(utf16.Decode(units)), nil
}

func decodeLatin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// allocStoreString is store_string_into_range for a UTF-8 source, which is
// what a Go string is: it allocates through realloc as the reference does for
// one, writes s in realloc.Encoding, and returns (pointer, tagged code units)
// and the memory view valid after the writes.
func allocStoreString(mem []byte, s string, realloc Realloc) (uint32, uint32, []byte, error) {
	// A Go string longer than the canonical ABI can address cannot be lowered;
	// truncating its length would hand the guest a wrong one.
	if uint64(len(s)) > MaxStringByteLength {
		return 0, 0, mem, fmt.Errorf("string of %d bytes exceeds the canonical ABI's limit of %d", len(s), MaxStringByteLength)
	}
	n := uint32(len(s))
	switch realloc.Encoding {
	case UTF16:
		return storeUTF8ToUTF16(mem, s, n, realloc)
	case Latin1UTF16:
		return storeUTF8ToLatin1OrUTF16(mem, s, n, realloc)
	default:
		ptr, mem, err := realloc.allocate(mem, 0, 0, 1, n, "string")
		if err != nil {
			return 0, 0, mem, err
		}
		copy(mem[ptr:ptr+n], s)
		return ptr, n, mem, nil
	}
}

// storeUTF8ToUTF16 is store_utf8_to_utf16: allocate the worst case, two bytes
// per source byte, then shrink to what the encoding took.
func storeUTF8ToUTF16(mem []byte, s string, n uint32, realloc Realloc) (uint32, uint32, []byte, error) {
	worst := 2 * n
	ptr, mem, err := realloc.allocate(mem, 0, 0, 2, worst, "string")
	if err != nil {
		return 0, 0, mem, err
	}
	used := putUTF16(mem[ptr:], s, 0)
	if used < worst {
		if ptr, mem, err = realloc.allocate(mem, ptr, worst, 2, used, "string"); err != nil {
			return 0, 0, mem, err
		}
	}
	return ptr, used / 2, mem, nil
}

// storeUTF8ToLatin1OrUTF16 is store_string_to_latin1_or_utf16: write Latin-1
// while every code point fits a byte; at the first that does not, grow to the
// UTF-16 worst case, widen what was written, finish in UTF-16, and tag the
// length.
func storeUTF8ToLatin1OrUTF16(mem []byte, s string, n uint32, realloc Realloc) (uint32, uint32, []byte, error) {
	ptr, mem, err := realloc.allocate(mem, 0, 0, 2, n, "string")
	if err != nil {
		return 0, 0, mem, err
	}
	var latin1 uint32
	for i, r := range s {
		if r < 1<<8 {
			mem[ptr+latin1] = byte(r)
			latin1++
			continue
		}
		worst := 2 * n
		if ptr, mem, err = realloc.allocate(mem, ptr, n, 2, worst, "string"); err != nil {
			return 0, 0, mem, err
		}
		for j := int64(latin1) - 1; j >= 0; j-- {
			mem[ptr+2*uint32(j)] = mem[ptr+uint32(j)]
			mem[ptr+2*uint32(j)+1] = 0
		}
		used := putUTF16(mem[ptr:], s[i:], 2*latin1)
		if used < worst {
			if ptr, mem, err = realloc.allocate(mem, ptr, worst, 2, used, "string"); err != nil {
				return 0, 0, mem, err
			}
		}
		return ptr, used/2 | utf16Tag, mem, nil
	}
	if latin1 < n {
		if ptr, mem, err = realloc.allocate(mem, ptr, n, 2, latin1, "string"); err != nil {
			return 0, 0, mem, err
		}
	}
	return ptr, latin1, mem, nil
}

// putUTF16 writes s as little-endian UTF-16 into dst starting at byte off,
// and returns the byte offset it ended at.
func putUTF16(dst []byte, s string, off uint32) uint32 {
	for _, r := range s {
		if r >= 0x10000 {
			hi, lo := utf16.EncodeRune(r)
			binary.LittleEndian.PutUint16(dst[off:], uint16(hi))
			binary.LittleEndian.PutUint16(dst[off+2:], uint16(lo))
			off += 4
			continue
		}
		binary.LittleEndian.PutUint16(dst[off:], uint16(r))
		off += 2
	}
	return off
}
