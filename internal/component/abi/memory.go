package abi

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// Realloc is the guest memory allocator used when lowering strings/lists into
// linear memory. It behaves like the WebAssembly memory.grow/realloc: given an
// existing (origPtr, origSize), it allocates newSize bytes aligned to align,
// returning the new pointer (unaligned when align == 1); for a fresh grow,
// origPtr and origSize are 0.
//
// It is a struct rather than a bare func so it can carry the call context WITHOUT
// a per-call closure: the Call func is built once at bind time (it captures only
// the core cabi_realloc function), and Ctx is filled in per call. A zero Call
// means the module exports no cabi_realloc, and grow fails loud. Passed by value
// through the lower/store tree; it stays on the stack (nothing retains it).
type Realloc struct {
	Ctx  context.Context
	Call func(ctx context.Context, origPtr, origSize, align, newSize uint32) (uint32, error)
}

// Grow performs one allocation, threading Ctx into the cached Call.
func (r Realloc) Grow(origPtr, origSize, align, newSize uint32) (uint32, error) {
	if r.Call == nil {
		return 0, fmt.Errorf("component/abi: memory allocation requires a \"cabi_realloc\" export on the core module, which is not present")
	}
	return r.Call(r.Ctx, origPtr, origSize, align, newSize)
}

// ReallocFunc adapts a context-free allocator into a Realloc (the ctx is
// ignored). For simple in-memory allocators -- notably tests -- that don't need
// the call context.
func ReallocFunc(fn func(origPtr, origSize, align, newSize uint32) (uint32, error)) Realloc {
	if fn == nil {
		return Realloc{}
	}
	return Realloc{Call: func(_ context.Context, o, os, a, n uint32) (uint32, error) { return fn(o, os, a, n) }}
}

// Load reads a value of the given type from memory at the given pointer.
// This mirrors the canonical ABI load() function.
func Load(mem []byte, ptr uint32, t bintype.TypeDesc, resolve Resolver) (Value, error) {
	// Check alignment and bounds
	align, err := Alignment(t, resolve)
	if err != nil {
		return nil, err
	}
	if ptr != Align(ptr, align) {
		return nil, fmt.Errorf("load: pointer %d not aligned to %d", ptr, align)
	}

	size, err := Size(t, resolve)
	if err != nil {
		return nil, err
	}
	if uint32(len(mem)) < ptr+size {
		return nil, fmt.Errorf("load: buffer overflow: ptr=%d size=%d mem_len=%d", ptr, size, len(mem))
	}

	return loadValue(mem, ptr, t, resolve)
}

func loadValue(mem []byte, ptr uint32, t bintype.TypeDesc, resolve Resolver) (Value, error) {
	switch desc := t.(type) {
	case bintype.PrimitiveDesc:
		return loadPrimitive(mem, ptr, desc.Prim)

	case bintype.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return loadList(mem, ptr, elemType, resolve)

	case bintype.RecordDesc:
		return loadRecord(mem, ptr, desc, resolve)

	case bintype.VariantDesc:
		return loadVariant(mem, ptr, desc, resolve)

	case bintype.TupleDesc:
		return loadTuple(mem, ptr, desc, resolve)

	case bintype.FlagsDesc:
		return loadFlags(mem, ptr, desc)

	case bintype.EnumDesc:
		return loadEnum(mem, ptr, desc)

	case bintype.OptionDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return loadOption(mem, ptr, elemType, resolve)

	case bintype.ResultDesc:
		return loadResult(mem, ptr, desc, resolve)

	case bintype.OwnDesc, bintype.BorrowDesc:
		return loadInt(mem, ptr, 4, false)

	default:
		return nil, fmt.Errorf("load: unsupported type %T", t)
	}
}

