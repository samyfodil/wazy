package abi

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// FlatWidth exists only as a cheaper way to ask len(Flatten(...)). The moment
// the two disagree, a lift reads the wrong number of core values and the guest
// gets silently mangled data, so the contract is checked directly against
// Flatten over every shape rather than assumed.

// prim is a TypeRef for a primitive.
func prim(p string) binary.TypeRef { return binary.TypeRef{Primitive: p} }

// tableResolver interns descs and resolves them by index, standing in for a
// component's own type table.
type tableResolver struct{ entries []binary.TypeDesc }

func (r *tableResolver) add(d binary.TypeDesc) binary.TypeRef {
	idx := uint32(len(r.entries))
	r.entries = append(r.entries, d)
	return binary.TypeRef{TypeIndex: &idx}
}

func (r *tableResolver) resolver() Resolver {
	return func(i uint32) binary.TypeDesc {
		if int(i) >= len(r.entries) {
			return nil
		}
		return r.entries[i]
	}
}

// flatWidthCases builds a corpus covering every descriptor kind, each nested
// inside the composites that join rather than concatenate.
func flatWidthCases() (map[string]binary.TypeDesc, Resolver) {
	r := &tableResolver{}

	strRec := r.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: prim("string")},
		{Name: "b", Type: prim("u64")},
	}})
	wideRec := r.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "a", Type: prim("f64")},
		{Name: "b", Type: prim("string")},
		{Name: "c", Type: prim("s8")},
	}})

	cases := map[string]binary.TypeDesc{
		// Primitives: string is the one that is two slots wide.
		"bool":          binary.PrimitiveDesc{Prim: "bool"},
		"u8":            binary.PrimitiveDesc{Prim: "u8"},
		"s64":           binary.PrimitiveDesc{Prim: "s64"},
		"f32":           binary.PrimitiveDesc{Prim: "f32"},
		"f64":           binary.PrimitiveDesc{Prim: "f64"},
		"char":          binary.PrimitiveDesc{Prim: "char"},
		"string":        binary.PrimitiveDesc{Prim: "string"},
		"error-context": binary.PrimitiveDesc{Prim: "error-context"},

		"list":   binary.ListDesc{Element: prim("u8")},
		"enum":   binary.EnumDesc{Cases: []string{"a", "b", "c"}},
		"flags":  binary.FlagsDesc{Names: []string{"x", "y"}},
		"own":    binary.OwnDesc{ResourceType: 1},
		"borrow": binary.BorrowDesc{ResourceType: 1},
		// Async handles flatten as a bare i32 like own/borrow: the element
		// type they carry is not flattened.
		"stream": binary.StreamDesc{},
		"future": binary.FutureDesc{},

		"record":       binary.RecordDesc{Fields: []binary.RecordField{{Name: "a", Type: prim("u32")}}},
		"record empty": binary.RecordDesc{},
		"record nested": binary.RecordDesc{Fields: []binary.RecordField{
			{Name: "a", Type: strRec},
			{Name: "b", Type: wideRec},
		}},

		"tuple":       binary.TupleDesc{Elements: []binary.TypeRef{prim("u32"), prim("string")}},
		"tuple empty": binary.TupleDesc{},

		"option":        binary.OptionDesc{Element: prim("u32")},
		"option string": binary.OptionDesc{Element: prim("string")},
		"option record": binary.OptionDesc{Element: wideRec},

		// A variant whose cases are of different widths: the join is as wide
		// as the widest, which is the rule most easily got wrong.
		"variant mixed widths": binary.VariantDesc{Cases: []binary.VariantCase{
			{Name: "none"},
			{Name: "one", Type: refOf(prim("u32"))},
			{Name: "wide", Type: refOf(wideRec)},
		}},
		"variant all empty": binary.VariantDesc{Cases: []binary.VariantCase{
			{Name: "a"}, {Name: "b"},
		}},
		"variant one case": binary.VariantDesc{Cases: []binary.VariantCase{
			{Name: "only", Type: refOf(prim("f64"))},
		}},

		"result both":  binary.ResultDesc{Ok: refOf(prim("u32")), Err: refOf(prim("string"))},
		"result ok":    binary.ResultDesc{Ok: refOf(prim("u64"))},
		"result err":   binary.ResultDesc{Err: refOf(wideRec)},
		"result empty": binary.ResultDesc{},
		"result nested": binary.ResultDesc{
			Ok:  refOf(binary.TypeRef{TypeIndex: strRec.TypeIndex}),
			Err: refOf(prim("string")),
		},
	}

	// Deeply nested: the shape the wasi:http path actually carries.
	deep := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "opt", Type: r.add(binary.OptionDesc{Element: wideRec})},
		{Name: "res", Type: r.add(binary.ResultDesc{Ok: refOf(strRec), Err: refOf(prim("string"))})},
		{Name: "var", Type: r.add(binary.VariantDesc{Cases: []binary.VariantCase{
			{Name: "a", Type: refOf(prim("u32"))},
			{Name: "b", Type: refOf(wideRec)},
		}})},
	}}
	cases["deeply nested"] = deep

	return cases, r.resolver()
}

