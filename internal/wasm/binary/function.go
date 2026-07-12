package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeFunctionType(enabledFeatures api.CoreFeatures, buf []byte, offset int, arena *valueTypeArena, ret *wasm.FunctionType) (int, error) {
	b, offset, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read leading byte: %w", err)
	}

	if b != 0x60 {
		return offset, fmt.Errorf("%w: %#x != 0x60", ErrInvalidByte, b)
	}

	paramCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, fmt.Errorf("could not read parameter count: %w", err)
	}
	offset += int(n)

	paramTypes, offset, err := decodeValueTypes(buf, offset, paramCount, arena)
	if err != nil {
		return offset, fmt.Errorf("could not read parameter types: %w", err)
	}

	resultCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, fmt.Errorf("could not read result count: %w", err)
	}
	offset += int(n)

	// Guard >1.0 feature multi-value
	if resultCount > 1 {
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureMultiValue); err != nil {
			return offset, fmt.Errorf("multiple result types invalid as %v", err)
		}
	}

	resultTypes, offset, err := decodeValueTypes(buf, offset, resultCount, arena)
	if err != nil {
		return offset, fmt.Errorf("could not read result types: %w", err)
	}

	ret.Params = paramTypes
	ret.Results = resultTypes

	// Eagerly cache the key here, while decoding is single-threaded. This is NOT deferred to first use because
	// key() lazily populates FunctionType.string, and that first population can happen concurrently: the runtime
	// call_indirect helpers reach it through Store.GetFunctionTypeID on a *shared* FunctionType — e.g.
	// internal/emscripten (*InvokeFunc).Call and experimental/table.LookupFunction both call key() while
	// executing guest code, which is multi-goroutine. Populating it now makes every later key()/String() a pure
	// read of an already-set field. key() itself is cheap (single pre-sized Builder), so eager caching is cheap.
	_ = ret.String()

	return offset, nil
}