func loadPrimitive(mem []byte, ptr uint32, prim string) (Value, error) {
	switch prim {
	case "bool":
		v, err := loadInt(mem, ptr, 1, false)
		if err != nil {
			return nil, err
		}
		return v.(uint32) != 0, nil

	case "u8":
		return loadInt(mem, ptr, 1, false)
	case "u16":
		return loadInt(mem, ptr, 2, false)
	case "u32":
		return loadInt(mem, ptr, 4, false)

	case "s8":
		return loadInt(mem, ptr, 1, true)
	case "s16":
		return loadInt(mem, ptr, 2, true)
	case "s32":
		return loadInt(mem, ptr, 4, true)

	case "u64":
		return loadInt(mem, ptr, 8, false)
	case "s64":
		return loadInt(mem, ptr, 8, true)

	case "f32":
		bits, err := loadInt(mem, ptr, 4, false)
		if err != nil {
			return nil, err
		}
		i32 := uint32(bits.(uint32))
		f := math.Float32frombits(i32)
		return f, nil

	case "f64":
		bits, err := loadInt(mem, ptr, 8, false)
		if err != nil {
			return nil, err
		}
		i64 := uint64(bits.(uint64))
		f := math.Float64frombits(i64)
		return f, nil

	case "char":
		v, err := loadInt(mem, ptr, 4, false)
		if err != nil {
			return nil, err
		}
		i := v.(uint32)
		if i >= 0x110000 {
			return nil, fmt.Errorf("load char: value %d out of range", i)
		}
		if i >= 0xD800 && i <= 0xDFFF {
			return nil, fmt.Errorf("load char: surrogate half %d not allowed", i)
		}
		return rune(i), nil

	case "string":
		return loadString(mem, ptr)

	default:
		return nil, fmt.Errorf("load: unknown primitive %s", prim)
	}
}

// loadInt reads nbytes from memory at ptr as a little-endian integer.
func loadInt(mem []byte, ptr uint32, nbytes uint32, signed bool) (Value, error) {
	if uint32(len(mem)) < ptr+nbytes {
		return nil, fmt.Errorf("loadInt: buffer overflow at ptr=%d nbytes=%d mem_len=%d", ptr, nbytes, len(mem))
	}

	bytes := mem[ptr : ptr+nbytes]
	switch nbytes {
	case 1:
		v := uint32(bytes[0])
		if signed {
			if bytes[0]&0x80 != 0 {
				return int32(int8(bytes[0])), nil
			}
		}
		return v, nil

	case 2:
		v := binary.LittleEndian.Uint16(bytes)
		if signed {
			return int32(int16(v)), nil
		}
		return uint32(v), nil

	case 4:
		v := binary.LittleEndian.Uint32(bytes)
		if signed {
			return int32(v), nil
		}
		return v, nil

	case 8:
		v := binary.LittleEndian.Uint64(bytes)
		if signed {
			return int64(v), nil
		}
		return v, nil

	default:
		return nil, fmt.Errorf("loadInt: invalid nbytes %d", nbytes)
	}
}

// readU32LE reads an unsigned, little-endian 4-byte integer at ptr, exactly
// like loadInt(mem, ptr, 4, false) but returning a raw uint32 instead of a
// boxed Value. Factored out for loadString's own ptr/len fields, which are
// consumed as unboxed uint32s one line later (see loadString) -- boxing them
// into Value only to immediately type-assert back out is a wasted allocation
// on every string load, the hottest allocation on the string round-trip
// path. Error text matches loadInt's for the same failure so callers
// (including loadString) see identical messages.
func readU32LE(mem []byte, ptr uint32) (uint32, error) {
	const nbytes = 4
	if uint32(len(mem)) < ptr+nbytes {
		return 0, fmt.Errorf("loadInt: buffer overflow at ptr=%d nbytes=%d mem_len=%d", ptr, nbytes, len(mem))
	}
	return binary.LittleEndian.Uint32(mem[ptr : ptr+nbytes]), nil
}

