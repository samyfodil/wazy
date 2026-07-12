package binary

import (
	"io"
	"unsafe"

	"github.com/samyfodil/wazy/internal/wasm"
)

// stringArena batches the immutable strings decoded for one module into a small number of geometrically-grown
// []byte chunks, handing back each string as a subslice of a chunk. This replaces the per-string make+copy in
// decodeUTF8 with amortized O(number-of-chunks) allocations instead of O(number-of-strings), while preserving the
// exact retention semantics: the returned strings never alias the caller's input binary (bytes are copied into a
// chunk), and every string lives as long as the Module it is stored on — which is also how long its chunk lives.
type stringArena struct {
	buf []byte // current chunk; earlier chunks stay alive via the strings already carved from them.
}

// string copies src into the arena and returns it as an immutable string. src must not be retained by the caller.
func (a *stringArena) string(src []byte) string {
	n := len(src)
	if n == 0 {
		return ""
	}
	if cap(a.buf)-len(a.buf) < n {
		// The current chunk can't fit src: start a fresh chunk. The old chunk is not copied or freed — the
		// strings already carved from it keep it alive. Grow geometrically, but never below n so any single
		// string fits in one chunk.
		newCap := cap(a.buf) * 2
		if newCap < 256 {
			newCap = 256
		}
		if newCap < n {
			newCap = n
		}
		a.buf = make([]byte, 0, newCap)
	}
	start := len(a.buf)
	a.buf = append(a.buf, src...)
	// Cap the returned slice at its own length so a stray append on the string's backing array (should one ever
	// occur) cannot clobber the bytes of the next string appended into the same chunk.
	b := a.buf[start : start+n : start+n]
	return unsafe.String(&b[0], n)
}

// valueTypeArena batches the per-function Code.LocalTypes slices of one code section into a few geometrically-grown
// []wasm.ValueType chunks. LocalTypes are retained on wasm.Code for the module's lifetime and are only ever read
// (range/index/len) by the engines — never appended to or mutated after decode — so handing back capacity-capped
// subslices of a shared chunk has identical semantics to N separate slices, with far fewer allocations.
type valueTypeArena struct {
	buf []wasm.ValueType
}

// alloc returns a length-n subslice of the arena for the caller to fill by index. n must be > 0. The returned
// slice's capacity is capped to n so a later append reallocates instead of clobbering the next function's locals.
func (a *valueTypeArena) alloc(n int) []wasm.ValueType {
	if cap(a.buf)-len(a.buf) < n {
		// Current chunk can't fit n: start a fresh one. The old chunk stays alive via slices already carved from
		// it. Grow geometrically but never below n so any single function's locals fit in one chunk.
		newCap := cap(a.buf) * 2
		if newCap < 64 {
			newCap = 64
		}
		if newCap < n {
			newCap = n
		}
		a.buf = make([]wasm.ValueType, 0, newCap)
	}
	start := len(a.buf)
	a.buf = a.buf[:start+n]
	return a.buf[start : start+n : start+n]
}

// readByte returns buf[offset] and offset+1, or io.EOF if offset is out of range. It is the slice-indexed
// equivalent of (*bytes.Reader).ReadByte.
func readByte(buf []byte, offset int) (b byte, newOffset int, err error) {
	if offset >= len(buf) {
		return 0, offset, io.EOF
	}
	return buf[offset], offset + 1, nil
}

// readBytes returns buf[offset:offset+n] and offset+n. It replicates io.ReadFull's error semantics: io.EOF if
// nothing at all is available, io.ErrUnexpectedEOF if some but not all of the n bytes are available.
func readBytes(buf []byte, offset int, n int) (data []byte, newOffset int, err error) {
	if n == 0 {
		return nil, offset, nil
	}
	avail := len(buf) - offset
	if avail <= 0 {
		return nil, offset, io.EOF
	}
	if avail < n {
		return nil, offset, io.ErrUnexpectedEOF
	}
	return buf[offset : offset+n], offset + n, nil
}
