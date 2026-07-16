package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// TypeDesc represents a full component type descriptor, capturing the complete
// structure of a deftype. It is built by walking the component binary, mirroring
// the exact byte-consumption of the existing walker but also recording the
// semantic structure for use by the Canonical ABI and other tools.
type TypeDesc interface {
	isTypeDesc() // marker method for type safety

	// Kind returns the human-readable kind string for this descriptor
	// (e.g. "func", "record", "u32"). This is the single source of truth
	// for Type.Kind; there is no separate walk that derives it.
	Kind() string
}

// TypeRef is either a reference to a primitive type (encoded as a byte name)
// or an index into the type table.
type TypeRef struct {
	// Exactly one of these must be set.
	Primitive string // e.g. "bool", "u32", "string", etc.; empty means use TypeIndex
	TypeIndex *uint32
}

// PrimitiveDesc represents a primitive value type (bool, s32, u64, string, etc.).
type PrimitiveDesc struct {
	Prim string // "bool", "s8", "u8", "s16", "u16", "s32", "u32", "s64", "u64", "f32", "f64", "char", "string"
}

func (PrimitiveDesc) isTypeDesc()    {}
func (p PrimitiveDesc) Kind() string { return p.Prim }

// RecordDesc represents a record type (struct-like) with named fields.
type RecordDesc struct {
	Fields []RecordField
}

func (RecordDesc) isTypeDesc()  {}
func (RecordDesc) Kind() string { return "record" }

type RecordField struct {
	Name string
	Type TypeRef
}

// VariantDesc represents a discriminated union.
type VariantDesc struct {
	Cases []VariantCase
}

func (VariantDesc) isTypeDesc()  {}
func (VariantDesc) Kind() string { return "variant" }

type VariantCase struct {
	Name string
	Type *TypeRef // optional (nil means no payload)
}

// ListDesc represents a list (unbounded array).
type ListDesc struct {
	Element TypeRef
}

func (ListDesc) isTypeDesc()  {}
func (ListDesc) Kind() string { return "list" }

// TupleDesc represents a tuple (unnamed record).
type TupleDesc struct {
	Elements []TypeRef
}

func (TupleDesc) isTypeDesc()  {}
func (TupleDesc) Kind() string { return "tuple" }

// FlagsDesc represents a flags type (set of named booleans).
type FlagsDesc struct {
	Names []string
}

func (FlagsDesc) isTypeDesc()  {}
func (FlagsDesc) Kind() string { return "flags" }

// EnumDesc represents an enum type.
type EnumDesc struct {
	Cases []string
}

func (EnumDesc) isTypeDesc()  {}
func (EnumDesc) Kind() string { return "enum" }

// OptionDesc represents an optional value.
type OptionDesc struct {
	Element TypeRef
}

func (OptionDesc) isTypeDesc()  {}
func (OptionDesc) Kind() string { return "option" }

// ResultDesc represents a result type (success or error).
type ResultDesc struct {
	Ok  *TypeRef // optional
	Err *TypeRef // optional
}

func (ResultDesc) isTypeDesc()  {}
func (ResultDesc) Kind() string { return "result" }

// OwnDesc represents owned handle to a resource.
type OwnDesc struct {
	ResourceType uint32
}

func (OwnDesc) isTypeDesc()  {}
func (OwnDesc) Kind() string { return "own" }

// BorrowDesc represents a borrowed handle to a resource.
type BorrowDesc struct {
	ResourceType uint32
}

func (BorrowDesc) isTypeDesc()  {}
func (BorrowDesc) Kind() string { return "borrow" }

// FuncDesc represents a function type.
type FuncDesc struct {
	Params  []FuncParam
	Results FuncResults
}

func (FuncDesc) isTypeDesc()  {}
func (FuncDesc) Kind() string { return "func" }

type FuncParam struct {
	Name string
	Type TypeRef
}

// FuncResults captures the two forms of result lists:
// - Unnamed: a single unnamed result (tag 0x00)
// - Named: zero or more named results (tag 0x01)
type FuncResults struct {
	Unnamed *TypeRef     // non-nil if tag was 0x00
	Named   []FuncResult // non-empty if tag was 0x01
}

type FuncResult struct {
	Name string
	Type TypeRef
}

// InstanceDesc represents an instance type (collection of exports).
type InstanceDesc struct {
	Exports map[string]TypeRef // export name -> type
}

func (InstanceDesc) isTypeDesc()  {}
func (InstanceDesc) Kind() string { return "instance" }

