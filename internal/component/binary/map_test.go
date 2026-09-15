package binary

import "testing"

// map<K,V> (defvaltype tag 0x63, key valtype, value valtype) is a newer
// Component Model addition without a committed test fixture yet (see
// TestRichComponent_Descriptors' rich_component.wasm for the established
// per-type-fixture pattern); this exercises the decoder directly against a
// hand-built defvaltype body, the same shape `wasm-tools dump` would show for
// `63 73 79` -- Map(Primitive(String), Primitive(U32)).
func TestReadDefvaltypeDesc_Map(t *testing.T) {
	buf := []byte{0x73, 0x79} // key: string, value: u32
	desc, off, err := readDefvaltypeDesc(buf, 0, 0x63)
	if err != nil {
		t.Fatalf("readDefvaltypeDesc: %v", err)
	}
	if off != len(buf) {
		t.Errorf("consumed %d bytes, want %d", off, len(buf))
	}
	m, ok := desc.(MapDesc)
	if !ok {
		t.Fatalf("descriptor: got %T, want MapDesc", desc)
	}
	if m.Key.Primitive != "string" {
		t.Errorf("key: got %q, want string", m.Key.Primitive)
	}
	if m.Value.Primitive != "u32" {
		t.Errorf("value: got %q, want u32", m.Value.Primitive)
	}
	if m.Kind() != "map" {
		t.Errorf("Kind(): got %q, want map", m.Kind())
	}
}

// A map value type (not just its key) can itself be a composite, referenced
// by type index -- e.g. map<string, list<u8>> where the list lives in an
// earlier type-table slot.
func TestReadDefvaltypeDesc_MapValueByTypeIndex(t *testing.T) {
	buf := []byte{0x73, 0x05} // key: string, value: type index 5
	desc, off, err := readDefvaltypeDesc(buf, 0, 0x63)
	if err != nil {
		t.Fatalf("readDefvaltypeDesc: %v", err)
	}
	if off != len(buf) {
		t.Errorf("consumed %d bytes, want %d", off, len(buf))
	}
	m, ok := desc.(MapDesc)
	if !ok {
		t.Fatalf("descriptor: got %T, want MapDesc", desc)
	}
	if m.Value.TypeIndex == nil || *m.Value.TypeIndex != 5 {
		t.Errorf("value type index: got %v, want 5", m.Value.TypeIndex)
	}
}

// A truncated map body (missing the value valtype) is a decode error, not a
// silent mis-walk -- see readDefvaltypeDesc's doc on M1's "loud failure"
// design rule.
func TestReadDefvaltypeDesc_MapTruncated(t *testing.T) {
	buf := []byte{0x73} // key only, value valtype missing
	if _, _, err := readDefvaltypeDesc(buf, 0, 0x63); err == nil {
		t.Fatal("expected an error for a truncated map body")
	}
}

// readDeftypeDesc's own dispatch (the entry point real decoding uses) must
// route tag 0x63 to the map path -- TestReadDefvaltypeDesc_Map above only
// proves the leaf function once dispatched there.
func TestReadDeftypeDesc_Map(t *testing.T) {
	buf := []byte{0x63, 0x73, 0x79} // deftype tag + key + value
	desc, off, err := readDeftypeDesc(buf, 0)
	if err != nil {
		t.Fatalf("readDeftypeDesc: %v", err)
	}
	if off != len(buf) {
		t.Errorf("consumed %d bytes, want %d", off, len(buf))
	}
	if _, ok := desc.(MapDesc); !ok {
		t.Fatalf("descriptor: got %T, want MapDesc", desc)
	}
}
