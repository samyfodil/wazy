package abi

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// A fixed-length list, list<T, N> (binary.FixedListDesc), is the Canonical
// ABI's ListType with a length: it is laid out, flattened, lifted and lowered
// exactly as a tuple of N T's would be (CanonicalABI.md's maybe_length arms).
// Its alignment is the element's, its size N times the element's, it
// flattens to the element's flattening N times, and in memory it is the N
// elements back to back where the list itself sits -- no pointer, no length,
// no allocation.
//
// Its value is a dynamic list's (see loadAnyListFromRange): the Go slice of a
// fixed-width primitive element -- []uint32 for list<u32, 4> -- and a []Value
// for any other element, of exactly N elements. Lowering accepts either
// shape, and any other length is an error, not a trap: the Python reference
// asserts it, so a host that hands over three elements for a list<u32, 4>
// has a bug to be told about.

// maxValueSize is the largest a value's layout may be: Binary.md requires
// elem_size(t) < 2^28 of every type, which is what bounds a fixed-length list.
const maxValueSize = 1<<28 - 1

func alignmentFixedList(d binary.FixedListDesc, resolve Resolver) (uint32, error) {
	elem, err := resolveType(&d.Element, resolve)
	if err != nil {
		return 0, err
	}
	return Alignment(elem, resolve)
}

func sizeFixedList(d binary.FixedListDesc, resolve Resolver) (uint32, error) {
	elem, err := resolveType(&d.Element, resolve)
	if err != nil {
		return 0, err
	}
	size, err := Size(elem, resolve)
	if err != nil {
		return 0, err
	}
	if total := uint64(size) * uint64(d.Length); total > maxValueSize {
		return 0, fmt.Errorf("fixed-length list of %d elements of %d bytes exceeds maximum byte size of %d", d.Length, size, maxValueSize)
	}
	return size * d.Length, nil
}

// fixedListCopies is how many copies of its element's flattening a
// fixed-length list of n spells out: all n -- unless that is wider than
// MaxFlatParams, the most any caller flattens without spilling to memory,
// and then just enough copies to be wider. The copies past that are all
// alike, and a type holding them spills wherever it appears -- a composite
// is at least as wide as its widest part, and a dynamic list does not
// flatten its element -- so nothing reads them; spelling them out for a
// list<u8, 268435455> would build a quarter-billion entries. Flatten and
// FlatWidth both count this way, so they agree.
func fixedListCopies(elemWidth int, n uint32) int {
	switch {
	case elemWidth == 0:
		return 0
	case uint64(elemWidth)*uint64(n) <= MaxFlatParams:
		return int(n)
	}
	return MaxFlatParams/elemWidth + 1
}

func flattenFixedList(d binary.FixedListDesc, resolve Resolver) ([]string, error) {
	elem, err := resolveType(&d.Element, resolve)
	if err != nil {
		return nil, err
	}
	elemFlat, err := Flatten(elem, resolve)
	if err != nil {
		return nil, err
	}
	copies := fixedListCopies(len(elemFlat), d.Length)
	out := make([]string, 0, len(elemFlat)*copies)
	for range copies {
		out = append(out, elemFlat...)
	}
	return out, nil
}

func flatWidthFixedList(d binary.FixedListDesc, resolve Resolver) (int, error) {
	w, err := flatWidthRef(&d.Element, resolve)
	if err != nil {
		return 0, err
	}
	return w * fixedListCopies(w, d.Length), nil
}

// lengthError is the error for a fixed-length list value of the wrong length.
func lengthError(n uint32, got int) error {
	return fmt.Errorf("fixed-length list: expected %d elements, got %d", n, got)
}

// storeFixedList writes the n elements of v back to back at ptr.
func storeFixedList(mem []byte, ptr uint32, v Value, n uint32, elemType binary.TypeDesc, resolve Resolver, realloc Realloc) ([]byte, error) {
	elemAlign, err := Alignment(elemType, resolve)
	if err != nil {
		return mem, err
	}
	elemSize, err := Size(elemType, resolve)
	if err != nil {
		return mem, err
	}
	if prim, ok := scalarPrim(elemType); ok {
		if handled, err := storeFixedScalars(mem, ptr, v, prim, n, elemSize); handled {
			return mem, err
		}
	}
	list, ok := v.([]Value)
	if !ok {
		return mem, fmt.Errorf("store fixed-length list: expected []Value (or the typed slice for a primitive element), got %T", v)
	}
	if uint64(len(list)) != uint64(n) {
		return mem, lengthError(n, len(list))
	}
	// mem is rebound from every element's store, as allocStoreList does: an
	// element holding a string or a list allocates, and the next element must
	// be written through the view that allocation left live.
	for i, e := range list {
		if mem, err = storeValue(mem, ptr+uint32(i)*elemSize, elemType, e, elemAlign, resolve, realloc); err != nil {
			return mem, fmt.Errorf("fixed-length list [%d]: %w", i, err)
		}
	}
	return mem, nil
}

