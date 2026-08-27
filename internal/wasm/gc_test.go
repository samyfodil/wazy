package wasm

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

func structType(fields ...FieldType) FunctionType {
	return FunctionType{CompositeKind: CompositeKindStruct, Fields: fields}
}

func arrayType(f FieldType) FunctionType {
	return FunctionType{CompositeKind: CompositeKindArray, Fields: []FieldType{f}}
}

func TestValueTypeMatches_AbstractLattice(t *testing.T) {
	tests := []struct {
		name           string
		actual, expect ValueType
		want           bool
	}{
		{"identity", ValueTypeAnyref, ValueTypeAnyref, true},
		{"eq under any", ValueTypeEqref, ValueTypeAnyref, true},
		{"any not under eq", ValueTypeAnyref, ValueTypeEqref, false},
		{"i31 under eq", ValueTypeI31ref, ValueTypeEqref, true},
		{"i31 under any", ValueTypeI31ref, ValueTypeAnyref, true},
		{"struct under eq", ValueTypeStructref, ValueTypeEqref, true},
		{"array under any", ValueTypeArrayref, ValueTypeAnyref, true},
		{"struct and array unrelated", ValueTypeStructref, ValueTypeArrayref, false},
		{"none under any", ValueTypeNullref, ValueTypeAnyref, true},
		{"none under struct", ValueTypeNullref, ValueTypeStructref, true},
		{"none not under func", ValueTypeNullref, ValueTypeFuncref, false},
		{"nofunc under func", ValueTypeNullFuncref, ValueTypeFuncref, true},
		{"nofunc not under any", ValueTypeNullFuncref, ValueTypeAnyref, false},
		{"noextern under extern", ValueTypeNullExternref, ValueTypeExternref, true},
		{"noexn under exn", ValueTypeNullExnref, ValueTypeExnref, true},
		{"func and any hierarchies disjoint", ValueTypeFuncref, ValueTypeAnyref, false},
		{"extern and any hierarchies disjoint", ValueTypeExternref, ValueTypeAnyref, false},
		{"non-nullable under nullable", ValueTypeEqref.AsNonNullable(), ValueTypeAnyref, true},
		{"nullable not under non-nullable", ValueTypeEqref, ValueTypeAnyref.AsNonNullable(), false},
		{"i32 is not a ref", ValueTypeI32, ValueTypeAnyref, false},
		{"anyref is not i32", ValueTypeAnyref, ValueTypeI32, false},
		{"i32 matches i32", ValueTypeI32, ValueTypeI32, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, valueTypeMatches(tc.actual, tc.expect, nil))
		})
	}
}

func TestValueTypeMatches_ConcreteRefs(t *testing.T) {
	// type[0] struct, type[1] its declared subtype, type[2] an unrelated array, type[3] a func.
	types := []FunctionType{
		{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}}, Extensible: true},
		{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}, {Type: ValueTypeI64}}, HasSupertype: true, Supertype: 0},
		arrayType(FieldType{Type: ValueTypeI8}),
		{},
	}
	CanonicalizeTypes(types)

	ref := func(i uint32) ValueType { return ValueTypeConcreteRef(i, true) }

	require.True(t, valueTypeMatches(ref(1), ref(0), types))
	require.False(t, valueTypeMatches(ref(0), ref(1), types))
	require.True(t, valueTypeMatches(ref(0), ValueTypeStructref, types))
	require.True(t, valueTypeMatches(ref(1), ValueTypeEqref, types))
	require.False(t, valueTypeMatches(ref(0), ValueTypeArrayref, types))
	require.True(t, valueTypeMatches(ref(2), ValueTypeArrayref, types))
	require.True(t, valueTypeMatches(ref(3), ValueTypeFuncref, types))
	require.False(t, valueTypeMatches(ref(3), ValueTypeAnyref, types))
	require.False(t, valueTypeMatches(ref(0), ref(2), types))

	// Bottoms sit under concrete types of their own hierarchy only.
	require.True(t, valueTypeMatches(ValueTypeNullref, ref(0), types))
	require.False(t, valueTypeMatches(ValueTypeNullFuncref, ref(0), types))
	require.True(t, valueTypeMatches(ValueTypeNullFuncref, ref(3), types))
	require.False(t, valueTypeMatches(ValueTypeNullref, ref(3), types))
	require.False(t, valueTypeMatches(ValueTypeStructref, ref(0), types))

	// Without a type section a concrete ref is treated as a function ref, which is the pre-GC behaviour.
	require.True(t, valueTypeMatches(ref(0), ValueTypeFuncref, nil))
	require.True(t, valueTypeMatches(ValueTypeNullFuncref, ref(0), nil))
	require.True(t, valueTypeMatches(ref(0), ref(0), nil))
	require.False(t, valueTypeMatches(ref(0), ref(1), nil))

	// An out-of-range index resolves to nothing and matches only itself.
	require.True(t, valueTypeMatches(ref(99), ref(99), types))
	require.False(t, valueTypeMatches(ref(99), ValueTypeStructref, types))
}

