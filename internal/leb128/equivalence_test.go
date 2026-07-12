package leb128

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand"
	"testing"
)

// This file is the equivalence proof for the slice-indexed rewrite of the reader/closure based decoders that used
// to live in this package. referenceLoad* below are frozen copies of the pre-rewrite algorithms (byte-at-a-time via
// a closure), including their exact overflow-check branching. The new LoadInt32/LoadInt64/LoadInt33AsInt64 (whose
// overflow branches were folded into a single mask comparison, see int32OverflowCheck/int64OverflowCheck/
// int33Finish in leb128.go) are checked against these references over exhaustive and randomized inputs so that the
// fold is proven behavior preserving rather than merely "looks right".
//
// Because the fold only touches how the *classification* of a terminal-byte overflow is computed (not how many
// bytes are read to find that terminal byte), the comparison below treats two error results as equivalent whenever
// they agree on success-vs-failure and, on failure, on the class of error (overflow vs. truncation/EOF). It does
// not require identical error strings: leb128.go also intentionally drops the old "readByte failed: %w" wrapping
// on the reader-based decoders (see report), which is a deliberate, documented deviation, not a bug.

type refNextByte func(i int) (byte, error)

func refDecodeUint32(next refNextByte) (ret uint32, bytesRead uint64, err error) {
	var s uint32
	for i := 0; i < maxVarintLen32; i++ {
		b, err := next(i)
		if err != nil {
			return 0, 0, err
		}
		if b < 0x80 {
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

func refDecodeInt32(next refNextByte) (ret int32, bytesRead uint64, err error) {
	var shift int
	var b byte
	for {
		b, err = next(int(bytesRead))
		if err != nil {
			return 0, 0, errors.New("readByte failed: " + err.Error())
		}
		ret |= (int32(b) & 0x7f) << shift
		shift += 7
		bytesRead++
		if b&0x80 == 0 {
			if shift < 32 && (b&0x40) != 0 {
				ret |= ^0 << shift
			}
			// Over flow checks.
			if bytesRead > maxVarintLen32 {
				return 0, 0, errOverflow32
			} else if unused := b & 0b00110000; bytesRead == maxVarintLen32 && ret < 0 && unused != 0b00110000 {
				return 0, 0, errOverflow32
			} else if bytesRead == maxVarintLen32 && ret >= 0 && unused != 0x00 {
				return 0, 0, errOverflow32
			}
			return
		}
	}
}

func refDecodeInt64(next refNextByte) (ret int64, bytesRead uint64, err error) {
	var shift int
	var b byte
	for {
		b, err = next(int(bytesRead))
		if err != nil {
			return 0, 0, errors.New("readByte failed: " + err.Error())
		}
		ret |= (int64(b) & 0x7f) << shift
		shift += 7
		bytesRead++
		if b&0x80 == 0 {
			if shift < 64 && (b&int64Mask3) == int64Mask3 {
				ret |= int64Mask4 << shift
			}
			if bytesRead > maxVarintLen64 {
				return 0, 0, errOverflow64
			} else if unused := b & 0b00111110; bytesRead == maxVarintLen64 && ret < 0 && unused != 0b00111110 {
				return 0, 0, errOverflow64
			} else if bytesRead == maxVarintLen64 && ret >= 0 && unused != 0x00 {
				return 0, 0, errOverflow64
			}
			return
		}
	}
}

func refDecodeInt33AsInt64(next refNextByte) (ret int64, bytesRead uint64, err error) {
	var shift int
	var b int64
	var rb byte
	for shift < 35 {
		rb, err = next(int(bytesRead))
		if err != nil {
			return 0, 0, errors.New("readByte failed: " + err.Error())
		}
		b = int64(rb)
		ret |= (b & int33Mask2) << shift
		shift += 7
		bytesRead++
		if b&int33Mask == 0 {
			break
		}
	}

	if shift < 33 && (b&int33Mask3) == int33Mask3 {
		ret |= int33Mask4 << shift
	}
	ret = ret & int33Mask4

	if ret&int33Mask5 > 0 {
		ret = ret - int33Mask6
	}
	if bytesRead > maxVarintLen33 {
		return 0, 0, errOverflow33
	} else if unused := b & 0b00100000; bytesRead == maxVarintLen33 && ret < 0 && unused != 0b00100000 {
		return 0, 0, errOverflow33
	} else if bytesRead == maxVarintLen33 && ret >= 0 && unused != 0x00 {
		return 0, 0, errOverflow33
	}
	return ret, bytesRead, nil
}

// sliceNextByte reproduces the pre-rewrite LoadXxx behavior of indexing into buf with an io.EOF once the index
// runs past the end - this is exactly what the old closures (`func(i int) (byte, error) {... }`) did.
func sliceNextByte(buf []byte) refNextByte {
	return func(i int) (byte, error) {
		if i >= len(buf) {
			return 0, io.EOF
		}
		return buf[i], nil
	}
}

// errClass buckets an error into "nil", "overflow" or "other" (EOF/truncation) so that the comparison below is
// insensitive to the message-wrapping deviation described at the top of the file.
func errClass(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, errOverflow32), errors.Is(err, errOverflow33), errors.Is(err, errOverflow64):
		return "overflow"
	default:
		return "other"
	}
}

func requireSameOutcome[T comparable](t *testing.T, desc string, buf []byte, refVal T, refN uint64, refErr error, gotVal T, gotN uint64, gotErr error) {
	t.Helper()
	if errClass(refErr) != errClass(gotErr) {
		t.Fatalf("%s: buf=%x: error class mismatch: ref=%v (%s) got=%v (%s)", desc, buf, refErr, errClass(refErr), gotErr, errClass(gotErr))
	}
	if refErr == nil {
		if refVal != gotVal {
			t.Fatalf("%s: buf=%x: value mismatch: ref=%v got=%v", desc, buf, refVal, gotVal)
		}
		if refN != gotN {
			t.Fatalf("%s: buf=%x: bytesRead mismatch: ref=%v got=%v", desc, buf, refN, gotN)
		}
	}
}

func TestLoadUint32_Equivalence(t *testing.T) {
	check := func(buf []byte) {
		refVal, refN, refErr := refDecodeUint32(sliceNextByte(buf))
		gotVal, gotN, gotErr := LoadUint32(buf)
		requireSameOutcome(t, "LoadUint32", buf, refVal, refN, refErr, gotVal, gotN, gotErr)

		// DecodeUint32 must agree with LoadUint32 (and hence the reference) too.
		dVal, dN, dErr := DecodeUint32(bytes.NewReader(buf))
		requireSameOutcome(t, "DecodeUint32", buf, refVal, refN, refErr, dVal, dN, dErr)
	}
	exhaustiveByteSequences(check)
}

func TestLoadInt32_Equivalence(t *testing.T) {
	check := func(buf []byte) {
		refVal, refN, refErr := refDecodeInt32(sliceNextByte(buf))
		gotVal, gotN, gotErr := LoadInt32(buf)
		requireSameOutcome(t, "LoadInt32", buf, refVal, refN, refErr, gotVal, gotN, gotErr)

		dVal, dN, dErr := DecodeInt32(bytes.NewReader(buf))
		requireSameOutcome(t, "DecodeInt32", buf, refVal, refN, refErr, dVal, dN, dErr)
	}
	exhaustiveByteSequences(check)
}

func TestLoadInt64_Equivalence(t *testing.T) {
	check := func(buf []byte) {
		refVal, refN, refErr := refDecodeInt64(sliceNextByte(buf))
		gotVal, gotN, gotErr := LoadInt64(buf)
		requireSameOutcome(t, "LoadInt64", buf, refVal, refN, refErr, gotVal, gotN, gotErr)

		dVal, dN, dErr := DecodeInt64(bytes.NewReader(buf))
		requireSameOutcome(t, "DecodeInt64", buf, refVal, refN, refErr, dVal, dN, dErr)
	}
	exhaustiveByteSequences(check)
}

func TestLoadInt33AsInt64_Equivalence(t *testing.T) {
	check := func(buf []byte) {
		refVal, refN, refErr := refDecodeInt33AsInt64(sliceNextByte(buf))
		gotVal, gotN, gotErr := LoadInt33AsInt64(buf)
		requireSameOutcome(t, "LoadInt33AsInt64", buf, refVal, refN, refErr, gotVal, gotN, gotErr)

		dVal, dN, dErr := DecodeInt33AsInt64(bytes.NewReader(buf))
		requireSameOutcome(t, "DecodeInt33AsInt64", buf, refVal, refN, refErr, dVal, dN, dErr)
	}
	exhaustiveByteSequences(check)
}

// exhaustiveByteSequences feeds check every 1-byte sequence and a wide 2-byte sweep exhaustively, every
// boundary-crafted sequence up to 12 bytes (varying continuation bits and the terminal byte's high bits across the
// full 0x00-0x7f range to hit every overflow seam), 100k random sequences of length 3-12 bytes (mixing
// forced-continuation and free-random bytes so both truncated-mid-stream and full-length cases are explored), and
// canonical encodings (plus truncations/over-long variants) of boundary and random values for uint32/int32/int64.
func exhaustiveByteSequences(check func(buf []byte)) {
	// All 1-byte sequences.
	for b := 0; b < 256; b++ {
		check([]byte{byte(b)})
	}

	// All 2-byte sequences where the first byte is a continuation byte (if it isn't, decoding never reaches byte
	// 2, so exhaustively varying byte 2 there is redundant work, but cheap enough to just do anyway below).
	for b0 := 0; b0 < 256; b0++ {
		for b1 := 0; b1 < 256; b1++ {
			check([]byte{byte(b0), byte(b1)})
		}
	}

	// Boundary-crafted sequences: for every length 1..12 and every possible terminal byte value, with all
	// preceding bytes forced-continuation (0x80). This directly stresses the overflow-check seam at
	// maxVarintLen{32,33,64} in both directions (one below, at, and one above).
	for n := 1; n <= 12; n++ {
		for last := 0; last < 128; last++ {
			buf := make([]byte, n)
			for i := 0; i < n-1; i++ {
				buf[i] = 0x80
			}
			buf[n-1] = byte(last)
			check(buf)
		}
		// Also the all-continuation (truncated / never-terminates) case of this length.
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = 0x80
		}
		check(buf)
	}

	// Canonical encodings (+ truncations, + over-long paddings) of boundary values.
	boundaryU32 := []uint32{
		0, 1, 0x7f, 0x80, 0x3fff, 0x4000, 0x1fffff, 0x200000,
		0xfffffff, 0x10000000, math.MaxInt32, math.MaxUint32, math.MaxUint32 - 1,
	}
	for _, v := range boundaryU32 {
		checkVariants(EncodeUint32(v), check)
	}
	boundaryI32 := []int32{0, 1, -1, math.MaxInt32, math.MinInt32, 64, -64, 8191, -8192}
	for _, v := range boundaryI32 {
		checkVariants(EncodeInt32(v), check)
	}
	boundaryI64 := []int64{0, 1, -1, math.MaxInt64, math.MinInt64, math.MaxInt32, math.MinInt32}
	for _, v := range boundaryI64 {
		checkVariants(EncodeInt64(v), check)
	}

	// 100k random 3-12 byte sequences.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100000; i++ {
		n := 3 + rng.Intn(10)
		buf := make([]byte, n)
		for j := range buf {
			buf[j] = byte(rng.Intn(256))
			if j < n-1 && rng.Intn(4) != 0 {
				// Bias most non-terminal bytes toward continuation so the sequences plausibly extend
				// deep into (or past) the encoding, exercising both over-long and truncated paths.
				buf[j] |= 0x80
			}
		}
		check(buf)
	}

	// 100k random full-range uint32 values run through their canonical encoding plus truncations/over-long
	// variants (covers every byte-length class: 1-2 byte exhaustively above, 3-5 byte here at scale).
	for i := 0; i < 100000; i++ {
		v := rng.Uint32()
		checkVariants(EncodeUint32(v), check)
	}
}

// checkVariants runs check on the canonical encoding, every truncation (drop the last k bytes), and an over-long
// re-encoding (turn the terminal byte into a continuation byte and append a zero-payload terminal byte after it,
// i.e. a non-minimal encoding of the same value with one redundant byte).
func checkVariants(enc []byte, check func(buf []byte)) {
	check(enc)
	for k := 1; k < len(enc); k++ {
		check(enc[:len(enc)-k])
	}
	if len(enc) > 0 {
		overlong := make([]byte, 0, len(enc)+1)
		overlong = append(overlong, enc[:len(enc)-1]...)
		last := enc[len(enc)-1]
		overlong = append(overlong, last|0x80, 0x00)
		check(overlong)
	}
}