// ComponentDesc represents a component type (collection of declarations).
type ComponentDesc struct {
	// For now, component decls are not fully represented.
	// This is a stub; M1 focuses on function and data types.
}

func (ComponentDesc) isTypeDesc()  {}
func (ComponentDesc) Kind() string { return "component" }

// ResourceDesc represents a resource type.
type ResourceDesc struct {
	Rep  TypeRef // representation type (usually a primitive)
	Dtor *uint32 // optional destructor function index
}

func (ResourceDesc) isTypeDesc()  {}
func (ResourceDesc) Kind() string { return "resource" }

// readValTypeRef consumes a valtype and returns a TypeRef.
func readValTypeRef(buf []byte, off int) (TypeRef, int, error) {
	v, n, err := leb128.LoadInt33AsInt64(buf[off:])
	if err != nil {
		return TypeRef{}, off, fmt.Errorf("valtype: %w", err)
	}
	off += int(n)
	if v < 0 {
		b := byte(v & 0x7f)
		if !isPrimValtype(b) {
			return TypeRef{}, off, fmt.Errorf("valtype: invalid primitive code %#x", b)
		}
		return TypeRef{Primitive: primName(b)}, off, nil
	}
	// Positive value is a type index
	idx := uint32(v)
	return TypeRef{TypeIndex: &idx}, off, nil
}

// readOptValTypeRef consumes an optional valtype and returns a TypeRef.
// Returns nil if the option tag is 0x00 (none).
func readOptValTypeRef(buf []byte, off int) (*TypeRef, int, error) {
	if off >= len(buf) {
		return nil, off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x00:
		return nil, off, nil
	case 0x01:
		ref, off, err := readValTypeRef(buf, off)
		return &ref, off, err
	default:
		return nil, off, fmt.Errorf("optional valtype: invalid tag %#x", tag)
	}
}

// readLabelValtypeVecDesc reads a vec(label valtype) and returns the slice of
// (name, type) pairs.
func readLabelValtypeVecDesc(buf []byte, off int) ([]struct {
	Name string
	Type TypeRef
}, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return nil, off, err
	}
	off += int(n)
	result := make([]struct {
		Name string
		Type TypeRef
	}, count)
	for i := range count {
		name, newOff, err := readLabel(buf, off)
		if err != nil {
			return nil, newOff, err
		}
		ref, newOff, err := readValTypeRef(buf, newOff)
		if err != nil {
			return nil, newOff, err
		}
		result[i].Name = name
		result[i].Type = ref
		off = newOff
	}
	return result, off, nil
}

// readLabelVecDesc reads a vec(label) and returns the slice of names.
func readLabelVecDesc(buf []byte, off int) ([]string, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return nil, off, err
	}
	off += int(n)
	result := make([]string, count)
	for i := range count {
		name, newOff, err := readLabel(buf, off)
		if err != nil {
			return nil, newOff, err
		}
		result[i] = name
		off = newOff
	}
	return result, off, nil
}

// readValtypeVecDesc reads a vec(valtype) and returns the slice of TypeRefs.
func readValtypeVecDesc(buf []byte, off int) ([]TypeRef, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return nil, off, err
	}
	off += int(n)
	result := make([]TypeRef, count)
	for i := range count {
		ref, newOff, err := readValTypeRef(buf, off)
		if err != nil {
			return nil, newOff, err
		}
		result[i] = ref
		off = newOff
	}
	return result, off, nil
}

// readRecordDesc reads a record type: vec(label valtype).
func readRecordDesc(buf []byte, off int) (RecordDesc, int, error) {
	fields, off, err := readLabelValtypeVecDesc(buf, off)
	if err != nil {
		return RecordDesc{}, off, err
	}
	result := RecordDesc{Fields: make([]RecordField, len(fields))}
	for i, f := range fields {
		result.Fields[i] = RecordField{Name: f.Name, Type: f.Type}
	}
	return result, off, nil
}

// readVariantCaseDesc reads one variant case: label option(valtype) option(refines:u32).
func readVariantCaseDesc(buf []byte, off int) (VariantCase, int, error) {
	name, newOff, err := readLabel(buf, off)
	if err != nil {
		return VariantCase{}, newOff, err
	}
	off = newOff

	typ, newOff, err := readOptValTypeRef(buf, off)
	if err != nil {
		return VariantCase{}, newOff, err
	}
	off = newOff

	// option(refines:u32) — currently always absent in practice
	if off >= len(buf) {
		return VariantCase{}, off, ErrTruncatedBinary
	}
	switch buf[off] {
	case 0x00:
		off++
	case 0x01:
		off++
		_, m, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return VariantCase{}, off, e
		}
		off += int(m)
	default:
		return VariantCase{}, off, fmt.Errorf("variant case refines: invalid tag %#x", buf[off])
	}

	return VariantCase{Name: name, Type: typ}, off, nil
}

