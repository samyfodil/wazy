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

	data := make([]byte, limit)
	n := copy(data, buf[offset:])

	result = &wasm.CustomSection{
		Name: name,
		Data: data,
	}
	return result, offset + n, nil
}
