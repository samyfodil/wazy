package wasm

import (
	"fmt"
	"math"
	"sync"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
)

// Table describes the limits of elements and its type in a table.
type Table struct {
	Min      uint32
	Max      *uint32
	Type     RefType
	InitExpr *ConstantExpression
}

// RefType is a reference type used for table elements.
type RefType = ValueType

const (
	// RefTypeFuncref represents a reference to a function.
	RefTypeFuncref = ValueTypeFuncref
	// RefTypeExternref represents a reference to a host object, which is not currently supported in wazy.
	RefTypeExternref = ValueTypeExternref
)

func RefTypeName(t RefType) (ret string) {
	switch t {
	case RefTypeFuncref:
		ret = "funcref"
	case RefTypeExternref:
		ret = "externref"
	default:
		ret = fmt.Sprintf("unknown(0x%x)", t)
	}
	return
}

// ElementMode represents a mode of element segment which is either active, passive or declarative.
//
// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/syntax/modules.html#element-segments
type ElementMode = byte

const (
	// ElementModeActive is the mode which requires the runtime to initialize table with the contents in .Init field combined with OffsetExpr.
	ElementModeActive ElementMode = iota
	// ElementModePassive is the mode which doesn't require the runtime to initialize table, and only used with OpcodeTableInitName.
	ElementModePassive
	// ElementModeDeclarative is introduced in reference-types proposal which can be used to declare function indexes used by OpcodeRefFunc.
	ElementModeDeclarative
)

// ElementSegment are initialization instructions for a TableInstance
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#syntax-elem
type ElementSegment struct {
	// OffsetExpr returns the table element offset to apply to Init indices.
	// Note: This can be validated prior to instantiation unless it includes OpcodeGlobalGet (an imported global).
	OffsetExpr ConstantExpression

	// TableIndex is the table's index to which this element segment is applied.
	// Note: This is used if and only if the Mode is active.
	TableIndex Index

	// Followings are set/used regardless of the Mode.

	// Init holds, per table element, a compact encoding of the expression that produces its reference,
	// avoiding a heap-allocated ConstantExpression (plus a LEB128 round-trip) per entry for the
	// overwhelmingly common case of large all-funcref tables (e.g. TinyGo's function table can have
	// thousands of entries). Each entry is one of:
	//
	//   - a plain function index (the "ref.func $idx" case): the entry itself, with neither of the two
	//     high flag bits below set, and not equal to ElementInitNullReference.
	//   - ElementInitNullReference: a "ref.null" entry whose own encoded heap type matched this segment's
	//     Type exactly at decode time (the standard, unambiguous shape real producers emit). Its value is
	//     always the null reference (0) and its type is this segment's Type.
	//   - a global index tagged with elementInitImportedGlobalReference (the "global.get $idx" case):
	//     mask the flag off to recover the index. The type/value is still resolved dynamically through
	//     the same global-resolver callback evaluateConstExpr would have used, so no type information
	//     needs to be cached here.
	//   - an index into Exprs tagged with elementInitExprReference: the rare-path fallback for anything
	//     the three forms above can't represent -- extended-const arithmetic, a mismatched or
	//     concrete/typed "ref.null", or any other decodable expression. Mask the flag off to index Exprs.
	//
	// Function and global indices are both bounded by 1<<27 (MaximumFunctionIndex / MaximumGlobals), so
	// the top 5 bits of the uint32 are always free for the two flags/sentinel above without ambiguity:
	// the largest possible flagged value, elementInitImportedGlobalReference|(1<<27-1), is still far
	// below ElementInitNullReference (every bit set), and elementInitExprReference (bit 30) is never set
	// alongside elementInitImportedGlobalReference (bit 31) by construction.
	Init []Index

	// Exprs holds the rare-path ConstantExpression for entries in Init tagged with
	// elementInitExprReference. This is nil unless the segment actually contains such an entry.
	Exprs []ConstantExpression

	// Type holds the type of this element segment, which is the RefType in WebAssembly 2.0.
	Type RefType

	// Mode is the mode of this element segment.
	Mode ElementMode
}

const (
	// ElementInitNullReference is the ElementSegment.Init sentinel for a "ref.null" entry whose heap type
	// matches the segment's declared Type. See ElementSegment.Init's doc for the full encoding scheme.
	ElementInitNullReference = Index(math.MaxUint32)

	// elementInitImportedGlobalReference flags an ElementSegment.Init entry as a global index (used by a
	// "global.get" entry) rather than a plain function index (used by "ref.func"). Global indices are
	// bounded by MaximumGlobals (1<<27), so bit 31 is always free to use as a flag.
	elementInitImportedGlobalReference = Index(1) << 31

	// elementInitExprReference flags an ElementSegment.Init entry as an index into ElementSegment.Exprs:
	// the rare-path fallback for anything a plain function/global index can't represent.
	elementInitExprReference = Index(1) << 30
)