// loadString reads a string (ptr, length in UTF-8 bytes) from memory.
// Currently supports UTF-8 only.
func loadString(mem []byte, ptr uint32) (Value, error) {
	// String is stored as: [ptr:4/8][len:4/8]
	// For now, assuming 32-bit pointers (4 bytes each)
	ptrSize := uint32(4)

	strPtr, err := readU32LE(mem, ptr)
	if err != nil {
		return nil, err
	}
	strLen, err := readU32LE(mem, ptr+ptrSize)
	if err != nil {
		return nil, err
	}

	s, err := loadStringFromRange(mem, strPtr, strLen)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// loadStringFromRange reads a UTF-8 string of byteLen bytes starting at ptr.
// This mirrors the canonical ABI's load_string_from_range() (definitions.py).
// It is factored out of loadString so that the flat-ABI path (liftFlatString,
// where ptr/len arrive as two separate core values instead of already being
// packed together in memory) can reuse the exact same bytes-to-string logic
// instead of a second implementation.
func loadStringFromRange(mem []byte, ptr, byteLen uint32) (string, error) {
	// UTF-8: byte length is the code unit length.
	if uint32(len(mem)) < ptr+byteLen {
		return "", fmt.Errorf("loadStringFromRange: string buffer overflow at ptr=%d len=%d mem_len=%d", ptr, byteLen, len(mem))
	}
	b := mem[ptr : ptr+byteLen]
	// The canonical ABI's load_string_from_range traps on malformed UTF-8
	// (invalid bytes or an incomplete trailing sequence) rather than lifting a
	// lossy string -- definitions.py decodes with 'strict' and errors out.
	if !utf8.Valid(b) {
		return "", fmt.Errorf("loadStringFromRange: invalid utf-8 in string at ptr=%d len=%d", ptr, byteLen)
	}
	return string(b), nil
}

func loadList(mem []byte, ptr uint32, elemType bintype.TypeDesc, resolve Resolver) (Value, error) {
	// List is stored as: [ptr:4/8][len:4/8]
	// For now, assuming 32-bit pointers (4 bytes each)
	ptrSize := uint32(4)

	listPtr, err := loadInt(mem, ptr, ptrSize, false)
	if err != nil {
		return nil, err
	}
	listLen, err := loadInt(mem, ptr+ptrSize, ptrSize, false)
	if err != nil {
		return nil, err
	}

	return loadListFromRange(mem, listPtr.(uint32), listLen.(uint32), elemType, resolve)
}

// loadListFromRange reads `length` elements of elemType starting at ptr.
// This mirrors the canonical ABI's load_list_from_range() (definitions.py).
// It is factored out of loadList so that the flat-ABI path (liftFlatList,
// where ptr/len arrive as two separate core values instead of already being
// packed together in memory) can reuse the exact same element-loading loop
// instead of a second implementation.
func loadListFromRange(mem []byte, ptr, length uint32, elemType bintype.TypeDesc, resolve Resolver) ([]Value, error) {
	elemSize, err := Size(elemType, resolve)
	if err != nil {
		return nil, err
	}

	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return nil, err
	}

	// Check bounds
	byteLen := length * elemSize
	if uint32(len(mem)) < ptr+byteLen {
		return nil, fmt.Errorf("loadListFromRange: list buffer overflow at ptr=%d len=%d mem_len=%d", ptr, byteLen, len(mem))
	}

	// Check alignment
	if ptr != Align(ptr, elemAlign) {
		return nil, fmt.Errorf("loadListFromRange: list pointer %d not aligned to %d", ptr, elemAlign)
	}

	result := make([]Value, length)
	for i := range length {
		v, err := loadValue(mem, ptr+i*elemSize, elemType, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadListFromRange[%d]: %w", i, err)
		}
		result[i] = v
	}
	return result, nil
}

func loadRecord(mem []byte, ptr uint32, desc bintype.RecordDesc, resolve Resolver) (Value, error) {
	result := make([]Value, len(desc.Fields))
	offset := ptr

	for i, field := range desc.Fields {
		fieldType, err := resolveType(&field.Type, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadRecord: field %s: %w", field.Name, err)
		}

		fieldAlign, err := Alignment(fieldType, resolve)
		if err != nil {
			return nil, err
		}
		offset = Align(offset, fieldAlign)

		v, err := loadValue(mem, offset, fieldType, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadRecord: field %s: %w", field.Name, err)
		}
		result[i] = v

		fieldSize, err := Size(fieldType, resolve)
		if err != nil {
			return nil, err
		}
		offset += fieldSize
	}

	return result, nil
}

func loadVariant(mem []byte, ptr uint32, desc bintype.VariantDesc, resolve Resolver) (Value, error) {
	// Read discriminant
	discType := DiscriminantType(len(desc.Cases))
	discSize, err := sizePrimitive(discType)
	if err != nil {
		return nil, err
	}

	discVal, err := loadInt(mem, ptr, discSize, false)
	if err != nil {
		return nil, err
	}
	caseIdx := discVal.(uint32)

	if int(caseIdx) >= len(desc.Cases) {
		return nil, fmt.Errorf("loadVariant: case index %d out of range [0,%d)", caseIdx, len(desc.Cases))
	}

	// Compute offset to payload
	offset := ptr + discSize
	maxCaseAlign, err := MaxCaseAlignment(desc.Cases, resolve)
	if err != nil {
		return nil, err
	}
	offset = Align(offset, maxCaseAlign)

	// Load payload if present
	c := desc.Cases[caseIdx]
	var payload Value
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return nil, err
		}
		payload, err = loadValue(mem, offset, caseType, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadVariant case %d: %w", caseIdx, err)
		}
	}

	return VariantValue{Disc: caseIdx, Payload: payload}, nil
}