func TestConcreteIsSubtypeOf_CycleIsBounded(t *testing.T) {
	// validateTypeSection rejects these, but concreteIsSubtypeOf runs during that very validation, so a cycle
	// must terminate rather than spin.
	types := []FunctionType{
		{HasSupertype: true, Supertype: 1},
		{HasSupertype: true, Supertype: 0},
	}
	CanonicalizeTypes(types)
	require.False(t, concreteIsSubtypeOf(0, 2, types))
}

func TestCanonicalizeTypes(t *testing.T) {
	t.Run("identical standalone types share a canonical index", func(t *testing.T) {
		types := []FunctionType{
			{Params: []ValueType{ValueTypeI32}},
			{Params: []ValueType{ValueTypeI32}},
			{Params: []ValueType{ValueTypeI64}},
		}
		CanonicalizeTypes(types)
		require.Equal(t, Index(0), types[0].CanonicalIndex)
		require.Equal(t, Index(0), types[1].CanonicalIndex)
		require.Equal(t, Index(2), types[2].CanonicalIndex)
	})

	t.Run("self-referential types are compared positionally", func(t *testing.T) {
		// (rec (type $t1 (func (param (ref $t1))))) twice: the two are the same type.
		types := []FunctionType{
			{Params: []ValueType{ValueTypeConcreteRef(0, false)}, RecGroupSize: 1},
			{Params: []ValueType{ValueTypeConcreteRef(1, false)}, RecGroupSize: 1},
		}
		CanonicalizeTypes(types)
		require.Equal(t, Index(0), types[1].CanonicalIndex)
		require.True(t, valueTypeMatches(ValueTypeConcreteRef(1, false), ValueTypeConcreteRef(0, false), types))
	})

	t.Run("rec group position matters", func(t *testing.T) {
		// (rec (func) (struct)) then (rec (struct) (func)): the func members are NOT the same type.
		types := []FunctionType{
			{RecGroupSize: 2, RecGroupPosition: 0},
			mkRec(structType(), 2, 1),
			mkRec(structType(), 2, 0),
			{RecGroupSize: 2, RecGroupPosition: 1},
		}
		CanonicalizeTypes(types)
		require.Equal(t, Index(0), types[0].CanonicalIndex)
		require.Equal(t, Index(2), types[2].CanonicalIndex)
		require.False(t, valueTypeMatches(ValueTypeConcreteRef(3, false), ValueTypeConcreteRef(0, false), types))
	})

	t.Run("a differing member splits the whole group", func(t *testing.T) {
		types := []FunctionType{
			{RecGroupSize: 2, RecGroupPosition: 0},
			mkRec(structType(FieldType{Type: ValueTypeI32}), 2, 1),
			{RecGroupSize: 2, RecGroupPosition: 0},
			mkRec(structType(FieldType{Type: ValueTypeI64}), 2, 1),
		}
		CanonicalizeTypes(types)
		require.Equal(t, Index(2), types[2].CanonicalIndex)
	})

	t.Run("supertype and mutability are part of identity", func(t *testing.T) {
		types := []FunctionType{
			structType(FieldType{Type: ValueTypeI32}),
			structType(FieldType{Type: ValueTypeI32, Mutable: true}),
			arrayType(FieldType{Type: ValueTypeI32}),
		}
		types[0].Extensible = true
		CanonicalizeTypes(types)
		require.Equal(t, Index(1), types[1].CanonicalIndex)
		require.Equal(t, Index(2), types[2].CanonicalIndex)
	})
}