func refOf(r binary.TypeRef) *binary.TypeRef { return &r }

func TestFlatWidthMatchesFlatten(t *testing.T) {
	cases, resolve := flatWidthCases()

	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			flat, flatErr := Flatten(desc, resolve)
			width, widthErr := FlatWidth(desc, resolve)

			if (flatErr == nil) != (widthErr == nil) {
				t.Fatalf("Flatten err = %v but FlatWidth err = %v", flatErr, widthErr)
			}
			if flatErr != nil {
				return
			}
			if width != len(flat) {
				t.Fatalf("FlatWidth = %d, len(Flatten) = %d (%v)", width, len(flat), flat)
			}
		})
	}
}

// The unsupported and malformed descriptors have to fail the same way, since a
// caller that swapped Flatten for FlatWidth must not start accepting a type the
// other rejects.
func TestFlatWidthErrorsMatchFlatten(t *testing.T) {
	_, resolve := flatWidthCases()

	bad := map[string]binary.TypeDesc{
		"func":              binary.FuncDesc{},
		"instance":          binary.InstanceDesc{},
		"component":         binary.ComponentDesc{},
		"resource":          binary.ResourceDesc{},
		"unknown primitive": binary.PrimitiveDesc{Prim: "not-a-primitive"},
		"flags none":        binary.FlagsDesc{},
		"flags too many":    binary.FlagsDesc{Names: make([]string, 33)},
		"enum none":         binary.EnumDesc{},
		"record bad field": binary.RecordDesc{Fields: []binary.RecordField{
			{Name: "a", Type: prim("nope")},
		}},
		"option bad element":  binary.OptionDesc{Element: prim("nope")},
		"variant bad payload": binary.VariantDesc{Cases: []binary.VariantCase{{Name: "a", Type: refOf(prim("nope"))}}},
		"result bad ok":       binary.ResultDesc{Ok: refOf(prim("nope"))},
		"result bad err":      binary.ResultDesc{Err: refOf(prim("nope"))},
		"tuple bad element":   binary.TupleDesc{Elements: []binary.TypeRef{prim("nope")}},
	}

	for name, desc := range bad {
		t.Run(name, func(t *testing.T) {
			_, flatErr := Flatten(desc, resolve)
			_, widthErr := FlatWidth(desc, resolve)

			if flatErr == nil {
				t.Fatalf("expected Flatten to reject %s", name)
			}
			if widthErr == nil {
				t.Fatalf("Flatten rejected %s but FlatWidth accepted it", name)
			}
		})
	}
}

// FlatWidth is only worth having if it does not allocate; that is the entire
// reason it exists.
func TestFlatWidthDoesNotAllocate(t *testing.T) {
	cases, resolve := flatWidthCases()

	// Every shape, not one: the flags and enum arms reach different helpers
	// than the rest, so a single deep case would leave them unproven -- and
	// they did in fact allocate until those helpers stopped returning fresh
	// literals.
	for name, desc := range cases {
		t.Run(name, func(t *testing.T) {
			// Warm any lazily built state so it is not counted.
			if _, err := FlatWidth(desc, resolve); err != nil {
				t.Fatal(err)
			}
			allocs := testing.AllocsPerRun(100, func() {
				if _, err := FlatWidth(desc, resolve); err != nil {
					t.Fatal(err)
				}
			})
			if allocs != 0 {
				t.Errorf("FlatWidth allocated %v objects per call, want 0", allocs)
			}
		})
	}

	desc := cases["deeply nested"]

	// For contrast, and to document why this function exists at all.
	flatAllocs := testing.AllocsPerRun(100, func() {
		if _, err := Flatten(desc, resolve); err != nil {
			t.Fatal(err)
		}
	})
	if flatAllocs == 0 {
		t.Skip("Flatten no longer allocates; FlatWidth may be redundant")
	}
	t.Logf("Flatten allocates %v objects per call for the same shape; FlatWidth 0", flatAllocs)
}
