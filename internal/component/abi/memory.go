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
// through the lower/store tree; it stays on the stack. (The one thing that
// retains a Realloc past its call is internal/component/instance's guestBuffer,
// which holds one from the stream/future parking call until the rendezvous --
// which is exactly why Mem below closes over the MODULE and never over a slice.)
//
// Mem returns a LIVE view of the same linear memory Call allocates out of, and
// exists because a []byte taken before a grow may be worthless after it.
// wazy's MemoryInstance.Grow64 (internal/wasm/memory.go) has three branches:
// an in-capacity grow re-slices the SAME backing array to a longer length; a
// grow past the memory's capacity allocates a brand-new backing array and
// copies into it (Go has no in-place realloc, so this always moves); and an
// allocator-backed memory's Reallocate may or may not move. The default runtime
// config reserves no capacity beyond the declared minimum, so for most guests
// the very FIRST memory.grow relocates. Both outcomes break a pre-grow
// snapshot, in two different ways: its length is short, so a bounds check
// rejects a perfectly legitimate pointer past the old end (the loud failure),
// and its backing array may be detached, so a write that does pass the check
// lands in an array nobody will ever read (the silent one).
//
// Mem may be nil, meaning "cannot refresh". A zero Realloc{} (async_builtins'
// storeEvent, which stores two u32s and therefore never grows) and the
// module-less ReallocFunc adapter both rely on that: GrowMem then hands back
// the caller's own slice, reproducing the pre-refresh behavior byte for byte.
//
// Threads caveat: with the threads proposal another agent can grow this memory
// between a refresh and the copy that follows it, so a view is only ever fresh
// with respect to OUR OWN allocations. Closing that window would need a lock
// around every host write; it is out of scope here.
type Realloc struct {
	Ctx  context.Context
	Call func(ctx context.Context, origPtr, origSize, align, newSize uint32) (uint32, error)
	Mem  func() []byte
}

// Grow performs one allocation, threading Ctx into the cached Call.
//
// A caller that goes on to READ OR WRITE linear memory must use GrowMem
// instead: the slice it was holding may be stale the moment this returns.
func (r Realloc) Grow(origPtr, origSize, align, newSize uint32) (uint32, error) {
	if r.Call == nil {
		return 0, fmt.Errorf("component/abi: memory allocation requires a \"cabi_realloc\" export on the core module, which is not present")
	}
	return r.Call(r.Ctx, origPtr, origSize, align, newSize)
}

// GrowMem is Grow plus the memory view that is valid AFTER the growth: it
// returns the allocated pointer and the slice through which every subsequent
// access to this memory must go. mem is the caller's (possibly already stale)
// view, and is returned verbatim when the allocation fails or when Mem is nil,
// so a caller can rebind unconditionally.
//
// Mem is consulted ONLY on a successful Grow: the failure path, and every path
// that never grows at all, pay nothing for the refresh.
func (r Realloc) GrowMem(mem []byte, origPtr, origSize, align, newSize uint32) (uint32, []byte, error) {
	ptr, err := r.Grow(origPtr, origSize, align, newSize)
	if err != nil {
		return 0, mem, err
	}
	if r.Mem != nil {
		// A nil refresh (the module lost its memory) is not an improvement on
		// what the caller already holds, so keep the caller's slice and let the
		// bounds check that follows report the failure in its own terms.
		if fresh := r.Mem(); fresh != nil {
			mem = fresh
		}
	}
	return ptr, mem, nil
}

// checkAllocated verifies that [ptr, ptr+size) lies within mem, which MUST be
// the view refreshed after the allocation (see GrowMem) -- checking against a
// pre-grow snapshot rejects a pointer that is in fact perfectly valid, which is
// the exact bug this helper exists to make hard to reintroduce.
//
// The add is wrap-checked: a guest realloc returning a high pointer, or a huge
// size, would otherwise wrap around and sail through the length comparison. A
// zero-size allocation landing at ptr == len(mem) stays legal -- an empty
// string or empty list allocates nothing and some guests hand back the end
// pointer for it.
func checkAllocated(mem []byte, ptr, size uint32) error {
	if ptr+size < ptr || uint32(len(mem)) < ptr+size {
		return fmt.Errorf("allocated memory out of bounds: ptr=%d size=%d mem_len=%d", ptr, size, len(mem))
	}
	return nil
}

