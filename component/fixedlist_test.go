package component_test

import (
	"context"
	"slices"
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/componenttest"
)

// TypeTable.FixedList registers a host import taking a list<u32, 4> and
// returning a list<string, 2>: the typed slice and the []Value shapes, read
// with ListOf.
func TestTypeTable_FixedList(t *testing.T) {
	const iface = "acme:api/host@1.0.0"

	tbl := component.NewTypeTable()
	quad := tbl.FixedList(component.Prim("u32"), 4)
	pair := tbl.FixedList(component.Prim("string"), 2)
	if d, ok := tbl.Resolver()(*quad.TypeIndex).(component.FixedListDesc); !ok || d.Length != 4 || d.Element.Primitive != "u32" {
		t.Fatalf("interned %#v", tbl.Resolver()(*quad.TypeIndex))
	}
	fd := tbl.Func([]component.TypeRef{quad}, pair)

	sum := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		v, err := component.ListOf[uint32](args[0])
		if err != nil {
			return nil, err
		}
		names := []component.Value{"n" + string(rune('0'+len(v))), "last"}
		return []component.Value{names}, nil
	}
	h := componenttest.New(component.WithImportCustom(iface, "sum", sum, fd, tbl.Resolver()))
	fn := h.MustFunc(iface, "sum")

	for _, in := range []component.Value{
		[]uint32{1, 2, 3, 4},
		[]component.Value{uint32(1), uint32(2), uint32(3), uint32(4)},
	} {
		got, err := fn(context.Background(), []component.Value{in})
		if err != nil {
			t.Fatalf("sum(%v): %v", in, err)
		}
		names, err := component.ListOf[string](got[0])
		if err != nil || !slices.Equal(names, []string{"n4", "last"}) {
			t.Fatalf("sum(%v) = %v, %v", in, got, err)
		}
	}
}

func TestTypeTable_FixedListOfNone(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic for a fixed-length list of 0")
		}
	}()
	component.NewTypeTable().FixedList(component.Prim("u8"), 0)
}