func loadTuple(mem []byte, ptr uint32, desc bintype.TupleDesc, resolve Resolver) (Value, error) {
	result := make([]Value, len(desc.Elements))
	offset := ptr

	for i, elemRef := range desc.Elements {
		elemType, err := resolveType(&elemRef, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadTuple: element %d: %w", i, err)
		}

		elemAlign, err := Alignment(elemType, resolve)
		if err != nil {
			return nil, err
		}
		offset = Align(offset, elemAlign)

		v, err := loadValue(mem, offset, elemType, resolve)
		if err != nil {
			return nil, fmt.Errorf("loadTuple: element %d: %w", i, err)
		}
		result[i] = v

		elemSize, err := Size(elemType, resolve)
		if err != nil {
			return nil, err
		}
		offset += elemSize
	}

	return result, nil
}

func loadFlags(mem []byte, ptr uint32, desc bintype.FlagsDesc) (Value, error) {
	flagsSize, err := sizeFlagsNumLabels(len(desc.Names))
	if err != nil {
		return nil, err
	}

	bits, err := loadInt(mem, ptr, flagsSize, false)
	if err != nil {
		return nil, err
	}

	return bits, nil
}

func loadEnum(mem []byte, ptr uint32, desc bintype.EnumDesc) (Value, error) {
	enumSize, err := sizeEnumNumCases(len(desc.Cases))
	if err != nil {
		return nil, err
	}

	caseIdx, err := loadInt(mem, ptr, enumSize, false)
	if err != nil {
		return nil, err
	}

	idx := caseIdx.(uint32)
	if int(idx) >= len(desc.Cases) {
		return nil, fmt.Errorf("loadEnum: case index %d out of range [0,%d)", idx, len(desc.Cases))
	}

	return idx, nil
}

func loadOption(mem []byte, ptr uint32, elemType bintype.TypeDesc, resolve Resolver) (Value, error) {
	// Option is a variant with discriminant (0=none, 1=some)
	// Read discriminant (u8)
	disc, err := loadInt(mem, ptr, 1, false)
	if err != nil {
		return nil, err
	}

	discIdx := disc.(uint32)
	offset := ptr + 1

	// Align to element type alignment
	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return nil, err
	}
	offset = Align(offset, elemAlign)

	switch discIdx {
	case 0:
		// None - represented as nil Value
		return nil, nil
	case 1:
		// Some
		return loadValue(mem, offset, elemType, resolve)
	default:
		return nil, fmt.Errorf("loadOption: invalid discriminant %d", discIdx)
	}
}

func loadResult(mem []byte, ptr uint32, desc bintype.ResultDesc, resolve Resolver) (Value, error) {
	// Result is a variant with discriminant (0=ok, 1=err)
	disc, err := loadInt(mem, ptr, 1, false)
	if err != nil {
		return nil, err
	}

	discIdx := disc.(uint32)
	offset := ptr + 1

	// Compute max alignment of both arms
	maxAlign := uint32(1)
	if desc.Ok != nil {
		okType, err := resolveType(desc.Ok, resolve)
		if err != nil {
			return nil, err
		}
		okAlign, err := Alignment(okType, resolve)
		if err != nil {
			return nil, err
		}
		if okAlign > maxAlign {
			maxAlign = okAlign
		}
	}
	if desc.Err != nil {
		errType, err := resolveType(desc.Err, resolve)
		if err != nil {
			return nil, err
		}
		errAlign, err := Alignment(errType, resolve)
		if err != nil {
			return nil, err
		}
		if errAlign > maxAlign {
			maxAlign = errAlign
		}
	}
	offset = Align(offset, maxAlign)

	switch discIdx {
	case 0:
		// Ok
		var payload Value
		if desc.Ok != nil {
			okType, err := resolveType(desc.Ok, resolve)
			if err != nil {
				return nil, err
			}
			payload, err = loadValue(mem, offset, okType, resolve)
			if err != nil {
				return nil, fmt.Errorf("loadResult ok: %w", err)
			}
		}
		return ResultValue{IsErr: false, Payload: payload}, nil
	case 1:
		// Err
		var payload Value
		if desc.Err != nil {
			errType, err := resolveType(desc.Err, resolve)
			if err != nil {
				return nil, err
			}
			payload, err = loadValue(mem, offset, errType, resolve)
			if err != nil {
				return nil, fmt.Errorf("loadResult err: %w", err)
			}
		}
		return ResultValue{IsErr: true, Payload: payload}, nil
	default:
		return nil, fmt.Errorf("loadResult: invalid discriminant %d", discIdx)
	}
}