// ReallocFunc adapts a context-free allocator into a Realloc (the ctx is
// ignored). For simple in-memory allocators -- notably tests -- that don't need
// the call context. Mem is left nil: there is no module behind such an
// allocator, and the fixed Go slice it hands out never relocates, so "cannot
// refresh" is the correct answer rather than a limitation.
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

	case bintype.OwnDesc, bintype.BorrowDesc, bintype.StreamDesc, bintype.FutureDesc:
		// stream/future values are opaque i32 handles, same load as own/borrow.
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

	case "error-context":
		// Opaque i32 handle -- same load as own/borrow.
		return loadInt(mem, ptr, 4, false)

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
		// A signed byte lifts as int32 whatever its sign. Returning early only
		// for negatives, as this did, meant a non-negative s8 came back as a
		// uint32 -- which storePrimitive then refused, so an s8 the host lifted
		// could not be stored back unless it happened to be negative. The wider
		// signed widths below never had the split.
		if signed {
			return int32(int8(bytes[0])), nil
		}
		return uint32(bytes[0]), nil

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

	return loadAnyListFromRange(mem, listPtr.(uint32), listLen.(uint32), elemType, resolve)
}

// loadAnyListFromRange is loadListFromRange behind the typed-slice rule: a
// list of a fixed-width primitive is read into the Go slice of that primitive
// -- []byte for list<u8>, []uint32 for list<u32>, and so on -- rather than a
// []Value costing a machine word per element. Every other element type takes
// the general []Value path unchanged.
//
// Both lift directions go through here (loadList for the in-memory shape,
// liftFlatList for the flat ABI) so the two shapes a consumer can observe for
// one list type stay the same everywhere.
func loadAnyListFromRange(mem []byte, ptr, length uint32, elemType bintype.TypeDesc, resolve Resolver) (Value, error) {
	if prim, ok := scalarPrim(elemType); ok {
		if v, handled, err := liftScalarList(mem, ptr, length, prim); handled {
			return v, err
		}
	}
	return loadListFromRange(mem, ptr, length, elemType, resolve)
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

	// Bits above the label count are meaningless and are dropped, matching
	// the reference's load_flags -> unpack_flags_from_int. This is not
	// redundant with the load width: a 1..7-label flags occupies a whole
	// byte, and a 9..15-label one a whole u16, so a guest can hand over set
	// bits the type does not define -- see liftFlatFlags for the flat side.
	// sizeFlagsNumLabels above already bounded the label count to 1..32, and
	// loadInt returns uint32 for every unsigned width flags can occupy -- the
	// same bare assertion loadEnum makes just below.
	return bits.(uint32) & flagsBitMask(len(desc.Names)), nil
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
// This mirrors the canonical ABI store() function. A caller that stores the
// same type repeatedly (a bound import's result) should compile the layout
// once with CompileStore rather than pay the two type-graph walks per call.
//
// The caller's mem may be DEAD on return: storing a string or list grows guest
// memory, which can replace the backing array (see Realloc.Mem). Nothing in
// this package hands the refreshed view back through this entry point because
// no caller of it writes through mem afterwards; a caller that needs to keep
// going should use StoreStep.Store, which returns the live view.
func Store(mem []byte, ptr uint32, t bintype.TypeDesc, v Value, resolve Resolver, realloc Realloc) error {
	s, err := CompileStore(t, resolve)
	if err != nil {
		return err
	}
	_, err = s.Store(mem, ptr, v, realloc)
	return err
}

// storeValue writes v of type t at ptr. align is t's own alignment when the
// caller already had to compute it to place ptr (so ptr is a multiple of it),
// or 0 when it doesn't know: only the option/variant/result cases consume it,
// to find their payload offset, and deriving it there walks every field/case
// of the type again on every call -- wasi:http's 30-case error-code arm was
// re-walked on every guest->host result store.
// The []byte it returns is the memory view valid AFTER the store: storing a
// string or a list grows guest memory, and a grow can replace the backing array
// (see Realloc.Mem), so the caller's own slice may be dead by the time this
// returns. Every composite store below therefore rebinds its `mem` from this
// return on each field/element, and the whole store tree threads the live view
// back up to StoreStep.Store. It is returned rather than taken as a *[]byte
// precisely so an unconverted frame is a build error rather than a silent stale
// write. On the error path, and on every leaf that cannot grow, the caller's
// own slice comes straight back.
func storeValue(mem []byte, ptr uint32, t bintype.TypeDesc, v Value, align uint32, resolve Resolver, realloc Realloc) ([]byte, error) {
	switch desc := t.(type) {
	case bintype.PrimitiveDesc:
		return storePrimitive(mem, ptr, desc.Prim, v, realloc)

	case bintype.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return mem, err
		}
		return storeList(mem, ptr, v, elemType, resolve, realloc)

	case bintype.RecordDesc:
		return storeRecord(mem, ptr, v, desc, resolve, realloc)

	case bintype.VariantDesc:
		return storeVariant(mem, ptr, v, desc, align, resolve, realloc)

	case bintype.TupleDesc:
		return storeTuple(mem, ptr, v, desc, resolve, realloc)

	case bintype.FlagsDesc:
		return mem, storeFlags(mem, ptr, v, desc)

	case bintype.EnumDesc:
		return mem, storeEnum(mem, ptr, v, desc)

	case bintype.OptionDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return mem, err
		}
		return storeOption(mem, ptr, v, elemType, align, resolve, realloc)

	case bintype.ResultDesc:
		return storeResult(mem, ptr, v, desc, align, resolve, realloc)

	case bintype.OwnDesc, bintype.BorrowDesc, bintype.StreamDesc, bintype.FutureDesc:
		// stream/future values are opaque i32 handles, same store as
		// own/borrow. A handle is a plain i32 write: it cannot grow memory,
		// so mem comes back unchanged.
		if h, ok := v.(uint32); ok {
			return mem, storeInt(mem, ptr, h, 4)
		}
		return mem, fmt.Errorf("store: handle expected uint32, got %T", v)

	default:
		return mem, fmt.Errorf("store: unsupported type %T", t)
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

// storePrimitive writes one primitive at ptr. Only the string case can grow
// memory (it allocates and copies the UTF-8 bytes), so every other case hands
// the caller's own view straight back; the string case returns whatever
// storeString's allocation left live. See storeValue's doc for why this
// function returns a view at all.
func storePrimitive(mem []byte, ptr uint32, prim string, v Value, realloc Realloc) ([]byte, error) {
	switch prim {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return mem, fmt.Errorf("store bool: expected bool, got %T", v)
		}
		var val uint32
		if b {
			val = 1
		}
		return mem, storeInt(mem, ptr, val, 1)

	case "u8", "u16", "u32":
		if u, ok := v.(uint32); ok {
			return mem, storeInt(mem, ptr, u, intByteSize(prim))
		}
		return mem, fmt.Errorf("store %s: expected uint32, got %T", prim, v)

	case "s8", "s16", "s32":
		if i, ok := v.(int32); ok {
			return mem, storeInt(mem, ptr, uint32(i), intByteSize(prim))
		}
		return mem, fmt.Errorf("store %s: expected int32, got %T", prim, v)

	case "u64":
		if u, ok := v.(uint64); ok {
			return mem, storeInt(mem, ptr, u, 8)
		}
		return mem, fmt.Errorf("store u64: expected uint64, got %T", v)

	case "s64":
		if i, ok := v.(int64); ok {
			return mem, storeInt(mem, ptr, uint64(i), 8)
		}
		return mem, fmt.Errorf("store s64: expected int64, got %T", v)

	case "f32":
		if f, ok := v.(float32); ok {
			bits := math.Float32bits(f)
			return mem, storeInt(mem, ptr, uint32(bits), 4)
		}
		return mem, fmt.Errorf("store f32: expected float32, got %T", v)

	case "f64":
		if f, ok := v.(float64); ok {
			bits := math.Float64bits(f)
			return mem, storeInt(mem, ptr, bits, 8)
		}
		return mem, fmt.Errorf("store f64: expected float64, got %T", v)

	case "char":
		if r, ok := v.(rune); ok {
			if r < 0 || r >= 0x110000 {
				return mem, fmt.Errorf("store char: value %d out of range", r)
			}
			if r >= 0xD800 && r <= 0xDFFF {
				return mem, fmt.Errorf("store char: surrogate half %d not allowed", r)
			}
			return mem, storeInt(mem, ptr, uint32(r), 4)
		}
		return mem, fmt.Errorf("store char: expected rune, got %T", v)

	case "string":
		if s, ok := v.(string); ok {
			return storeString(mem, ptr, s, realloc)
		}
		return mem, fmt.Errorf("store string: expected string, got %T", v)

	case "error-context":
		// Opaque i32 handle -- same store as own/borrow.
		if h, ok := v.(uint32); ok {
			return mem, storeInt(mem, ptr, h, 4)
		}
		return mem, fmt.Errorf("store error-context: expected uint32 handle, got %T", v)

	default:
		return mem, fmt.Errorf("store: unknown primitive %s", prim)
	}
}

