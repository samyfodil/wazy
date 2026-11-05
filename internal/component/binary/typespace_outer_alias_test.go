package binary

import (
	"reflect"
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
