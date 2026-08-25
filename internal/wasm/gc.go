package wasm

import (
	"fmt"
	"strings"

	"github.com/samyfodil/wazy/api"
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
	var sb strings.Builder
	for start := 0; start < len(types); {
		n := types[start].RecGroupSize
		if n < 1 {
			n = 1
		}
		end := min(start+n, len(types))

		sb.Reset()
		for i := start; i < end; i++ {
			writeCanonicalType(&sb, types, i, start, end)
			sb.WriteByte(';')
		}
		canonStart, ok := interned[sb.String()]
		if !ok {
			canonStart = Index(start)
			interned[sb.String()] = canonStart
		}
		for i := start; i < end; i++ {
			types[i].CanonicalIndex = canonStart + Index(i-start)
		}
		start = end
	}
}

func writeCanonicalType(sb *strings.Builder, types []FunctionType, i, start, end int) {
	t := &types[i]
	fmt.Fprintf(sb, "k%d", t.CompositeKind)
	if t.Extensible {
		sb.WriteString("|open")
	}
	if t.HasSupertype {
		sb.WriteString("|sub")
		writeCanonicalTypeRef(sb, types, t.Supertype, start, end)
	}
	for _, p := range t.Params {
		sb.WriteByte(' ')
		writeCanonicalValueType(sb, types, p, start, end)
	}
	sb.WriteByte('_')
	for _, r := range t.Results {
		sb.WriteByte(' ')
		writeCanonicalValueType(sb, types, r, start, end)
	}
	for _, f := range t.Fields {
		sb.WriteByte(' ')
		if f.Mutable {
			sb.WriteString("mut")
		}
		writeCanonicalValueType(sb, types, f.Type, start, end)
	}
}

func writeCanonicalValueType(sb *strings.Builder, types []FunctionType, vt ValueType, start, end int) {
	if !vt.IsConcreteRef() {
		sb.WriteString(ValueTypeName(vt))
		return
	}
	if vt.IsNullable() {
		sb.WriteString("null")
	}
	writeCanonicalTypeRef(sb, types, vt.TypeIndex(), start, end)
}

// writeCanonicalTypeRef writes a reference to type index idx: by position when it points inside the rec group
// being keyed (so a group is compared as a whole, not by the indices it happens to sit at), and by the target's
// already-assigned canonical index otherwise. Groups are keyed in order and references may only point backwards
// or inside the group, so the canonical index of an outside target is always known by the time it is read.
func writeCanonicalTypeRef(sb *strings.Builder, types []FunctionType, idx Index, start, end int) {
	if int(idx) >= start && int(idx) < end {
		fmt.Fprintf(sb, "@%d", int(idx)-start)
		return
	}
	if def := typeAt(types, idx); def != nil {
		fmt.Fprintf(sb, "#%d", def.CanonicalIndex)
		return
	}
	fmt.Fprintf(sb, "?%d", idx)
}
