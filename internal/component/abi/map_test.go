package abi

import (
	"reflect"
	"testing"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// map<K,V> has no Canonical ABI representation of its own: per
// despecialize() (design/mvp/CanonicalABI.md), it is byte-for-byte
// list<tuple<K,V>>. These tests hold MapDesc to that contract directly,
// including cross-checking it against the equivalent ListDesc{TupleDesc}
// wherever the two must agree exactly.

func mapListEquivalent() (binary.MapDesc, binary.ListDesc) {
	m := binary.MapDesc{Key: prim("string"), Value: prim("u32")}
	l := binary.ListDesc{Element: binary.TypeRef{}} // element unused by list's own size/align/flatten
	return m, l
}

func TestMapSizeAndAlignment(t *testing.T) {
	m, l := mapListEquivalent()

	size, err := Size(m, noResolver)
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	wantSize, err := Size(l, noResolver)
	if err != nil {
		t.Fatalf("Size(list): %v", err)
	}
	if size != wantSize {
		t.Errorf("Size: got %d, want %d (list<tuple<K,V>>'s)", size, wantSize)
	}
	if size != 8 {
		t.Errorf("Size: got %d, want 8 (ptr+len)", size)
	}

	align, err := Alignment(m, noResolver)
	if err != nil {
		t.Fatalf("Alignment: %v", err)
	}
	if align != 4 {
		t.Errorf("Alignment: got %d, want 4", align)
	}
}

func TestMapFlattenAndFlatWidth(t *testing.T) {
	m := binary.MapDesc{Key: prim("string"), Value: prim("u32")}

	flat, err := Flatten(m, noResolver)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if !reflect.DeepEqual(flat, []string{"i32", "i32"}) {
		t.Errorf("Flatten: got %v, want [i32 i32]", flat)
	}

	width, err := FlatWidth(m, noResolver)
	if err != nil {
		t.Fatalf("FlatWidth: %v", err)
	}
	if width != len(flat) {
		t.Errorf("FlatWidth: got %d, want %d (len(Flatten))", width, len(flat))
	}
}

// mapEntries is the Value shape for map<string,u32>{"a": 1, "bb": 2}: a
// []Value of two-element [key, value] pairs, positional like any
// list<tuple<K,V>>.
func mapEntries() Value {
	return []Value{
		[]Value{"a", uint32(1)},
		[]Value{"bb", uint32(2)},
	}
}

func TestMapStoreLoadRoundTrip(t *testing.T) {
	desc := binary.MapDesc{Key: prim("string"), Value: prim("u32")}
	mem := make([]byte, 4096)
	realloc := bumpRealloc(64)

	if err := Store(mem, 0, desc, mapEntries(), noResolver, realloc); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := Load(mem, 0, desc, noResolver)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, mapEntries()) {
		t.Errorf("round trip: got %#v, want %#v", got, mapEntries())
	}
}

// Storing a map and storing the equivalent list<tuple<K,V>> must produce
// identical bytes -- the whole point of despecialization is that there is no
// map-specific encoding to get subtly wrong.
func TestMapStoreMatchesListOfTuple(t *testing.T) {
	mapDesc := binary.MapDesc{Key: prim("string"), Value: prim("u32")}
	listDesc := binary.ListDesc{Element: binary.TypeRef{TypeIndex: refUint32(0)}}
	resolve := func(uint32) binary.TypeDesc {
		return binary.TupleDesc{Elements: []binary.TypeRef{prim("string"), prim("u32")}}
	}

	memMap := make([]byte, 4096)
	if err := Store(memMap, 0, mapDesc, mapEntries(), noResolver, bumpRealloc(64)); err != nil {
		t.Fatalf("Store map: %v", err)
	}

	memList := make([]byte, 4096)
	if err := Store(memList, 0, listDesc, mapEntries(), resolve, bumpRealloc(64)); err != nil {
		t.Fatalf("Store list<tuple>: %v", err)
	}

	if !reflect.DeepEqual(memMap, memList) {
		t.Error("map and the equivalent list<tuple<K,V>> stored different bytes")
	}
}

func refUint32(v uint32) *uint32 { return &v }

