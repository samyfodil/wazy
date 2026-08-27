package wasm

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

// This file holds the type system of the GC proposal: the abstract heap type lattice, subtyping over it, and
// validation of the declared `sub`/`sub final` relation in the type section.
//
// See https://github.com/WebAssembly/gc/blob/main/proposals/gc/MVP.md#types

// heapTypeParent returns the immediate supertype of an abstract heap type kind, and false for the tops (any,
// func, extern, exn) which have none. Concrete types are not handled here: their supertypes are declared, and
// the lattice is reached only once the chain runs out (see concreteTop).
func heapTypeParent(kind byte) (byte, bool) {
	switch kind {
	case ValueTypeEqref.Kind():
		return ValueTypeAnyref.Kind(), true
	case ValueTypeI31ref.Kind(), ValueTypeStructref.Kind(), ValueTypeArrayref.Kind():
		return ValueTypeEqref.Kind(), true
	}
	// The bottom types (none, nofunc, noextern, noexn) have no single parent to climb to: each sits below
	// *every* type in its hierarchy, which isAbstractSubtype handles from the other end via bottomKind.
	return 0, false
}

// concreteTop returns the abstract heap type kind that a concrete type of the given composite kind sits under.
func concreteTop(k CompositeKind) byte {
	switch k {
	case CompositeKindStruct:
		return ValueTypeStructref.Kind()
	case CompositeKindArray:
		return ValueTypeArrayref.Kind()
	default:
		return ValueTypeFuncref.Kind()
	}
}

// bottomKind returns the bottom heap type of the hierarchy the given abstract kind belongs to, and false for
// kinds that are themselves bottoms.
func bottomKind(kind byte) (byte, bool) {
	switch kind {
	case ValueTypeAnyref.Kind(), ValueTypeEqref.Kind(), ValueTypeI31ref.Kind(),
		ValueTypeStructref.Kind(), ValueTypeArrayref.Kind():
		return ValueTypeNullref.Kind(), true
	case ValueTypeFuncref.Kind():
		return ValueTypeNullFuncref.Kind(), true
	case ValueTypeExternref.Kind():
		return ValueTypeNullExternref.Kind(), true
	case ValueTypeExnref.Kind():
		return ValueTypeNullExnref.Kind(), true
	}
	return 0, false
}

// isHeapSubtype reports whether the heap type of a is a subtype of the heap type of b, ignoring nullability.
//
// types resolves concrete references to their declared supertype chain and composite kind. It may be nil at
// call sites that compare types from different modules, where a local type index carries no meaning; there,
// concrete types only match each other by exact index, which is what the pre-GC code already did.
func isHeapSubtype(a, b ValueType, types []FunctionType) bool {
	if a.IsConcreteRef() {
		if b.IsConcreteRef() {
			return concreteIsSubtypeOf(a.TypeIndex(), b.TypeIndex(), types)
		}
		// A concrete type reaches the lattice at the top of its composite kind. Without a type section to
		// resolve the index, assume func, which is how concrete refs were classified before GC.
		top := ValueTypeFuncref.Kind()
		if def := typeAt(types, a.TypeIndex()); def != nil {
			top = concreteTop(def.CompositeKind)
		}
		return isAbstractSubtype(top, b.Kind())
	}
	if b.IsConcreteRef() {
		// Only a bottom type is below a concrete type.
		bot := ValueTypeNullFuncref.Kind()
		if def := typeAt(types, b.TypeIndex()); def != nil {
			bot, _ = bottomKind(concreteTop(def.CompositeKind))
		}
		return a.Kind() == bot
	}
	return isAbstractSubtype(a.Kind(), b.Kind())
}

// isAbstractSubtype walks the abstract lattice from a up to b.
func isAbstractSubtype(a, b byte) bool {
	// A bottom type is below every type in its hierarchy, not just the one above it, so ask whether b's
	// hierarchy bottoms out at a rather than trying to climb from a.
	if bot, ok := bottomKind(b); ok && bot == a {
		return true
	}
	for {
		if a == b {
			return true
		}
		parent, ok := heapTypeParent(a)
		if !ok {
			return false
		}
		a = parent
	}
}

