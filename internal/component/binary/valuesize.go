package binary

import (
	"fmt"
	"sync"
)

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
// maxValueSize bytes or more: one of its own, or one an instance or
// component type it defines declares, at any depth.
//
// It runs on every decode, so it allocates little: the type index spaces it
// walks share one memo table and live in one slice, which the decodes after
// it reuse (sizeChecks), and where a type is declared is put into words only
// for an error.
func (c *Component) validateValueSizes() error {
	v := sizeChecks.Get().(*sizeCheck)
	defer v.release()
	v.comp = c
	top := v.push(sizeSpace{up: -1})
	for i := range c.Types {
		v.path = append(v.path, pathElem{"", i})
		if err := v.check(top, c.Types[i].Descriptor); err != nil {
			return err
		}
		if err := v.decls(c.Types[i].Descriptor, top); err != nil {
			return err
		}
		v.path = v.path[:0]
	}
	return nil
}

// sizeCheck computes layouts over the type index spaces of one component:
// its own, and those the instance and component types it declares open --
// each a sizeSpace, named by its position in spaces.
type sizeCheck struct {
	comp   *Component
	spaces []sizeSpace
	memo   map[sizeKey]layout64
	path   []pathElem
	nextID int
}

// sizeChecks keeps sizeChecks between decodes. The walk is recursive, so
// escape analysis puts a sizeCheck's slices on the heap however they start
// out; reusing them is what keeps a decode from allocating them.
var sizeChecks = sync.Pool{New: func() any { return new(sizeCheck) }}

// release empties v -- of everything that would keep the component it
// checked alive, too -- and returns it to sizeChecks.
func (v *sizeCheck) release() {
	clear(v.spaces[:cap(v.spaces)])
	clear(v.memo)
	*v = sizeCheck{spaces: v.spaces[:0], memo: v.memo, path: v.path[:0]}
	sizeChecks.Put(v)
}

// sizeSpace is one type index space: the component's (local false), or the
// one an instance or component type declares -- types, and outer for the
// indices it aliases from the spaces around it, up being the next one out.
// id tells it apart in the memo from a space that held its position before.
type sizeSpace struct {
	id    int
	local bool
	types []TypeDesc
	outer map[uint32]OuterAlias
	up    int
}

type sizeKey struct {
	space int
	idx   uint32
}

// pathElem is one step of where a type is: `type[3]` (kind empty), then
// `instance type[1]` inside the instance type it declares.
type pathElem struct {
	kind string
	idx  int
}

// push adds a space, returning its position; pop drops it and every space
// pushed after it.
func (v *sizeCheck) push(s sizeSpace) int {
	s.id = v.nextID
	v.nextID++
	v.spaces = append(v.spaces, s)
	return len(v.spaces) - 1
}

func (v *sizeCheck) pop(sp int) { v.spaces = v.spaces[:sp] }

// decls checks the types an instance or component type declares, in its own
// type index space, opened inside space enclosing.
func (v *sizeCheck) decls(d TypeDesc, enclosing int) error {
	var types []TypeDesc
	var outer map[uint32]OuterAlias
	switch t := d.(type) {
	case InstanceDesc:
		types, outer = t.Types, t.Outer
	case ComponentDesc:
		types, outer = t.Types, t.Outer
	default:
		return nil
	}
	sp := v.push(sizeSpace{local: true, types: types, outer: outer, up: enclosing})
	defer v.pop(sp)
	depth := len(v.path)
	for j, td := range types {
		v.path = append(v.path[:depth], pathElem{d.Kind(), j})
		if err := v.check(sp, td); err != nil {
			return err
		}
		if err := v.decls(td, sp); err != nil {
			return err
		}
	}
	v.path = v.path[:depth]
	return nil
}

func (v *sizeCheck) check(sp int, d TypeDesc) error {
	if l := v.layoutOf(sp, d, 0); l.known && l.size >= maxValueSize {
		where := ""
		for i, p := range v.path {
			if i > 0 {
				where += ": "
			}
			if p.kind != "" {
				where += p.kind + " "
			}
			where += fmt.Sprintf("type[%d]", p.idx)
		}
		return fmt.Errorf("type section: %s: %s of %d bytes exceeds maximum byte size of %d", where, d.Kind(), l.size, uint64(maxValueSize-1))
	}
	return nil
}

// resolve is type idx of space sp, when it resolves structurally.
func (v *sizeCheck) resolve(sp int, idx uint32) (TypeDesc, bool) {
	s := &v.spaces[sp]
	if s.local {
		if int(idx) < len(s.types) && s.types[idx] != nil {
			return s.types[idx], true
		}
		return nil, false
	}
	d, err := v.comp.ResolveType(idx)
	return d, err == nil
}

// fallback sizes an index resolve cannot: in the component's space, a type
// an imported instance exports; in a type's own space, one it aliases from
// an enclosing space.
func (v *sizeCheck) fallback(sp int, idx uint32, depth int) layout64 {
	s := &v.spaces[sp]
	if !s.local {
		return v.importedExportLayout(sp, idx, depth)
	}
	o, ok := s.outer[idx]
	if !ok {
		return layout64{}
	}
	space := sp
	for range o.Count {
		if space = v.spaces[space].up; space < 0 {
			return layout64{}
		}
	}
	return v.layoutRef(space, &TypeRef{TypeIndex: &o.Index}, depth)
}

