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

// The modes of RunGC, which both engines route the GC proposal's runtime work through. Each names how the
// five operands are read; see the switch in RunGC.
const (
	// GCCheckRefTest answers ref.test: a is the reference, b its target descriptor, and the result is 0 or 1.
	GCCheckRefTest uint64 = iota
	// GCCheckRefCast answers ref.cast: same operands, but a failure traps instead of returning 0.
	GCCheckRefCast
	// GCCheckIndirectCall answers the call_indirect type check for a module that declares a subtype
	// relation: a is the callee's FunctionTypeID and b the declared one, and a mismatch traps.
	GCCheckIndirectCall
	// GCStructNew allocates a struct: a is the type index and the fields are read from the scratch area.
	GCStructNew
	// GCStructNewDefault allocates a struct with every field at its type's default: a is the type index.
	GCStructNewDefault
	// GCStructGet reads one word of a field: a is the reference, b the word's slot (see
	// FunctionType.FieldSlots), c 1 for a signed packed read. A vector field is two slots, read separately.
	GCStructGet
	// GCStructSet writes one word of a field: a is the reference, b the slot, c the value.
	GCStructSet
	// GCArrayNew allocates an array of b elements all equal to (c, d): a is the type index. d is the high
	// word of a vector element and zero otherwise.
	GCArrayNew
	// GCArrayNewDefault allocates an array of b elements at the element type's default: a is the type index.
	GCArrayNewDefault
	// GCArrayNewFixed allocates an array of b elements read from the scratch area: a is the type index.
	GCArrayNewFixed
	// GCArrayNewData allocates an array of d elements read from data segment b at byte offset c: a is the
	// type index. GCArrayNewElem is the same over an element segment.
	GCArrayNewData
	GCArrayNewElem
	// GCArrayInitData writes e elements of data segment c, from its offset d, into array a at index b.
	// GCArrayInitElem is the same over an element segment.
	GCArrayInitData
	GCArrayInitElem
	// GCArrayGet reads one word of an element: a is the reference, b the element index, c the flags -- bit 0
	// for a signed packed read, bit 1 for the high word of a vector element.
	GCArrayGet
	// GCArraySet writes one word of an element: a is the reference, b the element index, c the value, d the
	// flags, whose bit 1 selects the high word of a vector element.
	GCArraySet
	// GCArrayLen returns the length of the array a.
	GCArrayLen
	// GCArrayFill writes d elements equal to (c, e) into a starting at element b. e is the high word of a
	// vector element and zero otherwise.
	GCArrayFill
	// GCArrayCopy copies e elements from array c at index d into array a at index b.
	GCArrayCopy
	// GCRefEq compares references a and b for identity.
	GCRefEq
	// GCRefI31 makes an i31 from the i32 a; GCI31GetS and GCI31GetU read one back.
	GCRefI31
	GCI31GetS
	GCI31GetU
	// GCSafepoint is not a type check at all: it is how the native engine's loop-header poll reaches Go,
	// sharing the GC trampoline so the safepoint costs no field in the execution context. It never reaches
	// RunGC -- the engine handles it before dispatching -- and exists here so both sides name one constant.
	GCSafepoint
)

// EncodeRefTarget packs the target of ref.test / ref.cast into the descriptor RunGC expects: the type
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

