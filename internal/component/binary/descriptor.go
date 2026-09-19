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

// MapDesc represents a map<K,V>: an unordered association of keys to values.
// Per the Canonical ABI's despecialize() step, a map has no wire
// representation of its own -- it is byte-for-byte identical to
// list<tuple<K,V>> (same alignment, size, flattening, and lift/lower), so
// every ABI function that handles MapDesc does so by delegating to its
// TupleDesc/ListDesc counterpart with a synthetic `tuple<Key,Value>` element
// rather than repeating the layout. A map's Value is therefore the same
// shape as list<tuple<K,V>>: a []Value of two-element []Value pairs
// ([]Value{key, value}), in whatever order the guest produced them -- a map
// carries no ordering guarantee.
//
// Key is restricted by the spec's own `keytype` grammar (design/mvp/
// Explainer.md) to bool, an integer, char, or string -- a conservative
// subset of `valtype` picked to simplify bindings generation, deliberately
// excluding f32/f64 (float equality/NaN) and every composite, handle, and
// async type. See IsValidMapKeyPrimitive.
type MapDesc struct {
	Key   TypeRef
	Value TypeRef
}

func (MapDesc) isTypeDesc()  {}
func (MapDesc) Kind() string { return "map" }

// IsValidMapKeyPrimitive reports whether prim is one of the primitives the
// Component Model spec's `keytype` grammar allows as a map key: bool, every
// integer width, char, or string. It only ever sees MapDesc.Key.Primitive,
// so it cannot itself resolve a key given by type index -- every keytype is,
// however, a bare primitive a conformant producer always spells inline (see
// readValTypeRef), so an index there (Key.Primitive == "") already names
// something keytype does not permit: a composite, a resource handle, or a
// stream/future. TypeTable.Map treats it that way for the one type index
// that can occur in this codebase's own type tables (which never assign an
// index to a primitive to begin with -- see TypeTable.Add).
func IsValidMapKeyPrimitive(prim string) bool {
	switch prim {
	case "bool", "s8", "u8", "s16", "u16", "s32", "u32", "s64", "u64", "char", "string":
		return true
	default:
		return false
	}
}

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

// StreamDesc represents a `stream<T>` (or bare `stream` with no element)
// async-ABI type. Element is nil for the bare form. Phase 0 treats a stream
// value as an opaque i32 handle throughout the ABI layer (like own/borrow);
// Element is recorded for completeness but not otherwise consumed until
// stream/future runtime support lands.
type StreamDesc struct {
	Element *TypeRef // nil if the stream carries no element type
}

func (StreamDesc) isTypeDesc()  {}
func (StreamDesc) Kind() string { return "stream" }

// FutureDesc represents a `future<T>` (or bare `future` with no element)
// async-ABI type. Element is nil for the bare form. Same opaque-handle
// treatment as StreamDesc.
type FutureDesc struct {
	Element *TypeRef // nil if the future carries no element type
}

func (FutureDesc) isTypeDesc()  {}
func (FutureDesc) Kind() string { return "future" }

