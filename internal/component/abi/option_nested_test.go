package abi

import (
	"reflect"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// option<option<u32>> keeps none and some(none) apart, flat and in memory:
// lifting gives Some exactly where the payload is itself an option, and
// lowering takes Some or a bare value.
func TestNestedOption(t *testing.T) {
	inner := uint32(0)
	descs := []binary.TypeDesc{binary.OptionDesc{Element: binary.TypeRef{Primitive: "u32"}}}
	resolve := func(i uint32) binary.TypeDesc { return descs[i] }
	outer := binary.OptionDesc{Element: binary.TypeRef{TypeIndex: &inner}}

	for _, c := range []struct {
		name string
		in   Value
		want Value
		flat []uint32
		mem  []byte // discriminants: outer at 0, inner at 4
	}{
		{"none", nil, nil, []uint32{0, 0, 0}, []byte{0, 0}},
		{"some(none)", Some{nil}, Some{nil}, []uint32{1, 0, 0}, []byte{1, 0}},
		{"some(some(5))", Some{uint32(5)}, Some{uint32(5)}, []uint32{1, 1, 5}, []byte{1, 1}},
		{"bare some(some(5))", uint32(5), Some{uint32(5)}, []uint32{1, 1, 5}, []byte{1, 1}},
	} {
		vals, err := LowerFlat(c.in, outer, resolve, Realloc{}, nil)
		if err != nil {
			t.Fatalf("%s: LowerFlat: %v", c.name, err)
		}
		got := make([]uint32, len(vals))
		for i, v := range vals {
			got[i] = v.AsI32()
		}
		if !reflect.DeepEqual(got, c.flat) {
			t.Fatalf("%s: flat %v, want %v", c.name, got, c.flat)
		}
		lifted, err := LiftFlat(vals, outer, resolve, nil)
		if err != nil || !reflect.DeepEqual(lifted, c.want) {
			t.Fatalf("%s: LiftFlat = %#v, %v; want %#v", c.name, lifted, err, c.want)
		}

		mem := make([]byte, 16)
		if err := Store(mem, 0, outer, c.in, resolve, Realloc{}); err != nil {
			t.Fatalf("%s: Store: %v", c.name, err)
		}
		if mem[0] != c.mem[0] || (c.mem[0] == 1 && mem[4] != c.mem[1]) {
			t.Fatalf("%s: memory % x, want discriminants %v", c.name, mem[:12], c.mem)
		}
		loaded, err := Load(mem, 0, outer, resolve)
		if err != nil || !reflect.DeepEqual(loaded, c.want) {
			t.Fatalf("%s: Load = %#v, %v; want %#v", c.name, loaded, err, c.want)
		}
	}

	// A plain option still lifts as the bare value, and takes Some too.
	plain := descs[0]
	vals, err := LowerFlat(Some{uint32(7)}, plain, resolve, Realloc{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := LiftFlat(vals, plain, resolve, nil); err != nil || v != uint32(7) {
		t.Fatalf("option<u32> = %#v, %v; want the bare 7", v, err)
	}
	mem := make([]byte, 8)
	if err := Store(mem, 0, plain, Some{uint32(7)}, resolve, Realloc{}); err != nil {
		t.Fatal(err)
	}
	if v, err := Load(mem, 0, plain, resolve); err != nil || v != uint32(7) {
		t.Fatalf("option<u32> from memory = %#v, %v; want the bare 7", v, err)
	}
}
