package binary

import (
	"strings"
	"testing"
)

// A fixed-length list is defvaltype 0x67: its element valtype, then its
// length as a u32 LEB128 (Binary.md: "0x67 t:<valtype> len:<u32> =>
// (list t len) (if len > 0)"). The bodies here are what `wasm-tools dump`
// shows after the tag.
func TestReadDefvaltypeDesc_FixedList(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  []byte
		want FixedListDesc
	}{
		{"list<u32, 4>", []byte{0x79, 0x04}, FixedListDesc{Element: TypeRef{Primitive: "u32"}, Length: 4}},
		{"list<string, 300>", []byte{0x73, 0xac, 0x02}, FixedListDesc{Element: TypeRef{Primitive: "string"}, Length: 300}},
		{"list<u8, 2^32-1>", []byte{0x7d, 0xff, 0xff, 0xff, 0xff, 0x0f}, FixedListDesc{Element: TypeRef{Primitive: "u8"}, Length: 1<<32 - 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, off, err := readDefvaltypeDesc(tc.buf, 0, 0x67)
			if err != nil {
				t.Fatal(err)
			}
			if off != len(tc.buf) {
				t.Errorf("consumed %d bytes, want %d", off, len(tc.buf))
			}
			if desc != tc.want || desc.Kind() != "fixed-length-list" {
				t.Errorf("got %#v (%s)", desc, desc.Kind())
			}
		})
	}

	// An element named by type index, as a list of records is.
	desc, _, err := readDefvaltypeDesc([]byte{0x05, 0x02}, 0, 0x67)
	if fl, ok := desc.(FixedListDesc); err != nil || !ok || fl.Element.TypeIndex == nil || *fl.Element.TypeIndex != 5 || fl.Length != 2 {
		t.Errorf("by index: %#v, %v", desc, err)
	}
}

func TestReadDefvaltypeDesc_FixedListErrors(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		buf        []byte
	}{
		{"no element", "EOF", nil},
		{"no length", "fixed-length list: length", []byte{0x79}},
		{"length overflows u32", "fixed-length list: length", []byte{0x79, 0xff, 0xff, 0xff, 0xff, 0x7f}},
		// Binary.md: "(if len > 0)"; wasm-tools: "Fixed-length lists must
		// have more than zero elements".
		{"empty", "fixed-length lists must have more than zero elements", []byte{0x79, 0x00}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readDefvaltypeDesc(tc.buf, 0, 0x67)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}
