package binary

import (
	"reflect"
	"strings"
	"testing"
)

// A type an imported instance re-exports from an `alias outer 1 N` resolves to this
// component's type N: wit-component encodes WIT's `use iface.{T}` that way (issue #102).
func TestResolveType_InstanceTypeOuterAlias(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/outer_alias_future.wasm")
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	want := EnumDesc{Cases: []string{"io", "pipe"}}
	for _, idx := range []uint32{1, 3} {
		got, err := c.ResolveType(idx)
		if err != nil {
			t.Fatalf("ResolveType(%d): %v", idx, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ResolveType(%d) = %#v, want %#v", idx, got, want)
		}
	}
}

// A type a nested component aliases out of a component around it holds indices of that
// component's type space; resolved in the nested one, each must still name the type it named
// there, not the nested component's own type at that index. Decode resolves them all, so the
// nested components are frozen and resolving them again interns nothing.
func TestResolveType_OuterAliasRemapsNestedIndices(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/outer_alias_nested.wasm")
	if err != nil {
		t.Fatal(err)
	}
	root, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	child := root.NestedComponents[0]
	grandchild := child.NestedComponents[0]
	enum := EnumDesc{Cases: []string{"a", "b", "c"}}

	resolve := func(c *Component, ref TypeRef) TypeDesc {
		t.Helper()
		if ref.TypeIndex == nil {
			return PrimitiveDesc{Prim: ref.Primitive}
		}
		td, err := c.ResolveType(*ref.TypeIndex)
		if err != nil {
			t.Fatalf("ResolveType(%d): %v", *ref.TypeIndex, err)
		}
		return td
	}
	idx := func(i uint32) TypeRef { return TypeRef{TypeIndex: &i} }
	checkRecord := func(c *Component, td TypeDesc) {
		t.Helper()
		rec, ok := td.(RecordDesc)
		if !ok || len(rec.Fields) != 2 {
			t.Fatalf("got %#v, want the two-field record", td)
		}
		if got := resolve(c, rec.Fields[0].Type); !reflect.DeepEqual(got, enum) {
			t.Fatalf("field x = %#v, want %#v", got, enum)
		}
		if got := resolve(c, rec.Fields[1].Type); !reflect.DeepEqual(got, PrimitiveDesc{Prim: "u8"}) {
			t.Fatalf("field y = %#v, want u8", got)
		}
	}

	checkRecord(child, resolve(child, idx(2)))
	checkRecord(grandchild, resolve(grandchild, idx(1)))

	fn, ok := resolve(child, idx(3)).(FuncDesc)
	if !ok || len(fn.Params) != 1 || fn.Results.Unnamed == nil {
		t.Fatalf("type 3 = %#v, want func(r: record) -> enum", fn)
	}
	checkRecord(child, resolve(child, fn.Params[0].Type))
	if got := resolve(child, *fn.Results.Unnamed); !reflect.DeepEqual(got, enum) {
		t.Fatalf("result = %#v, want %#v", got, enum)
	}

	// An imported instance type's `alias outer 2` names the root's enum.
	if got := resolve(child, idx(5)); !reflect.DeepEqual(got, enum) {
		t.Fatalf("type 5 = %#v, want %#v", got, enum)
	}

	// An instance type is read in the space it is declared in, so it comes through as it is.
	if got, want := resolve(child, idx(6)), resolve(root, idx(3)); !reflect.DeepEqual(got, want) {
		t.Fatalf("type 6 = %#v, want the root's instance type %#v", got, want)
	}

	for name, c := range map[string]*Component{"child": child, "grandchild": grandchild} {
		if !c.typesFrozen {
			t.Fatalf("%s is not frozen", name)
		}
		before := len(c.extraTypes)
		for i := range c.TypeSpace {
			if _, err := c.ResolveType(uint32(i)); err != nil {
				t.Fatalf("%s ResolveType(%d): %v", name, i, err)
			}
		}
		if len(c.extraTypes) != before {
			t.Fatalf("%s: resolving its types again interned %d more", name, len(c.extraTypes)-before)
		}
	}
}

// Outer aliases the validator rejects fail to resolve rather than hand back an index of another
// component's space, and leave the component unfrozen, so they fail the same way every time.
func TestResolveType_OuterAliasRefusals(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/outer_alias_refused.wasm")
	if err != nil {
		t.Fatal(err)
	}
	root, err := DecodeBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		child int
		idx   uint32
		want  string
	}{
		{0, 0, "own handle"},
		{0, 2, "out of range"},
		{1, 1, "own handle"},
	} {
		c := root.NestedComponents[tc.child]
		if c.typesFrozen {
			t.Fatalf("child %d, with an unresolvable alias, is frozen", tc.child)
		}
		_, err := c.ResolveType(tc.idx)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("child %d ResolveType(%d) = %v, want an error mentioning %q", tc.child, tc.idx, err, tc.want)
		}
	}
	got, err := root.NestedComponents[0].ResolveType(1)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := root.ResolveType(0); !reflect.DeepEqual(got, want) {
		t.Fatalf("type 1 = %#v, want the root's resource %#v", got, want)
	}
}