// FuncDesc represents a function type.
type FuncDesc struct {
	Params  []FuncParam
	Results FuncResults
	// Async is true for an `async func` component-func type (deftype tag 0x43,
	// vs the synchronous 0x40). A canon lift/lower's own async CanonOpt (0x06)
	// is separate; the Canonical ABI requires the two to agree, but that
	// cross-check is a validation concern, not decode.
	Async bool
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
//
// An instancetype opens its own nested type-sort index space, the same way a
// nested component opens its own (see Component.Outer) -- so Types and the
// TypeIndex side of an Exports TypeRef both index into THIS InstanceDesc's
// own space, never into the enclosing component's TypeSpace. A TypeRef here
// with TypeIndex set is only ever resolvable through Types directly (see
// resolveAlias's case 0x00, which globalizes it into the enclosing
// Component's escape range before handing it to a caller) -- never through
// an ordinary Resolver.
type InstanceDesc struct {
	// Exports maps each type-sort export name to its type. A name absent
	// from this map was either not exported at all, or was exported but this
	// decoder could not resolve it structurally (a `sub`-bound/abstract
	// resource export, or a type-sort alias).
	Exports map[string]TypeRef

	// Types holds the deftypes declared locally within this instancetype's
	// own body (instancedecl tag 0x01), plus one nil placeholder per other
	// type-sort-index-producing instancedecl (a type-sort alias, or a
	// `sub`-bound export) this decoder cannot resolve structurally, in
	// declaration order -- see readInstanceDeclDescInto.
	Types []TypeDesc
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
}, int, error,
) {
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
	case 0x63: // map: key valtype, value valtype
		key, off2, e := readValTypeRef(buf, off)
		if e != nil {
			return nil, off2, e
		}
		// A conformant producer always spells a keytype inline (see
		// IsValidMapKeyPrimitive's doc); a type-index key already fails this
		// the same way an invalid inline primitive does, since Primitive is
		// "" for it.
		if key.TypeIndex != nil {
			return nil, off2, fmt.Errorf("map: invalid key type (type index %d): spec restricts map keys to bool, an integer, char, or string", *key.TypeIndex)
		}
		if !IsValidMapKeyPrimitive(key.Primitive) {
			return nil, off2, fmt.Errorf("map: invalid key type %q: spec restricts map keys to bool, an integer, char, or string", key.Primitive)
		}
		val, off3, e := readValTypeRef(buf, off2)
		desc, off, err = MapDesc{Key: key, Value: val}, off3, e
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
	case 0x66: // stream: option(valtype) -- 0x00 none, 0x01 <valtype> some.
		// Verified via `wasm-tools dump`: `66 01 7d` decodes as
		// Stream(Some(Primitive(U8))).
		elem, off2, e := readOptValTypeRef(buf, off)
		if e == nil && elem != nil && elem.Primitive == "char" {
			// `stream<char>` is a temporary spec-level limitation (a stream
			// element must be re-encodable independent of its neighbors,
			// which char's UTF-8 validation can't guarantee mid-stream);
			// test/async/validate-no-stream-char.wast vendors it as
			// assert_invalid. See
			// https://github.com/WebAssembly/component-model/pull/607.
			e = fmt.Errorf("`stream<char>` is not valid")
		}
		desc, off, err = StreamDesc{Element: elem}, off2, e
	case 0x65: // future: option(valtype), same shape as stream.
		// Verified via `wasm-tools dump`: `65 01 79` decodes as
		// Future(Some(Primitive(U32))).
		elem, off2, e := readOptValTypeRef(buf, off)
		desc, off, err = FutureDesc{Element: elem}, off2, e
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

// readInstanceDeclDesc consumes one instancedecl without retaining its
// structure. Used only where no caller needs the enclosing instancetype's own
// nested type-sort index space (a componenttype body -- see
// readComponentDecl); readInstancetypeDesc uses readInstanceDeclDescInto
// directly instead, since it does need that space to build InstanceDesc's
// Exports.
func readInstanceDeclDesc(buf []byte, off int) (int, error) {
	var localTypes []TypeDesc
	return readInstanceDeclDescInto(buf, off, &localTypes, nil)
}

// readInstanceDeclDescInto consumes one instancedecl, contributing to
// *localTypes -- the enclosing instancetype's own nested type-sort index
// space, opened fresh by the instancetype the same way a nested component
// opens its own (see Component.Outer) -- and, for a named type export, to
// exports (may be nil when the caller, like readInstanceDeclDesc, has no use
// for it). A TypeRef placed in exports whose TypeIndex is set names an index
// into *localTypes, NOT into any enclosing component's TypeSpace -- see
// InstanceDesc's doc.
//
// Per the component binary format, a "type" (tag 0x01), a type-sort "alias"
// (tag 0x02, sort byte 0x03), and a type-sort "export" (tag 0x04, externdesc
// sort 0x03) each introduce a new entry in *localTypes, in declaration order
// -- exactly mirroring how Component.TypeSpace is built for the enclosing
// component (see typespace.go's doc), just scoped to this one instancetype
// body. Tracking every contributor, not just "type" decls, matters even
// though only "type" decls produce a resolvable TypeDesc here: skipping the
// others would misindex any later decl that references one of their slots by
// number, silently returning the wrong local type instead of failing loud.
func readInstanceDeclDescInto(buf []byte, off int, localTypes *[]TypeDesc, exports *map[string]TypeRef) (int, error) {
	if off >= len(buf) {
		return off, ErrTruncatedBinary
	}
	tag := buf[off]
	off++
	switch tag {
	case 0x01: // type: a deftype -- a new local type-sort index
		d, off2, err := readDeftypeDesc(buf, off)
		if err != nil {
			return off2, err
		}
		*localTypes = append(*localTypes, d)
		return off2, nil

	case 0x04: // export decl: externname externdesc
		name, off2, err := readExternName(buf, off)
		if err != nil {
			return off2, err
		}
		sort, idx, hasIdx, off3, err := readExterndesc(buf, off2)
		if err != nil {
			return off3, err
		}
		if sort == 0x03 { // type-sort export: a new local type-sort index
			// hasIdx is false for a `sub`-bound (abstract resource) export,
			// which this decoder cannot resolve structurally -- its slot is
			// still reserved (nil) so a later decl referencing it by number
			// fails loud rather than misindexing, but it contributes nothing
			// to exports.
			*localTypes = append(*localTypes, nil)
			if hasIdx && exports != nil { // `eq N`-bound: an alias of local type N
				if *exports == nil {
					// Built on first type-sort export rather than up front:
					// most instancetypes export only functions, and an always
					// allocated map is one of the few per-import costs of
					// retaining these declarations at all.
					*exports = make(map[string]TypeRef, 4)
				}
				idx := idx
				(*exports)[name] = TypeRef{TypeIndex: &idx}
			}
		}
		return off3, nil

	case 0x02: // alias -- a new local type-sort index, but only when its sort
		// is type (0x03); readAlias itself discards the sort byte (its other
		// caller, the top-level alias section, reads it separately -- see
		// decodeAliasSection), so peek it here first. This decoder cannot
		// follow an alias target structurally from inside an instancetype
		// body (see resolveAlias), so the slot is recorded unresolved.
		if off < len(buf) && buf[off] == 0x03 {
			*localTypes = append(*localTypes, nil)
		}
		return readAlias(buf, off)

	case 0x00: // core:type -- a core func/module type defined inline in the
		// instance or component type. This is the CORE type-sort index space,
		// disjoint from the component type-sort space *localTypes tracks, so
		// it never contributes here. It carries no runtime obligation either
		// way (these type-only validation shapes are opaque tags -- see the
		// package doc); consume its bytes to stay synchronized.
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

// readInstancetypeDesc reads an instance type: vec(instancedecl), building
// Types (this instancetype's own nested type-sort index space) and Exports
// via readInstanceDeclDescInto. See InstanceDesc's doc for why a TypeRef in
// Exports indexes Types, not any enclosing component's TypeSpace.
func readInstancetypeDesc(buf []byte, off int) (InstanceDesc, int, error) {
	count, n, err := leb128.LoadUint32(buf[off:])
	if err != nil {
		return InstanceDesc{}, off, err
	}
	off += int(n)

	var localTypes []TypeDesc
	var exports map[string]TypeRef
	// Every declaration consumes at least one byte, and only some of them
	// produce a local type, so both count and the bytes left are upper bounds
	// -- take the smaller so a bogus LEB128 count cannot make this allocate
	// before the read that would reject it.
	if cap := min(int(count), len(buf)-off); cap > 0 {
		localTypes = make([]TypeDesc, 0, cap)
	}
	for i := range count {
		off, err = readInstanceDeclDescInto(buf, off, &localTypes, &exports)
		if err != nil {
			return InstanceDesc{}, off, fmt.Errorf("instancedecl[%d]: %w", i, err)
		}
	}
	return InstanceDesc{Exports: exports, Types: localTypes}, off, nil
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
	case tag == 0x43: // async func: same grammar as 0x40, marked async.
		d, off, err := readFunctypeDesc(buf, off)
		d.Async = true
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