// importedExportLayout sizes type idx of the component's space sp when it is
// an alias of a type an imported instance exports, which ResolveType does
// not follow when the instance type itself aliases it from the component.
func (v *sizeCheck) importedExportLayout(sp int, idx uint32, depth int) layout64 {
	c := v.comp
	if int(idx) >= len(c.TypeSpace) || c.TypeSpace[idx].Kind != TypeSpaceAlias {
		return layout64{}
	}
	al := c.Aliases[c.TypeSpace[idx].Alias]
	if al.TargetKind != 0x00 {
		return layout64{}
	}
	inst, ok := c.importedInstanceType(al.InstanceIdx, c.ResolveType)
	exp, exported := inst.Exports[al.Name]
	if !ok || !exported {
		return layout64{}
	}
	local := v.push(sizeSpace{local: true, types: inst.Types, outer: inst.Outer, up: sp})
	defer v.pop(local)
	return v.layoutRef(local, &exp, depth)
}

// maxLayoutSize is where a size stops growing: far past maxValueSize, and
// far from wrapping.
const maxLayoutSize = 1 << 62

// maxLayoutDepth bounds the walk, as the decoder's other type walks are
// bounded: a cycle, which a well-formed component cannot have, ends there.
const maxLayoutDepth = 128

func (v *sizeCheck) layoutRef(sp int, ref *TypeRef, depth int) layout64 {
	if ref.Primitive != "" {
		return primLayout64(ref.Primitive)
	}
	if ref.TypeIndex == nil || depth > maxLayoutDepth {
		return layout64{}
	}
	key := sizeKey{v.spaces[sp].id, *ref.TypeIndex}
	if l, ok := v.memo[key]; ok {
		return l
	}
	var l layout64
	if d, ok := v.resolve(sp, *ref.TypeIndex); ok {
		l = v.layoutOf(sp, d, depth+1)
	} else {
		l = v.fallback(sp, *ref.TypeIndex, depth+1)
	}
	if v.memo == nil {
		v.memo = map[sizeKey]layout64{}
	}
	v.memo[key] = l
	return l
}

// layoutOf mirrors the Canonical ABI's elem_size and alignment for ptr_type
// i64. A type that is not a value type (a func, a resource, an instance) is
// not known: there is nothing to check.
func (v *sizeCheck) layoutOf(sp int, d TypeDesc, depth int) layout64 {
	switch t := d.(type) {
	case PrimitiveDesc:
		return primLayout64(t.Prim)
	case ListDesc, MapDesc:
		return layout64{16, 8, true}
	case FixedListDesc:
		e := v.layoutRef(sp, &t.Element, depth)
		size := e.size * uint64(t.Length)
		if e.size != 0 && size/e.size != uint64(t.Length) {
			size = maxLayoutSize
		}
		return layout64{min(size, maxLayoutSize), e.align, e.known}
	case RecordDesc:
		out := layout64{0, 1, true}
		for i := range t.Fields {
			out = v.field(sp, out, &t.Fields[i].Type, depth)
		}
		return recordEnd(out)
	case TupleDesc:
		out := layout64{0, 1, true}
		for i := range t.Elements {
			out = v.field(sp, out, &t.Elements[i], depth)
		}
		return recordEnd(out)
	case VariantDesc:
		disc := discSize(len(t.Cases))
		out := layout64{0, disc, true}
		for i := range t.Cases {
			out = v.payload(sp, out, t.Cases[i].Type, depth)
		}
		return variantEnd(disc, out)
	case EnumDesc:
		disc := discSize(len(t.Cases))
		return variantEnd(disc, layout64{0, disc, true})
	case OptionDesc:
		return variantEnd(1, v.payload(sp, layout64{0, 1, true}, &t.Element, depth))
	case ResultDesc:
		return variantEnd(1, v.payload(sp, v.payload(sp, layout64{0, 1, true}, t.Ok, depth), t.Err, depth))
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

// A record is laid out a field at a time, each at its alignment; a variant
// as a discriminant sized for its case count, then the widest payload at the
// widest payload's alignment. An unknown field or payload makes the whole
// unknown.

func (v *sizeCheck) field(sp int, out layout64, ref *TypeRef, depth int) layout64 {
	if !out.known {
		return out
	}
	l := v.layoutRef(sp, ref, depth)
	if !l.known {
		return layout64{}
	}
	return layout64{min(alignTo(out.size, l.align)+l.size, maxLayoutSize), max(out.align, l.align), true}
}

func recordEnd(l layout64) layout64 {
	if !l.known {
		return layout64{}
	}
	return layout64{alignTo(l.size, l.align), l.align, true}
}

// discSize is the size of the discriminant of a variant of n cases.
func discSize(n int) uint64 {
	switch {
	case n > 1<<16:
		return 4
	case n > 1<<8:
		return 2
	}
	return 1
}

// payload widens a variant's widest payload (out.size) and alignment for
// one case's; a nil case carries none.
func (v *sizeCheck) payload(sp int, out layout64, ref *TypeRef, depth int) layout64 {
	if ref == nil || !out.known {
		return out
	}
	l := v.layoutRef(sp, ref, depth)
	if !l.known {
		return layout64{}
	}
	return layout64{max(out.size, l.size), max(out.align, l.align), true}
}

// variantEnd places the widest payload after a disc-byte discriminant.
func variantEnd(disc uint64, l layout64) layout64 {
	if !l.known {
		return layout64{}
	}
	return layout64{alignTo(alignTo(disc, l.align)+l.size, l.align), l.align, true}
}
