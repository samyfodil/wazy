package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

func TestI31Encoding(t *testing.T) {
	for _, tc := range []struct {
		v     uint32
		wantS uint32
		wantU uint32
	}{
		{0, 0, 0},
		{1, 1, 1},
		{0x3fffffff, 0x3fffffff, 0x3fffffff}, // the largest positive i31
		{0x40000000, 0xc0000000, 0x40000000}, // bit 30 set: negative when signed
		{0x7fffffff, 0xffffffff, 0x7fffffff}, // -1
		{0xffffffff, 0xffffffff, 0x7fffffff}, // the high bit is dropped, not carried
	} {
		ref := EncodeI31(tc.v)
		require.True(t, IsI31(ref), "%#x", tc.v)
		require.False(t, IsGCHeapRef(ref))
		require.NotEqual(t, GCRefNull, ref, "an i31 is never null, not even zero")
		require.Equal(t, tc.wantS, DecodeI31S(ref), "signed %#x", tc.v)
		require.Equal(t, tc.wantU, DecodeI31U(ref), "unsigned %#x", tc.v)
	}
}

func TestGCHeapRefTags(t *testing.T) {
	var h GCHeap
	ref := h.Alloc(&GCObject{Type: &FunctionType{CompositeKind: CompositeKindStruct}})
	require.True(t, IsGCHeapRef(ref))
	require.False(t, IsI31(ref))
	require.NotEqual(t, GCRefNull, ref)

	// A raw host pointer, which any.convert_extern can put in the any hierarchy, is neither.
	const hostPointer = uint64(0x7f0011223344)
	require.False(t, IsGCHeapRef(hostPointer))
	require.False(t, IsI31(hostPointer))
	require.False(t, IsGCHeapRef(GCRefNull))
}

func TestGCHeapDeref(t *testing.T) {
	var h GCHeap
	o := &GCObject{Type: &FunctionType{CompositeKind: CompositeKindStruct}, TypeID: 7}
	ref := h.Alloc(o)
	require.Equal(t, o, h.Deref(ref))

	id, ok := h.TypeIDOf(ref)
	require.True(t, ok)
	require.Equal(t, FunctionTypeID(7), id)

	t.Run("null traps", func(t *testing.T) {
		require.Equal(t, wasmruntime.ErrRuntimeNullReference, requirePanic(t, func() { h.Deref(GCRefNull) }))
	})
	t.Run("an i31 is not a heap object", func(t *testing.T) {
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, requirePanic(t, func() { h.Deref(EncodeI31(3)) }))
	})
	t.Run("a host reference is not a heap object", func(t *testing.T) {
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, requirePanic(t, func() { h.Deref(0x1234) }))
		_, ok := h.TypeIDOf(0x1234)
		require.False(t, ok)
	})
	t.Run("a tagged index past the end traps", func(t *testing.T) {
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, requirePanic(t, func() { h.Deref(ref + 100) }))
		_, ok := h.TypeIDOf(ref + 100)
		require.False(t, ok)
	})
}

func TestGCObjectPackedFields(t *testing.T) {
	st := &FunctionType{CompositeKind: CompositeKindStruct, Fields: []FieldType{
		{Type: ValueTypeI8}, {Type: ValueTypeI16}, {Type: ValueTypeI32}, {Type: ValueTypeI64},
	}}
	o := &GCObject{Type: st, Fields: make([]uint64, 4)}

	// A packed field is stored truncated, so the two reads differ only in how they widen it.
	o.Set(0, 0xfff1)
	require.Equal(t, uint64(0xf1), o.Fields[0], "the store truncates to the storage width")
	require.Equal(t, uint64(0xfffffff1), o.Get(0, true))
	require.Equal(t, uint64(0xf1), o.Get(0, false))

	o.Set(1, 0xffff8001)
	require.Equal(t, uint64(0x8001), o.Fields[1])
	require.Equal(t, uint64(0xffff8001), o.Get(1, true))
	require.Equal(t, uint64(0x8001), o.Get(1, false))

	// An unpacked field is stored and read whole, whatever signedness is asked for.
	o.Set(2, 0xdeadbeef)
	require.Equal(t, uint64(0xdeadbeef), o.Get(2, true))
	require.Equal(t, uint64(0xdeadbeef), o.Get(2, false))
	o.Set(3, 0xfedcba9876543210)
	require.Equal(t, uint64(0xfedcba9876543210), o.Get(3, false))

	t.Run("an array shares one storage type across every element", func(t *testing.T) {
		at := &FunctionType{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeI8}}}
		a := &GCObject{Type: at, Fields: make([]uint64, 3)}
		require.True(t, a.IsArray())
		a.Set(2, 0x1ff)
		require.Equal(t, uint64(0xff), a.Fields[2])
		require.Equal(t, uint64(0xffffffff), a.Get(2, true))
	})
}

