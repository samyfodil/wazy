package binary

import (
	"io"

	"github.com/samyfodil/wazy/internal/wasm"
)

// decodeCustomSection deserializes the data **not** associated with the "name" key in SectionIDCustom.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#custom-section%E2%91%A0
func decodeCustomSection(buf []byte, offset int, name string, limit uint64) (result *wasm.CustomSection, newOffset int, err error) {
	if offset >= len(buf) {
		return nil, offset, io.EOF
	}

	// Subslice the section rather than make+copy: custom sections (notably the
	// large .debug_* DWARF sections, copied on every compile even though they are
	// consumed only when formatting an error stack trace) are read-only views into
	// the source binary. n matches what copy() would have transferred.
	n := int(limit)
	if avail := len(buf) - offset; n > avail {
		n = avail
	}
	result = &wasm.CustomSection{
		Name: name,
		Data: buf[offset : offset+n],
	}
	return result, offset + n, nil
}