// concreteIsSubtypeOf walks a's declared supertype chain looking for b.
func concreteIsSubtypeOf(a, b Index, types []FunctionType) bool {
	a, b = canonicalIndex(types, a), canonicalIndex(types, b)
	// A chain longer than the type section means a cycle; the type section validator rejects those, but this
	// is also reached during that very validation, so bound the walk rather than trusting it.
	for i := 0; i <= len(types); i++ {
		if a == b {
			return true
		}
		def := typeAt(types, a)
		if def == nil || !def.HasSupertype {
			return false
		}
		a = canonicalIndex(types, def.Supertype)
	}
	return false
}

func canonicalIndex(types []FunctionType, i Index) Index {
	if def := typeAt(types, i); def != nil {
		return def.CanonicalIndex
	}
	return i
}

func typeAt(types []FunctionType, i Index) *FunctionType {
	if indexOutOfRange(i, len(types)) {
		return nil
	}
	return &types[i]
}

// validateTypeSection checks the declared subtype relation of every type: the supertype must exist, must not
// be final, and this type's structure must match the supertype's per the GC proposal's structural rules.
func (m *Module) validateTypeSection(enabledFeatures api.CoreFeatures) error {
	for i := range m.TypeSection {
		t := &m.TypeSection[i]
		if !t.HasSupertype {
			continue
		}
		if err := enabledFeatures.RequireEnabled(api.CoreFeatureGC); err != nil {
			return fmt.Errorf("type[%d] declares a supertype, which is invalid as %w", i, err)
		}
		super := typeAt(m.TypeSection, t.Supertype)
		if super == nil {
			return fmt.Errorf("type[%d]: unknown supertype %d", i, t.Supertype)
		}
		if !super.Extensible {
			return fmt.Errorf("type[%d]: supertype %d is final", i, t.Supertype)
		}
		// The spec requires the supertype index to be strictly below the subtype's own, which rules out
		// self- and forward references and so makes the relation acyclic by construction.
		if t.Supertype >= Index(i) {
			return fmt.Errorf("type[%d]: supertype %d is not defined before it", i, t.Supertype)
		}
		if err := m.checkSubtypeStructure(t, super); err != nil {
			return fmt.Errorf("type[%d] is not a subtype of type[%d]: %w", i, t.Supertype, err)
		}
	}
	return nil
}

// checkSubtypeStructure implements the structural side of the declared subtype relation: a function type may
// widen its params and narrow its results, a struct may append fields and narrow immutable ones, and an array
// may narrow its immutable element. Mutable fields must be identical, since they are read *and* written.
func (m *Module) checkSubtypeStructure(t, super *FunctionType) error {
	if t.CompositeKind != super.CompositeKind {
		return fmt.Errorf("composite kind differs")
	}
	ts := m.TypeSection
	switch t.CompositeKind {
	case CompositeKindFunc:
		if len(t.Params) != len(super.Params) || len(t.Results) != len(super.Results) {
			return fmt.Errorf("arity differs")
		}
		for i, p := range t.Params {
			if !valueTypeMatches(super.Params[i], p, ts) { // contravariant
				return fmt.Errorf("param[%d] is not a supertype of the supertype's", i)
			}
		}
		for i, r := range t.Results {
			if !valueTypeMatches(r, super.Results[i], ts) { // covariant
				return fmt.Errorf("result[%d] is not a subtype of the supertype's", i)
			}
		}
	case CompositeKindStruct:
		if len(t.Fields) < len(super.Fields) {
			return fmt.Errorf("has fewer fields than the supertype")
		}
		for i, f := range super.Fields {
			if err := checkFieldSubtype(t.Fields[i], f, ts); err != nil {
				return fmt.Errorf("field[%d]: %w", i, err)
			}
		}
	case CompositeKindArray:
		if err := checkFieldSubtype(t.Fields[0], super.Fields[0], ts); err != nil {
			return fmt.Errorf("element: %w", err)
		}
	}
	return nil
}

func checkFieldSubtype(sub, super FieldType, types []FunctionType) error {
	if sub.Mutable != super.Mutable {
		return fmt.Errorf("mutability differs")
	}
	if super.Mutable {
		if sub.Type != super.Type {
			return fmt.Errorf("mutable field types differ")
		}
		return nil
	}
	if !valueTypeMatches(sub.Type, super.Type, types) {
		return fmt.Errorf("%s is not a subtype of %s", ValueTypeName(sub.Type), ValueTypeName(super.Type))
	}
	return nil
}

