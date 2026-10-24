package instance

import (
	"fmt"
	"strings"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
	wasmbinary "github.com/samyfodil/wazy/internal/wasm/binary"
)

// This file is the core half of the component-level type check: core module
// types and core instance types, and the subtyping between them, ported rule
// for rule and message for message from wasmparser's SubtypeCx
// (module_type, entity_type and the table/memory/global/func cases under
// it) and its core instantiation check (instantiate_core_module).
//
// A core module type is known statically when a component declares it -- an
// import, an argument or export a type ascribes, an instance export -- and
// is read off the module's own sections when it is one the component embeds.
// Embedded modules are decoded only where a check needs their type: when one
// is passed where a module type is expected, or instantiates another module
// whose type is declared. An embedded module instantiated with embedded
// modules' instances is linked by the core linker, which checks the same
// thing as it links them; on a failure, checkCoreInstantiation states it
// this way. See ctEnv.checkCoreInstances.

// ctCoreFunc is a core function type.
type ctCoreFunc struct {
	params, results []wasm.ValueType
}

// ctCoreType is one index of a core type index space: a module type, a
// function type, or a struct or array type, which no check here looks into.
type ctCoreType struct {
	mod *ctModule
	fn  *ctCoreFunc
}

// ctCoreExtern is the type of a core import or export. known is false for a
// core function whose type the check does not derive (a canon built-in other
// than lower and the resource ones); its sort is all that is checked then.
type ctCoreExtern struct {
	kind  byte // 0x00 func, 0x01 table, 0x02 memory, 0x03 global, 0x04 tag
	known bool
	fn    *ctCoreFunc // func, tag

	elem         wasm.ValueType // table
	min, max     uint64         // table, memory
	hasMax       bool
	shared, is64 bool
	pageLog2     uint32         // memory
	val          wasm.ValueType // global
	mutable      bool
}

type ctCoreImport struct {
	module, name string
	ext          ctCoreExtern
}

type ctCoreExport struct {
	name string
	ext  ctCoreExtern
}

// ctModule is a core module type: what it imports, two-level, and exports.
type ctModule struct {
	imports []ctCoreImport
	exports []ctCoreExport
}

// ctCoreInstance is a core instance type: its exports.
type ctCoreInstance struct {
	exports []ctCoreExport
}

func (in *ctCoreInstance) export(name string) (ctCoreExtern, bool) {
	for i := range in.exports {
		if in.exports[i].name == name {
			return in.exports[i].ext, true
		}
	}
	return ctCoreExtern{}, false
}

// ctCoreFeatures are the core features an embedded module's type is read
// with: all of them, for the type is all that is read -- whether the runtime
// enables what the module uses is the core compiler's to say. Threads is
// experimental.CoreFeaturesThreads, which this package cannot import.
const ctCoreFeatures = api.CoreFeaturesV3 | api.CoreFeatureSIMD<<1 | api.CoreFeatureMultiMemory | api.CoreFeatureMemory64