// Store writes a value of the given type to memory at the given pointer.
// This mirrors the canonical ABI store() function.
func Store(mem []byte, ptr uint32, t bintype.TypeDesc, v Value, resolve Resolver, realloc Realloc) error {
	// Check alignment and bounds
	align, err := Alignment(t, resolve)
	if err != nil {
		return err
	}
	if ptr != Align(ptr, align) {
		return fmt.Errorf("store: pointer %d not aligned to %d", ptr, align)
	}

	size, err := Size(t, resolve)
	if err != nil {
		return err
	}
	if uint32(len(mem)) < ptr+size {
		return fmt.Errorf("store: buffer overflow: ptr=%d size=%d mem_len=%d", ptr, size, len(mem))
	}

	return storeValue(mem, ptr, t, v, resolve, realloc)
}

func storeValue(mem []byte, ptr uint32, t bintype.TypeDesc, v Value, resolve Resolver, realloc Realloc) error {
	switch desc := t.(type) {
	case bintype.PrimitiveDesc:
		return storePrimitive(mem, ptr, desc.Prim, v, realloc)

	case bintype.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return err
		}
		return storeList(mem, ptr, v, elemType, resolve, realloc)

	case bintype.RecordDesc:
		return storeRecord(mem, ptr, v, desc, resolve, realloc)

	case bintype.VariantDesc:
		return storeVariant(mem, ptr, v, desc, resolve, realloc)

	case bintype.TupleDesc:
		return storeTuple(mem, ptr, v, desc, resolve, realloc)

	case bintype.FlagsDesc:
		return storeFlags(mem, ptr, v, desc)

	case bintype.EnumDesc:
		return storeEnum(mem, ptr, v, desc)

	case bintype.OptionDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return err
		}
		return storeOption(mem, ptr, v, elemType, resolve, realloc)

	case bintype.ResultDesc:
		return storeResult(mem, ptr, v, desc, resolve, realloc)

	case bintype.OwnDesc, bintype.BorrowDesc:
		if h, ok := v.(uint32); ok {
			return storeInt(mem, ptr, h, 4)
		}
		return fmt.Errorf("store: handle expected uint32, got %T", v)

	default:
		return fmt.Errorf("store: unsupported type %T", t)
	}
}

// intByteSize returns the byte size for a fixed-width integer primitive name.
func intByteSize(prim string) uint32 {
	switch prim {
	case "u8", "s8":
		return 1
	case "u16", "s16":
		return 2
	default: // u32, s32
		return 4
	}
}

func storePrimitive(mem []byte, ptr uint32, prim string, v Value, realloc Realloc) error {
	switch prim {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("store bool: expected bool, got %T", v)
		}
		var val uint32
		if b {
			val = 1
		}
		return storeInt(mem, ptr, val, 1)

	case "u8", "u16", "u32":
		if u, ok := v.(uint32); ok {
			return storeInt(mem, ptr, u, intByteSize(prim))
		}
		return fmt.Errorf("store %s: expected uint32, got %T", prim, v)

	case "s8", "s16", "s32":
		if i, ok := v.(int32); ok {
			return storeInt(mem, ptr, uint32(i), intByteSize(prim))
		}
		return fmt.Errorf("store %s: expected int32, got %T", prim, v)

	case "u64":
		if u, ok := v.(uint64); ok {
			return storeInt(mem, ptr, u, 8)
		}
		return fmt.Errorf("store u64: expected uint64, got %T", v)

	case "s64":
		if i, ok := v.(int64); ok {
			return storeInt(mem, ptr, uint64(i), 8)
		}
		return fmt.Errorf("store s64: expected int64, got %T", v)

	case "f32":
		if f, ok := v.(float32); ok {
			bits := math.Float32bits(f)
			return storeInt(mem, ptr, uint32(bits), 4)
		}
		return fmt.Errorf("store f32: expected float32, got %T", v)

	case "f64":
		if f, ok := v.(float64); ok {
			bits := math.Float64bits(f)
			return storeInt(mem, ptr, bits, 8)
		}
		return fmt.Errorf("store f64: expected float64, got %T", v)

	case "char":
		if r, ok := v.(rune); ok {
			if r < 0 || r >= 0x110000 {
				return fmt.Errorf("store char: value %d out of range", r)
			}
			if r >= 0xD800 && r <= 0xDFFF {
				return fmt.Errorf("store char: surrogate half %d not allowed", r)
			}
			return storeInt(mem, ptr, uint32(r), 4)
		}
		return fmt.Errorf("store char: expected rune, got %T", v)

	case "string":
		if s, ok := v.(string); ok {
			return storeString(mem, ptr, s, realloc)
		}
		return fmt.Errorf("store string: expected string, got %T", v)

	default:
		return fmt.Errorf("store: unknown primitive %s", prim)
	}
}