// storeInt writes an integer to memory in little-endian format.
func storeInt(mem []byte, ptr uint32, v any, nbytes uint32) error {
	if ptr+nbytes < ptr || uint32(len(mem)) < ptr+nbytes {
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
//
// This is the archetypal allocate-then-write-through-stale-mem site: the
// allocation for the UTF-8 bytes can move the whole backing array, and the two
// writes that follow go to `ptr`, a pre-existing slot at a LOW address which is
// still inside the old length -- so with a stale view they pass every bounds
// check and silently land in an array nobody reads again. Hence `mem` is
// rebound from allocStoreString before them, and returned so the enclosing
// record/tuple/list loop keeps a live view too.
func storeString(mem []byte, ptr uint32, s string, realloc Realloc) ([]byte, error) {
	ptrSize := uint32(4) // assuming 32-bit pointers

	newPtr, byteLen, mem, err := allocStoreString(mem, s, realloc)
	if err != nil {
		return mem, fmt.Errorf("storeString: %w", err)
	}

	// Store pointer and length
	if err := storeInt(mem, ptr, newPtr, ptrSize); err != nil {
		return mem, fmt.Errorf("storeString: store ptr failed: %w", err)
	}
	if err := storeInt(mem, ptr+ptrSize, byteLen, ptrSize); err != nil {
		return mem, fmt.Errorf("storeString: store len failed: %w", err)
	}

	return mem, nil
}

// allocStoreString allocates room for the UTF-8 bytes of s via realloc,
// copies them into mem, and returns (dataPtr, byteLen). This mirrors the
// canonical ABI's store_string_into_range() (definitions.py). It is shared
// by storeString (the Store/Load path, which additionally writes the
// (ptr,len) pair into memory at a record/list slot) and lowerFlatString (the
// flat ABI path, which returns (ptr,len) directly as core values) so there
// is exactly one implementation of "allocate + copy string bytes".
//
// The third return is the memory view valid AFTER the allocation (see
// Realloc.GrowMem): the bounds check and the copy below both run against it,
// never against the caller's pre-grow snapshot, and callers rebind from it.
func allocStoreString(mem []byte, s string, realloc Realloc) (uint32, uint32, []byte, error) {
	// Allocate memory for string bytes (UTF-8)
	strBytes := []byte(s)
	// A Go string longer than 4 GiB cannot be addressed by the 32-bit
	// canonical ABI at all; truncating its length here would copy a prefix and
	// hand the guest a wrong length, so refuse it outright.
	if uint64(len(strBytes)) > math.MaxUint32 {
		return 0, 0, mem, fmt.Errorf("string of %d bytes exceeds the 32-bit canonical ABI limit", len(strBytes))
	}
	byteLen := uint32(len(strBytes))

	newPtr, mem, err := realloc.GrowMem(mem, 0, 0, 1, byteLen)
	if err != nil {
		return 0, 0, mem, fmt.Errorf("realloc failed: %w", err)
	}

	if err := checkAllocated(mem, newPtr, byteLen); err != nil {
		return 0, 0, mem, err
	}

	// Copy string bytes to memory
	copy(mem[newPtr:newPtr+byteLen], strBytes)

	return newPtr, byteLen, mem, nil
}

// storeList writes the (ptr,len) pair for a list at ptr, after allocating and
// filling the element region. Same staleness shape as storeString, only worse:
// a list of aggregates grows once for the element array and again per element,
// so `mem` is rebound from allocStoreAnyList before the two slot writes.
func storeList(mem []byte, ptr uint32, v Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) ([]byte, error) {
	ptrSize := uint32(4) // assuming 32-bit pointers

	newPtr, length, mem, err := allocStoreAnyList(mem, v, elemType, resolve, realloc)
	if err != nil {
		return mem, fmt.Errorf("storeList: %w", err)
	}

	// Store list pointer and length
	if err := storeInt(mem, ptr, newPtr, ptrSize); err != nil {
		return mem, err
	}
	return mem, storeInt(mem, ptr+ptrSize, length, ptrSize)
}

// allocStoreAnyList stores a list value that is EITHER the general []Value
// shape or the typed slice for a fixed-width primitive element -- []byte for
// list<u8>, []uint32 for list<u32>, and so on. Both are accepted for every
// such list; the typed one is what lifting now produces, and the one a host
// func writing a numeric list already holds.
// The third return is the memory view valid after whichever branch ran (both
// of them allocate), so storeList and lowerFlatList don't keep a pre-grow one.
func allocStoreAnyList(mem []byte, v Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) (uint32, uint32, []byte, error) {
	if prim, ok := scalarPrim(elemType); ok {
		if ptr, n, fresh, handled, err := storeScalarList(mem, v, prim, realloc); handled {
			return ptr, n, fresh, err
		}
	}
	list, ok := v.([]Value)
	if !ok {
		return 0, 0, mem, fmt.Errorf("expected []Value (or the typed slice for a primitive element), got %T", v)
	}
	return allocStoreList(mem, list, elemType, resolve, realloc)
}

// allocStoreBytes is allocStoreList's list<u8> fast path: one realloc and one
// copy, with no per-element dispatch. u8's size and alignment are both 1, so
// the layout is identical to what the generic path would have produced.
func allocStoreBytes(mem []byte, b []byte, realloc Realloc) (uint32, uint32, []byte, error) {
	if uint64(len(b)) > math.MaxUint32 {
		return 0, 0, mem, fmt.Errorf("list of %d bytes exceeds the 32-bit canonical ABI limit", len(b))
	}
	byteLen := uint32(len(b))
	// GrowMem, not Grow: the copy below must target the array the guest's
	// allocator left live, not the one this function was handed.
	newPtr, mem, err := realloc.GrowMem(mem, 0, 0, 1, byteLen)
	if err != nil {
		return 0, 0, mem, fmt.Errorf("realloc failed: %w", err)
	}
	if err := checkAllocated(mem, newPtr, byteLen); err != nil {
		return 0, 0, mem, err
	}
	copy(mem[newPtr:newPtr+byteLen], b)
	return newPtr, byteLen, mem, nil
}

// allocStoreList allocates room for len(list) elements of elemType via
// realloc, stores each element into the allocated region, and returns
// (dataPtr, length). This mirrors the canonical ABI's
// store_list_into_range() (definitions.py). It is shared by storeList (the
// Store/Load path, which additionally writes the (ptr,len) pair into memory
// at a record/list slot) and lowerFlatList (the flat ABI path, which returns
// (ptr,len) directly as core values) so there is exactly one implementation
// of "allocate + store list elements".
// The third return is the memory view valid after the whole list is written.
// Refreshing once after the outer allocation is NOT enough here: an element of
// an indirect type (list<string>, list<list<T>>, list<record{...string}>)
// allocates again on EVERY iteration, so element i's grow would leave elements
// i+1..n writing through an array element i abandoned. The loop therefore
// rebinds `mem` from each element's own storeValue.
func allocStoreList(mem []byte, list []Value, elemType bintype.TypeDesc, resolve Resolver, realloc Realloc) (uint32, uint32, []byte, error) {
	// Allocate memory for list elements
	elemSize, err := Size(elemType, resolve)
	if err != nil {
		return 0, 0, mem, err
	}

	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return 0, 0, mem, err
	}

	// The product is checked before it is formed: a long list of a wide
	// element wraps uint32 and would otherwise ask for a small allocation and
	// then write far past it (scalarlist.go guards the same product).
	length := uint32(len(list))
	if elemSize != 0 && length > (1<<32-1)/elemSize {
		return 0, 0, mem, fmt.Errorf("list length %d overflows at %d bytes per element", length, elemSize)
	}
	byteLen := length * elemSize
	newPtr, mem, err := realloc.GrowMem(mem, 0, 0, elemAlign, byteLen)
	if err != nil {
		return 0, 0, mem, fmt.Errorf("realloc failed: %w", err)
	}

	if err := checkAllocated(mem, newPtr, byteLen); err != nil {
		return 0, 0, mem, err
	}

	// Store each element
	for i, elem := range list {
		elemPtr := newPtr + uint32(i)*elemSize
		if mem, err = storeValue(mem, elemPtr, elemType, elem, elemAlign, resolve, realloc); err != nil {
			return 0, 0, mem, fmt.Errorf("[%d]: %w", i, err)
		}
	}

	return newPtr, length, mem, nil
}

// storeRecord writes each field at its own aligned offset.
//
// The loop rebinds `mem` from every field's storeValue: a record whose first
// field is a string or list grows memory while storing it, and fields 1..n then
// write at LOW, still-in-range offsets -- so through a stale view they pass
// every bounds check and vanish into the abandoned array. Fixing the leaf
// allocators alone does not fix this; the live view has to reach the loop.
func storeRecord(mem []byte, ptr uint32, v Value, desc bintype.RecordDesc, resolve Resolver, realloc Realloc) ([]byte, error) {
	fields, ok := v.([]Value)
	if !ok {
		return mem, fmt.Errorf("storeRecord: expected []Value, got %T", v)
	}

	if len(fields) != len(desc.Fields) {
		return mem, fmt.Errorf("storeRecord: expected %d fields, got %d", len(desc.Fields), len(fields))
	}

	offset := ptr
	for i, field := range desc.Fields {
		fieldType, err := resolveType(&field.Type, resolve)
		if err != nil {
			return mem, fmt.Errorf("storeRecord: field %s: %w", field.Name, err)
		}

		fieldAlign, err := Alignment(fieldType, resolve)
		if err != nil {
			return mem, err
		}
		offset = Align(offset, fieldAlign)

		if mem, err = storeValue(mem, offset, fieldType, fields[i], fieldAlign, resolve, realloc); err != nil {
			return mem, fmt.Errorf("storeRecord: field %s: %w", field.Name, err)
		}

		fieldSize, err := Size(fieldType, resolve)
		if err != nil {
			return mem, err
		}
		offset += fieldSize
	}

	return mem, nil
}

// storeVariant writes the discriminant and then the active case's payload.
//
// Nothing in this frame writes AFTER a grow -- the discriminant precedes the
// single payload store -- but it still has to hand the payload's refreshed view
// back, or the enclosing record/tuple/list loop would carry on through a slice
// the payload's own allocation abandoned. That seam is where a partial
// conversion silently reintroduces the bug.
func storeVariant(mem []byte, ptr uint32, v Value, desc bintype.VariantDesc, align uint32, resolve Resolver, realloc Realloc) ([]byte, error) {
	vv, ok := v.(VariantValue)
	if !ok {
		return mem, fmt.Errorf("storeVariant: expected VariantValue, got %T", v)
	}

	if int(vv.Disc) >= len(desc.Cases) {
		return mem, fmt.Errorf("storeVariant: case index %d out of range [0,%d)", vv.Disc, len(desc.Cases))
	}

	// Store discriminant
	discType := DiscriminantType(len(desc.Cases))
	discSize, err := sizePrimitive(discType)
	if err != nil {
		return mem, err
	}

	if err := storeInt(mem, ptr, vv.Disc, discSize); err != nil {
		return mem, err
	}

	// Compute offset to payload. Aligning to the variant's own alignment --
	// max(discriminant alignment, MaxCaseAlignment) -- lands on the same
	// offset as aligning to MaxCaseAlignment alone, since ptr is a multiple
	// of it and a discriminant's size equals its alignment; so the caller's
	// value is used when it has one, and only otherwise are the cases walked.
	payloadAlign := align
	if payloadAlign == 0 {
		if payloadAlign, err = MaxCaseAlignment(desc.Cases, resolve); err != nil {
			return mem, err
		}
	}
	offset := Align(ptr+discSize, payloadAlign)

	// Store payload if present
	c := desc.Cases[vv.Disc]
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return mem, err
		}
		if vv.Payload == nil {
			return mem, fmt.Errorf("storeVariant: case %d requires payload", vv.Disc)
		}
		if mem, err = storeValue(mem, offset, caseType, vv.Payload, 0, resolve, realloc); err != nil {
			return mem, fmt.Errorf("storeVariant case %d: %w", vv.Disc, err)
		}
	}

	return mem, nil
}