func mkRec(ft FunctionType, size, pos int) FunctionType {
	ft.RecGroupSize, ft.RecGroupPosition = size, pos
	return ft
}

func TestValidateTypeSection(t *testing.T) {
	gc := api.CoreFeaturesV2 | api.CoreFeatureTypedFunctionReferences | api.CoreFeatureGC

	tests := []struct {
		name        string
		types       []FunctionType
		features    api.CoreFeatures
		expectedErr string
	}{
		{
			name:  "no supertype is always fine",
			types: []FunctionType{{}, structType()},
		},
		{
			name: "struct may add fields",
			types: []FunctionType{
				{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}}, Extensible: true},
				{CompositeKind: CompositeKindStruct, Fields: []FieldType{{Type: ValueTypeI32}, {Type: ValueTypeI64}}, HasSupertype: true},
			},
		},
		{
			name:        "gc must be enabled",
			types:       []FunctionType{{Extensible: true}, {HasSupertype: true}},
			features:    api.CoreFeaturesV2,
			expectedErr: `type[1] declares a supertype, which is invalid as feature "gc" is disabled`,
		},
		{
			name:        "supertype must exist",
			types:       []FunctionType{{}, {HasSupertype: true, Supertype: 7}},
			expectedErr: "type[1]: unknown supertype 7",
		},
		{
			name:        "supertype must not be final",
			types:       []FunctionType{{}, {HasSupertype: true}},
			expectedErr: "type[1]: supertype 0 is final",
		},
		{
			name:        "supertype must precede the subtype",
			types:       []FunctionType{{Extensible: true, HasSupertype: true, Supertype: 0}},
			expectedErr: "type[0]: supertype 0 is not defined before it",
		},
		{
			name:        "composite kind must match",
			types:       []FunctionType{{Extensible: true}, mkSub(structType(), 0)},
			expectedErr: "type[1] is not a subtype of type[0]: composite kind differs",
		},
		{
			name: "func arity must match",
			types: []FunctionType{
				{Params: []ValueType{ValueTypeI32}, Extensible: true},
				mkSub(FunctionType{}, 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: arity differs",
		},
		{
			name: "func params are contravariant",
			types: []FunctionType{
				{Params: []ValueType{ValueTypeEqref}, Extensible: true},
				mkSub(FunctionType{Params: []ValueType{ValueTypeAnyref}}, 0),
			},
		},
		{
			name: "narrowing a param is rejected",
			types: []FunctionType{
				{Params: []ValueType{ValueTypeAnyref}, Extensible: true},
				mkSub(FunctionType{Params: []ValueType{ValueTypeEqref}}, 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: param[0] is not a supertype of the supertype's",
		},
		{
			name: "func results are covariant",
			types: []FunctionType{
				{Results: []ValueType{ValueTypeAnyref}, Extensible: true},
				mkSub(FunctionType{Results: []ValueType{ValueTypeEqref}}, 0),
			},
		},
		{
			name: "widening a result is rejected",
			types: []FunctionType{
				{Results: []ValueType{ValueTypeEqref}, Extensible: true},
				mkSub(FunctionType{Results: []ValueType{ValueTypeAnyref}}, 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: result[0] is not a subtype of the supertype's",
		},
		{
			name: "a struct may not drop fields",
			types: []FunctionType{
				mkOpen(structType(FieldType{Type: ValueTypeI32}, FieldType{Type: ValueTypeI64})),
				mkSub(structType(FieldType{Type: ValueTypeI32}), 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: has fewer fields than the supertype",
		},
		{
			name: "an immutable field may narrow",
			types: []FunctionType{
				mkOpen(structType(FieldType{Type: ValueTypeAnyref})),
				mkSub(structType(FieldType{Type: ValueTypeEqref}), 0),
			},
		},
		{
			name: "an immutable field may not widen",
			types: []FunctionType{
				mkOpen(structType(FieldType{Type: ValueTypeEqref})),
				mkSub(structType(FieldType{Type: ValueTypeAnyref}), 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: field[0]: anyref is not a subtype of eqref",
		},
		{
			name: "a mutable field must be identical",
			types: []FunctionType{
				mkOpen(structType(FieldType{Type: ValueTypeAnyref, Mutable: true})),
				mkSub(structType(FieldType{Type: ValueTypeEqref, Mutable: true}), 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: field[0]: mutable field types differ",
		},
		{
			name: "mutability may not change",
			types: []FunctionType{
				mkOpen(structType(FieldType{Type: ValueTypeI32, Mutable: true})),
				mkSub(structType(FieldType{Type: ValueTypeI32}), 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: field[0]: mutability differs",
		},
		{
			name: "an array element may narrow",
			types: []FunctionType{
				mkOpen(arrayType(FieldType{Type: ValueTypeAnyref})),
				mkSub(arrayType(FieldType{Type: ValueTypeEqref}), 0),
			},
		},
		{
			name: "an array element may not widen",
			types: []FunctionType{
				mkOpen(arrayType(FieldType{Type: ValueTypeEqref})),
				mkSub(arrayType(FieldType{Type: ValueTypeAnyref}), 0),
			},
			expectedErr: "type[1] is not a subtype of type[0]: element: anyref is not a subtype of eqref",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			features := tc.features
			if features == 0 {
				features = gc
			}
			m := &Module{TypeSection: tc.types}
			CanonicalizeTypes(m.TypeSection)
			err := m.validateTypeSection(features)
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, tc.expectedErr)
			}
		})
	}
}

func mkOpen(ft FunctionType) FunctionType {
	ft.Extensible = true
	return ft
}

func mkSub(ft FunctionType, super Index) FunctionType {
	ft.HasSupertype, ft.Supertype = true, super
	return ft
}

func TestGCValueTypeNames(t *testing.T) {
	for vt, want := range map[ValueType]string{
		ValueTypeAnyref:                     "anyref",
		ValueTypeAnyref.AsNonNullable():     "(ref any)",
		ValueTypeEqref:                      "eqref",
		ValueTypeI31ref:                     "i31ref",
		ValueTypeStructref.AsNonNullable():  "(ref struct)",
		ValueTypeArrayref:                   "arrayref",
		ValueTypeNullref:                    "nullref",
		ValueTypeNullFuncref:                "nullfuncref",
		ValueTypeNullExternref:              "nullexternref",
		ValueTypeNullExnref.AsNonNullable(): "(ref noexn)",
		ValueTypeI8:                         "i8",
		ValueTypeI16:                        "i16",
	} {
		require.Equal(t, want, ValueTypeName(vt))
	}
}

func TestGCHeapTypesAreRefs(t *testing.T) {
	for _, vt := range []ValueType{
		ValueTypeAnyref, ValueTypeEqref, ValueTypeI31ref, ValueTypeStructref, ValueTypeArrayref,
		ValueTypeNullref, ValueTypeNullFuncref, ValueTypeNullExternref, ValueTypeNullExnref,
	} {
		require.True(t, vt.IsRef(), "expected %s to be a reference type", ValueTypeName(vt))
		require.True(t, vt.IsGCHeapType(), "expected %s to be a GC heap type", ValueTypeName(vt))
	}
	for _, vt := range []ValueType{ValueTypeI32, ValueTypeI64, ValueTypeF32, ValueTypeF64, ValueTypeV128, ValueTypeI8, ValueTypeI16} {
		require.False(t, vt.IsRef(), "expected %s to not be a reference type", ValueTypeName(vt))
		require.False(t, vt.IsGCHeapType())
	}
	// The pre-GC reference kinds keep answering the way they did, and are not GC heap types.
	for _, vt := range []ValueType{ValueTypeFuncref, ValueTypeExternref, ValueTypeExnref} {
		require.True(t, vt.IsRef())
		require.False(t, vt.IsGCHeapType())
	}
}

func TestAbstractHeapTypeValueType(t *testing.T) {
	for ht, want := range map[int64]ValueType{
		HeapTypeFunc:     ValueTypeFuncref,
		HeapTypeExtern:   ValueTypeExternref,
		HeapTypeExn:      ValueTypeExnref,
		HeapTypeNoExn:    ValueTypeNullExnref,
		HeapTypeNoFunc:   ValueTypeNullFuncref,
		HeapTypeNoExtern: ValueTypeNullExternref,
		HeapTypeNone:     ValueTypeNullref,
		HeapTypeAny:      ValueTypeAnyref,
		HeapTypeEq:       ValueTypeEqref,
		HeapTypeI31:      ValueTypeI31ref,
		HeapTypeStruct:   ValueTypeStructref,
		HeapTypeArray:    ValueTypeArrayref,
	} {
		got, ok := AbstractHeapTypeValueType(ht)
		require.True(t, ok, "heap type %d", ht)
		require.Equal(t, want, got)
	}
	for _, ht := range []int64{-11, -24, -100} {
		_, ok := AbstractHeapTypeValueType(ht)
		require.False(t, ok, "heap type %d", ht)
	}
}

func TestFunctionTypeKeyDistinguishesGCTypes(t *testing.T) {
	distinct := map[string]FunctionType{
		"func":           {},
		"struct":         structType(),
		"struct i32":     structType(FieldType{Type: ValueTypeI32}),
		"struct mut i32": structType(FieldType{Type: ValueTypeI32, Mutable: true}),
		"array i32":      arrayType(FieldType{Type: ValueTypeI32}),
		"array i8":       arrayType(FieldType{Type: ValueTypeI8}),
		"open func":      mkOpen(FunctionType{}),
		"sub func":       mkSub(FunctionType{}, 3),
	}
	seen := make(map[string]string, len(distinct))
	for name, ft := range distinct {
		key := ft.String()
		if prev, ok := seen[key]; ok {
			t.Fatalf("%s and %s share the key %q", prev, name, key)
		}
		seen[key] = name
	}
	// A plain function type keeps its historical key spelling exactly.
	ft := FunctionType{Params: []ValueType{ValueTypeI32}}
	require.Equal(t, "i32_v", ft.String())
}

func TestHierarchyTop(t *testing.T) {
	types := []FunctionType{structType(), arrayType(FieldType{Type: ValueTypeI32}), {}}
	CanonicalizeTypes(types)
	for _, tc := range []struct {
		vt   ValueType
		want ValueType
	}{
		{ValueTypeAnyref, ValueTypeAnyref},
		{ValueTypeEqref, ValueTypeAnyref},
		{ValueTypeI31ref, ValueTypeAnyref},
		{ValueTypeStructref, ValueTypeAnyref},
		{ValueTypeArrayref, ValueTypeAnyref},
		{ValueTypeNullref, ValueTypeAnyref},
		{ValueTypeFuncref, ValueTypeFuncref},
		{ValueTypeNullFuncref, ValueTypeFuncref},
		{ValueTypeExternref, ValueTypeExternref},
		{ValueTypeNullExternref, ValueTypeExternref},
		{ValueTypeExnref, ValueTypeExnref},
		{ValueTypeNullExnref, ValueTypeExnref},
		{ValueTypeConcreteRef(0, true), ValueTypeAnyref},  // struct
		{ValueTypeConcreteRef(1, true), ValueTypeAnyref},  // array
		{ValueTypeConcreteRef(2, true), ValueTypeFuncref}, // func
	} {
		require.Equal(t, tc.want.Kind(), hierarchyTop(tc.vt, types), ValueTypeName(tc.vt))
	}
	// Without a type section a concrete ref is assumed to be a function reference.
	require.Equal(t, ValueTypeFuncref.Kind(), hierarchyTop(ValueTypeConcreteRef(0, true), nil))
}

func TestEncodeRefTarget(t *testing.T) {
	for _, tc := range []struct {
		indexOrKind        uint32
		nullable, concrete bool
	}{
		{0, false, false},
		{7, true, true},
		{uint32(ValueTypeAnyref.Kind()), true, false},
	} {
		d := EncodeRefTarget(tc.indexOrKind, tc.nullable, tc.concrete)
		require.Equal(t, tc.indexOrKind, uint32(d>>2))
		require.Equal(t, tc.nullable, d&1 != 0)
		require.Equal(t, tc.concrete, d&2 != 0)
	}
}

// refEngine is a ModuleEngine that only answers TypeIDOfReference, which is all a GC runtime type check on a
// function reference needs from one.
type refEngine struct {
	ModuleEngine
	typeID FunctionTypeID
}

func (e *refEngine) TypeIDOfReference(Reference) FunctionTypeID { return e.typeID }

func TestRunGCCheck(t *testing.T) {
	s := NewStore(api.CoreFeaturesV2, nil)
	// $sup is extensible, $sub declares it as its supertype, $other is unrelated.
	ids, err := s.GetFunctionTypeIDs([]FunctionType{
		mkOpen(FunctionType{}),
		mkSub(FunctionType{}, 0),
		{Params: []ValueType{ValueTypeI32}},
	})
	require.NoError(t, err)
	sup, sub, other := ids[0], ids[1], ids[2]
	require.NotEqual(t, sup, sub)

	m := &ModuleInstance{s: s, TypeIDs: ids, Engine: &refEngine{typeID: sub}}
	const someRef = uint64(0x1234)

	t.Run("ref.test on a concrete target", func(t *testing.T) {
		require.Equal(t, uint64(1), RunGCCheck(m, someRef, EncodeRefTarget(1, false, true), GCCheckRefTest))
		require.Equal(t, uint64(1), RunGCCheck(m, someRef, EncodeRefTarget(0, false, true), GCCheckRefTest))
		require.Equal(t, uint64(0), RunGCCheck(m, someRef, EncodeRefTarget(2, false, true), GCCheckRefTest))
	})

	t.Run("null is of exactly the nullable types", func(t *testing.T) {
		require.Equal(t, uint64(1), RunGCCheck(m, 0, EncodeRefTarget(0, true, true), GCCheckRefTest))
		require.Equal(t, uint64(0), RunGCCheck(m, 0, EncodeRefTarget(0, false, true), GCCheckRefTest))
	})

	t.Run("abstract targets", func(t *testing.T) {
		top := EncodeRefTarget(uint32(ValueTypeFuncref.Kind()), false, false)
		require.Equal(t, uint64(1), RunGCCheck(m, someRef, top, GCCheckRefTest))
		bottom := EncodeRefTarget(uint32(ValueTypeNullFuncref.Kind()), false, false)
		require.Equal(t, uint64(0), RunGCCheck(m, someRef, bottom, GCCheckRefTest))
	})

	t.Run("an out of range type index matches nothing", func(t *testing.T) {
		require.Equal(t, uint64(0), RunGCCheck(m, someRef, EncodeRefTarget(99, false, true), GCCheckRefTest))
	})

	t.Run("ref.cast traps instead of returning zero", func(t *testing.T) {
		require.Equal(t, uint64(1), RunGCCheck(m, someRef, EncodeRefTarget(0, false, true), GCCheckRefCast))
		captured := requirePanic(t, func() {
			RunGCCheck(m, someRef, EncodeRefTarget(2, false, true), GCCheckRefCast)
		})
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, captured)
	})

	t.Run("an indirect call accepts a subtype and traps otherwise", func(t *testing.T) {
		require.Equal(t, uint64(0), RunGCCheck(m, uint64(sub), uint64(sup), GCCheckIndirectCall))
		captured := requirePanic(t, func() {
			RunGCCheck(m, uint64(sup), uint64(sub), GCCheckIndirectCall)
		})
		require.Equal(t, wasmruntime.ErrRuntimeIndirectCallTypeMismatch, captured)
		captured = requirePanic(t, func() {
			RunGCCheck(m, uint64(other), uint64(sup), GCCheckIndirectCall)
		})
		require.Equal(t, wasmruntime.ErrRuntimeIndirectCallTypeMismatch, captured)
	})
}

func requirePanic(t *testing.T, f func()) (captured any) {
	t.Helper()
	defer func() { captured = recover() }()
	f()
	t.Fatal("expected a panic, but there was none")
	return
}

func TestGetFunctionTypeIDs_RecGroupIdentity(t *testing.T) {
	// (rec (func) (struct)) and (rec (struct) (func)) are different types even though each member looks
	// identical in isolation, and a group whose sibling differs splits too.
	group := func(members ...FunctionType) []FunctionType {
		for i := range members {
			members[i].RecGroupSize, members[i].RecGroupPosition = len(members), i
		}
		return members
	}
	s := NewStore(api.CoreFeaturesV2, nil)

	a := group(FunctionType{}, structType())
	b := group(structType(), FunctionType{})
	c := group(FunctionType{}, structType(FieldType{Type: ValueTypeI32}))

	idsA, err := s.GetFunctionTypeIDs(a)
	require.NoError(t, err)
	idsB, err := s.GetFunctionTypeIDs(b)
	require.NoError(t, err)
	idsC, err := s.GetFunctionTypeIDs(c)
	require.NoError(t, err)

	require.NotEqual(t, idsA[0], idsB[1]) // same shape, different position in the group
	require.NotEqual(t, idsA[0], idsC[0]) // same shape, but a sibling differs

	// Declaring the same group twice lands on the same IDs.
	idsA2, err := s.GetFunctionTypeIDs(group(FunctionType{}, structType()))
	require.NoError(t, err)
	require.Equal(t, idsA, idsA2)

	// A plain function type is still identified by its signature alone, whatever else the store has seen.
	plain, err := s.GetFunctionTypeIDs([]FunctionType{{Params: []ValueType{ValueTypeI32}}})
	require.NoError(t, err)
	plain2, err := s.GetFunctionTypeIDs([]FunctionType{{Params: []ValueType{ValueTypeI32}}})
	require.NoError(t, err)
	require.Equal(t, plain, plain2)
}

func TestTypeIDIsSubtypeOf(t *testing.T) {
	s := NewStore(api.CoreFeaturesV2, nil)
	ids, err := s.GetFunctionTypeIDs([]FunctionType{
		mkOpen(FunctionType{}),
		mkOpen(mkSub(FunctionType{}, 0)),
		mkSub(FunctionType{}, 1),
		{Params: []ValueType{ValueTypeI32}},
	})
	require.NoError(t, err)

	require.True(t, s.TypeIDIsSubtypeOf(ids[2], ids[2]))
	require.True(t, s.TypeIDIsSubtypeOf(ids[2], ids[1]))
	require.True(t, s.TypeIDIsSubtypeOf(ids[2], ids[0])) // two links up the chain
	require.False(t, s.TypeIDIsSubtypeOf(ids[0], ids[2]))
	require.False(t, s.TypeIDIsSubtypeOf(ids[2], ids[3]))
	// An ID the store never handed out has no chain to walk.
	require.False(t, s.TypeIDIsSubtypeOf(FunctionTypeID(1<<20), ids[0]))
}

func TestBottomKind(t *testing.T) {
	for _, tc := range []struct {
		kind ValueType
		want ValueType
		ok   bool
	}{
		{ValueTypeAnyref, ValueTypeNullref, true},
		{ValueTypeStructref, ValueTypeNullref, true},
		{ValueTypeFuncref, ValueTypeNullFuncref, true},
		{ValueTypeExternref, ValueTypeNullExternref, true},
		{ValueTypeExnref, ValueTypeNullExnref, true},
		// The bottoms have none of their own, and neither do the numeric kinds.
		{ValueTypeNullref, 0, false},
		{ValueTypeNullFuncref, 0, false},
		{ValueTypeI32, 0, false},
	} {
		got, ok := bottomKind(tc.kind.Kind())
		require.Equal(t, tc.ok, ok, ValueTypeName(tc.kind))
		require.Equal(t, tc.want.Kind(), got, ValueTypeName(tc.kind))
	}
}

func TestHasConcreteRef(t *testing.T) {
	ref := ValueTypeConcreteRef(0, true)
	require.False(t, hasConcreteRef(&FunctionType{}))
	require.False(t, hasConcreteRef(&FunctionType{Params: []ValueType{ValueTypeI32}, Results: []ValueType{ValueTypeFuncref}}))
	require.True(t, hasConcreteRef(&FunctionType{Params: []ValueType{ValueTypeI32, ref}}))
	require.True(t, hasConcreteRef(&FunctionType{Results: []ValueType{ValueTypeI32, ref}}))
}

func TestCheckFieldSubtype_PackedTypes(t *testing.T) {
	// A packed field narrows to nothing but itself, mutable or not.
	i8, i16 := FieldType{Type: ValueTypeI8}, FieldType{Type: ValueTypeI16}
	require.NoError(t, checkFieldSubtype(i8, i8, nil))
	require.EqualError(t, checkFieldSubtype(i8, i16, nil), "i8 is not a subtype of i16")
	mutI8 := FieldType{Type: ValueTypeI8, Mutable: true}
	require.NoError(t, checkFieldSubtype(mutI8, mutI8, nil))
}
