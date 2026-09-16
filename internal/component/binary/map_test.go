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

// TestIsValidMapKeyPrimitive pins the exact keytype grammar (design/mvp/
// Explainer.md): every primvaltype is a valtype, but only a subset is a
// keytype -- notably f32/f64 (float equality/NaN) and error-context are
// excluded even though they decode via isPrimValtype like any other
// primitive.
func TestIsValidMapKeyPrimitive(t *testing.T) {
	valid := []string{"bool", "s8", "u8", "s16", "u16", "s32", "u32", "s64", "u64", "char", "string"}
	for _, p := range valid {
		if !IsValidMapKeyPrimitive(p) {
			t.Errorf("IsValidMapKeyPrimitive(%q) = false, want true", p)
		}
	}
	invalid := []string{"f32", "f64", "error-context", "", "u128", "record"}
	for _, p := range invalid {
		if IsValidMapKeyPrimitive(p) {
			t.Errorf("IsValidMapKeyPrimitive(%q) = true, want false", p)
		}
	}
}

// A map key outside the keytype grammar is a decode error -- both when it is
// an inline primitive the spec excludes (f32) and when it is a type index
// (which, per IsValidMapKeyPrimitive's doc, can never itself be a valid
// keytype: every keytype is always spelled inline).
func TestReadDefvaltypeDesc_MapInvalidKey(t *testing.T) {
	t.Run("excluded primitive (f32)", func(t *testing.T) {
		buf := []byte{0x76, 0x79} // key: f32, value: u32
		if _, _, err := readDefvaltypeDesc(buf, 0, 0x63); err == nil {
			t.Fatal("expected an error for map<f32, u32>")
		}
	})

	t.Run("excluded primitive (error-context)", func(t *testing.T) {
		buf := []byte{0x64, 0x79} // key: error-context, value: u32
		if _, _, err := readDefvaltypeDesc(buf, 0, 0x63); err == nil {
			t.Fatal("expected an error for map<error-context, u32>")
		}
	})

	t.Run("key by type index", func(t *testing.T) {
		buf := []byte{0x02, 0x79} // key: type index 2, value: u32
		if _, _, err := readDefvaltypeDesc(buf, 0, 0x63); err == nil {
			t.Fatal("expected an error for a map key given by type index")
		}
	})
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