// IsActive returns true if the element segment is "active" mode which requires the runtime to initialize table
// with the contents in .Init field.
func (e *ElementSegment) IsActive() bool {
	return e.Mode == ElementModeActive
}

// FuncIndex returns (Init[i], true) if that entry is a plain "ref.func" function index, or (0, false) if
// it is a "ref.null", "global.get", or Exprs-backed entry. This is used by the test binary encoder, which
// only supports encoding plain-funcref active element segments.
func (e *ElementSegment) FuncIndex(i int) (Index, bool) {
	v := e.Init[i]
	if v == ElementInitNullReference || v&elementInitImportedGlobalReference != 0 || v&elementInitExprReference != 0 {
		return 0, false
	}
	return v, true
}

// CompactElementInit converts a single decoded element-vector entry (expr) into ElementSegment's compact
// Init representation, appending to exprs and returning the corresponding tagged index when the entry
// doesn't fit one of the compact forms. See ElementSegment.Init's doc for the full encoding this
// implements. This is used by the binary decoder for element segments whose entries are full const-expr
// vectors (element section prefixes 4-7); the plain function-index vectors (prefixes 0-3) never need
// this, since every entry there is already known at decode time to be a bare function index.
//
// elemType is the segment's declared Type: a "ref.null" entry is only compacted to
// ElementInitNullReference when elemType is exactly the bare nullable RefTypeFuncref or RefTypeExternref
// constant and the entry's own encoded heap type matches it (the standard shape real producers emit).
// Comparing full RefType equality here -- not just the Kind() byte -- matters: evaluateElementInit
// reports the sentinel's type as elemType itself, and for any other elemType (a non-nullable variant, a
// concrete/typed ref, one sharing a Kind() byte by coincidence, etc.) validateTable's default case would
// then compare elemType against itself and trivially "pass" a comparison that should have compared the
// null's *actual* (always bare, always nullable) type against elemType instead. Anything else --
// including such a mismatch, a concrete/typed ref.null, or any other decodable-but-non-standard
// expression (e.g. an extended-const arithmetic sequence) -- is preserved byte-for-byte in exprs so
// downstream evaluation and validation behave exactly as if this compaction never happened.
func CompactElementInit(expr ConstantExpression, elemType RefType, exprs []ConstantExpression) (Index, []ConstantExpression) {
	if data := expr.Data; len(data) >= 3 && data[len(data)-1] == OpcodeEnd {
		switch data[0] {
		case OpcodeRefFunc:
			if idx, n, err := leb128.LoadUint32(data[1:]); err == nil && int(n) == len(data)-2 && idx < uint32(elementInitExprReference) {
				return Index(idx), exprs
			}
		case OpcodeGlobalGet:
			if idx, n, err := leb128.LoadUint32(data[1:]); err == nil && int(n) == len(data)-2 && idx < uint32(elementInitExprReference) {
				return Index(idx) | elementInitImportedGlobalReference, exprs
			}
		case OpcodeRefNull:
			if len(data) == 3 && (elemType == RefTypeFuncref && data[1] == RefTypeFuncref.Kind() ||
				elemType == RefTypeExternref && data[1] == RefTypeExternref.Kind()) {
				return ElementInitNullReference, exprs
			}
		}
	}
	exprs = append(exprs, expr)
	return Index(len(exprs)-1) | elementInitExprReference, exprs
}

// NewElementInitGlobalGet returns the ElementSegment.Init entry equivalent to a "global.get $idx" entry
// (the compact form CompactElementInit produces for one). This is primarily useful for tests exercising
// CompactElementInit/decodeElementConstExprVector's output directly, since the flag bit it sets is
// otherwise a package-private implementation detail of ElementSegment.Init's encoding.
func NewElementInitGlobalGet(idx Index) Index {
	return idx | elementInitImportedGlobalReference
}

// TableInstance represents a table of (RefTypeFuncref) elements in a module.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#table-instances%E2%91%A0
type TableInstance struct {
	// References holds references whose type is either RefTypeFuncref or RefTypeExternref (unsupported).
	//
	// Currently, only function references are supported.
	References []Reference

	// Min is the minimum (function) elements in this table and cannot grow to accommodate ElementSegment.
	Min uint32

	// Max if present is the maximum (function) elements in this table, or nil if unbounded.
	Max *uint32

	// Type is either RefTypeFuncref or RefTypeExternRef.
	Type RefType

	// The following is only used when the table is exported.

	// involvingModuleInstances is a set of module instances which are involved in the table instance.
	// This is critical for safety purpose because once a table is imported, it can hold any reference to
	// any function in the owner and importing module instances. Therefore, these module instance,
	// transitively the compiled modules, must be alive as long as the table instance is alive.
	involvingModuleInstances []*ModuleInstance
	// involvingModuleInstancesMutex is a mutex to protect involvingModuleInstances.
	involvingModuleInstancesMutex sync.RWMutex
}

