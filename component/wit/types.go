package wit

import (
	"fmt"

	"github.com/samyfodil/wazy/component"
	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// builder lowers WIT types into one component.TypeTable -- the table a single
// func's Signature hands to WithImportCustom.
type builder struct {
	tbl   *component.TypeTable
	names map[uint32]string
	tags  Tags
	// probe builds without tags, for validating at parse time: a resource
	// with no tag gets 0 rather than an error.
	probe bool
	// memo interns each named type once per table, which is also what makes
	// a type used twice in one signature share a slot.
	memo map[typeKey]component.TypeRef
}

// typeKey names one named type: the scope defining it, its name, and for a
// resource whether it is an own or a borrow handle.
type typeKey struct {
	s      *scope
	name   string
	handle string
}

func newBuilder(tags Tags, probe bool) *builder {
	return &builder{
		tbl:   component.NewTypeTable(),
		names: map[uint32]string{},
		tags:  tags,
		probe: probe,
		memo:  map[typeKey]component.TypeRef{},
	}
}

func (b *builder) funcDesc(f *Func) (component.FuncDesc, error) {
	fd := component.FuncDesc{Async: f.fn.Async}
	if f.Kind == Method {
		self, err := b.handle(f.scope, f.Resource, "borrow")
		if err != nil {
			return fd, err
		}
		fd.Params = append(fd.Params, component.FuncParam{Name: "self", Type: self})
	}
	for i := range f.fn.Params {
		p := &f.fn.Params[i]
		ref, err := b.ref(f.scope, &p.Type)
		if err != nil {
			return fd, fmt.Errorf("param %s: %w", p.Name, err)
		}
		fd.Params = append(fd.Params, component.FuncParam{Name: p.Name, Type: ref})
	}
	var result component.TypeRef
	var err error
	switch {
	case f.fn.Result != nil:
		result, err = b.ref(f.scope, f.fn.Result)
	case f.Kind == Constructor:
		result, err = b.handle(f.scope, f.Resource, "own")
	default:
		return fd, nil
	}
	if err != nil {
		return fd, fmt.Errorf("result: %w", err)
	}
	fd.Results.Unnamed = &result
	return fd, nil
}

// ref lowers one type expression written in scope s.
func (b *builder) ref(s *scope, t *ast.TypeRef) (component.TypeRef, error) {
	switch t.Kind {
	case "bool", "s8", "s16", "s32", "s64", "u8", "u16", "u32", "u64",
		"f32", "f64", "char", "string", "error-context":
		return component.Prim(t.Kind), nil
	case "named":
		return b.named(s, t.Name)
	case "own", "borrow":
		return b.handle(s, t.Name, t.Kind)
	}
	// Every other kind is a composite over zero to two inner types.
	inner, err := b.opt(s, t.Inner)
	if err != nil {
		return component.TypeRef{}, err
	}
	switch t.Kind {
	case "list":
		switch {
		case !t.Fixed:
			return b.tbl.List(*inner), nil
		case t.Length == 0 && b.probe:
			// wit-parser reads list<T, 0>, so a parse does too ...
			return b.tbl.List(*inner), nil
		case t.Length == 0:
			// ... but the binary format, and so a Signature, refuses it.
			return component.TypeRef{}, fmt.Errorf("%s: a fixed-length list needs at least one element", t)
		}
		return b.tbl.FixedList(*inner, t.Length), nil
	case "option":
		return b.tbl.Option(*inner), nil
	case "future":
		return b.tbl.Add(component.FutureDesc{Element: inner}), nil
	case "stream":
		return b.tbl.Add(component.StreamDesc{Element: inner}), nil
	case "tuple":
		elems := make([]component.TypeRef, 0, len(t.Tuple))
		for _, e := range t.Tuple {
			ref, err := b.ref(s, e)
			if err != nil {
				return component.TypeRef{}, err
			}
			elems = append(elems, ref)
		}
		return b.tbl.Tuple(elems...), nil
	}
	second, err := b.opt(s, t.Inner2)
	if err != nil {
		return component.TypeRef{}, err
	}
	switch t.Kind {
	case "result":
		return b.tbl.Add(component.ResultDesc{Ok: inner, Err: second}), nil
	case "map":
		return b.tbl.Map(*inner, *second), nil
	}
	return component.TypeRef{}, fmt.Errorf("type kind %q is not supported", t.Kind)
}

// opt lowers an optional inner type; nil stays nil.
func (b *builder) opt(s *scope, t *ast.TypeRef) (*component.TypeRef, error) {
	if t == nil {
		return nil, nil
	}
	ref, err := b.ref(s, t)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// named lowers a reference to the type called name in scope s.
func (b *builder) named(s *scope, name string) (component.TypeRef, error) {
	ds, td, err := s.lookupType(name)
	if err != nil {
		return component.TypeRef{}, err
	}
	if _, ok := td.Type.(*ast.Resource); ok {
		// A resource named as a value type is an owned handle to it.
		return b.handle(ds, td.Name, "own")
	}
	key := typeKey{s: ds, name: td.Name}
	if ref, ok := b.memo[key]; ok {
		return ref, nil
	}
	var ref component.TypeRef
	switch body := td.Type.(type) {
	case *ast.TypeAlias:
		ref, err = b.ref(ds, &body.Target)
	case *ast.Record:
		fields := make([]component.RecordField, 0, len(body.Fields))
		for i := range body.Fields {
			fr, err := b.ref(ds, &body.Fields[i].Type)
			if err != nil {
				return component.TypeRef{}, fmt.Errorf("%s: type %s: field %s: %w", ds.where, td.Name, body.Fields[i].Name, err)
			}
			fields = append(fields, component.RecordField{Name: body.Fields[i].Name, Type: fr})
		}
		ref = b.tbl.Add(component.RecordDesc{Fields: fields})
	case *ast.Variant:
		cases := make([]component.VariantCase, 0, len(body.Cases))
		for _, c := range body.Cases {
			payload, err := b.opt(ds, c.Type)
			if err != nil {
				return component.TypeRef{}, fmt.Errorf("%s: type %s: case %s: %w", ds.where, td.Name, c.Name, err)
			}
			cases = append(cases, component.VariantCase{Name: c.Name, Type: payload})
		}
		ref = b.tbl.Add(component.VariantDesc{Cases: cases})
	case *ast.Enum:
		ref = b.tbl.Enum(body.Cases...)
	case *ast.Flags:
		ref = b.tbl.Flags(body.Flags...)
	default:
		err = fmt.Errorf("unsupported definition %T", body)
	}
	if err != nil {
		return component.TypeRef{}, fmt.Errorf("%s: type %s: %w", ds.where, td.Name, err)
	}
	b.memo[key] = ref
	b.name(ref, td.Name)
	return ref, nil
}

// handle lowers own<name> or borrow<name> in scope s; name is the resource or
// an alias of it.
func (b *builder) handle(s *scope, name, kind string) (component.TypeRef, error) {
	ds, td, err := s.lookupResource(name)
	if err != nil {
		return component.TypeRef{}, err
	}
	if _, ok := td.Type.(*ast.Resource); !ok {
		// check refuses such a handle in its own package; this is one
		// whose resource is another package's.
		return component.TypeRef{}, fmt.Errorf("%s<%s>: type used in a handle must be a resource", kind, name)
	}
	key := typeKey{s: ds, name: td.Name, handle: kind}
	if ref, ok := b.memo[key]; ok {
		return ref, nil
	}
	tag, ok := b.tags[ResourceKey{Interface: ds.iface, Name: td.Name}]
	if !ok && !b.probe {
		return component.TypeRef{}, fmt.Errorf("no tag for resource %s of %s", td.Name, ds.where)
	}
	var ref component.TypeRef
	if kind == "own" {
		ref = b.tbl.Own(tag)
	} else {
		ref = b.tbl.Borrow(tag)
	}
	b.memo[key] = ref
	b.name(ref, td.Name)
	return ref, nil
}

// name records the WIT name of the slot ref interns, unless the slot already
// has one: an alias of a named type keeps the defining name.
func (b *builder) name(ref component.TypeRef, name string) {
	if ref.TypeIndex == nil {
		return
	}
	if _, ok := b.names[*ref.TypeIndex]; !ok {
		b.names[*ref.TypeIndex] = name
	}
}