// RunGC performs one GC runtime operation on behalf of an engine, returning the value the instruction pushes
// (zero for those that push nothing). It panics with the matching trap on failure.
//
// It lives here rather than in either engine because both need exactly this, and because most of it depends on
// store-wide state -- the type identity behind Store.TypeIDIsSubtypeOf, and the heap behind Store.GC -- that
// neither engine owns. scratch carries the operands of the variadic forms (struct.new, array.new_fixed) in
// stack order; it is nil for every other mode.
func RunGC(m *ModuleInstance, mode, a, b, c, d, e uint64, scratch []uint64) uint64 {
	switch mode {
	case GCCheckRefTest, GCCheckRefCast:
		ok := m.refMatchesTarget(a, b)
		if mode == GCCheckRefCast && !ok {
			panic(wasmruntime.ErrRuntimeCastFailure)
		}
		if ok {
			return 1
		}
		return 0

	case GCCheckIndirectCall:
		if !m.TypeIDIsSubtypeOf(FunctionTypeID(a), FunctionTypeID(b)) {
			panic(wasmruntime.ErrRuntimeIndirectCallTypeMismatch)
		}
		return 0

	case GCStructNew, GCStructNewDefault, GCArrayNew, GCArrayNewDefault, GCArrayNewFixed:
		return m.allocGC(mode, a, b, c, d, scratch)

	case GCArrayNewData, GCArrayNewElem:
		fromData := mode == GCArrayNewData
		width := storageWidth(m.Source.TypeSection[a].Fields[0].Type)
		// The segment is read before the array is allocated, so a range overrunning the segment reports the
		// segment's trap even when the same length would also overrun the array-length cap.
		m.checkSegmentRange(fromData, b, c, d, width)
		o := m.newArray(a, d)
		m.fillArrayFromSegment(o, 0, fromData, b, c, d, width)
		return m.allocRef(o)

	case GCArrayInitData, GCArrayInitElem:
		o := m.s.GC.Deref(a)
		checkArrayRange(o.Len(), b, e)
		fromData := mode == GCArrayInitData
		width := storageWidth(o.Type.Fields[0].Type)
		m.checkSegmentRange(fromData, c, d, e, width)
		m.fillArrayFromSegment(o, b, fromData, c, d, e, width)
		return 0

	case GCStructGet:
		return m.s.GC.Deref(a).Get(int(b), c&1 != 0)

	case GCStructSet:
		m.s.GC.Deref(a).Set(int(b), c)
		return 0

	case GCArrayGet:
		o := m.s.GC.Deref(a)
		return o.Get(arraySlot(o, b, c), c&1 != 0)

	case GCArraySet:
		o := m.s.GC.Deref(a)
		o.Set(arraySlot(o, b, d), c)
		return 0

	case GCArrayLen:
		return uint64(m.s.GC.Deref(a).Len())

	case GCArrayFill:
		o := m.s.GC.Deref(a)
		checkArrayRange(o.Len(), b, d)
		stride := uint64(o.ElemSlots())
		for i := uint64(0); i < d; i++ {
			slot := (b + i) * stride
			o.Set(int(slot), c)
			if stride == 2 {
				o.Set(int(slot+1), e)
			}
		}
		return 0

	case GCArrayCopy:
		dst, src := m.s.GC.Deref(a), m.s.GC.Deref(c)
		checkArrayRange(dst.Len(), b, e)
		checkArrayRange(src.Len(), d, e)
		stride := uint64(dst.ElemSlots())
		// The two arrays may be the same object, so copy in the direction that cannot clobber a source
		// element before it is read -- exactly what memory.copy does for overlapping ranges.
		if b <= d {
			for i := uint64(0); i < e*stride; i++ {
				dst.Set(int(b*stride+i), src.Get(int(d*stride+i), true))
			}
		} else {
			for i := e * stride; i > 0; i-- {
				dst.Set(int(b*stride+i-1), src.Get(int(d*stride+i-1), true))
			}
		}
		return 0

	case GCRefEq:
		if a == b {
			return 1
		}
		return 0

	case GCRefI31:
		return EncodeI31(uint32(a))
	case GCI31GetS, GCI31GetU:
		if a == GCRefNull {
			panic(wasmruntime.ErrRuntimeNullReference)
		}
		if mode == GCI31GetS {
			return uint64(DecodeI31S(a))
		}
		return uint64(DecodeI31U(a))
	}
	panic("BUG: unknown GC runtime mode")
}

// structSlots is how many words a struct instance occupies.
func structSlots(def *FunctionType) int {
	if def.FieldSlots != nil {
		return int(def.FieldSlots[len(def.Fields)])
	}
	return len(def.Fields)
}

