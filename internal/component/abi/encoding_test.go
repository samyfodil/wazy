package abi

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncodingString(t *testing.T) {
	for enc, want := range map[Encoding]string{UTF8: "utf8", UTF16: "utf16", Latin1UTF16: "latin1+utf16"} {
		if got := enc.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", enc, got, want)
		}
	}
}

func le16(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return b
}

func TestLoadStringFromRange(t *testing.T) {
	mem := make([]byte, 64)
	put := func(at int, b []byte) { copy(mem[at:], b) }

	put(0, []byte("hé☃🍰"))
	if s, err := loadStringFromRange(mem, 0, uint32(len("hé☃🍰")), UTF8); err != nil || s != "hé☃🍰" {
		t.Fatalf("utf8: %q, %v", s, err)
	}

	u16 := le16("hö☃🍰")
	put(16, u16)
	if s, err := loadStringFromRange(mem, 16, uint32(len(u16)/2), UTF16); err != nil || s != "hö☃🍰" {
		t.Fatalf("utf16: %q, %v", s, err)
	}
	if s, err := loadStringFromRange(mem, 16, uint32(len(u16)/2)|utf16Tag, Latin1UTF16); err != nil || s != "hö☃🍰" {
		t.Fatalf("latin1+utf16, tagged: %q, %v", s, err)
	}
	put(40, []byte{'c', 0xe9, 0xff})
	if s, err := loadStringFromRange(mem, 40, 3, Latin1UTF16); err != nil || s != "céÿ" {
		t.Fatalf("latin1+utf16, latin1: %q, %v", s, err)
	}

	for _, tc := range []struct {
		name         string
		ptr, tagged  uint32
		enc          Encoding
		bytes        []byte
		want         string
		alsoContains string
	}{
		{"utf16 unaligned", 1, 1, UTF16, nil, "unaligned pointer", ""},
		{"latin1+utf16 unaligned latin1", 1, 0, Latin1UTF16, nil, "unaligned pointer", ""},
		{"latin1+utf16 unaligned utf16", 1, utf16Tag, Latin1UTF16, nil, "unaligned pointer", ""},
		{"utf8 out of bounds", 60, 8, UTF8, nil, "string content out-of-bounds", "string pointer/length out of bounds of memory"},
		{"utf16 out of bounds", 60, 4, UTF16, nil, "string content out-of-bounds", ""},
		{"too long", 0, MaxStringByteLength + 1, UTF8, nil, "string content out-of-bounds", ""},
		{"utf16 too long", 0, MaxStringByteLength/2 + 1, UTF16, nil, "string content out-of-bounds", ""},
		{"lone high surrogate", 48, 2, UTF16, []byte{0x3c, 0xd8, 'a', 0}, "unpaired surrogate 0xd83c at code unit 0", ""},
		{"lone low surrogate", 48, 1, UTF16, []byte{0x70, 0xdf}, "unpaired surrogate 0xdf70 at code unit 0", ""},
		{"high surrogate at the end", 48, 1, UTF16, []byte{0x3c, 0xd8}, "unpaired surrogate", ""},
		{"incomplete utf-8", 48, 3, UTF8, []byte{'a', 0xe2, 0x98}, "incomplete utf-8 byte sequence from index 1", "invalid utf-8"},
		{"invalid utf-8", 48, 3, UTF8, []byte{'a', 0xff, 'b'}, "invalid utf-8 sequence of 1 bytes from index 1", ""},
	} {
		if tc.bytes != nil {
			put(int(tc.ptr), tc.bytes)
		}
		_, err := loadStringFromRange(mem, tc.ptr, tc.tagged, tc.enc)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.alsoContains) {
			t.Errorf("%s: %v, want it to contain %q and %q", tc.name, err, tc.want, tc.alsoContains)
		}
	}
}

// reallocCall is one call the guest's realloc received.
type reallocCall struct{ old, oldSize, align, size uint32 }