// storeFixedScalars writes a typed slice for prim into the n elements at
// ptr. handled is false when v is not that typed slice, which the caller then
// stores from its []Value shape.
func storeFixedScalars(mem []byte, ptr uint32, v Value, prim string, n, elemSize uint32) (handled bool, err error) {
	switch prim {
	case "u8":
		in, ok := v.([]byte)
		if !ok {
			return false, nil
		}
		span, err := fixedSpan(mem, ptr, n, elemSize, len(in))
		if err == nil {
			copy(span, in)
		}
		return true, err
	case "s8":
		return putFixed(mem, ptr, v, n, elemSize, putS8)
	case "bool":
		return putFixed(mem, ptr, v, n, elemSize, putBool)
	case "u16":
		return putFixed(mem, ptr, v, n, elemSize, putU16)
	case "s16":
		return putFixed(mem, ptr, v, n, elemSize, putS16)
	case "u32":
		return putFixed(mem, ptr, v, n, elemSize, putU32)
	case "s32":
		return putFixed(mem, ptr, v, n, elemSize, putS32)
	case "u64":
		return putFixed(mem, ptr, v, n, elemSize, putU64)
	case "s64":
		return putFixed(mem, ptr, v, n, elemSize, putS64)
	case "f32":
		return putFixed(mem, ptr, v, n, elemSize, putF32)
	case "f64":
		return putFixed(mem, ptr, v, n, elemSize, putF64)
	case "char":
		return putFixed(mem, ptr, v, n, elemSize, putChar)
	}
	return false, nil
}

// putFixed is storeFixedScalars for one element type T.
func putFixed[T any](mem []byte, ptr uint32, v Value, n, elemSize uint32, put func([]byte, T) error) (bool, error) {
	in, ok := v.([]T)
	if !ok {
		return false, nil
	}
	span, err := fixedSpan(mem, ptr, n, elemSize, len(in))
	if err != nil {
		return true, err
	}
	for i, e := range in {
		if err := put(span[uint32(i)*elemSize:], e); err != nil {
			return true, err
		}
	}
	return true, nil
}

// fixedSpan checks a typed slice's length against n and returns the n
// elements' bytes at ptr.
func fixedSpan(mem []byte, ptr, n, elemSize uint32, got int) ([]byte, error) {
	if uint64(got) != uint64(n) {
		return nil, lengthError(n, got)
	}
	end := uint64(ptr) + uint64(n)*uint64(elemSize)
	if end > uint64(len(mem)) {
		return nil, fmt.Errorf("store fixed-length list: %d bytes at %d overflow memory of %d", end-uint64(ptr), ptr, len(mem))
	}
	return mem[ptr:end], nil
}

// lowerFlatFixedList lowers each element in turn, as a tuple's are lowered.
func lowerFlatFixedList(v Value, n uint32, elemType binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	if prim, ok := scalarPrim(elemType); ok {
		if flat, handled, err := lowerFixedScalars(v, prim, n); handled {
			return flat, err
		}
	}
	list, ok := v.([]Value)
	if !ok {
		return nil, fmt.Errorf("lowerFlat fixed-length list: expected []Value (or the typed slice for a primitive element), got %T", v)
	}
	if uint64(len(list)) != uint64(n) {
		return nil, lengthError(n, len(list))
	}
	var flat []CoreValue
	for i, e := range list {
		elemFlat, err := lowerFlatImpl(e, elemType, nil, resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("lowerFlat fixed-length list [%d]: %w", i, err)
		}
		flat = append(flat, elemFlat...)
	}
	return flat, nil
}

// lowerFixedScalars lowers a typed slice for prim straight to core values,
// with no element boxed into a Value on the way. handled is false when v is
// not that typed slice.
func lowerFixedScalars(v Value, prim string, n uint32) ([]CoreValue, bool, error) {
	switch prim {
	case "u8":
		return lowerFixed(v, n, func(e byte) (CoreValue, error) { return NewCoreValueI32(uint32(e)), nil })
	case "s8":
		return lowerFixed(v, n, func(e int8) (CoreValue, error) { return NewCoreValueI32(uint32(int32(e))), nil })
	case "bool":
		return lowerFixed(v, n, func(e bool) (CoreValue, error) {
			if e {
				return NewCoreValueI32(1), nil
			}
			return NewCoreValueI32(0), nil
		})
	case "u16":
		return lowerFixed(v, n, func(e uint16) (CoreValue, error) { return NewCoreValueI32(uint32(e)), nil })
	case "s16":
		return lowerFixed(v, n, func(e int16) (CoreValue, error) { return NewCoreValueI32(uint32(int32(e))), nil })
	case "u32":
		return lowerFixed(v, n, func(e uint32) (CoreValue, error) { return NewCoreValueI32(e), nil })
	case "s32":
		return lowerFixed(v, n, func(e int32) (CoreValue, error) { return NewCoreValueI32(uint32(e)), nil })
	case "u64":
		return lowerFixed(v, n, func(e uint64) (CoreValue, error) { return NewCoreValueI64(e), nil })
	case "s64":
		return lowerFixed(v, n, func(e int64) (CoreValue, error) { return NewCoreValueI64(uint64(e)), nil })
	case "f32":
		return lowerFixed(v, n, func(e float32) (CoreValue, error) { return NewCoreValueF32(e), nil })
	case "f64":
		return lowerFixed(v, n, func(e float64) (CoreValue, error) { return NewCoreValueF64(e), nil })
	case "char":
		return lowerFixed(v, n, lowerChar)
	}
	return nil, false, nil
}