// readVariantDesc reads a variant type: vec(case).
func readVariantDesc(buf []byte, off int) (VariantDesc, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return VariantDesc{}, off, err
	}
	off += int(n)
	cases := make([]VariantCase, count)
	for i := range count {
		c, newOff, err := readVariantCaseDesc(buf, off)
		if err != nil {
			return VariantDesc{}, newOff, err
		}
		cases[i] = c
		off = newOff
	}
	return VariantDesc{Cases: cases}, off, nil
}

// readResultListDesc reads a functype result list.
func readResultListDesc(buf []byte, off int) (FuncResults, int, error) {
	if off >= len(buf) {
		return FuncResults{}, off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x00:
		ref, off, err := readValTypeRef(buf, off)
		if err != nil {
			return FuncResults{}, off, err
		}
		return FuncResults{Unnamed: &ref}, off, nil
	case 0x01:
		fields, off, err := readLabelValtypeVecDesc(buf, off)
		if err != nil {
			return FuncResults{}, off, err
		}
		results := make([]FuncResult, len(fields))
		for i, f := range fields {
			results[i] = FuncResult{Name: f.Name, Type: f.Type}
		}
		return FuncResults{Named: results}, off, nil
	default:
		return FuncResults{}, off, fmt.Errorf("result list: invalid tag %#x", tag)
	}
}

// readFunctypeDesc reads a functype: params then result list.
func readFunctypeDesc(buf []byte, off int) (FuncDesc, int, error) {
	fields, off, err := readLabelValtypeVecDesc(buf, off)
	if err != nil {
		return FuncDesc{}, off, fmt.Errorf("functype params: %w", err)
	}
	params := make([]FuncParam, len(fields))
	for i, f := range fields {
		params[i] = FuncParam{Name: f.Name, Type: f.Type}
	}

	results, off, err := readResultListDesc(buf, off)
	if err != nil {
		return FuncDesc{}, off, fmt.Errorf("functype results: %w", err)
	}

	return FuncDesc{Params: params, Results: results}, off, nil
}

// readDefvaltypeDesc consumes a defvaltype body (tag already read) and returns
// a TypeDesc. If a construct is not yet fully represented in the descriptor
// model, it returns an error rather than silently dropping structure.
func readDefvaltypeDesc(buf []byte, off int, tag byte) (TypeDesc, int, error) {
	if isPrimValtype(tag) {
		return PrimitiveDesc{Prim: primName(tag)}, off, nil
	}
	var desc TypeDesc
	var err error
	switch tag {
	case 0x72: // record
		d, off2, e := readRecordDesc(buf, off)
		desc, off, err = d, off2, e
	case 0x71: // variant
		d, off2, e := readVariantDesc(buf, off)
		desc, off, err = d, off2, e
	case 0x70: // list: one valtype
		elem, off2, e := readValTypeRef(buf, off)
		desc, off, err = ListDesc{Element: elem}, off2, e
	case 0x6f: // tuple: vec(valtype)
		elems, off2, e := readValtypeVecDesc(buf, off)
		desc, off, err = TupleDesc{Elements: elems}, off2, e
	case 0x6e: // flags: vec(label)
		names, off2, e := readLabelVecDesc(buf, off)
		desc, off, err = FlagsDesc{Names: names}, off2, e
	case 0x6d: // enum: vec(label)
		cases, off2, e := readLabelVecDesc(buf, off)
		desc, off, err = EnumDesc{Cases: cases}, off2, e
	case 0x6b: // option: one valtype
		elem, off2, e := readValTypeRef(buf, off)
		desc, off, err = OptionDesc{Element: elem}, off2, e
	case 0x6a: // result: opt(valtype) opt(valtype)
		ok, off2, e := readOptValTypeRef(buf, off)
		if e != nil {
			return nil, off2, e
		}
		errType, off3, e := readOptValTypeRef(buf, off2)
		if e != nil {
			return nil, off3, e
		}
		desc, off, err = ResultDesc{Ok: ok, Err: errType}, off3, nil
	case 0x69: // own: typeidx
		idx, off2, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return nil, off, e
		}
		desc, off, err = OwnDesc{ResourceType: idx}, off+int(off2), nil
	case 0x68: // borrow: typeidx
		idx, off2, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return nil, off, e
		}
		desc, off, err = BorrowDesc{ResourceType: idx}, off+int(off2), nil
	default:
		return nil, off, fmt.Errorf("unsupported (M1): defvaltype tag %#x", tag)
	}
	return desc, off, err
}