// gcTestModule builds a module instance over the given types, with a store and a heap, ready for RunGC.
func gcTestModule(t *testing.T, types []FunctionType, data []DataInstance, elems []ElementInstance) *ModuleInstance {
	t.Helper()
	s := NewStore(api.CoreFeaturesV2, nil)
	CanonicalizeTypes(types)
	ids, err := s.GetFunctionTypeIDs(types)
	require.NoError(t, err)
	return &ModuleInstance{
		s: s, TypeIDs: ids, Source: &Module{TypeSection: types},
		DataInstances: data, ElementInstances: elems,
	}
}

func TestRunGC_Structs(t *testing.T) {
	// type[0] is a struct of a mutable packed i8 and an immutable i64.
	m := gcTestModule(t, []FunctionType{
		{CompositeKind: CompositeKindStruct, Fields: []FieldType{
			{Type: ValueTypeI8, Mutable: true}, {Type: ValueTypeI64},
		}},
	}, nil, nil)

	t.Run("new_default zeroes every field", func(t *testing.T) {
		ref := RunGC(m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
		require.Equal(t, uint64(0), RunGC(m, GCStructGet, ref, 0, 0, 0, 0, nil))
		require.Equal(t, uint64(0), RunGC(m, GCStructGet, ref, 1, 0, 0, 0, nil))
	})

	t.Run("new takes its fields from the scratch area", func(t *testing.T) {
		ref := RunGC(m, GCStructNew, 0, 0, 0, 0, 0, []uint64{0xff, 42})
		require.Equal(t, uint64(0xffffffff), RunGC(m, GCStructGet, ref, 0, 1, 0, 0, nil), "signed read")
		require.Equal(t, uint64(0xff), RunGC(m, GCStructGet, ref, 0, 0, 0, 0, nil), "unsigned read")
		require.Equal(t, uint64(42), RunGC(m, GCStructGet, ref, 1, 0, 0, 0, nil))
	})

	t.Run("set writes through the storage type", func(t *testing.T) {
		ref := RunGC(m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
		RunGC(m, GCStructSet, ref, 0, 0x1234, 0, 0, nil)
		require.Equal(t, uint64(0x34), RunGC(m, GCStructGet, ref, 0, 0, 0, 0, nil))
	})

	t.Run("a null reference traps", func(t *testing.T) {
		require.Equal(t, wasmruntime.ErrRuntimeNullReference, requirePanic(t, func() {
			RunGC(m, GCStructGet, GCRefNull, 0, 0, 0, 0, nil)
		}))
	})
}

func TestRunGC_Arrays(t *testing.T) {
	// type[0] is an array of mutable i32.
	m := gcTestModule(t, []FunctionType{
		{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeI32, Mutable: true}}},
	}, nil, nil)
	newArray := func(n, init uint64) uint64 { return RunGC(m, GCArrayNew, 0, n, init, 0, 0, nil) }

	t.Run("new fills with the initial value", func(t *testing.T) {
		ref := newArray(3, 7)
		require.Equal(t, uint64(3), RunGC(m, GCArrayLen, ref, 0, 0, 0, 0, nil))
		for i := uint64(0); i < 3; i++ {
			require.Equal(t, uint64(7), RunGC(m, GCArrayGet, ref, i, 0, 0, 0, nil))
		}
	})

	t.Run("new_default zeroes", func(t *testing.T) {
		ref := RunGC(m, GCArrayNewDefault, 0, 2, 0, 0, 0, nil)
		require.Equal(t, uint64(0), RunGC(m, GCArrayGet, ref, 1, 0, 0, 0, nil))
	})

	t.Run("new_fixed takes its elements from the scratch area", func(t *testing.T) {
		ref := RunGC(m, GCArrayNewFixed, 0, 0, 0, 0, 0, []uint64{4, 5, 6})
		require.Equal(t, uint64(3), RunGC(m, GCArrayLen, ref, 0, 0, 0, 0, nil))
		require.Equal(t, uint64(6), RunGC(m, GCArrayGet, ref, 2, 0, 0, 0, nil))
	})

	t.Run("fill writes a range", func(t *testing.T) {
		ref := newArray(5, 0)
		RunGC(m, GCArrayFill, ref, 1, 9, 3, 0, nil)
		want := []uint64{0, 9, 9, 9, 0}
		for i, w := range want {
			require.Equal(t, w, RunGC(m, GCArrayGet, ref, uint64(i), 0, 0, 0, nil), "element %d", i)
		}
	})

	t.Run("copy handles overlap in both directions", func(t *testing.T) {
		forward := RunGC(m, GCArrayNewFixed, 0, 0, 0, 0, 0, []uint64{1, 2, 3, 4, 5})
		RunGC(m, GCArrayCopy, forward, 0, forward, 2, 3, nil) // dst < src
		require.Equal(t, []uint64{3, 4, 5, 4, 5}, gcArrayContents(m, forward, 5))

		backward := RunGC(m, GCArrayNewFixed, 0, 0, 0, 0, 0, []uint64{1, 2, 3, 4, 5})
		RunGC(m, GCArrayCopy, backward, 2, backward, 0, 3, nil) // dst > src
		require.Equal(t, []uint64{1, 2, 1, 2, 3}, gcArrayContents(m, backward, 5))
	})

	t.Run("out of bounds traps", func(t *testing.T) {
		ref := newArray(2, 0)
		oob := wasmruntime.ErrRuntimeOutOfBoundsArrayAccess
		require.Equal(t, oob, requirePanic(t, func() { RunGC(m, GCArrayGet, ref, 2, 0, 0, 0, nil) }))
		require.Equal(t, oob, requirePanic(t, func() { RunGC(m, GCArraySet, ref, 2, 0, 0, 0, nil) }))
		require.Equal(t, oob, requirePanic(t, func() { RunGC(m, GCArrayFill, ref, 1, 0, 2, 0, nil) }))
		require.Equal(t, oob, requirePanic(t, func() { RunGC(m, GCArrayCopy, ref, 0, ref, 0, 3, nil) }))
		// A length past the ceiling has to trap rather than be handed to make.
		require.Equal(t, oob, requirePanic(t, func() { RunGC(m, GCArrayNewDefault, 0, 1<<40, 0, 0, 0, nil) }))
	})
}

