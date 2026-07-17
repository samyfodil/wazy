package abi

import (
	"fmt"
	"math"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// LowerFlat converts a Value to a slice of CoreValues in the flat ABI.
// If the flattened type exceeds MaxFlatParams, the value is stored to memory
// via realloc and a single i32 pointer is returned instead.
//
// This mirrors the canonical ABI lower_flat() function.
func LowerFlat(v Value, t binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	return LowerFlatInto(nil, v, t, resolve, realloc, mem)
}

// LowerFlatInto is LowerFlat that appends into dst instead of allocating a
// fresh result slice, returning the extended slice (append semantics). It lets
// the per-call hot path (instance.lowerParams) lower a whole parameter list
// straight into one pooled buffer rather than allocating a small slice per
// parameter and copying. LowerFlat is the dst==nil case.
func LowerFlatInto(dst []CoreValue, v Value, t binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	// Fast path: a top-level non-string primitive flattens to exactly one core
	// value and can never spill (MaxFlatParams is far above 1), so append it
	// directly -- no intermediate one-element slice, no Flatten/spill re-check.
	// This is the common case for numeric and char parameters.
	if p, ok := t.(binary.PrimitiveDesc); ok && p.Prim != "string" {
		cv, err := lowerPrimitiveCore(v, p.Prim)
		if err != nil {
			return nil, err
		}
		return append(dst, cv), nil
	}

	// Decide spill BEFORE lowering: Flatten needs only the type. A value that
	// spills is stored to memory by spillValue, so doing lowerFlatImpl first
	// (as this used to) would flatten -- and realloc+copy every string/list in
	// it -- only to throw that away and re-store via spillValue: wasted work
	// AND leaked allocations that shift the bump allocator. (LowerStep.Lower
	// makes the same spill-first decision; found via the complex ABI battery.)
	flatTypes, err := Flatten(t, resolve)
	if err != nil {
		return nil, err
	}

	if len(flatTypes) > MaxFlatParams {
		// Spill: Store the value to memory and return a pointer.
		ptr, err := spillValue(v, t, mem, resolve, realloc)
		if err != nil {
			return nil, err
		}
		return append(dst, NewCoreValueI32(ptr)), nil
	}

	flat, err := lowerFlatImpl(v, t, resolve, realloc, mem)
	if err != nil {
		return nil, err
	}
	return append(dst, flat...), nil
}

// lowerFlatImpl recursively lowers a value to its flat representation.
// Does not handle spilling; that is done by LowerFlat.
func lowerFlatImpl(v Value, t binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	switch desc := t.(type) {
	case binary.PrimitiveDesc:
		return lowerFlatPrimitive(v, desc.Prim, realloc, mem)

	case binary.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return lowerFlatList(v, elemType, resolve, realloc, mem)

	case binary.RecordDesc:
		return lowerFlatRecord(v, desc, resolve, realloc, mem)

	case binary.VariantDesc:
		return lowerFlatVariant(v, desc, resolve, realloc, mem)

	case binary.TupleDesc:
		return lowerFlatTuple(v, desc, resolve, realloc, mem)

	case binary.FlagsDesc:
		return lowerFlatFlags(v, desc)

	case binary.EnumDesc:
		return lowerFlatEnum(v, desc)

	case binary.OptionDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return lowerFlatOption(v, elemType, resolve, realloc, mem)

	case binary.ResultDesc:
		return lowerFlatResult(v, desc, resolve, realloc, mem)

	case binary.OwnDesc, binary.BorrowDesc, binary.StreamDesc, binary.FutureDesc:
		// stream/future values are opaque i32 handles, same lowering as
		// own/borrow (see Flatten's comment on why the element type isn't
		// touched here).
		if h, ok := v.(uint32); ok {
			return []CoreValue{NewCoreValueI32(h)}, nil
		}
		return nil, fmt.Errorf("lowerFlat: handle expected uint32, got %T", v)

	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return nil, fmt.Errorf("lowerFlat: unsupported type %T", t)

	default:
		return nil, fmt.Errorf("lowerFlat: unknown type descriptor %T", t)
	}
}

// lowerFlatPrimitive lowers a primitive value. realloc/mem are only used by
// the string case (strings must allocate and copy their UTF-8 bytes into
// linear memory even in the flat ABI).
func lowerFlatPrimitive(v Value, prim string, realloc Realloc, mem []byte) ([]CoreValue, error) {
	if prim == "string" {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lowerFlat string: expected string, got %T", v)
		}
		return lowerFlatString(s, realloc, mem)
	}
	cv, err := lowerPrimitiveCore(v, prim)
	if err != nil {
		return nil, err
	}
	return []CoreValue{cv}, nil
}

