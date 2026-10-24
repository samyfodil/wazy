package binary

import "fmt"

// maxValueSize bounds every value type: Binary.md requires that, for every
// defvaltype t, elem_size(t, 'i64') -- its Canonical ABI size with 64-bit
// pointers -- is less than 2^28. Only a fixed-length list can come near it,
// but a list of them, a record of them, or a list of strings sized for i32
// can cross it too.
const maxValueSize = 1 << 28

// layout64 is a value type's Canonical ABI size and alignment with 64-bit
// pointers. known is false when the type names something this decoder does
// not resolve structurally (see ResolveType), which is then not checked.
type layout64 struct {
	size, align uint64
	known       bool
}

// validateValueSizes refuses a component that defines a value type of
// maxValueSize bytes or more: one of its own, or one an instance type it
// defines declares.
func (c *Component) validateValueSizes() error {
	s := sizer{resolve: c.ResolveType, memo: map[uint32]layout64{}}
	for i, td := range c.Types {
		if err := s.check(td.Descriptor, fmt.Sprintf("type[%d]", i)); err != nil {
			return err
		}
		inst, ok := td.Descriptor.(InstanceDesc)
		if !ok {
			continue
		}
		local := sizer{resolve: func(idx uint32) (TypeDesc, error) {
			if int(idx) < len(inst.Types) && inst.Types[idx] != nil {
				return inst.Types[idx], nil
			}
			return nil, fmt.Errorf("local type %d is not resolved", idx)
		}, memo: map[uint32]layout64{}}
		for j, d := range inst.Types {
			if err := local.check(d, fmt.Sprintf("type[%d]: instance type[%d]", i, j)); err != nil {
				return err
			}
		}
	}
	return nil
}

// sizer computes layouts over one type index space.
type sizer struct {
	resolve func(uint32) (TypeDesc, error)
	memo    map[uint32]layout64
}

func (s sizer) check(d TypeDesc, where string) error {
	if l := s.layoutOf(d, 0); l.known && l.size >= maxValueSize {
		return fmt.Errorf("type section: %s: %s of %d bytes exceeds maximum byte size of %d", where, d.Kind(), l.size, uint64(maxValueSize-1))
	}
	return nil
}

// maxLayoutSize is where a size stops growing: far past maxValueSize, and
// far from wrapping.
const maxLayoutSize = 1 << 62

// maxLayoutDepth bounds the walk, as the decoder's other type walks are
// bounded: a cycle, which a well-formed component cannot have, ends there.
const maxLayoutDepth = 128

func (s sizer) layoutRef(ref *TypeRef, depth int) layout64 {
	if ref.Primitive != "" {
		return primLayout64(ref.Primitive)
	}
	if ref.TypeIndex == nil || depth > maxLayoutDepth {
		return layout64{}
	}
	if l, ok := s.memo[*ref.TypeIndex]; ok {
		return l
	}
	d, err := s.resolve(*ref.TypeIndex)
	if err != nil {
		return layout64{}
	}
	l := s.layoutOf(d, depth+1)
	s.memo[*ref.TypeIndex] = l
	return l
}

// layoutOf mirrors the Canonical ABI's elem_size and alignment for ptr_type
// i64. A type that is not a value type (a func, a resource, an instance) is
// not known: there is nothing to check.
func (s sizer) layoutOf(d TypeDesc, depth int) layout64 {
	switch t := d.(type) {
	case PrimitiveDesc:
		return primLayout64(t.Prim)
	case ListDesc, MapDesc:
		return layout64{16, 8, true}
	case FixedListDesc:
		e := s.layoutRef(&t.Element, depth)
		size := e.size * uint64(t.Length)
		if e.size != 0 && size/e.size != uint64(t.Length) {
			size = maxLayoutSize
		}
		return layout64{min(size, maxLayoutSize), e.align, e.known}
	case RecordDesc:
		refs := make([]*TypeRef, len(t.Fields))
		for i := range t.Fields {
			refs[i] = &t.Fields[i].Type
		}
		return s.recordLayout(refs, depth)
	case TupleDesc:
		refs := make([]*TypeRef, len(t.Elements))
		for i := range t.Elements {
			refs[i] = &t.Elements[i]
		}
		return s.recordLayout(refs, depth)
	case VariantDesc:
		refs := make([]*TypeRef, len(t.Cases))
		for i := range t.Cases {
			refs[i] = t.Cases[i].Type
		}
		return s.variantLayout(refs, depth)
	case EnumDesc:
		return s.variantLayout(make([]*TypeRef, len(t.Cases)), depth)
	case OptionDesc:
		return s.variantLayout([]*TypeRef{nil, &t.Element}, depth)
	case ResultDesc:
		return s.variantLayout([]*TypeRef{t.Ok, t.Err}, depth)
	case FlagsDesc:
		switch n := len(t.Names); {
		case n <= 8:
			return layout64{1, 1, true}
		case n <= 16:
			return layout64{2, 2, true}
		default:
			return layout64{4 * uint64((n+31)/32), 4, true}
		}
	case OwnDesc, BorrowDesc, StreamDesc, FutureDesc:
		return layout64{4, 4, true}
	}
	return layout64{}
}

func primLayout64(prim string) layout64 {
	switch prim {
	case "bool", "s8", "u8":
		return layout64{1, 1, true}
	case "s16", "u16":
		return layout64{2, 2, true}
	case "s32", "u32", "f32", "char", "error-context":
		return layout64{4, 4, true}
	case "s64", "u64", "f64":
		return layout64{8, 8, true}
	case "string":
		return layout64{16, 8, true}
	}
	return layout64{}
}

func alignTo(n, align uint64) uint64 { return (n + align - 1) / align * align }

// recordLayout lays fields out in order, each at its alignment.
func (s sizer) recordLayout(fields []*TypeRef, depth int) layout64 {
	out := layout64{0, 1, true}
	for _, f := range fields {
		l := s.layoutRef(f, depth)
		if !l.known {
			return layout64{}
		}
		out.size = min(alignTo(out.size, l.align)+l.size, maxLayoutSize)
		out.align = max(out.align, l.align)
	}
	out.size = alignTo(out.size, out.align)
	return out
}

// variantLayout is a discriminant sized for the case count, then the widest
// case at the widest case's alignment. A nil case carries no payload.
func (s sizer) variantLayout(cases []*TypeRef, depth int) layout64 {
	disc := uint64(1)
	switch n := len(cases); {
	case n > 1<<16:
		disc = 4
	case n > 1<<8:
		disc = 2
	}
	out := layout64{0, disc, true}
	for _, ref := range cases {
		if ref == nil {
			continue
		}
		l := s.layoutRef(ref, depth)
		if !l.known {
			return layout64{}
		}
		out.size = max(out.size, l.size)
		out.align = max(out.align, l.align)
	}
	out.size = alignTo(alignTo(disc, out.align)+out.size, out.align)
	return out
}