// ElementInstance represents an element instance in a module.
//
// See https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/exec/runtime.html#element-instances
type ElementInstance = []Reference

// Reference is the runtime representation of RefType which is either RefTypeFuncref or RefTypeExternref.
type Reference = uintptr

// validateTable ensures any ElementSegment is valid. This caches results via Module.validatedActiveElementSegments.
// Note: limitsType are validated by decoders, so not re-validated here.
func (m *Module) validateTable(enabledFeatures api.CoreFeatures, tables []Table, maximumTableIndex uint32) error {
	if len(tables) > int(maximumTableIndex) {
		return fmt.Errorf("too many tables in a module: %d given with limit %d", len(tables), maximumTableIndex)
	}

	importedTableCount := m.ImportTableCount

	// Create bounds checks as these can err prior to instantiation
	funcCount := m.ImportFunctionCount + m.SectionElementCount(SectionIDFunction)
	globalsCount := m.ImportGlobalCount + m.SectionElementCount(SectionIDGlobal)

	// Now, we have to figure out which table elements can be resolved before instantiation and also fail early if there
	// are any imported globals that are known to be invalid by their declarations.
	for i := range m.ElementSection {
		elem := &m.ElementSection[i]
		idx := Index(i)
		initCount := uint32(len(elem.Init))

		// Any offset applied is to the element, not the function index: validate here if the funcidx is sound.
		for ei := range elem.Init {
			_, initType, err := evaluateElementInit(
				elem, ei,
				func(globalIndex Index) (ValueType, uint64, uint64, error) {
					if globalIndex >= Index(globalsCount) {
						return 0, 0, 0, fmt.Errorf("%s[%d].init[%d] global index %d out of range", SectionIDName(SectionIDElement), idx, ei, globalIndex)
					}
					vt, err := m.resolveConstExprGlobalType(enabledFeatures, SectionIDElement, idx, globalIndex)
					return vt, 0, 0, err
				},
				func(funcIndex Index) (Reference, error) {
					if funcIndex >= Index(funcCount) {
						return 0, fmt.Errorf("%s[%d].init[%d] func index %d out of range", SectionIDName(SectionIDElement), idx, ei, funcIndex)
					}
					return 0, nil
				},
			)
			if err != nil {
				return err
			}

			switch elem.Type {
			case RefTypeFuncref:
				if initType != ValueTypeFuncref {
					return fmt.Errorf("%s[%d].init[%d] must be funcref but was %s", SectionIDName(SectionIDElement), idx, ei, ValueTypeName(initType))
				}
			case RefTypeExternref:
				if initType != ValueTypeExternref {
					return fmt.Errorf("%s[%d].init[%d] must be externref but was %s", SectionIDName(SectionIDElement), idx, ei, ValueTypeName(initType))
				}
			default:
				if !isRefSubtypeOf(initType, elem.Type) && initType != ValueTypeFuncref {
					return fmt.Errorf("%s[%d].init[%d] must be %s but was %s",
						SectionIDName(SectionIDElement), idx, ei, ValueTypeName(elem.Type), ValueTypeName(initType))
				}
			}
		}

		if elem.IsActive() {
			if len(tables) <= int(elem.TableIndex) {
				return fmt.Errorf("unknown table %d as active element target", elem.TableIndex)
			}

			t := tables[elem.TableIndex]
			if !isRefSubtypeOf(elem.Type, t.Type) {
				return fmt.Errorf("element type mismatch: table has %s but element has %s",
					RefTypeName(t.Type), RefTypeName(elem.Type),
				)
			}

			hasGlobalRef := false

			offsetExprResults, offsetExprType, err := evaluateConstExpr(
				&elem.OffsetExpr,
				func(globalIndex Index) (ValueType, uint64, uint64, error) {
					hasGlobalRef = true

					if globalIndex >= Index(globalsCount) {
						return 0, 0, 0, fmt.Errorf("%s[%d] global index %d out of range", SectionIDName(SectionIDElement), idx, globalIndex)
					}

					vt, err := m.resolveConstExprGlobalType(enabledFeatures, SectionIDElement, idx, globalIndex)
					if err != nil {
						return 0, 0, 0, err
					}

					if vt != ValueTypeI32 {
						return 0, 0, 0, fmt.Errorf("%s[%d] (global.get %d): import[%d].global.ValType != i32", SectionIDName(SectionIDElement), idx, globalIndex, i)
					}
					return ValueTypeI32, 0, 0, nil
				},
				func(funcIndex Index) (Reference, error) {
					return 0, nil
				},
			)
			if err != nil {
				return fmt.Errorf("%s[%d] couldn't evaluate offset expression: %w", SectionIDName(SectionIDElement), idx, err)
			}
			if offsetExprType != ValueTypeI32 {
				return fmt.Errorf("%s[%d] offset expression must return i32 but was %s", SectionIDName(SectionIDElement), idx, ValueTypeName(offsetExprType))
			}

			if !enabledFeatures.IsEnabled(api.CoreFeatureReferenceTypes) && !hasGlobalRef && elem.TableIndex >= importedTableCount {
				offset := uint32(offsetExprResults[0])
				if err = checkSegmentBounds(t.Min, uint64(initCount)+uint64(offset), idx); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// buildTable returns TableInstances if the module defines or imports a table.
//   - importedTables: returned as `tables` unmodified.
//   - importedGlobals: include all instantiated, imported globals.
//
// If the result `init` is non-nil, it is the `tableInit` parameter of Engine.NewModuleEngine.
//
// Note: An error is only possible when an ElementSegment.OffsetExpr is out of range of the TableInstance.Min.
func (m *ModuleInstance) buildTables(module *Module, skipBoundCheck bool) (err error) {
	idx := module.ImportTableCount
	for i := range module.TableSection {
		tsec := &module.TableSection[i]
		t := &TableInstance{
			References: make([]Reference, tsec.Min), Min: tsec.Min, Max: tsec.Max,
			Type: tsec.Type,
		}
		if tsec.InitExpr != nil {
			initVals := evaluateConstExprInModuleInstance(tsec.InitExpr, m)
			if len(initVals) > 0 && initVals[0] != 0 {
				initRef := Reference(initVals[0])
				for j := range t.References {
					t.References[j] = initRef
				}
			}
		}
		m.Tables[idx] = t
		idx++
	}

	if !skipBoundCheck {
		for elemI := range module.ElementSection { // Do not loop over the value since elementSegments is a slice of value.
			elem := &module.ElementSection[elemI]
			table := m.Tables[elem.TableIndex]
			offset := uint32(evaluateConstExprInModuleInstance(&elem.OffsetExpr, m)[0])
			// Check to see if we are out-of-bounds
			initCount := uint64(len(elem.Init))
			if err = checkSegmentBounds(table.Min, uint64(offset)+initCount, Index(elemI)); err != nil {
				return
			}
		}
	}
	return
}

// checkSegmentBounds fails if the capacity needed for an ElementSegment.Init is larger than limitsType.Min
//
// WebAssembly 1.0 (20191205) doesn't forbid growing to accommodate element segments, and spectests are inconsistent.
// For example, the spectests enforce elements within Table limitsType.Min, but ignore Import.DescTable min. What this
// means is we have to delay offset checks on imported tables until we link to them.
// e.g. https://github.com/WebAssembly/spec/blob/wg-1.0/test/core/elem.wast#L117 wants pass on min=0 for import
// e.g. https://github.com/WebAssembly/spec/blob/wg-1.0/test/core/elem.wast#L142 wants fail on min=0 module-defined
func checkSegmentBounds(min uint32, requireMin uint64, idx Index) error { // uint64 in case offset was set to -1
	if requireMin > uint64(min) {
		return fmt.Errorf("%s[%d].init exceeds min table size", SectionIDName(SectionIDElement), idx)
	}
	return nil
}

// Grow appends the `initialRef` by `delta` times into the References slice.
// Returns -1 if the operation is not valid, otherwise the old length of the table.
//
// https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/exec/instructions.html#xref-syntax-instructions-syntax-instr-table-mathsf-table-grow-x
func (t *TableInstance) Grow(delta uint32, initialRef Reference) (currentLen uint32) {
	currentLen = uint32(len(t.References))
	if delta == 0 {
		return
	}

	if newLen := int64(currentLen) + int64(delta); // adding as 64bit ints to avoid overflow.
	newLen >= math.MaxUint32 || (t.Max != nil && newLen > int64(*t.Max)) {
		return 0xffffffff // = -1 in signed 32-bit integer.
	}

	t.References = append(t.References, make([]uintptr, delta)...)
	if initialRef == 0 {
		return
	}

	// Uses the copy trick for faster filling the new region with the initial value.
	// https://github.com/golang/go/blob/go1.24.0/src/slices/slices.go#L514-L517
	newRegion := t.References[currentLen:]
	newRegion[0] = initialRef
	for i := 1; i < len(newRegion); i *= 2 {
		copy(newRegion[i:], newRegion[:i])
	}
	return
}