// valueTypeMatches reports whether actual is a subtype of expected, over both numeric/packed types (where it
// is equality) and reference types (where it is the lattice above plus nullability).
func valueTypeMatches(actual, expected ValueType, types []FunctionType) bool {
	if actual == expected {
		return true
	}
	if !actual.IsRef() || !expected.IsRef() {
		return false
	}
	if !actual.IsNullable() || expected.IsNullable() {
		return isHeapSubtype(actual, expected, types)
	}
	return false
}

// CanonicalizeTypes assigns FunctionType.CanonicalIndex over a whole type section, implementing iso-recursive
// canonicalization: two defined types are the same type when their whole rec groups are structurally identical,
// with references inside the group compared by position and references outside it by the canonical index they
// already resolved to.
//
// Comparing concrete references by canonical index is what makes `(ref $a)` and `(ref $b)` interchangeable when
// $a and $b are separate but identical declarations, which is the rule type-equivalence.wast exercises.
func CanonicalizeTypes(types []FunctionType) {
	// Keyed by rec group structure, holding the index its first member canonicalizes to.
	interned := make(map[string]Index, len(types))
	outside := func(idx Index) string {
		if def := typeAt(types, idx); def != nil {
			return "#" + strconv.FormatUint(uint64(def.CanonicalIndex), 10)
		}
		return "?" + strconv.FormatUint(uint64(idx), 10)
	}
	ForEachRecGroup(types, func(start, end int) {
		key := RecGroupKey(types, start, end, outside)
		canonStart, ok := interned[key]
		if !ok {
			canonStart = Index(start)
			interned[key] = canonStart
		}
		for i := start; i < end; i++ {
			types[i].CanonicalIndex = canonStart + Index(i-start)
		}
	})
}

// ForEachRecGroup calls f with the [start, end) bounds of each rec group in a type section. A type not
// declared inside `rec` is its own group of one.
func ForEachRecGroup(types []FunctionType, f func(start, end int)) {
	for start := 0; start < len(types); {
		n := types[start].RecGroupSize
		if n < 1 {
			n = 1
		}
		end := min(start+n, len(types))
		f(start, end)
		start = end
	}
}

// RecGroupKey builds a structural key for the rec group types[start:end]. A reference to a type inside the
// group is written by position, so the group is compared as a whole rather than by the indices it happens to
// sit at; a reference to one outside it is written by refOutside, which decides what identity means to the
// caller -- a canonical index within one module, or a FunctionTypeID across a store.
//
// Keying the group and not the member is the whole point: two function types can be identical in isolation and
// still be different types because a sibling in their rec group differs.
func RecGroupKey(types []FunctionType, start, end int, refOutside func(Index) string) string {
	var sb strings.Builder
	ref := func(idx Index) string {
		if int(idx) >= start && int(idx) < end {
			return "@" + strconv.Itoa(int(idx)-start)
		}
		return refOutside(idx)
	}
	valueType := func(vt ValueType) string {
		if !vt.IsConcreteRef() {
			return ValueTypeName(vt)
		}
		if vt.IsNullable() {
			return "null" + ref(vt.TypeIndex())
		}
		return ref(vt.TypeIndex())
	}
	for i := start; i < end; i++ {
		t := &types[i]
		fmt.Fprintf(&sb, "k%d", t.CompositeKind)
		for _, p := range t.Params {
			sb.WriteByte(' ')
			sb.WriteString(valueType(p))
		}
		sb.WriteByte('_')
		for _, r := range t.Results {
			sb.WriteByte(' ')
			sb.WriteString(valueType(r))
		}
		super := "none"
		if t.HasSupertype {
			super = ref(t.Supertype)
		}
		t.writeCompositeKey(&sb, super, valueType)
		sb.WriteByte(';')
	}
	return sb.String()
}