func gcArrayContents(m *ModuleInstance, ref uint64, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = RunGC(m, GCArrayGet, ref, uint64(i), 0, 0, 0, nil)
	}
	return out
}

func TestRunGC_ArraysFromSegments(t *testing.T) {
	data := []DataInstance{{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}}
	elems := []ElementInstance{{11, 22, 33}}

	t.Run("from a data segment, elements are read at their storage width", func(t *testing.T) {
		m := gcTestModule(t, []FunctionType{
			{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeI16, Mutable: true}}},
		}, data, nil)
		ref := RunGC(m, GCArrayNewData, 0, 0, 0, 3, 0, nil)
		require.Equal(t, []uint64{0x0201, 0x0403, 0x0605}, gcArrayContents(m, ref, 3))

		// init_data writes into an existing array at an offset.
		dst := RunGC(m, GCArrayNewDefault, 0, 3, 0, 0, 0, nil)
		RunGC(m, GCArrayInitData, dst, 1, 0, 2, 2, nil)
		require.Equal(t, []uint64{0, 0x0403, 0x0605}, gcArrayContents(m, dst, 3))
	})

	t.Run("reading past a data segment traps", func(t *testing.T) {
		m := gcTestModule(t, []FunctionType{
			{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeI16, Mutable: true}}},
		}, data, nil)
		require.Equal(t, wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess, requirePanic(t, func() {
			RunGC(m, GCArrayNewData, 0, 0, 0, 4, 0, nil) // 4 * 2 bytes > 6
		}))
		require.Equal(t, wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess, requirePanic(t, func() {
			RunGC(m, GCArrayNewData, 0, 9, 0, 1, 0, nil) // unknown segment
		}))
	})

	t.Run("from an element segment, elements are references", func(t *testing.T) {
		m := gcTestModule(t, []FunctionType{
			{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeAnyref, Mutable: true}}},
		}, nil, elems)
		ref := RunGC(m, GCArrayNewElem, 0, 0, 1, 2, 0, nil)
		require.Equal(t, []uint64{22, 33}, gcArrayContents(m, ref, 2))

		// An element segment is a table, so overrunning one is a table trap, not a memory one.
		require.Equal(t, wasmruntime.ErrRuntimeInvalidTableAccess, requirePanic(t, func() {
			RunGC(m, GCArrayNewElem, 0, 0, 0, 4, 0, nil)
		}))
		require.Equal(t, wasmruntime.ErrRuntimeInvalidTableAccess, requirePanic(t, func() {
			RunGC(m, GCArrayNewElem, 0, 9, 0, 1, 0, nil)
		}))
	})
}

