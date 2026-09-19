package binary

import (
	"strings"
	"testing"
)

// globalizeLocalTypeDesc has to rewrite every TypeRef nested in a TypeDesc --
// miss one kind and an instance-local index escapes into a descriptor handed
// to a Resolver caller, where it silently names some unrelated component type.
// So walk every TypeDesc kind: each is given one local reference, and the
// result must carry an escape index (>= extraTypeBase) in its place, never the
// local 0 it started as.
func TestGlobalizeLocalTypeDesc_EveryKind(t *testing.T) {
	local := func() *uint32 { i := uint32(0); return &i }
	ref := func() TypeRef { return TypeRef{TypeIndex: local()} }
	refp := func() *TypeRef { r := ref(); return &r }

	// refsOf returns every TypeRef reachable one level inside d.
	refsOf := func(d TypeDesc) []TypeRef {
		switch t := d.(type) {
		case ListDesc:
			return []TypeRef{t.Element}
		case OptionDesc:
			return []TypeRef{t.Element}
		case MapDesc:
			return []TypeRef{t.Key, t.Value}
		case TupleDesc:
			return t.Elements
		case RecordDesc:
			out := make([]TypeRef, len(t.Fields))
			for i, f := range t.Fields {
				out[i] = f.Type
			}
			return out
		case VariantDesc:
			var out []TypeRef
			for _, c := range t.Cases {
				if c.Type != nil {
					out = append(out, *c.Type)
				}
			}
			return out
		case ResultDesc:
			var out []TypeRef
			if t.Ok != nil {
				out = append(out, *t.Ok)
			}
			if t.Err != nil {
				out = append(out, *t.Err)
			}
			return out
		case StreamDesc:
			if t.Element != nil {
				return []TypeRef{*t.Element}
			}
		case FutureDesc:
			if t.Element != nil {
				return []TypeRef{*t.Element}
			}
		}
		return nil
	}

	for _, tc := range []struct {
		name string
		in   TypeDesc
		want int // TypeRefs expected to be rewritten
	}{
		{"list", ListDesc{Element: ref()}, 1},
		{"option", OptionDesc{Element: ref()}, 1},
		{"map", MapDesc{Key: ref(), Value: ref()}, 2},
		{"tuple", TupleDesc{Elements: []TypeRef{ref(), ref()}}, 2},
		{"record", RecordDesc{Fields: []RecordField{{Name: "a", Type: ref()}, {Name: "b", Type: ref()}}}, 2},
		{"variant", VariantDesc{Cases: []VariantCase{{Name: "some", Type: refp()}, {Name: "none"}}}, 1},
		{"result both", ResultDesc{Ok: refp(), Err: refp()}, 2},
		{"result neither", ResultDesc{}, 0},
		{"stream", StreamDesc{Element: refp()}, 1},
		{"stream bare", StreamDesc{}, 0},
		{"future", FutureDesc{Element: refp()}, 1},
		{"future bare", FutureDesc{}, 0},
		// Passed through unchanged: they hold no TypeRefs at all.
		{"primitive", PrimitiveDesc{Prim: "u32"}, 0},
		{"flags", FlagsDesc{Names: []string{"a"}}, 0},
		{"enum", EnumDesc{Cases: []string{"a"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Component{}
			localTypes := []TypeDesc{PrimitiveDesc{Prim: "u32"}}

			got, err := c.globalizeLocalTypeDesc(localTypes, tc.in, map[uint32]uint32{})
			if err != nil {
				t.Fatalf("globalizeLocalTypeDesc: %v", err)
			}
			refs := refsOf(got)
			if len(refs) != tc.want {
				t.Fatalf("got %d nested TypeRefs, want %d: %#v", len(refs), tc.want, got)
			}
			for i, r := range refs {
				if r.TypeIndex == nil {
					t.Errorf("ref %d was not rewritten to an index: %#v", i, r)
					continue
				}
				if *r.TypeIndex < extraTypeBase {
					t.Errorf("ref %d is still instance-local (%d), not an escape index", i, *r.TypeIndex)
				}
			}

			// Every rewritten ref must resolve back to what it named.
			for i, r := range refs {
				if r.TypeIndex == nil {
					continue
				}
				td, err := c.ResolveType(*r.TypeIndex)
				if err != nil {
					t.Fatalf("ref %d does not resolve: %v", i, err)
				}
				if td != (PrimitiveDesc{Prim: "u32"}) {
					t.Errorf("ref %d resolved to %#v, want u32", i, td)
				}
			}
		})
	}
}

// A nested ref this decoder cannot follow must surface as an error from every
// kind, not as a half-rewritten descriptor.
func TestGlobalizeLocalTypeDesc_PropagatesRefErrors(t *testing.T) {
	oob := func() TypeRef { i := uint32(99); return TypeRef{TypeIndex: &i} }
	oobp := func() *TypeRef { r := oob(); return &r }

	for _, tc := range []struct {
		name string
		in   TypeDesc
	}{
		{"list", ListDesc{Element: oob()}},
		{"option", OptionDesc{Element: oob()}},
		{"map key", MapDesc{Key: oob(), Value: TypeRef{Primitive: "u32"}}},
		{"map value", MapDesc{Key: TypeRef{Primitive: "u32"}, Value: oob()}},
		{"tuple", TupleDesc{Elements: []TypeRef{oob()}}},
		{"record", RecordDesc{Fields: []RecordField{{Name: "a", Type: oob()}}}},
		{"variant", VariantDesc{Cases: []VariantCase{{Name: "some", Type: oobp()}}}},
		{"result ok", ResultDesc{Ok: oobp()}},
		{"result err", ResultDesc{Err: oobp()}},
		{"stream", StreamDesc{Element: oobp()}},
		{"future", FutureDesc{Element: oobp()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Component{}
			if _, err := c.globalizeLocalTypeDesc([]TypeDesc{PrimitiveDesc{Prim: "u32"}}, tc.in, map[uint32]uint32{}); err == nil {
				t.Fatal("accepted an out-of-range local type index")
			}
		})
	}
}

// own/borrow carry a ResourceType that is an index into the COMPONENT's
// TypeSpace (composition.go reads it directly, never through a Resolver), so
// it cannot be globalized into the escape range -- and handing back the
// instance-local index instead would silently canonicalize the wrong resource.
// It has to fail. Likewise anything this decoder has no case for.
func TestGlobalizeLocalTypeDesc_RefusesHandlesAndUnknowns(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		in         TypeDesc
	}{
		{"own", "own handle", OwnDesc{ResourceType: 0}},
		{"borrow", "borrow handle", BorrowDesc{ResourceType: 0}},
		{"func", "locally-declared func type", FuncDesc{}},
		{"resource", "locally-declared resource type", ResourceDesc{}},
		{"instance", "locally-declared instance type", InstanceDesc{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Component{}
			_, err := c.globalizeLocalTypeDesc(nil, tc.in, map[uint32]uint32{})
			if err == nil {
				t.Fatalf("accepted a %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// globalizeLocalRef reserves its escape slot BEFORE recursing, so a type that
// refers to itself terminates on the memo instead of recursing forever.
func TestGlobalizeLocalRef_SelfReferentialTerminates(t *testing.T) {
	self := uint32(0)
	c := &Component{}
	localTypes := []TypeDesc{
		OptionDesc{Element: TypeRef{TypeIndex: &self}}, // option<itself>
	}

	got, err := c.globalizeLocalRef(localTypes, TypeRef{TypeIndex: &self}, map[uint32]uint32{})
	if err != nil {
		t.Fatalf("globalizeLocalRef: %v", err)
	}
	if got.TypeIndex == nil || *got.TypeIndex < extraTypeBase {
		t.Fatalf("got %#v, want an escape index", got)
	}
	td, err := c.ResolveType(*got.TypeIndex)
	if err != nil {
		t.Fatalf("ResolveType: %v", err)
	}
	opt, ok := td.(OptionDesc)
	if !ok {
		t.Fatalf("got %T, want OptionDesc", td)
	}
	// The cycle closes on the same escape index it started at.
	if opt.Element.TypeIndex == nil || *opt.Element.TypeIndex != *got.TypeIndex {
		t.Fatalf("element is %#v, want the same escape index %d", opt.Element, *got.TypeIndex)
	}
}