// ctModuleOfDefinition reads the type of an embedded core module off its
// sections.
func ctModuleOfDefinition(bin []byte) (*ctModule, error) {
	m, err := wasmbinary.DecodeModule(bin, ctCoreFeatures, wasm.MemoryLimitPages, false, 0, false, false)
	if err != nil {
		return nil, err
	}
	fn := func(idx wasm.Index) *ctCoreFunc {
		if int(idx) >= len(m.TypeSection) || m.TypeSection[idx].CompositeKind != wasm.CompositeKindFunc {
			return nil
		}
		ft := &m.TypeSection[idx]
		return &ctCoreFunc{params: ft.Params, results: ft.Results}
	}
	table := func(t *wasm.Table) ctCoreExtern {
		x := ctCoreExtern{kind: 0x01, known: true, elem: t.Type, min: t.Min, is64: t.IsTable64}
		if t.Max != nil {
			x.max, x.hasMax = *t.Max, true
		}
		return x
	}
	memory := func(mem *wasm.Memory) ctCoreExtern {
		return ctCoreExtern{kind: 0x02, known: true, min: mem.Min, max: mem.Max, hasMax: mem.IsMaxEncoded, shared: mem.IsShared, is64: mem.IsMemory64, pageLog2: 16}
	}
	global := func(g wasm.GlobalType) ctCoreExtern {
		return ctCoreExtern{kind: 0x03, known: true, val: g.ValType, mutable: g.Mutable}
	}
	out := &ctModule{}
	var funcs []*ctCoreFunc
	var tables, mems, globals []ctCoreExtern
	var tags []*ctCoreFunc
	for i := range m.ImportSection {
		im := &m.ImportSection[i]
		var x ctCoreExtern
		switch im.Type {
		case wasm.ExternTypeFunc:
			x = ctCoreExtern{kind: 0x00, known: true, fn: fn(im.DescFunc)}
			funcs = append(funcs, x.fn)
		case wasm.ExternTypeTable:
			x = table(&im.DescTable)
			tables = append(tables, x)
		case wasm.ExternTypeMemory:
			x = memory(im.DescMem)
			mems = append(mems, x)
		case wasm.ExternTypeGlobal:
			x = global(im.DescGlobal)
			globals = append(globals, x)
		default: // wasm.ExternTypeTag
			x = ctCoreExtern{kind: 0x04, known: true, fn: fn(im.DescTag)}
			tags = append(tags, x.fn)
		}
		out.imports = append(out.imports, ctCoreImport{module: im.Module, name: im.Name, ext: x})
	}
	for _, t := range m.FunctionSection {
		funcs = append(funcs, fn(t))
	}
	for i := range m.TableSection {
		tables = append(tables, table(&m.TableSection[i]))
	}
	for i := range m.MemorySection {
		mems = append(mems, memory(&m.MemorySection[i]))
	}
	for i := range m.GlobalSection {
		globals = append(globals, global(m.GlobalSection[i].Type))
	}
	for i := range m.TagSection {
		tags = append(tags, fn(m.TagSection[i].Type))
	}
	for _, ex := range m.ExportSection {
		var x ctCoreExtern
		i := int(ex.Index)
		switch ex.Type {
		case wasm.ExternTypeFunc:
			if i < len(funcs) {
				x = ctCoreExtern{kind: 0x00, known: true, fn: funcs[i]}
			}
		case wasm.ExternTypeTable:
			if i < len(tables) {
				x = tables[i]
			}
		case wasm.ExternTypeMemory:
			if i < len(mems) {
				x = mems[i]
			}
		case wasm.ExternTypeGlobal:
			if i < len(globals) {
				x = globals[i]
			}
		default: // wasm.ExternTypeTag
			if i < len(tags) {
				x = ctCoreExtern{kind: 0x04, known: true, fn: tags[i]}
			}
		}
		x.kind = byte(ex.Type)
		out.exports = append(out.exports, ctCoreExport{name: ex.Name, ext: x})
	}
	return out, nil
}

// ctCoreScope is where a core module type's outer type alias resolves: the
// core type index space of the component or type body the module type is
// declared in.
type ctCoreScope interface {
	coreTypeAt(idx uint32) (ctCoreType, error)
}

// ctModuleOfType is the module type a core:moduletype declares in scope s.
// It opens a core type index space of its own, which starts empty: its type
// declarations and its outer aliases of s's core types fill it.
func ctModuleOfType(s ctCoreScope, mt *binary.CoreModuleType) (*ctModule, error) {
	var types []ctCoreType
	out := &ctModule{}
	for i, d := range mt.Decls {
		var err error
		switch d.Kind {
		case binary.CoreModuleDeclType:
			for _, f := range ctCoreRec(d.Rec) {
				types = append(types, ctCoreType{fn: f})
			}
		case binary.CoreModuleDeclAlias:
			var t ctCoreType
			switch d.AliasCount {
			case 0:
				if int(d.AliasIndex) >= len(types) {
					err = fmt.Errorf("unknown type %d: type index out of bounds", d.AliasIndex)
				} else {
					t = types[d.AliasIndex]
				}
			case 1:
				if t, err = s.coreTypeAt(d.AliasIndex); err == nil && t.mod != nil {
					err = fmt.Errorf("a module type cannot alias a module type")
				}
			default:
				err = fmt.Errorf("outer type aliases in module type declarations are limited to a maximum count of 1")
			}
			types = append(types, t)
		default: // binary.CoreModuleDeclImport, binary.CoreModuleDeclExport
			var x ctCoreExtern
			if x, err = ctCoreExternOf(d.Extern, types); err != nil {
				break
			}
			if d.Kind == binary.CoreModuleDeclImport {
				out.imports = append(out.imports, ctCoreImport{module: d.Module, name: d.Name, ext: x})
			} else {
				out.exports = append(out.exports, ctCoreExport{name: d.Name, ext: x})
			}
		}
		if err != nil {
			return nil, fmt.Errorf("module type declaration %d: %w", i, err)
		}
	}
	return out, nil
}