func TestMapLowerFlatLiftFlatRoundTrip(t *testing.T) {
	desc := binary.MapDesc{Key: prim("string"), Value: prim("u32")}
	mem := make([]byte, 4096)
	realloc := bumpRealloc(64)

	flat, err := LowerFlat(mapEntries(), desc, noResolver, realloc, mem)
	if err != nil {
		t.Fatalf("LowerFlat: %v", err)
	}
	if len(flat) != 2 {
		t.Fatalf("LowerFlat: got %d core values, want 2 (ptr, len)", len(flat))
	}

	got, err := LiftFlat(flat, desc, noResolver, mem)
	if err != nil {
		t.Fatalf("LiftFlat: %v", err)
	}
	if !reflect.DeepEqual(got, mapEntries()) {
		t.Errorf("round trip: got %#v, want %#v", got, mapEntries())
	}
}

func TestMapEmptyRoundTrip(t *testing.T) {
	desc := binary.MapDesc{Key: prim("string"), Value: prim("u32")}
	mem := make([]byte, 256)

	if err := Store(mem, 0, desc, []Value{}, noResolver, bumpRealloc(64)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load(mem, 0, desc, noResolver)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	list, ok := got.([]Value)
	if !ok || len(list) != 0 {
		t.Errorf("got %#v, want an empty []Value", got)
	}
}

// A map value nested inside a record (i.e. record{tags: map<string,u32>})
// exercises the resolver path (mapElemType is built from an already-resolved
// MapDesc, but the record field itself still resolves its own TypeRef).
func TestMapNestedInRecord(t *testing.T) {
	r := &tableResolver{}
	mapRef := r.add(binary.MapDesc{Key: prim("string"), Value: prim("u32")})
	rec := binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "tags", Type: mapRef},
	}}
	resolve := r.resolver()

	mem := make([]byte, 4096)
	val := []Value{mapEntries()}
	if err := Store(mem, 0, rec, val, resolve, bumpRealloc(64)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load(mem, 0, rec, resolve)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, val) {
		t.Errorf("got %#v, want %#v", got, val)
	}
}

// map<K, option<V>> -- an optional VALUE, per entry -- is the shape that
// actually motivated MapDesc's map-in-a-record/tuple handling: the map's
// synthetic tuple<K, option<V>> element must resolve its second field to an
// OptionDesc and let Option's own none/some machinery run, same as any other
// tuple field. Nothing map-specific is involved; this is the proof.
func TestMapWithOptionalValue(t *testing.T) {
	r := &tableResolver{}
	optRef := r.add(binary.OptionDesc{Element: prim("u32")})
	desc := binary.MapDesc{Key: prim("string"), Value: optRef}
	resolve := r.resolver()

	entries := []Value{
		[]Value{"a", uint32(1)}, // some(1)
		[]Value{"b", nil},       // none
	}

	mem := make([]byte, 4096)
	if err := Store(mem, 0, desc, entries, resolve, bumpRealloc(64)); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := Load(mem, 0, desc, resolve)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Errorf("got %#v, want %#v", got, entries)
	}

	// Same round trip through the flat ABI (LowerFlat/LiftFlat).
	mem2 := make([]byte, 4096)
	flat, err := LowerFlat(entries, desc, resolve, bumpRealloc(64), mem2)
	if err != nil {
		t.Fatalf("LowerFlat: %v", err)
	}
	got2, err := LiftFlat(flat, desc, resolve, mem2)
	if err != nil {
		t.Fatalf("LiftFlat: %v", err)
	}
	if !reflect.DeepEqual(got2, entries) {
		t.Errorf("flat round trip: got %#v, want %#v", got2, entries)
	}
}

// option<map<K,V>> is unremarkable at the Option level -- Option is generic
// over its element's Size/Alignment/Load/Store, so this is really a check
// that MapDesc plugs into that generic machinery cleanly, for both arms.
func TestMapInsideOption(t *testing.T) {
	r := &tableResolver{}
	mapRef := r.add(binary.MapDesc{Key: prim("string"), Value: prim("u32")})
	opt := binary.OptionDesc{Element: mapRef}
	resolve := r.resolver()

	t.Run("some", func(t *testing.T) {
		mem := make([]byte, 4096)
		if err := Store(mem, 0, opt, mapEntries(), resolve, bumpRealloc(64)); err != nil {
			t.Fatalf("Store: %v", err)
		}
		got, err := Load(mem, 0, opt, resolve)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !reflect.DeepEqual(got, mapEntries()) {
			t.Errorf("got %#v, want %#v", got, mapEntries())
		}
	})

	t.Run("none", func(t *testing.T) {
		mem := make([]byte, 4096)
		if err := Store(mem, 0, opt, nil, resolve, bumpRealloc(64)); err != nil {
			t.Fatalf("Store: %v", err)
		}
		got, err := Load(mem, 0, opt, resolve)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != nil {
			t.Errorf("got %#v, want nil (none)", got)
		}
	})
}