// readResourcetypeDesc reads a resourcetype: a core:valtype rep then a dtor
// option. The rep is a CORE type (a single byte, i32=0x7f in practice), NOT a
// component valtype -- so 0x7f here means i32, not bool.
func readResourcetypeDesc(buf []byte, off int) (ResourceDesc, int, error) {
	if off >= len(buf) {
		return ResourceDesc{}, off, ErrTruncatedBinary
	}
	repName, err := coreValtypeName(buf[off])
	if err != nil {
		return ResourceDesc{}, off, err
	}
	rep := TypeRef{Primitive: repName}
	off++
	if off >= len(buf) {
		return ResourceDesc{}, off, ErrTruncatedBinary
	}
	dtor := buf[off]
	off++
	var dtorIdx *uint32
	if dtor == 0x01 {
		idx, n, e := leb128.LoadUint32(buf[off:])
		if e != nil {
			return ResourceDesc{}, off, e
		}
		dtorIdx = &idx
		off += int(n)
	} else if dtor != 0x00 {
		return ResourceDesc{}, off, fmt.Errorf("resourcetype: invalid destructor tag %#x", dtor)
	}
	return ResourceDesc{Rep: rep, Dtor: dtorIdx}, off, nil
}

// coreValtypeName maps a core:valtype byte to its name. Core numeric types use
// a different table than component primvaltypes (e.g. 0x7f is i32 in core wasm
// but bool in the component model).
func coreValtypeName(b byte) (string, error) {
	switch b {
	case 0x7f:
		return "i32", nil
	case 0x7e:
		return "i64", nil
	case 0x7d:
		return "f32", nil
	case 0x7c:
		return "f64", nil
	case 0x7b:
		return "v128", nil
	default:
		return "", fmt.Errorf("resourcetype: invalid core rep valtype %#x", b)
	}
}

// readInstanceDeclDesc consumes one instancedecl and may contribute to the
// descriptor model. For M1, instance types with nested types or complex
// exports are not fully represented.
func readInstanceDeclDesc(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x01: // type: a deftype
		_, off2, err := readDeftypeDesc(buf, off)
		return off2, err
	case 0x04: // export decl: externname externdesc
		_, off2, err := readExternName(buf, off)
		if err != nil {
			return off2, err
		}
		_, _, _, off2, err = readExterndesc(buf, off2)
		return off2, err
	case 0x02: // alias
		return readAlias(buf, off)
	case 0x00: // core:type -- a core func/module type defined inline in the
		// instance or component type. It carries no runtime obligation (these
		// type-only validation shapes are opaque tags -- see the package doc);
		// consume its bytes to stay synchronized.
		return readCoretypeDef(buf, off)
	default:
		return off, fmt.Errorf("instancedecl: invalid tag %#x", tag)
	}
}

// readCoretypeDef consumes one core:type definition -- a core func type (0x60)
// or a core module type (0x50) -- to keep the decoder synchronized when one
// appears inside an instance/component type. It parses the grammar rather than
// guessing a length, so a non-empty module type (imports/exports/nested types/
// aliases) is consumed correctly too.
func readCoretypeDef(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x60: // core:functype = vec(valtype) params, vec(valtype) results
		var err error
		if off, err = readCoreValtypeVec(buf, off); err != nil {
			return off, err
		}
		return readCoreValtypeVec(buf, off)
	case 0x50: // core:moduletype = vec(core:moduledecl)
		count, n, err := leb128.LoadUint32(buf[off:])
		if err != nil {
			return off, err
		}
		off += int(n)
		for i := uint32(0); i < count; i++ {
			if off, err = readCoreModuleDecl(buf, off); err != nil {
				return off, fmt.Errorf("coremoduledecl[%d]: %w", i, err)
			}
		}
		return off, nil
	default:
		return off, fmt.Errorf("core:type: invalid tag %#x", tag)
	}
}

// readCoreValtypeVec consumes vec(core:valtype); each core valtype is a single
// byte (0x7f i32 … 0x7b v128, plus reference-type bytes), so the whole vector
// is count single bytes.
func readCoreValtypeVec(buf []byte, off int) (int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(n)
	if off+int(count) > len(buf) {
		return off, ErrTruncatedBinary
	}
	return off + int(count), nil
}