// storeInt writes an integer to memory in little-endian format.
func storeInt(mem []byte, ptr uint32, v any, nbytes uint32) error {
	if uint32(len(mem)) < ptr+nbytes {
		return fmt.Errorf("storeInt: buffer overflow at ptr=%d nbytes=%d mem_len=%d", ptr, nbytes, len(mem))
	}

	var u64Val uint64
	switch val := v.(type) {
	case uint32:
		u64Val = uint64(val)
	case int32:
		u64Val = uint64(uint32(val))
	case uint64:
		u64Val = val
	case int64:
		u64Val = uint64(val)
	default:
		return fmt.Errorf("storeInt: unsupported type %T", v)
	}

	bytes := mem[ptr : ptr+nbytes]
	switch nbytes {
	case 1:
		bytes[0] = byte(u64Val & 0xFF)
	case 2:
		binary.LittleEndian.PutUint16(bytes, uint16(u64Val&0xFFFF))
	case 4:
		binary.LittleEndian.PutUint32(bytes, uint32(u64Val&0xFFFFFFFF))
	case 8:
		binary.LittleEndian.PutUint64(bytes, u64Val)
	default:
		return fmt.Errorf("storeInt: invalid nbytes %d", nbytes)
	}
	return nil
}

// storeString writes a string to memory, using realloc for the string data.
// Currently supports UTF-8 only.
func storeString(mem []byte, ptr uint32, s string, realloc Realloc) error {
	ptrSize := uint32(4) // assuming 32-bit pointers

	newPtr, byteLen, err := allocStoreString(mem, s, realloc)
	if err != nil {
		return fmt.Errorf("storeString: %w", err)
	}

	// Store pointer and length
	if err := storeInt(mem, ptr, newPtr, ptrSize); err != nil {
		return fmt.Errorf("storeString: store ptr failed: %w", err)
	}
	if err := storeInt(mem, ptr+ptrSize, byteLen, ptrSize); err != nil {
		return fmt.Errorf("storeString: store len failed: %w", err)
	}

	return nil
}

// allocStoreString allocates room for the UTF-8 bytes of s via realloc,
// copies them into mem, and returns (dataPtr, byteLen). This mirrors the
// canonical ABI's store_string_into_range() (definitions.py). It is shared
// by storeString (the Store/Load path, which additionally writes the
// (ptr,len) pair into memory at a record/list slot) and lowerFlatString (the
// flat ABI path, which returns (ptr,len) directly as core values) so there
// is exactly one implementation of "allocate + copy string bytes".
func allocStoreString(mem []byte, s string, realloc Realloc) (uint32, uint32, error) {
	// Allocate memory for string bytes (UTF-8)
	strBytes := []byte(s)
	byteLen := uint32(len(strBytes))

	newPtr, err := realloc.Grow(0, 0, 1, byteLen)
	if err != nil {
		return 0, 0, fmt.Errorf("realloc failed: %w", err)
	}

	if uint32(len(mem)) < newPtr+byteLen {
		return 0, 0, fmt.Errorf("allocated memory out of bounds: ptr=%d size=%d", newPtr, byteLen)
	}

	// Copy string bytes to memory
	copy(mem[newPtr:newPtr+byteLen], strBytes)

	return newPtr, byteLen, nil
}

func storeList(mem []byte, ptr uint32, v Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) error {
	list, ok := v.([]Value)
	if !ok {
		return fmt.Errorf("storeList: expected []Value, got %T", v)
	}

	ptrSize := uint32(4) // assuming 32-bit pointers

	newPtr, length, err := allocStoreList(mem, list, elemType, resolve, realloc)
	if err != nil {
		return fmt.Errorf("storeList: %w", err)
	}

	// Store list pointer and length
	if err := storeInt(mem, ptr, newPtr, ptrSize); err != nil {
		return err
	}
	return storeInt(mem, ptr+ptrSize, length, ptrSize)
}

