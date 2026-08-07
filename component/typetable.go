package component

import (
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// TypeTable interns the composite types one host func's signature refers to,
// handing back a TypeRef for each. It exists because a WIT type is a graph,
// not a tree: `list<list<u8>>` needs the inner `list<u8>` to be nameable from
// the outer one, and a TypeRef names it by index into exactly this table.
//
// One table per func signature. Indices are local to it, and the Resolver it
// produces is what the engine uses to walk back from a TypeRef to a
// descriptor, so a FuncDesc and the table it was built from must be passed to
// WithImportCustom together.
//
// The Add method is the primitive; the sugar methods below (List, Option,
// Result, Tuple, Record, Variant) cover the shapes that actually occur and
// keep TypeRef out of caller code:
//
//	tbl := component.NewTypeTable()
//	fd := tbl.Func(
//		[]component.TypeRef{tbl.Record("name", component.Prim("string"), "count", component.Prim("u32"))},
//		tbl.Result(tbl.List(component.Prim("string")), component.Prim("u32")),
//	)
//	opt := component.WithImportCustom("acme:api/host@1.0.0", "lookup", fn, fd, tbl.Resolver())
//
// A TypeTable is not safe for concurrent use; build a signature on one
// goroutine, then it is read-only.
type TypeTable struct {
	entries []binary.TypeDesc
}

// NewTypeTable returns an empty TypeTable.
func NewTypeTable() *TypeTable { return &TypeTable{} }

// Add interns td and returns the TypeRef naming it. A primitive is returned
// inline (it needs no table slot), so Add(PrimitiveDesc{...}) and Prim(...)
// are interchangeable.
func (t *TypeTable) Add(td TypeDesc) TypeRef {
	if p, ok := td.(binary.PrimitiveDesc); ok {
		return TypeRef{Primitive: p.Prim}
	}
	idx := uint32(len(t.entries))
	t.entries = append(t.entries, td)
	return TypeRef{TypeIndex: &idx}
}

// Resolver returns the Resolver over the table's current entries. Call it
// after the signature is fully built.
func (t *TypeTable) Resolver() Resolver {
	return func(idx uint32) binary.TypeDesc {
		if int(idx) >= len(t.entries) {
			return nil
		}
		return t.entries[idx]
	}
}

// List interns list<elem>.
func (t *TypeTable) List(elem TypeRef) TypeRef {
	return t.Add(binary.ListDesc{Element: elem})
}

// Option interns option<elem>.
func (t *TypeTable) Option(elem TypeRef) TypeRef {
	return t.Add(binary.OptionDesc{Element: elem})
}

// Result interns result<ok, err>. Pass the zero TypeRef for an arm the WIT
// declares without a type: Result(ok, TypeRef{}) is `result<ok>`, and
// Result(TypeRef{}, TypeRef{}) is a bare `result`.
func (t *TypeTable) Result(ok, err TypeRef) TypeRef {
	d := binary.ResultDesc{}
	if ok != (TypeRef{}) {
		okCopy := ok
		d.Ok = &okCopy
	}
	if err != (TypeRef{}) {
		errCopy := err
		d.Err = &errCopy
	}
	return t.Add(d)
}

// Tuple interns tuple<elems...>.
func (t *TypeTable) Tuple(elems ...TypeRef) TypeRef {
	return t.Add(binary.TupleDesc{Elements: elems})
}

// Record interns a record from alternating name/type pairs:
// Record("port", Prim("u16"), "host", Prim("string")). Panics on an odd
// number of arguments or a non-string in a name position, since both are
// programmer errors in a signature that is built once at startup.
func (t *TypeTable) Record(nameTypePairs ...any) TypeRef {
	if len(nameTypePairs)%2 != 0 {
		panic("component: TypeTable.Record expects alternating name, type pairs")
	}
	fields := make([]binary.RecordField, 0, len(nameTypePairs)/2)
	for i := 0; i < len(nameTypePairs); i += 2 {
		name, ok := nameTypePairs[i].(string)
		if !ok {
			panic("component: TypeTable.Record: expected a field name string")
		}
		ref, ok := nameTypePairs[i+1].(TypeRef)
		if !ok {
			panic("component: TypeTable.Record: expected a TypeRef for field " + name)
		}
		fields = append(fields, binary.RecordField{Name: name, Type: ref})
	}
	return t.Add(binary.RecordDesc{Fields: fields})
}

// Variant interns a variant. Each case is a name and an optional payload;
// pass the zero TypeRef for a case that carries none. Case order is the
// discriminant order a VariantValue's Disc indexes into.
func (t *TypeTable) Variant(cases ...VariantCaseSpec) TypeRef {
	out := make([]binary.VariantCase, 0, len(cases))
	for _, c := range cases {
		vc := binary.VariantCase{Name: c.Name}
		if c.Type != (TypeRef{}) {
			ref := c.Type
			vc.Type = &ref
		}
		out = append(out, vc)
	}
	return t.Add(binary.VariantDesc{Cases: out})
}

// VariantCaseSpec is one case for TypeTable.Variant: a name, and a payload
// type or the zero TypeRef for none.
type VariantCaseSpec struct {
	Name string
	Type TypeRef
}

// Enum interns an enum of the named cases. A value of it is the case index
// as a uint32.
func (t *TypeTable) Enum(cases ...string) TypeRef {
	return t.Add(binary.EnumDesc{Cases: cases})
}

// Flags interns a flags bitset of the named flags. A value of it is a uint32
// whose bits are set in declaration order.
func (t *TypeTable) Flags(names ...string) TypeRef {
	return t.Add(binary.FlagsDesc{Names: names})
}

// Own interns own<R> for the resource identified by tag -- the same tag
// passed to WithResourceTag and used when minting handles.
func (t *TypeTable) Own(tag uint32) TypeRef {
	return t.Add(binary.OwnDesc{ResourceType: tag})
}

// Borrow interns borrow<R> for the resource identified by tag.
func (t *TypeTable) Borrow(tag uint32) TypeRef {
	return t.Add(binary.BorrowDesc{ResourceType: tag})
}

// Func assembles a FuncDesc from params and a single unnamed result. Pass
// the zero TypeRef for a func that returns nothing.
func (t *TypeTable) Func(params []TypeRef, result TypeRef) FuncDesc {
	fd := binary.FuncDesc{}
	for i, p := range params {
		fd.Params = append(fd.Params, binary.FuncParam{Name: paramName(i), Type: p})
	}
	if result != (TypeRef{}) {
		r := result
		fd.Results.Unnamed = &r
	}
	return fd
}

// paramName generates the positional names a synthesized signature uses.
// Param names are not observable to the guest -- the Canonical ABI matches
// positionally -- so they exist only to make a FuncDesc readable.
func paramName(i int) string {
	return "p" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

// compile-time proof the alias and the internal resolver agree.
var _ abi.Resolver = (*TypeTable)(nil).Resolver()