// recordingRealloc is a bump allocator that, like the conformance suites',
// shrinks in place and copies on growth, and records every call.
func recordingRealloc(mem []byte, enc Encoding) (Realloc, *[]reallocCall) {
	calls := &[]reallocCall{}
	next := uint32(8)
	r := ReallocFunc(func(old, oldSize, align, size uint32) (uint32, error) {
		*calls = append(*calls, reallocCall{old, oldSize, align, size})
		if old != 0 && size <= oldSize {
			return old, nil
		}
		next = (next + align - 1) &^ (align - 1)
		p := next
		next += size
		if old != 0 {
			copy(mem[p:], mem[old:old+oldSize])
		}
		return p, nil
	})
	r.Encoding = enc
	return r, calls
}

func TestAllocStoreString(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enc    Encoding
		s      string
		bytes  []byte
		tagged uint32
		calls  []reallocCall
	}{
		{"utf8 copies", UTF8, "hé", []byte("hé"), 3, []reallocCall{{0, 0, 1, 3}}},
		{
			"utf16 allocates the worst case and shrinks", UTF16, "hé☃🍰", le16("hé☃🍰"), 5,
			[]reallocCall{{0, 0, 2, 20}, {8, 20, 2, 10}},
		},
		{"utf16 of ascii fits its worst case", UTF16, "ab", le16("ab"), 2, []reallocCall{{0, 0, 2, 4}}},
		{"latin1 of ascii fits", Latin1UTF16, "ab", []byte("ab"), 2, []reallocCall{{0, 0, 2, 2}}},
		{
			"latin1 shrinks to the code points", Latin1UTF16, "éÿ",
			[]byte{0xe9, 0xff},
			2,
			[]reallocCall{{0, 0, 2, 4}, {8, 4, 2, 2}},
		},
		{
			"a code point past latin1 widens to utf16", Latin1UTF16, "aé☃🍰", le16("aé☃🍰"), 5 | utf16Tag,
			[]reallocCall{{0, 0, 2, 10}, {8, 10, 2, 20}, {18, 20, 2, 10}},
		},
		{"empty", UTF16, "", nil, 0, []reallocCall{{0, 0, 2, 0}}},
	} {
		mem := make([]byte, 256)
		r, calls := recordingRealloc(mem, tc.enc)
		ptr, tagged, mem, err := allocStoreString(mem, tc.s, r)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tagged != tc.tagged {
			t.Errorf("%s: tagged code units %#x, want %#x", tc.name, tagged, tc.tagged)
		}
		n := uint32(len(tc.bytes))
		if got := mem[ptr : ptr+n]; string(got) != string(tc.bytes) {
			t.Errorf("%s: bytes % x, want % x", tc.name, got, tc.bytes)
		}
		if len(*calls) != len(tc.calls) {
			t.Errorf("%s: realloc calls %v, want %v", tc.name, *calls, tc.calls)
			continue
		}
		for i := range tc.calls {
			if (*calls)[i] != tc.calls[i] {
				t.Errorf("%s: realloc calls %v, want %v", tc.name, *calls, tc.calls)
				break
			}
		}
		// What was stored lifts back to the same string.
		if s, err := loadStringFromRange(mem, ptr, tagged, tc.enc); err != nil || s != tc.s {
			t.Errorf("%s: round trip %q, %v", tc.name, s, err)
		}
	}
}