// readCoreModuleDecl consumes one core:moduledecl inside a core module type:
// an import (0x00), a nested core type (0x01), a core alias (0x02), or an
// export (0x03).
func readCoreModuleDecl(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x00: // import: nm nm core:importdesc
		var err error
		if off, err = skipName(buf, off); err != nil {
			return off, err
		}
		if off, err = skipName(buf, off); err != nil {
			return off, err
		}
		return readCoreImportdesc(buf, off)
	case 0x01: // type: core:type (recursive)
		return readCoretypeDef(buf, off)
	case 0x02: // alias: core:alias -- sort byte + target; consumed conservatively
		return readCoreModuleAlias(buf, off)
	case 0x03: // export: nm core:importdesc
		var err error
		if off, err = skipName(buf, off); err != nil {
			return off, err
		}
		return readCoreImportdesc(buf, off)
	default:
		return off, fmt.Errorf("coremoduledecl: invalid tag %#x", tag)
	}
}

// readCoreImportdesc consumes a core:importdesc = sort byte + typeidx (func
// 0x00, table 0x01, memory 0x02, global 0x03, tag 0x04). Table/memory/global
// carry a small inline type rather than an index, but these type-only shapes
// only ever use func imports/exports in practice; a non-func desc fails loud.
func readCoreImportdesc(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	sort := buf[off]
	off++
	switch sort {
	case 0x00: // func: typeidx
		_, n, err := leb128.LoadUint32(buf[off:])
		if err != nil {
			return off, err
		}
		return off + int(n), nil
	default:
		return off, fmt.Errorf("core:importdesc: unsupported sort %#x", sort)
	}
}

// readCoreModuleAlias consumes a core:alias inside a core module type: a sort
// byte then a target (0x00 export: instanceidx + name, 0x01 outer: ct + idx).
func readCoreModuleAlias(buf []byte, off int) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	off++ // sort byte
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	target := buf[off]
	off++
	switch target {
	case 0x01: // outer: ct idx
		_, n, err := leb128.LoadUint32(buf[off:])
		if err != nil {
			return off, err
		}
		off += int(n)
		_, n, err = leb128.LoadUint32(buf[off:])
		if err != nil {
			return off, err
		}
		return off + int(n), nil
	default:
		return off, fmt.Errorf("core:alias: unsupported target %#x", target)
	}
}

// skipName consumes a name = vec(byte) (leb length + that many bytes).
func skipName(buf []byte, off int) (int, error) {
	n, m, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return off, err
	}
	off += int(m)
	if off+int(n) > len(buf) {
		return off, ErrTruncatedBinary
	}
	return off + int(n), nil
}

// readInstancetypeDesc reads an instance type: vec(instancedecl).
// For M1, we consume the bytes but do not fully represent all nested structures.
func readInstancetypeDesc(buf []byte, off int) (InstanceDesc, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return InstanceDesc{}, off, err
	}
	off += int(n)
	// For M1, we skip building a full descriptor for instance contents.
	// Just consume the bytes to stay synchronized.
	for i := range count {
		off, err = readInstanceDeclDesc(buf, off)
		if err != nil {
			return InstanceDesc{}, off, fmt.Errorf("instancedecl[%d]: %w", i, err)
		}
	}
	return InstanceDesc{Exports: make(map[string]TypeRef)}, off, nil
}

// readDeftypeDesc consumes one deftype and returns a TypeDesc, OR a kind string
// for backward compatibility. This is the main entry point for building
// descriptors.
func readDeftypeDesc(buf []byte, off int) (TypeDesc, int, error) {
	if off >= len(buf) {
		return nil, off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch {
	case tag == 0x40:
		d, off, err := readFunctypeDesc(buf, off)
		return d, off, err
	case tag == 0x41:
		// Component type: consume but do not fully represent in M1.
		off, err := readComponenttype(buf, off)
		return ComponentDesc{}, off, err
	case tag == 0x42:
		d, off, err := readInstancetypeDesc(buf, off)
		return d, off, err
	case tag == 0x3f:
		d, off, err := readResourcetypeDesc(buf, off)
		return d, off, err
	case isPrimValtype(tag):
		return PrimitiveDesc{Prim: primName(tag)}, off, nil
	default:
		d, off, err := readDefvaltypeDesc(buf, off, tag)
		return d, off, err
	}
}
