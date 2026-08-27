package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// The leading bytes of a type section entry. 0x60 is the only one WebAssembly 1.0 has; the rest come from the
// GC proposal. 0x4e (rec) is consumed by decodeTypeSection before this is reached.
const (
	compositeFunc   = 0x60
	compositeStruct = 0x5f
	compositeArray  = 0x5e
	subFinal        = 0x4f
	subOpen         = 0x50
)

// decodeDefinedType decodes one entry of the type section: a composite type, optionally wrapped in the GC
// proposal's `sub`/`sub final` declaration of a supertype.
func decodeDefinedType(enabledFeatures api.CoreFeatures, buf []byte, offset int, arena *valueTypeArena, ret *wasm.FunctionType) (int, error) {
	b, o, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read leading byte: %w", err)
	}

	if b == subFinal || b == subOpen {
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureGC); err != nil {
			return offset, fmt.Errorf("subtype declaration is invalid as %w", err)
		}
		offset = o
		ret.Extensible = b == subOpen

		count, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("read supertype count: %w", err)
		}
		offset += int(n)
		// The GC proposal's MVP allows at most one supertype, though the binary format encodes a vector.
		if count > 1 {
			return offset, fmt.Errorf("more than one supertype is not supported: %d", count)
		}
		if count == 1 {
			idx, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return offset, fmt.Errorf("read supertype index: %w", err)
			}
			offset += int(n)
			ret.Supertype = idx
			ret.HasSupertype = true
		}
	}

	return decodeCompositeType(enabledFeatures, buf, offset, arena, ret)
}

func decodeCompositeType(enabledFeatures api.CoreFeatures, buf []byte, offset int, arena *valueTypeArena, ret *wasm.FunctionType) (int, error) {
	b, offset, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read leading byte: %w", err)
	}

	switch b {
	case compositeFunc:
		offset, err = decodeFuncBody(enabledFeatures, buf, offset, arena, ret)
	case compositeStruct, compositeArray:
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureGC); err != nil {
			return offset, fmt.Errorf("%s type is invalid as %w", map[byte]string{compositeStruct: "struct", compositeArray: "array"}[b], err)
		}
		offset, err = decodeAggregateBody(enabledFeatures, buf, offset, b == compositeArray, ret)
	default:
		return offset, fmt.Errorf("%w: %#x != 0x60", ErrInvalidByte, b)
	}
	if err != nil {
		return offset, err
	}

	return offset, nil
}

func decodeFuncBody(enabledFeatures api.CoreFeatures, buf []byte, offset int, arena *valueTypeArena, ret *wasm.FunctionType) (int, error) {
	paramCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, fmt.Errorf("could not read parameter count: %w", err)
	}
	offset += int(n)

	paramTypes, offset, err := decodeValueTypes(enabledFeatures, buf, offset, paramCount, arena)
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

	resultTypes, offset, err := decodeValueTypes(enabledFeatures, buf, offset, resultCount, arena)
	if err != nil {
		return offset, fmt.Errorf("could not read result types: %w", err)
	}

	ret.Params = paramTypes
	ret.Results = resultTypes
	return offset, nil
}

// decodeAggregateBody decodes a struct's field vector or an array's single element type. An array is stored as
// a one-field aggregate so struct and array share every field-shaped code path.
func decodeAggregateBody(enabledFeatures api.CoreFeatures, buf []byte, offset int, isArray bool, ret *wasm.FunctionType) (int, error) {
	count := uint32(1)
	if isArray {
		ret.CompositeKind = wasm.CompositeKindArray
	} else {
		ret.CompositeKind = wasm.CompositeKindStruct
		c, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("could not read field count: %w", err)
		}
		count, offset = c, offset+int(n)
	}

	fields := make([]wasm.FieldType, count)
	for i := range fields {
		st, o, err := decodeStorageType(enabledFeatures, buf, offset)
		if err != nil {
			return offset, fmt.Errorf("could not read field[%d] type: %w", i, err)
		}
		offset = o
		mut, o, err := readByte(buf, offset)
		if err != nil {
			return offset, fmt.Errorf("could not read field[%d] mutability: %w", i, err)
		}
		if mut > 1 {
			return offset, fmt.Errorf("invalid field[%d] mutability: %#x", i, mut)
		}
		offset = o
		fields[i] = wasm.FieldType{Type: st, Mutable: mut == 1}
	}
	ret.Fields = fields
	ret.CacheFieldSlots()
	return offset, nil
}