func TestRunGC_I31AndEq(t *testing.T) {
	m := gcTestModule(t, []FunctionType{{}}, nil, nil)

	ref := RunGC(m, GCRefI31, 0x7fffffff, 0, 0, 0, 0, nil)
	require.Equal(t, uint64(0xffffffff), RunGC(m, GCI31GetS, ref, 0, 0, 0, 0, nil))
	require.Equal(t, uint64(0x7fffffff), RunGC(m, GCI31GetU, ref, 0, 0, 0, 0, nil))

	require.Equal(t, wasmruntime.ErrRuntimeNullReference, requirePanic(t, func() {
		RunGC(m, GCI31GetS, GCRefNull, 0, 0, 0, 0, nil)
	}))

	require.Equal(t, uint64(1), RunGC(m, GCRefEq, ref, ref, 0, 0, 0, nil))
	require.Equal(t, uint64(0), RunGC(m, GCRefEq, ref, EncodeI31(1), 0, 0, 0, nil))
	require.Equal(t, uint64(1), RunGC(m, GCRefEq, GCRefNull, GCRefNull, 0, 0, 0, nil))
}

func TestRunGC_RefTestOverTheAnyHierarchy(t *testing.T) {
	m := gcTestModule(t, []FunctionType{
		{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}}},
		{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeI32}}},
	}, nil, nil)

	structRef := RunGC(m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
	arrayRef := RunGC(m, GCArrayNewDefault, 1, 1, 0, 0, 0, nil)
	i31Ref := EncodeI31(5)
	const hostRef = uint64(0x7f0011223344) // what any.convert_extern puts in the any hierarchy

	abstract := func(vt ValueType) uint64 { return EncodeRefTarget(uint32(vt.Kind()), false, false) }
	test := func(ref, target uint64) uint64 { return RunGC(m, GCCheckRefTest, ref, target, 0, 0, 0, nil) }

	for _, ref := range []uint64{structRef, arrayRef, i31Ref, hostRef} {
		require.Equal(t, uint64(1), test(ref, abstract(ValueTypeAnyref)), "%#x is anyref", ref)
		require.Equal(t, uint64(0), test(ref, abstract(ValueTypeNullref)), "%#x is not none", ref)
	}
	// A host value is in the any hierarchy but has no identity, so it is not an eq type.
	require.Equal(t, uint64(0), test(hostRef, abstract(ValueTypeEqref)))
	for _, ref := range []uint64{structRef, arrayRef, i31Ref} {
		require.Equal(t, uint64(1), test(ref, abstract(ValueTypeEqref)), "%#x is eqref", ref)
	}

	require.Equal(t, uint64(1), test(structRef, abstract(ValueTypeStructref)))
	require.Equal(t, uint64(0), test(arrayRef, abstract(ValueTypeStructref)))
	require.Equal(t, uint64(0), test(i31Ref, abstract(ValueTypeStructref)))
	require.Equal(t, uint64(0), test(hostRef, abstract(ValueTypeStructref)))

	require.Equal(t, uint64(1), test(arrayRef, abstract(ValueTypeArrayref)))
	require.Equal(t, uint64(0), test(structRef, abstract(ValueTypeArrayref)))

	require.Equal(t, uint64(1), test(i31Ref, abstract(ValueTypeI31ref)))
	require.Equal(t, uint64(0), test(structRef, abstract(ValueTypeI31ref)))

	// A concrete target resolves through the heap, so an i31 and a host value match nothing concrete.
	require.Equal(t, uint64(1), test(structRef, EncodeRefTarget(0, false, true)))
	require.Equal(t, uint64(0), test(arrayRef, EncodeRefTarget(0, false, true)))
	require.Equal(t, uint64(0), test(i31Ref, EncodeRefTarget(0, false, true)))
	require.Equal(t, uint64(0), test(hostRef, EncodeRefTarget(0, false, true)))

	// null is of exactly the nullable types, whatever the target names.
	require.Equal(t, uint64(1), test(GCRefNull, EncodeRefTarget(0, true, true)))
	require.Equal(t, uint64(0), test(GCRefNull, EncodeRefTarget(0, false, true)))
}

func TestStorageWidth(t *testing.T) {
	for vt, want := range map[ValueType]uint64{
		ValueTypeI8:   1,
		ValueTypeI16:  2,
		ValueTypeI32:  4,
		ValueTypeF32:  4,
		ValueTypeI64:  8,
		ValueTypeF64:  8,
		ValueTypeV128: 16,
	} {
		require.Equal(t, want, storageWidth(vt), ValueTypeName(vt))
	}
}

func TestRunGC_UnknownModePanics(t *testing.T) {
	m := gcTestModule(t, []FunctionType{{}}, nil, nil)
	require.Equal(t, "BUG: unknown GC runtime mode", requirePanic(t, func() {
		RunGC(m, 0xdead, 0, 0, 0, 0, 0, nil)
	}))
}