// allocStoreList allocates room for len(list) elements of elemType via
// realloc, stores each element into the allocated region, and returns
// (dataPtr, length). This mirrors the canonical ABI's
// store_list_into_range() (definitions.py). It is shared by storeList (the
// Store/Load path, which additionally writes the (ptr,len) pair into memory
// at a record/list slot) and lowerFlatList (the flat ABI path, which returns
// (ptr,len) directly as core values) so there is exactly one implementation
// of "allocate + store list elements".
func allocStoreList(mem []byte, list []Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) (uint32, uint32, error) {
	// Allocate memory for list elements
	elemSize, err := Size(elemType, resolve)
	if err != nil {
		return 0, 0, err
	}

	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return 0, 0, err
	}

	byteLen := uint32(len(list)) * elemSize
	newPtr, err := realloc.Grow(0, 0, elemAlign, byteLen)
	if err != nil {
		return 0, 0, fmt.Errorf("realloc failed: %w", err)
	}

	if uint32(len(mem)) < newPtr+byteLen {
		return 0, 0, fmt.Errorf("allocated memory out of bounds: ptr=%d size=%d", newPtr, byteLen)
	}

	// Store each element
	for i, elem := range list {
		elemPtr := newPtr + uint32(i)*elemSize
		if err := storeValue(mem, elemPtr, elemType, elem, resolve, realloc); err != nil {
			return 0, 0, fmt.Errorf("[%d]: %w", i, err)
		}
	}

	return newPtr, uint32(len(list)), nil
}

func storeRecord(mem []byte, ptr uint32, v Value, desc bintype.RecordDesc, resolve Resolver, realloc Realloc) error {
	fields, ok := v.([]Value)
	if !ok {
		return fmt.Errorf("storeRecord: expected []Value, got %T", v)
	}

	if len(fields) != len(desc.Fields) {
		return fmt.Errorf("storeRecord: expected %d fields, got %d", len(desc.Fields), len(fields))
	}

	offset := ptr
	for i, field := range desc.Fields {
		fieldType, err := resolveType(&field.Type, resolve)
		if err != nil {
			return fmt.Errorf("storeRecord: field %s: %w", field.Name, err)
		}

		fieldAlign, err := Alignment(fieldType, resolve)
		if err != nil {
			return err
		}
		offset = Align(offset, fieldAlign)

		if err := storeValue(mem, offset, fieldType, fields[i], resolve, realloc); err != nil {
			return fmt.Errorf("storeRecord: field %s: %w", field.Name, err)
		}

		fieldSize, err := Size(fieldType, resolve)
		if err != nil {
			return err
		}
		offset += fieldSize
	}

	return nil
}

func storeVariant(mem []byte, ptr uint32, v Value, desc bintype.VariantDesc, resolve Resolver, realloc Realloc) error {
	vv, ok := v.(VariantValue)
	if !ok {
		return fmt.Errorf("storeVariant: expected VariantValue, got %T", v)
	}

	if int(vv.Disc) >= len(desc.Cases) {
		return fmt.Errorf("storeVariant: case index %d out of range [0,%d)", vv.Disc, len(desc.Cases))
	}

	// Store discriminant
	discType := DiscriminantType(len(desc.Cases))
	discSize, err := sizePrimitive(discType)
	if err != nil {
		return err
	}

	if err := storeInt(mem, ptr, vv.Disc, discSize); err != nil {
		return err
	}

	// Compute offset to payload
	offset := ptr + discSize
	maxCaseAlign, err := MaxCaseAlignment(desc.Cases, resolve)
	if err != nil {
		return err
	}
	offset = Align(offset, maxCaseAlign)

	// Store payload if present
	c := desc.Cases[vv.Disc]
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return err
		}
		if vv.Payload == nil {
			return fmt.Errorf("storeVariant: case %d requires payload", vv.Disc)
		}
		if err := storeValue(mem, offset, caseType, vv.Payload, resolve, realloc); err != nil {
			return fmt.Errorf("storeVariant case %d: %w", vv.Disc, err)
		}
	}

	return nil
}