// ctCoreExternOf is the type a core:externtype declares, its func and tag
// type indices naming types.
func ctCoreExternOf(d binary.CoreExternType, types []ctCoreType) (ctCoreExtern, error) {
	x := ctCoreExtern{kind: d.Kind, known: true}
	r := &ctBytes{b: d.Bytes}
	switch d.Kind {
	case 0x00, 0x04: // func, tag
		if int(d.TypeIndex) >= len(types) || types[d.TypeIndex].fn == nil {
			return x, fmt.Errorf("type index %d is not a function type", d.TypeIndex)
		}
		x.fn = types[d.TypeIndex].fn
	case 0x01: // table: reftype limits
		x.elem = r.valType()
		r.limits(&x)
	case 0x02: // memory: limits
		r.limits(&x)
	default: // 0x03 global: valtype mut
		x.val = r.valType()
		x.mutable = r.byte() == 0x01
	}
	return x, nil
}

// ctBytes reads an encoding the binary package has already decoded -- a
// rectype or an extern type of a module type -- and so checked: no read of
// it can fail, and none reports an error.
type ctBytes struct {
	b   []byte
	off int
}

func (r *ctBytes) byte() byte {
	v := r.b[r.off]
	r.off++
	return v
}

func (r *ctBytes) u32() uint32 {
	v, k, _ := leb128.LoadUint32(r.b[r.off:])
	r.off += int(k)
	return v
}

func (r *ctBytes) u64() uint64 {
	v, k, _ := leb128.LoadUint64(r.b[r.off:])
	r.off += int(k)
	return v
}

// valType reads a core:valtype as wazy's core decoder represents it.
func (r *ctBytes) valType() wasm.ValueType {
	b := r.byte()
	if b != 0x63 && b != 0x64 { // a number, vector or abbreviated reference type
		return wasm.ValueType(b)
	}
	ht, k, _ := leb128.LoadInt33AsInt64(r.b[r.off:])
	r.off += int(k)
	if ht >= 0 {
		return wasm.ValueTypeConcreteRef(uint32(ht), b == 0x63)
	}
	v, _ := wasm.AbstractHeapTypeValueType(ht)
	if b == 0x64 {
		v = v.AsNonNullable()
	}
	return v
}

// limits reads a core:limits into x.
func (r *ctBytes) limits(x *ctCoreExtern) {
	flags := r.byte()
	x.min = r.u64()
	if flags&0x01 != 0 {
		x.max, x.hasMax = r.u64(), true
	}
	x.shared, x.is64, x.pageLog2 = flags&0x02 != 0, flags&0x04 != 0, 16
	if flags&0x08 != 0 {
		x.pageLog2 = r.u32()
	}
}

// ctCoreRec reads the types a core:rectype defines: a function type for each
// func, nil for a struct or array.
func ctCoreRec(rec []byte) []*ctCoreFunc {
	r := &ctBytes{b: rec}
	members := uint32(1)
	if rec[0] == 0x4e {
		r.off = 1
		members = r.u32()
	}
	out := make([]*ctCoreFunc, 0, members)
	for range members {
		if b := r.b[r.off]; b == 0x50 || b == 0x4f { // sub: step over the supertypes
			r.off++
			for n := r.u32(); n > 0; n-- {
				r.u32()
			}
		}
		switch r.byte() {
		case 0x60: // func
			f := &ctCoreFunc{}
			for _, list := range []*[]wasm.ValueType{&f.params, &f.results} {
				for n := r.u32(); n > 0; n-- {
					*list = append(*list, r.valType())
				}
			}
			out = append(out, f)
		case 0x5f: // struct
			for n := r.u32(); n > 0; n-- {
				r.fieldType()
			}
			out = append(out, nil)
		default: // 0x5e array
			r.fieldType()
			out = append(out, nil)
		}
	}
	return out
}