// storeTuple writes each element at its own aligned offset. Same rebinding rule
// as storeRecord -- and this is also the shape of the SPILLED PARAMETER LIST
// (boundExport.paramTuple), so it sits on the path every wide export takes.
func storeTuple(mem []byte, ptr uint32, v Value, desc bintype.TupleDesc, resolve Resolver, realloc Realloc) ([]byte, error) {
	elements, ok := v.([]Value)
	if !ok {
		return mem, fmt.Errorf("storeTuple: expected []Value, got %T", v)
	}

	if len(elements) != len(desc.Elements) {
		return mem, fmt.Errorf("storeTuple: expected %d elements, got %d", len(desc.Elements), len(elements))
	}

	offset := ptr
	for i, elemRef := range desc.Elements {
		elemType, err := resolveType(&elemRef, resolve)
		if err != nil {
			return mem, fmt.Errorf("storeTuple: element %d: %w", i, err)
		}

		elemAlign, err := Alignment(elemType, resolve)
		if err != nil {
			return mem, err
		}
		offset = Align(offset, elemAlign)

		if mem, err = storeValue(mem, offset, elemType, elements[i], elemAlign, resolve, realloc); err != nil {
			return mem, fmt.Errorf("storeTuple: element %d: %w", i, err)
		}

		elemSize, err := Size(elemType, resolve)
		if err != nil {
			return mem, err
		}
		offset += elemSize
	}

	return mem, nil
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

// storeOption writes the u8 discriminant and, for Some, the payload.
//
// Like storeVariant this frame performs no write after a grow, but it must
// still return the payload's refreshed view: option<string> and option<list<T>>
// allocate inside the payload store, and an error-only return would hide that
// from the enclosing record/tuple/list loop.
func storeOption(mem []byte, ptr uint32, v Value, elemType bintype.TypeDesc, align uint32, resolve Resolver, realloc Realloc) ([]byte, error) {
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
		return mem, err
	}

	// Compute offset to payload. An option's own alignment IS its element's
	// (the u8 discriminant never widens it), so the caller's value serves for
	// both the offset and the element's own store.
	elemAlign := align
	if elemAlign == 0 {
		var err error
		if elemAlign, err = Alignment(elemType, resolve); err != nil {
			return mem, err
		}
	}
	offset := Align(ptr+1, elemAlign)

	// Store payload if some
	if discIdx == 1 {
		var err error
		if mem, err = storeValue(mem, offset, elemType, payload, elemAlign, resolve, realloc); err != nil {
			return mem, fmt.Errorf("storeOption some: %w", err)
		}
	}

	return mem, nil
}

