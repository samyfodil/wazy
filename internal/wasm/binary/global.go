package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasm"
)

// decodeGlobal decodes the api.Global from buf[offset:] with the WebAssembly 1.0 (20191205) Binary Format,
// returning the offset after it.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-global
func decodeGlobal(buf []byte, offset int, enabledFeatures api.CoreFeatures, ret *wasm.Global) (int, error) {
	var err error
	ret.Type, offset, err = decodeGlobalType(buf, offset)
	if err != nil {
		return offset, err
	}

	return decodeConstantExpression(buf, offset, enabledFeatures, &ret.Init)
}

// decodeGlobalType returns the wasm.GlobalType decoded from buf[offset:] with the WebAssembly 1.0 (20191205)
// Binary Format, and the offset after it.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-globaltype
func decodeGlobalType(buf []byte, offset int) (wasm.GlobalType, int, error) {
	vt, offset, err := decodeValueType(buf, offset)
	if err != nil {
		return wasm.GlobalType{}, offset, fmt.Errorf("read value type: %w", err)
	}

	ret := wasm.GlobalType{
		ValType: vt,
	}

	b, offset, err := readByte(buf, offset)
	if err != nil {
		return wasm.GlobalType{}, offset, fmt.Errorf("read mutablity: %w", err)
	}

	switch mut := b; mut {
	case 0x00: // not mutable
	case 0x01: // mutable
		ret.Mutable = true
	default:
		return wasm.GlobalType{}, offset, fmt.Errorf("%w for mutability: %#x != 0x00 or 0x01", ErrInvalidByte, mut)
	}
	return ret, offset, nil
}