// Every allocation a string lowering makes is checked, the shrinking and
// widening ones included.
func TestAllocStoreStringReallocChecks(t *testing.T) {
	failAt := func(call int, ptr uint32) Realloc {
		n := 0
		r := ReallocFunc(func(old, _, _, _ uint32) (uint32, error) {
			n++
			if n == call {
				return ptr, nil
			}
			if old != 0 {
				return old, nil
			}
			return 8, nil
		})
		return r
	}
	for _, tc := range []struct {
		name string
		enc  Encoding
		s    string
		r    Realloc
		want string
	}{
		{"utf8 unaligned is fine", UTF8, "a", failAt(1, 3), ""},
		{"utf8 beyond memory", UTF8, "a", failAt(1, 64), "string content out-of-bounds: realloc return: beyond end of memory"},
		{"utf16 unaligned", UTF16, "a", failAt(1, 3), "unaligned pointer: realloc return: result not aligned"},
		{"utf16 shrink beyond memory", UTF16, "é", failAt(2, 64), "realloc return: beyond end of memory"},
		{"latin1 first allocation", Latin1UTF16, "a", failAt(1, 64), "realloc return: beyond end of memory"},
		{"latin1 widening", Latin1UTF16, "a☃🍰", failAt(2, 64), "realloc return: beyond end of memory"},
		{"latin1 shrinking after widening", Latin1UTF16, "a☃🍰", failAt(3, 63), "realloc return: result not aligned"},
		{"latin1 shrinking", Latin1UTF16, "éÿ", failAt(2, 63), "realloc return: result not aligned"},
	} {
		tc.r.Encoding = tc.enc
		_, _, _, err := allocStoreString(make([]byte, 64), tc.s, tc.r)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want it to contain %q", tc.name, err, tc.want)
		}
	}

	if _, _, _, err := allocStoreString(nil, strings.Repeat("a", MaxStringByteLength+1), Realloc{}); err == nil || !strings.Contains(err.Error(), "exceeds the canonical ABI's limit") {
		t.Errorf("oversized string: %v", err)
	}
	if _, _, _, err := allocStoreString(nil, "a", Realloc{}); err == nil || !strings.Contains(err.Error(), "cabi_realloc") {
		t.Errorf("no realloc: %v", err)
	}
}

func TestCheckRealloc(t *testing.T) {
	mem := make([]byte, 16)
	for _, tc := range []struct {
		name               string
		ptr, align, size   uint32
		what, want, absent string
	}{
		{"fits", 8, 4, 8, "list", "", ""},
		{"zero size at the end", 16, 4, 0, "list", "", ""},
		{"misaligned before out of bounds", 0xffffffff, 4, 8, "list", "result not aligned", "beyond"},
		{"list", 12, 4, 8, "list", "wasm trap: list content out-of-bounds: realloc return: beyond end of memory", ""},
		{"string", 12, 1, 8, "string", "wasm trap: string content out-of-bounds: realloc return: beyond end of memory", ""},
		{"anything else", 12, 4, 8, "", "wasm trap: realloc return: beyond end of memory", "content"},
		{"no wrap", 0xfffffff0, 4, 0x20, "list", "beyond end of memory", ""},
	} {
		err := checkRealloc(mem, tc.ptr, tc.align, tc.size, tc.what)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want it to contain %q", tc.name, err, tc.want)
		case tc.absent != "" && err != nil && strings.Contains(err.Error(), tc.absent):
			t.Errorf("%s: %v contains %q", tc.name, err, tc.absent)
		}
	}
}

func TestListSpan(t *testing.T) {
	mem := make([]byte, 64)
	if b, err := listSpan(mem, 8, 4, 4, 4); err != nil || len(b) != 16 {
		t.Fatalf("in range: %d bytes, %v", len(b), err)
	}
	for _, tc := range []struct {
		name                     string
		ptr, length, size, align uint32
		want                     string
	}{
		{"too long", 0, 1 << 27, 4, 4, "list content out-of-bounds"},
		{"misaligned", 2, 1, 4, 4, "unaligned pointer: list pointer 2 is not a multiple of 4"},
		{"out of bounds", 60, 2, 4, 4, "list content out-of-bounds"},
		{"wrapping length", 0, 0xffffffff, 8, 8, "list content out-of-bounds"},
	} {
		if _, err := listSpan(mem, tc.ptr, tc.length, tc.size, tc.align); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// Leaving the instance is forbidden while its realloc runs, and allowed again
// once realloc returns, whether or not it failed.
func TestReallocClearsMayLeave(t *testing.T) {
	mayLeave := true
	var during []bool
	r := ReallocFunc(func(_, _, _, size uint32) (uint32, error) {
		during = append(during, mayLeave)
		if size == 0 {
			return 0, errBoomRealloc
		}
		return 8, nil
	})
	r.MayLeave = &mayLeave
	if _, err := r.Grow(0, 0, 1, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Grow(0, 0, 1, 0); err != errBoomRealloc {
		t.Fatalf("err = %v", err)
	}
	if len(during) != 2 || during[0] || during[1] || !mayLeave {
		t.Fatalf("may_leave during %v, after %v; want false during, true after", during, mayLeave)
	}
}

var errBoomRealloc = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
