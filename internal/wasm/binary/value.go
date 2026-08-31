package binary

import (
	"fmt"
	"unicode/utf8"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeValueTypes(enabledFeatures api.CoreFeatures, buf []byte, offset int, num uint32, arena *valueTypeArena) ([]wasm.ValueType, int, error) {
	if num == 0 {
		return nil, offset, nil
	}

	// Batch the params/results of every function type in a section into the shared arena. FunctionType.Params and
	// .Results are only ever read (indexed/ranged/appended-from) after decode, never mutated in place, so sharing
	// one backing array across function types is safe; the returned subslice is capacity-capped for defense.
	ret := arena.alloc(int(num))
	for i := uint32(0); i < num; i++ {
		vt, o, err := decodeValueType(enabledFeatures, buf, offset)
		if err != nil {
			return nil, offset, err
		}
		offset = o
		ret[i] = vt
	}
	return ret, offset, nil
}

// decodeValueType decodes a single value type from buf[offset:], returning it and the offset after it. It is
// split out of decodeValueTypes so that single-value-type callers (e.g. decodeGlobalType) don't need to allocate
// a 1-element slice just to extract one wasm.ValueType.
func decodeValueType(enabledFeatures api.CoreFeatures, buf []byte, offset int) (wasm.ValueType, int, error) {
	b, offset, err := readByte(buf, offset)
	if err != nil {
		return 0, offset, err
	}
	switch b {
	case wasm.ValueTypeI32.Kind(), wasm.ValueTypeF32.Kind(), wasm.ValueTypeI64.Kind(), wasm.ValueTypeF64.Kind(),
		wasm.ValueTypeExternref.Kind(), wasm.ValueTypeFuncref.Kind(), wasm.ValueTypeV128.Kind(),
		wasm.ValueTypeExnref.Kind():
		return wasm.ValueType(b), offset, nil
	case wasm.RefPrefixNullable, wasm.RefPrefixNonNullable:
		return decodeRefType(enabledFeatures, buf, offset, b == wasm.RefPrefixNullable)
	default:
		if vt := wasm.ValueType(b); vt.IsGCHeapType() {
			// A GC abstract heap type in its short (nullable) spelling, e.g. 0x6e for anyref.
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureGC); err != nil {
				return 0, offset, fmt.Errorf("value type %s is invalid as %w", wasm.ValueTypeName(vt), err)
			}
			return vt, offset, nil
		}
		return 0, offset, fmt.Errorf("invalid value type: %d", b)
	}
}

// decodeStorageType decodes a struct field or array element type: a value type, or one of the packed types
// i8 (0x78) and i16 (0x77), which are valid nowhere else.
func decodeStorageType(enabledFeatures api.CoreFeatures, buf []byte, offset int) (wasm.ValueType, int, error) {
	b, o, err := readByte(buf, offset)
	if err != nil {
		return 0, offset, err
	}
	switch wasm.ValueType(b) {
	case wasm.ValueTypeI8, wasm.ValueTypeI16:
		return wasm.ValueType(b), o, nil
	}
	return decodeValueType(enabledFeatures, buf, offset)
}

// decodeRefType decodes a heap type from buf[offset:] and returns the corresponding ValueType with the given
// nullability, and the offset after the heap type. Abstract nullable refs are desugared to their short forms:
//   - (ref null func)   -> funcref
//   - (ref null extern) -> externref
//   - (ref null exn)    -> exnref
func decodeRefType(enabledFeatures api.CoreFeatures, buf []byte, offset int, nullable bool) (wasm.ValueType, int, error) {
	ht, n, err := leb128.LoadInt33AsInt64(buf[offset:])
	if err != nil {
		return 0, offset, fmt.Errorf("read ref heap type: %w", err)
	}
	offset += int(n)
	var vt wasm.ValueType
	switch ht {
	case wasm.HeapTypeFunc:
		vt = wasm.ValueTypeFuncref
	case wasm.HeapTypeExtern:
		vt = wasm.ValueTypeExternref
	case wasm.HeapTypeExn:
		vt = wasm.ValueTypeExnref
	default:
		if ht < 0 {
			abstract, ok := wasm.AbstractHeapTypeValueType(ht)
			if !ok {
				return 0, offset, fmt.Errorf("unknown abstract heap type: %d", ht)
			}
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureGC); err != nil {
				return 0, offset, fmt.Errorf("heap type %s is invalid as %w", wasm.ValueTypeName(abstract), err)
			}
			vt = abstract
			break
		}
		vt = wasm.ValueTypeConcreteRef(uint32(ht), nullable)
	}
	if !nullable {
		vt = vt.AsNonNullable()
	}
	return vt, offset, nil
}

// decodeUTF8Raw reads and validates a size-prefixed UTF-8 string from buf[offset:], returning the raw bytes
// (aliasing buf) and the offset after them. The returned slice MUST NOT be retained past DecodeModule: callers
// copy it (via a stringArena, or bytes.Equal-comparison against an already-owned string) before storing it.
// Error/offset semantics match the previous decodeUTF8 exactly: the size-read error returns the pre-size offset;
// the read/validation errors return the post-size offset; a zero-size string returns (nil, post-size offset, nil).
func decodeUTF8Raw(buf []byte, offset int, contextFormat string, contextArgs ...uint32) ([]byte, int, error) {
	size, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("failed to read %s size: %w", errContext(contextFormat, contextArgs), err)
	}
	offset += int(n)

	if size == 0 {
		return nil, offset, nil
	}

	strBytes, newOffset, err := readBytes(buf, offset, int(size))
	if err != nil {
		return nil, offset, fmt.Errorf("failed to read %s: %w", errContext(contextFormat, contextArgs), err)
	}

	if !utf8.Valid(strBytes) {
		return nil, offset, fmt.Errorf("%s is not valid UTF-8", errContext(contextFormat, contextArgs))
	}

	return strBytes, newOffset, nil
}

// errContext fills contextFormat's verbs, of which the callers have at most two (the name section's function and
// local indices). It exists so the arguments can be typed uint32 up to here rather than interface{}: an index
// above 255 heap-boxes on conversion, and on the success path -- once per named function and local -- that box is
// never read. Formatting stays where it belongs, on the error path.
func errContext(contextFormat string, contextArgs []uint32) string {
	switch len(contextArgs) {
	case 1:
		return fmt.Sprintf(contextFormat, contextArgs[0])
	case 2:
		return fmt.Sprintf(contextFormat, contextArgs[0], contextArgs[1])
	}
	return contextFormat
}

// decodeUTF8 decodes a size prefixed string from buf[offset:], returning it (arena-owned so it doesn't alias the
// caller's input slice) and the offset after it. contextFormat and contextArgs apply an error format when present.
func decodeUTF8(buf []byte, offset int, arena *stringArena, contextFormat string, contextArgs ...uint32) (string, int, error) {
	raw, newOffset, err := decodeUTF8Raw(buf, offset, contextFormat, contextArgs...)
	if err != nil {
		return "", newOffset, err
	}
	return arena.string(raw), newOffset, nil
}