func storeTuple(mem []byte, ptr uint32, v Value, desc bintype.TupleDesc, resolve Resolver, realloc Realloc) error {
	elements, ok := v.([]Value)
	if !ok {
		return fmt.Errorf("storeTuple: expected []Value, got %T", v)
	}

	if len(elements) != len(desc.Elements) {
		return fmt.Errorf("storeTuple: expected %d elements, got %d", len(desc.Elements), len(elements))
	}

	offset := ptr
	for i, elemRef := range desc.Elements {
		elemType, err := resolveType(&elemRef, resolve)
		if err != nil {
			return fmt.Errorf("storeTuple: element %d: %w", i, err)
		}

		elemAlign, err := Alignment(elemType, resolve)
		if err != nil {
			return err
		}
		offset = Align(offset, elemAlign)

		if err := storeValue(mem, offset, elemType, elements[i], resolve, realloc); err != nil {
			return fmt.Errorf("storeTuple: element %d: %w", i, err)
		}

		elemSize, err := Size(elemType, resolve)
		if err != nil {
			return err
		}
		offset += elemSize
	}

	return nil
}

func storeFlags(mem []byte, ptr uint32, v Value, desc bintype.FlagsDesc) error {
	bits, ok := v.(uint32)
	if !ok {
		return fmt.Errorf("storeFlags: expected uint32, got %T", v)
	}

	flagsSize, err := sizeFlagsNumLabels(len(desc.Names))
	if err != nil {
		return err
	}

	return storeInt(mem, ptr, bits, flagsSize)
}

func storeEnum(mem []byte, ptr uint32, v Value, desc bintype.EnumDesc) error {
	caseIdx, ok := v.(uint32)
	if !ok {
		return fmt.Errorf("storeEnum: expected uint32, got %T", v)
	}

	if int(caseIdx) >= len(desc.Cases) {
		return fmt.Errorf("storeEnum: case index %d out of range [0,%d)", caseIdx, len(desc.Cases))
	}

	enumSize, err := sizeEnumNumCases(len(desc.Cases))
	if err != nil {
		return err
	}

	return storeInt(mem, ptr, caseIdx, enumSize)
}

func storeOption(mem []byte, ptr uint32, v Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) error {
	// Option is a variant with discriminant (0=none, 1=some)
	var discIdx uint32
	var payload Value

	if v == nil {
		// None
		discIdx = 0
	} else {
		// Some
		discIdx = 1
		payload = v
	}

	// Store discriminant (u8)
	if err := storeInt(mem, ptr, discIdx, 1); err != nil {
		return err
	}

	// Compute offset to payload
	offset := ptr + 1
	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return err
	}
	offset = Align(offset, elemAlign)

	// Store payload if some
	if discIdx == 1 {
		if err := storeValue(mem, offset, elemType, payload, resolve, realloc); err != nil {
			return fmt.Errorf("storeOption some: %w", err)
		}
	}

	return nil
}

func storeResult(mem []byte, ptr uint32, v Value, desc bintype.ResultDesc, resolve Resolver, realloc Realloc) error {
	rv, ok := v.(ResultValue)
	if !ok {
		return fmt.Errorf("storeResult: expected ResultValue, got %T", v)
	}

	var discIdx uint32
	if rv.IsErr {
		discIdx = 1
	} else {
		discIdx = 0
	}

	// Store discriminant (u8)
	if err := storeInt(mem, ptr, discIdx, 1); err != nil {
		return err
	}

	// Compute offset to payload
	offset := ptr + 1
	maxAlign := uint32(1)
	if desc.Ok != nil {
		okType, err := resolveType(desc.Ok, resolve)
		if err != nil {
			return err
		}
		okAlign, err := Alignment(okType, resolve)
		if err != nil {
			return err
		}
		if okAlign > maxAlign {
			maxAlign = okAlign
		}
	}
	if desc.Err != nil {
		errType, err := resolveType(desc.Err, resolve)
		if err != nil {
			return err
		}
		errAlign, err := Alignment(errType, resolve)
		if err != nil {
			return err
		}
		if errAlign > maxAlign {
			maxAlign = errAlign
		}
	}
	offset = Align(offset, maxAlign)

	// Store payload
	if rv.IsErr {
		if desc.Err != nil {
			errType, err := resolveType(desc.Err, resolve)
			if err != nil {
				return err
			}
			if err := storeValue(mem, offset, errType, rv.Payload, resolve, realloc); err != nil {
				return fmt.Errorf("storeResult err: %w", err)
			}
		}
	} else {
		if desc.Ok != nil {
			okType, err := resolveType(desc.Ok, resolve)
			if err != nil {
				return err
			}
			if err := storeValue(mem, offset, okType, rv.Payload, resolve, realloc); err != nil {
				return fmt.Errorf("storeResult ok: %w", err)
			}
		}
	}

	return nil
}