// fieldType steps over a struct or array field: a storage type and its
// mutability.
func (r *ctBytes) fieldType() {
	if b := r.b[r.off]; b == 0x78 || b == 0x77 { // i8, i16
		r.off++
	} else {
		r.valType()
	}
	r.off++
}

// ctModuleSub reports whether module type a is a subtype of b: it may
// import less, contravariantly, and export more, covariantly
// (wasmparser's module_type).
func ctModuleSub(a, b *ctModule) error {
	for _, ai := range a.imports {
		bi, ok := ctFindImport(b.imports, ai.module, ai.name)
		if !ok {
			return fmt.Errorf("missing expected import `%s::%s`", ai.module, ai.name)
		}
		if err := ctCoreExternSub(bi, ai.ext); err != nil {
			return fmt.Errorf("type mismatch in import `%s::%s`: %w", ai.module, ai.name, err)
		}
	}
	for _, be := range b.exports {
		ae, ok := (&ctCoreInstance{exports: a.exports}).export(be.name)
		if !ok {
			return fmt.Errorf("missing expected export `%s`", be.name)
		}
		if err := ctCoreExternSub(ae, be.ext); err != nil {
			return fmt.Errorf("type mismatch in export `%s`: %w", be.name, err)
		}
	}
	return nil
}

func ctFindImport(list []ctCoreImport, module, name string) (ctCoreExtern, bool) {
	for i := range list {
		if list[i].module == module && list[i].name == name {
			return list[i].ext, true
		}
	}
	return ctCoreExtern{}, false
}

// ctCoreKindDesc names a core extern sort.
func ctCoreKindDesc(kind byte) string {
	switch kind {
	case 0x00:
		return "func"
	case 0x01:
		return "table"
	case 0x02:
		return "memory"
	case 0x03:
		return "global"
	}
	return "tag"
}

// ctCoreExternSub reports whether a is a subtype of b (wasmparser's
// entity_type).
func ctCoreExternSub(a, b ctCoreExtern) error {
	if a.kind != b.kind {
		return fmt.Errorf("expected %s, found %s", ctCoreKindDesc(b.kind), ctCoreKindDesc(a.kind))
	}
	if !a.known || !b.known {
		return nil
	}
	switch a.kind {
	case 0x00:
		return ctCoreFuncSub(a.fn, b.fn)
	case 0x01:
		if a.elem != b.elem {
			return fmt.Errorf("expected table element type %s, found %s", ctCoreValDesc(b.elem), ctCoreValDesc(a.elem))
		}
		if a.shared != b.shared {
			return fmt.Errorf("mismatch in the shared flag for tables")
		}
		if a.is64 != b.is64 {
			return fmt.Errorf("mismatch in index type used for tables")
		}
		if !ctLimitsMatch(a, b) {
			return fmt.Errorf("mismatch in table limits")
		}
	case 0x02:
		if a.shared != b.shared {
			return fmt.Errorf("mismatch in the shared flag for memories")
		}
		if a.is64 != b.is64 {
			return fmt.Errorf("mismatch in index type used for memories")
		}
		if a.pageLog2 != b.pageLog2 {
			return fmt.Errorf("mismatch in page size for memories")
		}
		if !ctLimitsMatch(a, b) {
			return fmt.Errorf("mismatch in memory limits")
		}
	case 0x03:
		if a.mutable != b.mutable {
			return fmt.Errorf("global types differ in mutability")
		}
		// A mutable global is invariant in its content type, an immutable
		// one covariant.
		ok := ctCoreValSub(a.val, b.val) && (!a.mutable || ctCoreValSub(b.val, a.val))
		if !ok {
			return fmt.Errorf("expected global type %s, found %s", ctCoreValDesc(b.val), ctCoreValDesc(a.val))
		}
	default: // a tag matches both ways
		if err := ctCoreFuncSub(b.fn, a.fn); err != nil {
			return err
		}
		return ctCoreFuncSub(a.fn, b.fn)
	}
	return nil
}

// ctLimitsMatch is wasmparser's limits_match: a's range lies within b's.
func ctLimitsMatch(a, b ctCoreExtern) bool {
	return a.min >= b.min && (!b.hasMax || (a.hasMax && a.max <= b.max))
}