// lowerFixed is lowerFixedScalars for one element type T.
func lowerFixed[T any](v Value, n uint32, lower func(T) (CoreValue, error)) ([]CoreValue, bool, error) {
	in, ok := v.([]T)
	if !ok {
		return nil, false, nil
	}
	if uint64(len(in)) != uint64(n) {
		return nil, true, lengthError(n, len(in))
	}
	flat := make([]CoreValue, len(in))
	for i, e := range in {
		cv, err := lower(e)
		if err != nil {
			return nil, true, fmt.Errorf("lowerFlat fixed-length list [%d]: %w", i, err)
		}
		flat[i] = cv
	}
	return flat, true, nil
}

// liftFlatFixedList lifts n elements in turn, as a tuple's are lifted, into a
// dynamic list's shape.
func liftFlatFixedList(vi valueIter, n uint32, elemType binary.TypeDesc, resolve Resolver, mem []byte) (Value, error) {
	if prim, ok := scalarPrim(elemType); ok {
		if v, handled, err := liftFixedScalars(vi, prim, n); handled {
			return v, err
		}
	}
	// Flat values are only ever a handful (a wider list spills to memory),
	// so the slice grows from a small start rather than trusting n.
	out := make([]Value, 0, min(n, MaxFlatParams))
	for i := range n {
		e, err := liftFlatImpl(vi, elemType, nil, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("liftFlat fixed-length list [%d]: %w", i, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// liftFixedScalars lifts n core values straight into the typed slice for
// prim, each as liftScalarPrimitive would lift it, with no element boxed
// into a Value on the way. handled is false for a primitive with no typed
// slice (string, error-context).
func liftFixedScalars(vi valueIter, prim string, n uint32) (Value, bool, error) {
	switch prim {
	case "u8":
		return liftFixed(vi, n, func(cv CoreValue) (byte, error) { return byte(cv.AsI32()), nil })
	case "s8":
		return liftFixed(vi, n, func(cv CoreValue) (int8, error) { return int8(cv.AsI32()), nil })
	case "bool":
		return liftFixed(vi, n, func(cv CoreValue) (bool, error) { return cv.AsI32() != 0, nil })
	case "u16":
		return liftFixed(vi, n, func(cv CoreValue) (uint16, error) { return uint16(cv.AsI32()), nil })
	case "s16":
		return liftFixed(vi, n, func(cv CoreValue) (int16, error) { return int16(cv.AsI32()), nil })
	case "u32":
		return liftFixed(vi, n, func(cv CoreValue) (uint32, error) { return cv.AsI32(), nil })
	case "s32":
		return liftFixed(vi, n, func(cv CoreValue) (int32, error) { return int32(cv.AsI32()), nil })
	case "u64":
		return liftFixed(vi, n, func(cv CoreValue) (uint64, error) { return cv.AsI64(), nil })
	case "s64":
		return liftFixed(vi, n, func(cv CoreValue) (int64, error) { return int64(cv.AsI64()), nil })
	case "f32":
		return liftFixed(vi, n, func(cv CoreValue) (float32, error) { return cv.AsF32(), nil })
	case "f64":
		return liftFixed(vi, n, func(cv CoreValue) (float64, error) { return cv.AsF64(), nil })
	case "char":
		return liftFixed(vi, n, liftChar)
	}
	return nil, false, nil
}

// liftFixed is liftFixedScalars for one element type T.
func liftFixed[T any](vi valueIter, n uint32, lift func(CoreValue) (T, error)) (Value, bool, error) {
	out := make([]T, 0, min(n, MaxFlatParams))
	for i := range n {
		cv, err := vi.Next()
		if err != nil {
			return nil, true, err
		}
		e, err := lift(cv)
		if err != nil {
			return nil, true, fmt.Errorf("liftFlat fixed-length list [%d]: %w", i, err)
		}
		out = append(out, e)
	}
	return out, true, nil
}
