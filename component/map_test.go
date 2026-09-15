package component_test

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/componenttest"
)

// TypeTable.Map registers a map<string,u32> host import taking counts and
// returning their total -- exercising the public sugar (TypeTable.Map,
// MapDesc) and MapOf together. componenttest.Harness calls the host func
// directly with already-lifted Values (see its doc), so this does not
// exercise guest memory -- that is internal/component/abi's map_test.go's
// job -- but it is the proof that an embedder outside this module can
// declare and read a map<K,V> host import using only the public API.
func TestTypeTable_Map(t *testing.T) {
	const iface = "acme:api/host@1.0.0"

	tbl := component.NewTypeTable()
	paramRef := tbl.Map(component.Prim("string"), component.Prim("u32"))
	fd := tbl.Func([]component.TypeRef{paramRef}, component.Prim("u32"))

	total := func(_ context.Context, args []component.Value) ([]component.Value, error) {
		counts, err := component.MapOf[string, uint32](args[0])
		if err != nil {
			return nil, err
		}
		var sum uint32
		for _, v := range counts {
			sum += v
		}
		return []component.Value{sum}, nil
	}

	h := componenttest.New(component.WithImportCustom(iface, "total", total, fd, tbl.Resolver()))
	fn := h.MustFunc(iface, "total")

	in := []component.Value{
		[]component.Value{"a", uint32(2)},
		[]component.Value{"b", uint32(3)},
		[]component.Value{"c", uint32(4)},
	}
	got, err := fn(context.Background(), []component.Value{in})
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if len(got) != 1 || got[0].(uint32) != 9 {
		t.Fatalf("total(a:2,b:3,c:4) = %v, want [9]", got)
	}
}

func TestMapOf(t *testing.T) {
	in := []component.Value{
		[]component.Value{"x", uint32(1)},
		[]component.Value{"y", uint32(2)},
	}
	got, err := component.MapOf[string, uint32](in)
	if err != nil {
		t.Fatalf("MapOf: %v", err)
	}
	if len(got) != 2 || got["x"] != 1 || got["y"] != 2 {
		t.Fatalf("got %v, want map[x:1 y:2]", got)
	}
}

// A scalar value arrives widened (a u16 as uint32), the same as a list
// element -- MapOf has to narrow it back for the caller's requested type.
func TestMapOfNarrowsWidenedScalars(t *testing.T) {
	in := []component.Value{
		[]component.Value{uint32(1), uint32(0xFFFF)},
	}
	got, err := component.MapOf[uint16, uint16](in)
	if err != nil {
		t.Fatalf("MapOf: %v", err)
	}
	if got[1] != 0xFFFF {
		t.Fatalf("got %v, want map[1:65535]", got)
	}
}

func TestMapOfErrors(t *testing.T) {
	t.Run("not a map", func(t *testing.T) {
		if _, err := component.MapOf[string, uint32](component.Value("nope")); err == nil {
			t.Error("expected an error for a non-map value")
		}
	})

	t.Run("entry not a pair", func(t *testing.T) {
		in := []component.Value{uint32(1)}
		if _, err := component.MapOf[string, uint32](in); err == nil {
			t.Error("expected an error for a non-pair entry")
		}
	})

	t.Run("key of an unconvertible type", func(t *testing.T) {
		in := []component.Value{[]component.Value{true, uint32(1)}}
		if _, err := component.MapOf[string, uint32](in); err == nil {
			t.Error("expected an error converting bool to string")
		}
	})
}