// hierarchyTop returns the top heap type of the hierarchy a value type belongs to: any, func, extern or exn.
// Every reference type has exactly one, which is what makes "are these two in the same hierarchy" -- the
// condition ref.test and ref.cast validate against -- a single comparison.
func hierarchyTop(vt ValueType, types []FunctionType) byte {
	kind := vt.Kind()
	if vt.IsConcreteRef() {
		kind = ValueTypeFuncref.Kind()
		if def := typeAt(types, vt.TypeIndex()); def != nil {
			kind = concreteTop(def.CompositeKind)
		}
	}
	switch kind {
	case ValueTypeNullref.Kind(), ValueTypeEqref.Kind(), ValueTypeI31ref.Kind(),
		ValueTypeStructref.Kind(), ValueTypeArrayref.Kind():
		return ValueTypeAnyref.Kind()
	case ValueTypeNullFuncref.Kind():
		return ValueTypeFuncref.Kind()
	case ValueTypeNullExternref.Kind():
		return ValueTypeExternref.Kind()
	case ValueTypeNullExnref.Kind():
		return ValueTypeExnref.Kind()
	}
	return kind
}

// The modes of RunGCCheck, which both engines route the GC proposal's runtime type checks through.
const (
	// GCCheckRefTest answers ref.test: a is the reference, b its target descriptor, and the result is 0 or 1.
	GCCheckRefTest uint64 = iota
	// GCCheckRefCast answers ref.cast: same operands, but a failure traps instead of returning 0.
	GCCheckRefCast
	// GCCheckIndirectCall answers the call_indirect type check for a module that declares a subtype
	// relation: a is the callee's FunctionTypeID and b the declared one, and a mismatch traps.
	GCCheckIndirectCall
)

// EncodeRefTarget packs the target of ref.test / ref.cast into the descriptor RunGCCheck expects: the type
// index of a concrete target, or the kind byte of an abstract one, plus the two flags.
func EncodeRefTarget(indexOrKind uint32, nullable, concrete bool) uint64 {
	d := uint64(indexOrKind) << 2
	if nullable {
		d |= 1
	}
	if concrete {
		d |= 2
	}
	return d
}

// RunGCCheck performs one GC runtime type check on behalf of an engine. It panics with the matching trap when
// a ref.cast or an indirect call fails, so a caller only has to look at the result for GCCheckRefTest.
//
// It lives here rather than in either engine because both need exactly this, and because the answer depends on
// store-wide type identity (see Store.TypeIDIsSubtypeOf) that neither engine owns.
func RunGCCheck(m *ModuleInstance, a, b, mode uint64) uint64 {
	switch mode {
	case GCCheckIndirectCall:
		if !m.TypeIDIsSubtypeOf(FunctionTypeID(a), FunctionTypeID(b)) {
			panic(wasmruntime.ErrRuntimeIndirectCallTypeMismatch)
		}
		return 0
	default:
		ok := m.refMatchesTarget(a, b)
		if mode == GCCheckRefCast && !ok {
			panic(wasmruntime.ErrRuntimeCastFailure)
		}
		if ok {
			return 1
		}
		return 0
	}
}

// refMatchesTarget reports whether the reference value ref -- zero for null, otherwise the engine's opaque
// representation of a funcref or externref -- is of the type the descriptor names. See EncodeRefTarget.
//
// Validation has already established that the operand and the target share a hierarchy, which is what lets the
// concrete case resolve ref as a function: a value reaching a concrete or func target is either null or a
// function reference, never an externref's raw pointer.
func (m *ModuleInstance) refMatchesTarget(ref, desc uint64) bool {
	nullable, concrete := desc&1 != 0, desc&2 != 0
	if ref == 0 {
		return nullable // null is of every nullable reference type in its hierarchy, and of no other.
	}
	if concrete {
		idx := Index(desc >> 2)
		if indexOutOfRange(idx, len(m.TypeIDs)) {
			return false
		}
		return m.TypeIDIsSubtypeOf(m.Engine.TypeIDOfReference(Reference(ref)), m.TypeIDs[idx])
	}
	switch ValueType(desc >> 2) {
	case ValueTypeFuncref, ValueTypeExternref, ValueTypeExnref:
		// The tops match anything non-null in their own hierarchy, and validation kept the others out.
		return true
	default:
		// The bottoms (nofunc, noextern, noexn, none) hold nothing but null, and no non-null value in the any
		// hierarchy can exist until struct, array and i31 do.
		return false
	}
}