// storeResult writes the u8 discriminant and the active arm's payload.
//
// Same propagation duty as storeVariant/storeOption, and it matters most here:
// result<string, error-code> and result<list<u8>, error-code> are the dominant
// wasi:http / wasi:io shapes, and they allocate inside the arm.
func storeResult(mem []byte, ptr uint32, v Value, desc bintype.ResultDesc, align uint32, resolve Resolver, realloc Realloc) ([]byte, error) {
	rv, ok := v.(ResultValue)
	if !ok {
		return mem, fmt.Errorf("storeResult: expected ResultValue, got %T", v)
	}

	var discIdx uint32
	if rv.IsErr {
		discIdx = 1
	} else {
		discIdx = 0
	}

	// Store discriminant (u8)
	if err := storeInt(mem, ptr, discIdx, 1); err != nil {
		return mem, err
	}

	// Compute offset to payload. A result's own alignment IS the max of its
	// arms' (the u8 discriminant never widens it), so the caller's value
	// replaces walking BOTH arm types on every call.
	maxAlign := align
	if maxAlign == 0 {
		var err error
		if maxAlign, err = alignmentResult(desc, resolve); err != nil {
			return mem, err
		}
	}
	offset := Align(ptr+1, maxAlign)

	// Store payload
	if rv.IsErr {
		if desc.Err != nil {
			errType, err := resolveType(desc.Err, resolve)
			if err != nil {
				return mem, err
			}
			if mem, err = storeValue(mem, offset, errType, rv.Payload, 0, resolve, realloc); err != nil {
				return mem, fmt.Errorf("storeResult err: %w", err)
			}
		}
	} else {
		if desc.Ok != nil {
			okType, err := resolveType(desc.Ok, resolve)
			if err != nil {
				return mem, err
			}
			if mem, err = storeValue(mem, offset, okType, rv.Payload, 0, resolve, realloc); err != nil {
				return mem, fmt.Errorf("storeResult ok: %w", err)
			}
		}
	}

	return mem, nil
}