// ctCoreFuncSub reports whether function type a is a subtype of b: the same
// parameters and results. (A concrete reference names a type of its own
// module's type space, so two of them are told apart only by index.)
func ctCoreFuncSub(a, b *ctCoreFunc) error {
	if a == nil || b == nil || (ctSameValTypes(a.params, b.params) && ctSameValTypes(a.results, b.results)) {
		return nil
	}
	return fmt.Errorf("expected: %s\nfound:    %s", b, a)
}

func ctSameValTypes(a, b []wasm.ValueType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// String renders the type as the text format does: (func (param ...)
// (result ...)).
func (f *ctCoreFunc) String() string {
	var sb strings.Builder
	sb.WriteString("(func")
	for _, part := range []struct {
		kw   string
		list []wasm.ValueType
	}{{"param", f.params}, {"result", f.results}} {
		if len(part.list) == 0 {
			continue
		}
		sb.WriteString(" (" + part.kw)
		for _, v := range part.list {
			sb.WriteString(" " + ctCoreValDesc(v))
		}
		sb.WriteString(")")
	}
	sb.WriteString(")")
	return sb.String()
}

// ctCoreValSub reports whether value type a is a subtype of b: equal, or a
// reference to a sub-heap-type, no more nullable.
func ctCoreValSub(a, b wasm.ValueType) bool {
	if a == b {
		return true
	}
	if !a.IsRef() || !b.IsRef() || (a.IsNullable() && !b.IsNullable()) {
		return false
	}
	return ctHeapSub(a.AsNullable(), b.AsNullable())
}

// ctHeapSub orders abstract heap types: the bottom of each hierarchy below
// all of it, i31, struct and array below eq below any, and a concrete
// function type below func.
func ctHeapSub(a, b wasm.ValueType) bool {
	if a == b {
		return true
	}
	if a.IsConcreteRef() {
		return b == wasm.ValueTypeFuncref
	}
	switch a {
	case wasm.ValueTypeNullref:
		return b == wasm.ValueTypeAnyref || b == wasm.ValueTypeEqref || b == wasm.ValueTypeI31ref || b == wasm.ValueTypeStructref || b == wasm.ValueTypeArrayref
	case wasm.ValueTypeNullFuncref:
		return b == wasm.ValueTypeFuncref || b.IsConcreteRef()
	case wasm.ValueTypeNullExternref:
		return b == wasm.ValueTypeExternref
	case wasm.ValueTypeNullExnref:
		return b == wasm.ValueTypeExnref
	case wasm.ValueTypeI31ref, wasm.ValueTypeStructref, wasm.ValueTypeArrayref:
		return b == wasm.ValueTypeEqref || b == wasm.ValueTypeAnyref
	case wasm.ValueTypeEqref:
		return b == wasm.ValueTypeAnyref
	}
	return false
}

// ctCoreValDesc renders a value type as wasmparser does.
func ctCoreValDesc(v wasm.ValueType) string {
	names := map[wasm.ValueType]string{
		wasm.ValueTypeI32: "i32", wasm.ValueTypeI64: "i64", wasm.ValueTypeF32: "f32", wasm.ValueTypeF64: "f64", wasm.ValueTypeV128: "v128",
		wasm.ValueTypeFuncref: "funcref", wasm.ValueTypeExternref: "externref", wasm.ValueTypeExnref: "exnref",
		wasm.ValueTypeAnyref: "anyref", wasm.ValueTypeEqref: "eqref", wasm.ValueTypeI31ref: "i31ref",
		wasm.ValueTypeStructref: "structref", wasm.ValueTypeArrayref: "arrayref", wasm.ValueTypeNullref: "nullref",
		wasm.ValueTypeNullFuncref: "nullfuncref", wasm.ValueTypeNullExternref: "nullexternref", wasm.ValueTypeNullExnref: "nullexnref",
	}
	if s, ok := names[v]; ok {
		return s
	}
	null := ""
	if v.IsNullable() {
		null = "null "
	}
	if v.IsConcreteRef() {
		return fmt.Sprintf("(ref %s%d)", null, v.TypeIndex())
	}
	heap := strings.TrimSuffix(names[v.AsNullable()], "ref")
	if strings.HasPrefix(heap, "null") { // nullref's heap type is none, nullfuncref's nofunc, ...
		heap = "no" + strings.TrimPrefix(heap, "null")
		if heap == "no" {
			heap = "none"
		}
	}
	return fmt.Sprintf("(ref %s%s)", null, heap)
}