// lowerPrimitiveCore lowers every non-string primitive to its single core
// value. Strings are the one primitive that flattens to two core values
// (ptr, len) and must allocate into linear memory (see lowerFlatString), so
// they are excluded here; passing "string" is a programming error. Split out
// of lowerFlatPrimitive so the hot path (LowerFlatInto) can append a top-level
// primitive parameter straight into its destination buffer without the
// one-element []CoreValue{...} slice lowerFlatPrimitive would otherwise return.
func lowerPrimitiveCore(v Value, prim string) (CoreValue, error) {
	switch prim {
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return CoreValue{}, fmt.Errorf("lowerFlat bool: expected bool, got %T", v)
		}
		var val uint32
		if b {
			val = 1
		}
		return NewCoreValueI32(val), nil

	case "u8", "u16", "u32":
		if u, ok := v.(uint32); ok {
			return NewCoreValueI32(u), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat %s: expected uint32, got %T", prim, v)

	case "s8", "s16", "s32":
		if i, ok := v.(int32); ok {
			// Signed values are converted to unsigned by modular arithmetic.
			var coreBits uint64 = 32
			if i < 0 {
				i += (1 << coreBits)
			}
			return NewCoreValueI32(uint32(i)), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat %s: expected int32, got %T", prim, v)

	case "u64":
		if u, ok := v.(uint64); ok {
			return NewCoreValueI64(u), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat u64: expected uint64, got %T", v)

	case "s64":
		if i, ok := v.(int64); ok {
			// Convert to unsigned via modular arithmetic.
			var coreBits uint64 = 64
			if i < 0 {
				i += (1 << coreBits)
			}
			return NewCoreValueI64(uint64(i)), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat s64: expected int64, got %T", v)

	case "f32":
		if f, ok := v.(float32); ok {
			return NewCoreValueF32(f), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat f32: expected float32, got %T", v)

	case "f64":
		if f, ok := v.(float64); ok {
			return NewCoreValueF64(f), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat f64: expected float64, got %T", v)

	case "char":
		if r, ok := v.(rune); ok {
			if r < 0 || r >= 0x110000 {
				return CoreValue{}, fmt.Errorf("lowerFlat char: value %d out of range", r)
			}
			if r >= 0xD800 && r <= 0xDFFF {
				return CoreValue{}, fmt.Errorf("lowerFlat char: surrogate half %d not allowed", r)
			}
			return NewCoreValueI32(uint32(r)), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat char: expected rune, got %T", v)

	case "string":
		return CoreValue{}, fmt.Errorf("lowerPrimitiveCore: string is not a single core value")

	case "error-context":
		// Opaque i32 handle -- same lowering as own/borrow.
		if h, ok := v.(uint32); ok {
			return NewCoreValueI32(h), nil
		}
		return CoreValue{}, fmt.Errorf("lowerFlat error-context: expected uint32 handle, got %T", v)

	default:
		return CoreValue{}, fmt.Errorf("lowerFlat: unknown primitive type %s", prim)
	}
}

// lowerFlatString lowers a string value.
// Strings are lowered as (ptr i32, length i32): the UTF-8 bytes are
// allocated and copied into linear memory via realloc, and the resulting
// pointer/length pair is returned as core values. This mirrors the
// canonical ABI's lower_flat_string() (definitions.py), which calls
// store_string_into_range() -- the same allocate+copy helper storeString
// uses for the Store/Load path (see allocStoreString in memory.go).
func lowerFlatString(s string, realloc Realloc, mem []byte) ([]CoreValue, error) {
	ptr, byteLen, err := allocStoreString(mem, s, realloc)
	if err != nil {
		return nil, fmt.Errorf("lowerFlatString: %w", err)
	}
	return []CoreValue{
		NewCoreValueI32(ptr),
		NewCoreValueI32(byteLen),
	}, nil
}

// lowerFlatList lowers a list value.
// Dynamic lists flatten to (ptr i32, length i32): each element is stored
// into a freshly realloc'd region of linear memory, and the resulting
// pointer/length pair is returned as core values. This mirrors the
// canonical ABI's lower_flat_list() (definitions.py), which calls
// store_list_into_range() -- the same allocate+store helper storeList uses
// for the Store/Load path (see allocStoreList in memory.go).
func lowerFlatList(v Value, elemType binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	list, ok := v.([]Value)
	if !ok {
		return nil, fmt.Errorf("lowerFlat list: expected []Value, got %T", v)
	}

	ptr, length, err := allocStoreList(mem, list, elemType, resolve, realloc)
	if err != nil {
		return nil, fmt.Errorf("lowerFlatList: %w", err)
	}

	return []CoreValue{
		NewCoreValueI32(ptr),
		NewCoreValueI32(length),
	}, nil
}

// lowerFlatRecord lowers a record value.
func lowerFlatRecord(v Value, desc binary.RecordDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	fields, ok := v.([]Value)
	if !ok {
		return nil, fmt.Errorf("lowerFlat record: expected []Value, got %T", v)
	}

	if len(fields) != len(desc.Fields) {
		return nil, fmt.Errorf("lowerFlat record: expected %d fields, got %d", len(desc.Fields), len(fields))
	}

	var flat []CoreValue
	for i, field := range desc.Fields {
		fieldType, err := resolveType(&field.Type, resolve)
		if err != nil {
			return nil, err
		}
		fieldFlat, err := lowerFlatImpl(fields[i], fieldType, resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("lowerFlat record field %s: %w", field.Name, err)
		}
		flat = append(flat, fieldFlat...)
	}
	return flat, nil
}

// lowerFlatVariant lowers a variant value.
// This is where the JOIN and COERCE operations happen.
func lowerFlatVariant(v Value, desc binary.VariantDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	vv, ok := v.(VariantValue)
	if !ok {
		return nil, fmt.Errorf("lowerFlat variant: expected VariantValue, got %T", v)
	}

	if int(vv.Disc) >= len(desc.Cases) {
		return nil, fmt.Errorf("lowerFlat variant: case index %d out of range [0,%d)", vv.Disc, len(desc.Cases))
	}

	// Get the flattened variant type to know the joined flat types.
	variantFlat, err := Flatten(desc, resolve)
	if err != nil {
		return nil, err
	}

	// First element is the discriminant (i32).
	if len(variantFlat) == 0 || variantFlat[0] != "i32" {
		return nil, fmt.Errorf("lowerFlat variant: expected first element to be i32 discriminant")
	}

	// Remove the discriminant from the flat types list.
	joinedFlat := variantFlat[1:]

	// Lower the case payload.
	c := desc.Cases[vv.Disc]
	var payload []CoreValue
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return nil, err
		}

		casePayload, err := lowerFlatImpl(vv.Payload, caseType, resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("lowerFlat variant case %d: %w", vv.Disc, err)
		}

		// Get the native flat types for this case.
		caseFlat, err := Flatten(caseType, resolve)
		if err != nil {
			return nil, err
		}

		// Coerce the payload core values to the joined flat types.
		// This is the critical part: reinterpret payload values to match joined types.
		for i := 0; i < len(casePayload) && i < len(joinedFlat); i++ {
			have := caseFlat[i]
			want := joinedFlat[i]
			payload = append(payload, coerceValue(casePayload[i], have, want))
		}
	}

	// Pad with zeros for remaining joined positions.
	for i := len(payload); i < len(joinedFlat); i++ {
		zv, err := zeroCoreValueForKind(joinedFlat[i])
		if err != nil {
			return nil, fmt.Errorf("lowerFlat variant: %w", err)
		}
		payload = append(payload, zv)
	}

	// Prepend discriminant.
	result := []CoreValue{NewCoreValueI32(vv.Disc)}
	result = append(result, payload...)
	return result, nil
}

// zeroCoreValueForKind returns the zero CoreValue for a flat core type name
// ("i32", "i64", "f32", "f64"). Used to pad a variant/option/result case
// whose own flattening is narrower than the type's joined flat width -- the
// canonical ABI always emits a value for every joined position, even when a
// given case has nothing to contribute there.
func zeroCoreValueForKind(kind string) (CoreValue, error) {
	switch kind {
	case "i32":
		return NewCoreValueI32(0), nil
	case "i64":
		return NewCoreValueI64(0), nil
	case "f32":
		return NewCoreValueF32(0), nil
	case "f64":
		return NewCoreValueF64(0), nil
	default:
		return CoreValue{}, fmt.Errorf("unknown joined type %s", kind)
	}
}

// coerceValue reinterprets a CoreValue from one core type to another.
// This implements the canonical ABI coercion in lower_flat_variant.
func coerceValue(cv CoreValue, have, want string) CoreValue {
	switch {
	case have == want:
		// No coercion needed.
		return cv

	case have == "f32" && want == "i32":
		// Encode f32 as i32 bits.
		f := cv.AsF32()
		bits := math.Float32bits(f)
		return NewCoreValueI32(bits)

	case have == "i32" && want == "i64":
		// Extend i32 to i64.
		return NewCoreValueI64(uint64(cv.AsI32()))

	case have == "f32" && want == "i64":
		// Encode f32 as i32 bits, then extend to i64.
		f := cv.AsF32()
		bits := math.Float32bits(f)
		return NewCoreValueI64(uint64(bits))

	case have == "f64" && want == "i64":
		// Encode f64 as i64 bits.
		f := cv.AsF64()
		bits := math.Float64bits(f)
		return NewCoreValueI64(bits)

	default:
		// Unknown coercion; should not happen if flatten is correct.
		// Return the value unchanged (which will likely cause issues downstream).
		return cv
	}
}

// lowerFlatTuple lowers a tuple value.
func lowerFlatTuple(v Value, desc binary.TupleDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	elements, ok := v.([]Value)
	if !ok {
		return nil, fmt.Errorf("lowerFlat tuple: expected []Value, got %T", v)
	}

	if len(elements) != len(desc.Elements) {
		return nil, fmt.Errorf("lowerFlat tuple: expected %d elements, got %d", len(desc.Elements), len(elements))
	}

	var flat []CoreValue
	for i, elemRef := range desc.Elements {
		elemType, err := resolveType(&elemRef, resolve)
		if err != nil {
			return nil, err
		}
		elemFlat, err := lowerFlatImpl(elements[i], elemType, resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("lowerFlat tuple element %d: %w", i, err)
		}
		flat = append(flat, elemFlat...)
	}
	return flat, nil
}

// lowerFlatFlags lowers a flags value.
func lowerFlatFlags(v Value, desc binary.FlagsDesc) ([]CoreValue, error) {
	// Flags are represented as a uint32 bitset.
	flags, ok := v.(uint32)
	if !ok {
		return nil, fmt.Errorf("lowerFlat flags: expected uint32, got %T", v)
	}
	return []CoreValue{NewCoreValueI32(flags)}, nil
}

// lowerFlatEnum lowers an enum value.
func lowerFlatEnum(v Value, desc binary.EnumDesc) ([]CoreValue, error) {
	// Enum is represented as a uint32 case index.
	idx, ok := v.(uint32)
	if !ok {
		return nil, fmt.Errorf("lowerFlat enum: expected uint32, got %T", v)
	}
	if int(idx) >= len(desc.Cases) {
		return nil, fmt.Errorf("lowerFlat enum: case index %d out of range [0,%d)", idx, len(desc.Cases))
	}
	return []CoreValue{NewCoreValueI32(idx)}, nil
}

// lowerFlatOption lowers an option value.
// Option despecializes to a 2-case variant (none, some(T)): the "none" case
// carries no payload of its own but must still be zero-padded out to T's
// flat width, since Flatten(OptionDesc) always reserves that many joined
// positions regardless of which case is active.
func lowerFlatOption(v Value, elemType binary.TypeDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	elemFlatTypes, err := Flatten(elemType, resolve)
	if err != nil {
		return nil, err
	}

	if v == nil {
		// None: discriminant 0, then zero-padding for the element's width.
		result := make([]CoreValue, 0, 1+len(elemFlatTypes))
		result = append(result, NewCoreValueI32(0))
		for _, ft := range elemFlatTypes {
			zv, err := zeroCoreValueForKind(ft)
			if err != nil {
				return nil, fmt.Errorf("lowerFlat option: %w", err)
			}
			result = append(result, zv)
		}
		return result, nil
	}

	// Some: discriminant 1, then the element. No coercion is needed here:
	// Option only ever joins one real case's flat types against zero
	// padding, which is always a no-op join (join(x, x) == x).
	elemVals, err := lowerFlatImpl(v, elemType, resolve, realloc, mem)
	if err != nil {
		return nil, err
	}
	result := []CoreValue{NewCoreValueI32(1)}
	result = append(result, elemVals...)
	return result, nil
}

// lowerFlatResult lowers a result value.
// Result despecializes to a 2-case variant (ok(T), error(E)): like a
// variant, the two arms' flat types are joined position-by-position (see
// join() in coerceValue), and whichever arm is inactive must still be
// zero-padded/coerced out to the joined width.
func lowerFlatResult(v Value, desc binary.ResultDesc, resolve Resolver, realloc Realloc, mem []byte) ([]CoreValue, error) {
	rv, ok := v.(ResultValue)
	if !ok {
		return nil, fmt.Errorf("lowerFlat result: expected ResultValue, got %T", v)
	}

	resultFlat, err := Flatten(desc, resolve)
	if err != nil {
		return nil, err
	}
	if len(resultFlat) == 0 || resultFlat[0] != "i32" {
		return nil, fmt.Errorf("lowerFlat result: expected first element to be i32 discriminant")
	}
	joinedFlat := resultFlat[1:]

	var disc uint32
	var armRef *binary.TypeRef
	if rv.IsErr {
		disc = 1
		armRef = desc.Err
	} else {
		disc = 0
		armRef = desc.Ok
	}

	var payload []CoreValue
	if armRef != nil {
		armType, err := resolveType(armRef, resolve)
		if err != nil {
			return nil, err
		}
		armVals, err := lowerFlatImpl(rv.Payload, armType, resolve, realloc, mem)
		if err != nil {
			return nil, fmt.Errorf("lowerFlat result payload: %w", err)
		}
		armFlatTypes, err := Flatten(armType, resolve)
		if err != nil {
			return nil, err
		}
		for i := 0; i < len(armVals) && i < len(joinedFlat); i++ {
			payload = append(payload, coerceValue(armVals[i], armFlatTypes[i], joinedFlat[i]))
		}
	}

	// Pad with zeros for remaining joined positions the active arm didn't
	// fill (e.g. a narrower Ok payload when Err is wider, or vice versa).
	for i := len(payload); i < len(joinedFlat); i++ {
		zv, err := zeroCoreValueForKind(joinedFlat[i])
		if err != nil {
			return nil, fmt.Errorf("lowerFlat result: %w", err)
		}
		payload = append(payload, zv)
	}

	result := []CoreValue{NewCoreValueI32(disc)}
	result = append(result, payload...)
	return result, nil
}

// SpillValue stores v (of type t) to a freshly realloc'd region of linear
// memory and returns the pointer -- the Canonical ABI's spill-to-memory path,
// exported so the instance layer can spill a whole parameter list (as a tuple)
// when it flattens beyond MaxFlatParams and the core func takes a single
// pointer instead. Equivalent to what LowerFlatInto does internally for a
// single spilling value.
func SpillValue(v Value, t binary.TypeDesc, mem []byte, resolve Resolver, realloc Realloc) (uint32, error) {
	return spillValue(v, t, mem, resolve, realloc)
}

// spillValue stores a value to memory and returns the pointer.
func spillValue(v Value, t binary.TypeDesc, mem []byte, resolve Resolver, realloc Realloc) (uint32, error) {
	align, err := Alignment(t, resolve)
	if err != nil {
		return 0, err
	}

	size, err := Size(t, resolve)
	if err != nil {
		return 0, err
	}

	// Allocate memory.
	ptr, err := realloc.Grow(0, 0, align, size)
	if err != nil {
		return 0, fmt.Errorf("spillValue: realloc failed: %w", err)
	}

	// Store the value.
	if err := Store(mem, ptr, t, v, resolve, realloc); err != nil {
		return 0, fmt.Errorf("spillValue: store failed: %w", err)
	}

	return ptr, nil
}

// ============================================================================
// LIFT
// ============================================================================

// LiftFlat converts a slice of CoreValues back to a Value in the flat ABI.
// If the flattened type exceeds MaxFlatParams, the first CoreValue is
// expected to be a pointer to memory where the value is stored.
//
// This mirrors the canonical ABI lift_flat() function.
func LiftFlat(vals []CoreValue, t binary.TypeDesc, resolve Resolver, mem []byte) (Value, error) {
	flatTypes, err := Flatten(t, resolve)
	if err != nil {
		return nil, err
	}

	if len(flatTypes) > MaxFlatParams {
		// Spilled: first value is a pointer to memory.
		if len(vals) != 1 {
			return nil, fmt.Errorf("LiftFlat: expected 1 value for spilled type, got %d", len(vals))
		}
		ptr := vals[0].AsI32()
		return Load(mem, ptr, t, resolve)
	}

	// Non-spilled: lift from the core values. The iterator is pooled (it
	// escapes into liftFlatImpl as a valueIter interface, so a fresh one would
	// allocate every call); nothing retains it past liftFlatImpl's return.
	vi := getCoreValueIter(vals)
	val, err := liftFlatImpl(vi, t, resolve, mem)
	putCoreValueIter(vi)
	return val, err
}

// valueIter is an interface for iterators that provide CoreValues.
type valueIter interface {
	Next() (CoreValue, error)
	Done() bool
}

// liftFlatImpl recursively lifts a value from core values.
// vi can be either a *CoreValueIter or any other type implementing valueIter.
// mem is only threaded through for the string/list cases, which must load
// their backing data from linear memory even in the flat ABI.
func liftFlatImpl(vi valueIter, t binary.TypeDesc, resolve Resolver, mem []byte) (Value, error) {
	switch desc := t.(type) {
	case binary.PrimitiveDesc:
		return liftFlatPrimitive(vi, desc.Prim, mem)

	case binary.ListDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return liftFlatList(vi, elemType, resolve, mem)

	case binary.RecordDesc:
		return liftFlatRecord(vi, desc, resolve, mem)

	case binary.VariantDesc:
		return liftFlatVariant(vi, desc, resolve, mem)

	case binary.TupleDesc:
		return liftFlatTuple(vi, desc, resolve, mem)

	case binary.FlagsDesc:
		return liftFlatFlags(vi, desc)

	case binary.EnumDesc:
		return liftFlatEnum(vi, desc)

	case binary.OptionDesc:
		elemType, err := resolveType(&desc.Element, resolve)
		if err != nil {
			return nil, err
		}
		return liftFlatOption(vi, elemType, resolve, mem)

	case binary.ResultDesc:
		return liftFlatResult(vi, desc, resolve, mem)

	case binary.OwnDesc, binary.BorrowDesc, binary.StreamDesc, binary.FutureDesc:
		// stream/future values are opaque i32 handles, same lifting as
		// own/borrow.
		cv, err := vi.Next()
		if err != nil {
			return nil, err
		}
		return cv.AsI32(), nil

	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return nil, fmt.Errorf("liftFlat: unsupported type %T", t)

	default:
		return nil, fmt.Errorf("liftFlat: unknown type descriptor %T", t)
	}
}

// liftFlatPrimitive lifts a primitive value. mem is only used by the string
// case (strings must load their UTF-8 bytes from linear memory even in the
// flat ABI).
func liftFlatPrimitive(vi valueIter, prim string, mem []byte) (Value, error) {
	// A string is the only primitive that reads more than one core value
	// (ptr, len) and loads from memory; every other primitive is a single
	// scalar core value, lifted by liftScalarPrimitive.
	if prim == "string" {
		return liftFlatString(vi, mem)
	}
	cv, err := vi.Next()
	if err != nil {
		return nil, err
	}
	return liftScalarPrimitive(cv, prim)
}

// liftScalarPrimitive lifts a single (non-string) primitive from one core value
// -- the direct path the compiled LiftStep uses for a scalar result, with no
// iterator and no Flatten. Kept byte-for-byte equivalent to the per-case logic
// liftFlatPrimitive used inline.
func liftScalarPrimitive(cv CoreValue, prim string) (Value, error) {
	switch prim {
	case "bool":
		return cv.AsI32() != 0, nil
	case "u8":
		return cv.AsI32() & 0xFF, nil
	case "u16":
		return cv.AsI32() & 0xFFFF, nil
	case "u32":
		return cv.AsI32(), nil
	case "u64":
		return cv.AsI64(), nil
	case "s8":
		i := cv.AsI32() & 0xFF
		if i >= 0x80 {
			return int32(int8(i)), nil
		}
		return int32(i), nil
	case "s16":
		i := cv.AsI32() & 0xFFFF
		if i >= 0x8000 {
			return int32(int16(i)), nil
		}
		return int32(i), nil
	case "s32":
		return int32(cv.AsI32()), nil
	case "s64":
		return int64(cv.AsI64()), nil
	case "f32":
		return cv.AsF32(), nil
	case "f64":
		return cv.AsF64(), nil
	case "char":
		i := cv.AsI32()
		if i >= 0x110000 {
			return nil, fmt.Errorf("liftFlat char: value %d out of range", i)
		}
		if i >= 0xD800 && i <= 0xDFFF {
			return nil, fmt.Errorf("liftFlat char: surrogate half %d not allowed", i)
		}
		return rune(i), nil
	case "error-context":
		// Opaque i32 handle -- same lifting as own/borrow.
		return cv.AsI32(), nil
	default:
		return nil, fmt.Errorf("liftFlat: unknown primitive type %s", prim)
	}
}

// liftFlatString lifts a string value.
// String is (ptr i32, length i32): the two core values are read from vi and
// the UTF-8 bytes are loaded from linear memory at that range. This mirrors
// the canonical ABI's lift_flat_string() (definitions.py), which calls
// load_string_from_range() -- the same helper loadString uses for the
// Store/Load path (see loadStringFromRange in memory.go).
func liftFlatString(vi valueIter, mem []byte) (Value, error) {
	ptrCV, err := vi.Next()
	if err != nil {
		return nil, err
	}
	lenCV, err := vi.Next()
	if err != nil {
		return nil, err
	}
	s, err := loadStringFromRange(mem, ptrCV.AsI32(), lenCV.AsI32())
	if err != nil {
		return nil, fmt.Errorf("liftFlatString: %w", err)
	}
	return s, nil
}

// liftFlatList lifts a list value.
// List is (ptr i32, length i32): the two core values are read from vi and
// each element is loaded from linear memory at that range. This mirrors the
// canonical ABI's lift_flat_list() (definitions.py), which calls
// load_list_from_range() -- the same helper loadList uses for the
// Store/Load path (see loadListFromRange in memory.go).
func liftFlatList(vi valueIter, elemType binary.TypeDesc, resolve Resolver, mem []byte) (Value, error) {
	ptrCV, err := vi.Next()
	if err != nil {
		return nil, err
	}
	lenCV, err := vi.Next()
	if err != nil {
		return nil, err
	}
	list, err := loadListFromRange(mem, ptrCV.AsI32(), lenCV.AsI32(), elemType, resolve)
	if err != nil {
		return nil, fmt.Errorf("liftFlatList: %w", err)
	}
	return list, nil
}

// liftFlatRecord lifts a record value.
func liftFlatRecord(vi valueIter, desc binary.RecordDesc, resolve Resolver, mem []byte) (Value, error) {
	result := make([]Value, len(desc.Fields))
	for i, field := range desc.Fields {
		fieldType, err := resolveType(&field.Type, resolve)
		if err != nil {
			return nil, err
		}
		v, err := liftFlatImpl(vi, fieldType, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("liftFlat record field %s: %w", field.Name, err)
		}
		result[i] = v
	}
	return result, nil
}

// liftFlatVariant lifts a variant value.
func liftFlatVariant(vi valueIter, desc binary.VariantDesc, resolve Resolver, mem []byte) (Value, error) {
	// Get the flattened variant type.
	variantFlat, err := Flatten(desc, resolve)
	if err != nil {
		return nil, err
	}

	if len(variantFlat) == 0 || variantFlat[0] != "i32" {
		return nil, fmt.Errorf("liftFlat variant: expected first element to be i32 discriminant")
	}

	// Read the discriminant.
	discCV, err := vi.Next()
	if err != nil {
		return nil, err
	}
	caseIdx := discCV.AsI32()

	if int(caseIdx) >= len(desc.Cases) {
		return nil, fmt.Errorf("liftFlat variant: case index %d out of range [0,%d)", caseIdx, len(desc.Cases))
	}

	// Get the joined flat types (excluding discriminant).
	joinedFlat := variantFlat[1:]

	// Create a coercing iterator for the case payload.
	c := desc.Cases[caseIdx]
	var caseFlat []string
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return nil, err
		}
		caseFlat, err = Flatten(caseType, resolve)
		if err != nil {
			return nil, err
		}
	}

	// Read the case payload THROUGH vi (which may itself be an enclosing
	// coercingValueIter), so a nested variant/result advances every level's
	// consumption count -- see coercingValueIter's doc.
	coerceVI := &coercingValueIter{
		underlying: vi,
		joinedFlat: joinedFlat,
		caseFlat:   caseFlat,
		idx:        0,
	}

	// Lift the case payload using the coercing iterator (implements valueIter).
	var payload Value
	if c.Type != nil {
		caseType, err := resolveType(c.Type, resolve)
		if err != nil {
			return nil, err
		}
		payload, err = liftFlatImpl(coerceVI, caseType, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("liftFlat variant case %d: %w", caseIdx, err)
		}
	}

	// Consume remaining joined flat values.
	remainingJoined := len(joinedFlat) - coerceVI.idx
	for i := 0; i < remainingJoined; i++ {
		_, err := vi.Next()
		if err != nil {
			return nil, err
		}
	}

	// Return the variant value.
	return VariantValue{
		Disc:    caseIdx,
		Payload: payload,
	}, nil
}

// coercingValueIter wraps another valueIter (a raw *CoreValueIter, or -- when a
// variant/result nests inside another -- an enclosing coercingValueIter) and
// applies the per-position join coercion for a variant/result case payload.
//
// underlying is a valueIter, NOT a concrete *CoreValueIter, precisely so these
// compose: a variant whose active case is itself a variant/result reads its
// payload THROUGH the enclosing coercingValueIter, one value at a time, so
// every level's idx advances in lockstep with the raw values actually consumed.
// (The previous version held a *CoreValueIter and unwrapped every nested
// variant/result back to the raw iterator, which read the raw values without
// advancing the enclosing level's idx -- so the enclosing "consume remaining
// joined padding" over-read the exhausted iterator: CoreValueIter index out of
// range, e.g. result<record, variant> lifting the variant err arm.)
type coercingValueIter struct {
	underlying valueIter
	joinedFlat []string
	caseFlat   []string
	idx        int
}

// Next reads one value from the underlying iterator and coerces it from the
// joined core type stored at this position back to the case's own core type.
func (cvi *coercingValueIter) Next() (CoreValue, error) {
	if cvi.idx >= len(cvi.caseFlat) {
		return CoreValue{}, fmt.Errorf("coercingValueIter: index out of range")
	}

	// have is the type actually stored at this joined position (what the
	// lowering side coerced *to*); want is the type this case's own flat
	// traversal is asking for (what the lowering side coerced *from*). This
	// order matters: e.g. a u32 case joined against a u64 case is stored as
	// i64, so lifting it back into the u32 case needs have="i64",
	// want="i32" to wrap it back down -- getting this backwards makes
	// deCoerceValue a no-op and CoreValue.AsI32() panics on an i64.
	have := cvi.joinedFlat[cvi.idx]
	want := cvi.caseFlat[cvi.idx]
	cvi.idx++

	cv, err := cvi.underlying.Next()
	if err != nil {
		return CoreValue{}, err
	}
	return deCoerceValue(cv, have, want), nil
}

// Done reports whether all case flat values have been consumed.
func (cvi *coercingValueIter) Done() bool {
	return cvi.idx == len(cvi.caseFlat)
}

// deCoerceValue applies the inverse of coerceValue for lifting (the read
// path): a value that was widened/reinterpreted to fit the variant's joined
// core type on the way out must be narrowed/reinterpreted back to the
// specific case's own core type on the way back in. This mirrors the
// canonical ABI's CoerceValueIter.next() (definitions.py, lift_flat_variant).
func deCoerceValue(cv CoreValue, have, want string) CoreValue {
	switch {
	case have == want:
		// No coercion needed.
		return cv

	case have == "i32" && want == "f32":
		// Decode i32 bits as f32 (inverse of coerceValue's f32->i32).
		return NewCoreValueF32(math.Float32frombits(cv.AsI32()))

	case have == "i64" && want == "i32":
		// Wrap i64 down to its low 32 bits (inverse of coerceValue's i32->i64).
		return NewCoreValueI32(uint32(cv.AsI64()))

	case have == "i64" && want == "f32":
		// Wrap i64 to i32, then decode those bits as f32 (inverse of
		// coerceValue's f32->i32->i64 chain).
		return NewCoreValueF32(math.Float32frombits(uint32(cv.AsI64())))

	case have == "i64" && want == "f64":
		// Decode i64 bits as f64 (inverse of coerceValue's f64->i64).
		return NewCoreValueF64(math.Float64frombits(cv.AsI64()))

	default:
		// Unknown coercion; should not happen if flatten is correct.
		// Return the value unchanged (which will likely cause issues downstream).
		return cv
	}
}

// liftFlatTuple lifts a tuple value.
func liftFlatTuple(vi valueIter, desc binary.TupleDesc, resolve Resolver, mem []byte) (Value, error) {
	result := make([]Value, len(desc.Elements))
	for i, elemRef := range desc.Elements {
		elemType, err := resolveType(&elemRef, resolve)
		if err != nil {
			return nil, err
		}
		v, err := liftFlatImpl(vi, elemType, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("liftFlat tuple element %d: %w", i, err)
		}
		result[i] = v
	}
	return result, nil
}

// liftFlatFlags lifts a flags value.
func liftFlatFlags(vi valueIter, desc binary.FlagsDesc) (Value, error) {
	cv, err := vi.Next()
	if err != nil {
		return nil, err
	}
	return cv.AsI32(), nil
}

// liftFlatEnum lifts an enum value.
func liftFlatEnum(vi valueIter, desc binary.EnumDesc) (Value, error) {
	cv, err := vi.Next()
	if err != nil {
		return nil, err
	}
	idx := cv.AsI32()
	if int(idx) >= len(desc.Cases) {
		return nil, fmt.Errorf("liftFlat enum: case index %d out of range [0,%d)", idx, len(desc.Cases))
	}
	return idx, nil
}

// liftFlatOption lifts an option value.
// Mirrors lowerFlatOption: the "none" case must consume (and discard) the
// same zero-padding lowerFlatOption emitted for T's flat width, so the
// value-iterator stays aligned for whatever follows the option.
func liftFlatOption(vi valueIter, elemType binary.TypeDesc, resolve Resolver, mem []byte) (Value, error) {
	cv, err := vi.Next()
	if err != nil {
		return nil, err
	}
	disc := cv.AsI32()

	switch disc {
	case 0:
		// None: consume and discard the padding.
		elemFlatTypes, err := Flatten(elemType, resolve)
		if err != nil {
			return nil, err
		}
		for range elemFlatTypes {
			if _, err := vi.Next(); err != nil {
				return nil, err
			}
		}
		return nil, nil
	case 1:
		// Some
		return liftFlatImpl(vi, elemType, resolve, mem)
	default:
		return nil, fmt.Errorf("liftFlat option: invalid discriminant %d", disc)
	}
}

// liftFlatResult lifts a result value.
// Mirrors lowerFlatResult: the active arm's core values are read through a
// coercingValueIter (the same mechanism liftFlatVariant uses) so values that
// were coerced up to the joined type on the way out are decoded correctly,
// and any joined positions the active arm didn't fill are consumed and
// discarded so the outer iterator stays aligned.
func liftFlatResult(vi valueIter, desc binary.ResultDesc, resolve Resolver, mem []byte) (Value, error) {
	cv, err := vi.Next()
	if err != nil {
		return nil, err
	}
	disc := cv.AsI32()

	resultFlat, err := Flatten(desc, resolve)
	if err != nil {
		return nil, err
	}
	if len(resultFlat) == 0 || resultFlat[0] != "i32" {
		return nil, fmt.Errorf("liftFlat result: expected first element to be i32 discriminant")
	}
	joinedFlat := resultFlat[1:]

	var armRef *binary.TypeRef
	switch disc {
	case 0:
		armRef = desc.Ok
	case 1:
		armRef = desc.Err
	default:
		return nil, fmt.Errorf("liftFlat result: invalid discriminant %d", disc)
	}

	var armFlatTypes []string
	if armRef != nil {
		armType, err := resolveType(armRef, resolve)
		if err != nil {
			return nil, err
		}
		armFlatTypes, err = Flatten(armType, resolve)
		if err != nil {
			return nil, err
		}
	}

	// Read the arm payload THROUGH vi (which may be an enclosing
	// coercingValueIter) so nested variant/result levels compose -- see
	// coercingValueIter's doc.
	coerceVI := &coercingValueIter{
		underlying: vi,
		joinedFlat: joinedFlat,
		caseFlat:   armFlatTypes,
	}

	var payload Value
	if armRef != nil {
		armType, err := resolveType(armRef, resolve)
		if err != nil {
			return nil, err
		}
		payload, err = liftFlatImpl(coerceVI, armType, resolve, mem)
		if err != nil {
			return nil, fmt.Errorf("liftFlat result payload: %w", err)
		}
	}

	// Consume remaining joined flat values the active arm didn't fill.
	remainingJoined := len(joinedFlat) - coerceVI.idx
	for i := 0; i < remainingJoined; i++ {
		if _, err := vi.Next(); err != nil {
			return nil, err
		}
	}

	return ResultValue{IsErr: disc == 1, Payload: payload}, nil
}