// arraySlot maps an element index to the word it starts at, trapping when the index is past the end. flags
// bit 1 selects the high word of a vector element.
func arraySlot(o *GCObject, index, flags uint64) int {
	if index >= uint64(o.Len()) {
		panic(wasmruntime.ErrRuntimeOutOfBoundsArrayAccess)
	}
	slot := index * uint64(o.ElemSlots())
	if flags&2 != 0 {
		slot++
	}
	return int(slot)
}

// allocGC handles the five allocating modes, which all end in one GCHeap.Alloc.
func (m *ModuleInstance) allocGC(mode, typeIndex, count, init, initHi uint64, scratch []uint64) uint64 {
	if indexOutOfRange(Index(typeIndex), len(m.TypeIDs)) {
		panic("BUG: validation should have rejected type index")
	}
	def := &m.Source.TypeSection[typeIndex]
	o := &GCObject{Type: def, TypeID: m.TypeIDs[typeIndex]}

	switch mode {
	case GCStructNewDefault:
		// Every default is the zero word, so a fresh slice already holds them.
		o.Fields = make([]uint64, structSlots(def))
	case GCStructNew, GCArrayNewFixed:
		// The operands were pushed in field order, so scratch is already word-for-word the layout. They go
		// in through Set so a packed field is stored truncated, as Get's widening assumes.
		o.Fields = make([]uint64, len(scratch))
		for i, v := range scratch {
			o.Set(i, v)
		}
	case GCArrayNew, GCArrayNewDefault:
		// Bound the element count before multiplying it out, so a huge one traps rather than wrapping.
		slots := int(SlotsForStorageType(def.Fields[0].Type))
		n := checkArrayLen(count) * slots
		o.Fields = make([]uint64, n)
		if mode == GCArrayNew {
			for i := 0; i < n; i += slots {
				o.Set(i, init)
				if slots == 2 {
					o.Set(i+1, initHi)
				}
			}
		}
	}
	return m.allocRef(o)
}

// allocRef puts an object on the heap and, when that pushed the heap past its threshold, asks every execution
// on the store to park so one of them can collect. Nothing collects here: a collection needs the wasm stacks
// to be stable, which they are only at a safepoint.
func (m *ModuleInstance) allocRef(o *GCObject) uint64 {
	ref, wantGC := m.s.GC.Alloc(o)
	if wantGC {
		m.s.RequestGC()
	}
	return ref
}

// newArray allocates an uninitialised array of n elements of the given type index.
func (m *ModuleInstance) newArray(typeIndex, n uint64) *GCObject {
	def := &m.Source.TypeSection[typeIndex]
	slots := SlotsForStorageType(def.Fields[0].Type)
	return &GCObject{
		Type: def, TypeID: m.TypeIDs[typeIndex],
		Fields: make([]uint64, checkArrayLen(n)*int(slots)),
	}
}

// checkSegmentRange traps unless the segment named by segment holds length elements starting at src: src
// counts bytes of a data segment, each element occupying width of them, and references of an element segment.
// A dropped segment is an empty one, so any non-zero range over it traps here.
//
// The operands are all i32-derived, so src+length*width cannot overflow a uint64.
func (m *ModuleInstance) checkSegmentRange(fromData bool, segment, src, length, width uint64) {
	if !fromData {
		if indexOutOfRange(Index(segment), len(m.ElementInstances)) ||
			src+length > uint64(len(m.ElementInstances[segment])) {
			panic(wasmruntime.ErrRuntimeInvalidTableAccess)
		}
		return
	}
	if indexOutOfRange(Index(segment), len(m.DataInstances)) ||
		src+length*width > uint64(len(m.DataInstances[segment])) {
		panic(wasmruntime.ErrRuntimeOutOfBoundsMemoryAccess)
	}
}

