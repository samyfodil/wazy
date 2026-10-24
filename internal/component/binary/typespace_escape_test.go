package binary

import (
	"bytes"
	"strings"
	"testing"
)

// escapeIdxBytes is extraTypeBase (2^30) as the signed-LEB128 int33 a valtype
// type index is encoded with.
var escapeIdxBytes = []byte{0x80, 0x80, 0x80, 0x80, 0x04}

// resolveTypeDepth routes anything >= extraTypeBase into extraTypes BEFORE it
// bounds-checks TypeSpace, so a type index forged into that range would
// resolve to an unrelated type this Component interned internally (or, with an
// empty table, produce an error about an "extra type table" the file never
// mentioned). Every valtype index is read at one site; reject it there.
func TestDecode_RejectsEscapeRangeValtype(t *testing.T) {
	optionBody := append([]byte{0x6b}, escapeIdxBytes...) // option<2^30>
	raw := synthComponent(synthSection(7, append([]byte{0x01}, optionBody...)))

	_, err := Decode(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("Decode accepted a valtype naming the reserved escape range")
	}
	if !strings.Contains(err.Error(), "reserved for internal use") {
		t.Fatalf("error does not name the reserved range: %v", err)
	}
}

// The top-level type indices -- read by their own leb128 calls, not through
// readValTypeRef -- reach resolveTypeDepth just as directly.
func TestValidateFileTypeIndices(t *testing.T) {
	const bad = extraTypeBase + 3

	for _, tc := range []struct {
		name string
		c    *Component
	}{
		{"type import", &Component{Imports: []Import{{Name: "t", ExternType: 0x03, ExternIndex: bad}}}},
		{"instance import", &Component{Imports: []Import{{Name: "i", ExternType: 0x05, ExternIndex: bad}}}},
		{"eq bound", &Component{Imports: []Import{{Name: "t", ExternType: 0x03, TypeEqBound: true, TypeEqIndex: bad}}}},
		{"type export", &Component{Exports: []Export{{Name: "t", ExternType: 0x03, ExternIndex: bad}}}},
		{"outer alias", &Component{Aliases: []AliasDef{{Sort: 0x03, TargetKind: 0x02, OuterIndex: bad}}}},
		{"inline export", &Component{Instances: []Instance{{Kind: 0x01, Exports: []InlineExport{{Name: "t", Sort: 0x03, SortIdx: bad}}}}}},
		{"canon", &Component{Canons: []Canon{{TypeIdx: bad}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.validateFileTypeIndices(); err == nil {
				t.Fatal("accepted an index in the reserved escape range")
			}
		})
	}

	// And the same fields at a legitimate index must still pass.
	ok := &Component{
		Imports:   []Import{{Name: "t", ExternType: 0x03, ExternIndex: 0, TypeEqBound: true, TypeEqIndex: 1}},
		Exports:   []Export{{Name: "t", ExternType: 0x03, ExternIndex: 2}},
		Aliases:   []AliasDef{{Sort: 0x03, TargetKind: 0x02, OuterIndex: 3}},
		Instances: []Instance{{Kind: 0x01, Exports: []InlineExport{{Name: "t", Sort: 0x03, SortIdx: 4}}}},
		Canons:    []Canon{{TypeIdx: 5}},
	}
	if err := ok.validateFileTypeIndices(); err != nil {
		t.Fatalf("rejected ordinary indices: %v", err)
	}
}

// An `eq N`-bound type-sort export inside an instancetype IS structurally
// resolvable -- it is local type N -- but its own new local index used to get
// a nil placeholder, so a LATER declaration in the same body that named the
// export (rather than N) failed as "not resolved structurally". That is the
// exact failure this PR exists to remove, one level down.
func TestDecode_InstancetypeExportEqIsUsableLater(t *testing.T) {
	// The instancetype body:
	//   local 0: option<string>
	//   local 1: export "e" -> eq local 0     (the slot that used to be nil)
	//   local 2: option<local 1>              (names the EXPORT, not local 0)
	//   local 3: export "wrapper" -> eq local 2
	body := []byte{0x04}                                              // 4 instancedecls
	body = append(body, synthInstanceDeclType([]byte{0x6b, 0x73})...) // option<string>
	body = append(body, synthInstanceDeclExportEq("e", 0)...)
	body = append(body, synthInstanceDeclType([]byte{0x6b, 0x01})...) // option<local 1>
	body = append(body, synthInstanceDeclExportEq("wrapper", 2)...)

	typeSec := synthSection(7, append([]byte{0x01, 0x42}, body...)) // 0x42 = instancetype
	importSec := synthSection(10, append([]byte{0x01}, synthInstanceImport("test:pkg/iface")...))
	aliasBody := append([]byte{0x01, 0x03, 0x00, 0x00}, synthLabel("wrapper")...)
	raw := synthComponent(typeSec, importSec, synthSection(6, aliasBody))

	c, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	td, err := c.ResolveType(1) // the `use iface.{wrapper}` alias
	if err != nil {
		t.Fatalf("ResolveType(1): %v", err)
	}
	outer, ok := td.(OptionDesc)
	if !ok {
		t.Fatalf("got %T, want OptionDesc", td)
	}
	if outer.Element.TypeIndex == nil {
		t.Fatalf("outer option's element is not a type index: %#v", outer.Element)
	}
	inner, err := c.ResolveType(*outer.Element.TypeIndex)
	if err != nil {
		t.Fatalf("resolving the element (the `eq`-exported type): %v", err)
	}
	innerOpt, ok := inner.(OptionDesc)
	if !ok {
		t.Fatalf("element is %T, want OptionDesc (option<string>)", inner)
	}
	if innerOpt.Element.Primitive != "string" {
		t.Fatalf("element is option<%s>, want option<string>", innerOpt.Element.Primitive)
	}
}