// fillArrayFromSegment writes length elements into o starting at dst, reading them from a data or an element
// segment starting at src. For a data segment src counts bytes and each element is read little-endian at its
// storage width; for an element segment it counts references.
//
// The caller passes width and has already called checkSegmentRange with it, because the two instructions that
// allocate have to make that check before they allocate. Doing it here as well would only be a second bounds
// check and a second storageWidth on every one of these.
func (m *ModuleInstance) fillArrayFromSegment(o *GCObject, dst uint64, fromData bool, segment, src, length, width uint64) {
	if !fromData {
		elems := m.ElementInstances[segment]
		for i := uint64(0); i < length; i++ {
			o.Set(int((dst+i)*uint64(o.ElemSlots())), uint64(elems[src+i]))
		}
		return
	}

	data := m.DataInstances[segment]
	stride := uint64(o.ElemSlots())
	for i := uint64(0); i < length; i++ {
		base := src + i*width
		slot := (dst + i) * stride
		// A vector element spans two words, low half first, so read the bytes eight at a time.
		for w := uint64(0); w < stride; w++ {
			var v uint64
			for b := uint64(0); b < width && b < 8; b++ {
				v |= uint64(data[base+w*8+b]) << (8 * b)
			}
			o.Set(int(slot+w), v)
		}
	}
}

// storageWidth is how many bytes of a data segment one element of the given storage type occupies.
func storageWidth(st ValueType) uint64 {
	switch st {
	case ValueTypeI8:
		return 1
	case ValueTypeI16:
		return 2
	case ValueTypeI32, ValueTypeF32:
		return 4
	case ValueTypeV128:
		return 16
	default:
		return 8
	}
}

// maxGCArrayLen bounds an array allocation. The spec's own limit is the implementation's, and a length that
// cannot be backed has to trap rather than be attempted: an array.new with a length near 2^32 would otherwise
// ask Go for 32GiB.
const maxGCArrayLen = 1 << 28

func checkArrayLen(n uint64) int {
	if n > maxGCArrayLen {
		panic(wasmruntime.ErrRuntimeOutOfBoundsArrayAccess)
	}
	return int(n)
}

// checkArrayRange traps unless [offset, offset+length) is inside an array of the given length. The addition
// is done in uint64 so it cannot wrap.
func checkArrayRange(arrayLen int, offset, length uint64) {
	if offset+length > uint64(arrayLen) {
		panic(wasmruntime.ErrRuntimeOutOfBoundsArrayAccess)
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
		actual, ok := m.runtimeTypeIDOf(ref, m.Source.TypeSection[idx].CompositeKind)
		return ok && m.TypeIDIsSubtypeOf(actual, m.TypeIDs[idx])
	}
	switch ValueType(desc >> 2) {
	case ValueTypeFuncref, ValueTypeExternref, ValueTypeExnref, ValueTypeAnyref:
		// A top matches anything non-null in its own hierarchy, and validation kept the others out.
		return true
	case ValueTypeEqref:
		// i31, struct and array instances are the eq types. A value the embedder handed over through
		// any.convert_extern is in the any hierarchy but has no identity wazy can compare.
		return IsI31(ref) || IsGCHeapRef(ref)
	case ValueTypeI31ref:
		return IsI31(ref)
	case ValueTypeStructref, ValueTypeArrayref:
		if !IsGCHeapRef(ref) {
			return false
		}
		o := m.s.GC.Deref(ref)
		if ValueType(desc>>2) == ValueTypeStructref {
			return !o.IsArray()
		}
		return o.IsArray()
	default:
		// The bottoms (none, nofunc, noextern, noexn) hold nothing but null.
		return false
	}
}

// runtimeTypeIDOf returns the store-wide type of a non-null reference, asking whichever of the engine or the
// heap owns values of the target's composite kind. An i31 has no defined type, so it is never one.
func (m *ModuleInstance) runtimeTypeIDOf(ref uint64, kind CompositeKind) (FunctionTypeID, bool) {
	if kind == CompositeKindFunc {
		return m.Engine.TypeIDOfReference(Reference(ref)), true
	}
	return m.s.GC.TypeIDOf(ref)
}
