package binary

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// Real fixtures (via wasm-tools)
// ---------------------------------------------------------------------

// TestExtendedComponentFixture verifies the descriptor model against
// extended_component.wasm, a real component assembled by wasm-tools from
// testdata/extended_component.wat. Ground truth (per `wasm-tools print`):
//
//	type0  record{a:bool,b:s8,c:u8,d:s16,e:u16,f:s32,g:u32,h:s64,i:u64,j:f32,k:f64,l:char,m:string}
//	type1  tuple<u32,string,bool>
//	type2  list<u8>              (nested list element, anonymous)
//	type3  list<2>  i.e. list<list<u8>>
//	type4  component{ import "x" func; export "y" func()->u32 }
//	import "test:pkg/comp" (component (type 4))
//	type5  resource (rep i32) (dtor)
//	type6  own<5>
//	type7  borrow<5>
//	type8  func(a:u32,b:u32,c:u32,d:u32,e:own<r>,f:borrow<r>)->u32
//	export "allprims" (type 0), "t" (type 1), "nested" (type 3),
//	       "r" (type 5), "many" (func)
func TestExtendedComponentFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "extended_component.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	wantKinds := []string{"record", "tuple", "list", "list", "component", "resource", "own", "borrow", "func"}
	if len(c.Types) != len(wantKinds) {
		t.Fatalf("types: got %d, want %d (%+v)", len(c.Types), len(wantKinds), c.Types)
	}
	for i, want := range wantKinds {
		if c.Types[i].Kind != want {
			t.Errorf("type[%d] kind: got %q, want %q", i, c.Types[i].Kind, want)
		}
	}

	// type0: record with all 13 primitives, in declaration order.
	rec, ok := c.Types[0].Descriptor.(RecordDesc)
	if !ok {
		t.Fatalf("type[0] descriptor: got %T, want RecordDesc", c.Types[0].Descriptor)
	}
	wantFields := []struct{ name, prim string }{
		{"a", "bool"}, {"b", "s8"}, {"c", "u8"}, {"d", "s16"}, {"e", "u16"},
		{"f", "s32"}, {"g", "u32"}, {"h", "s64"}, {"i", "u64"},
		{"j", "f32"}, {"k", "f64"}, {"l", "char"}, {"m", "string"},
	}
	if len(rec.Fields) != len(wantFields) {
		t.Fatalf("record fields: got %d, want %d", len(rec.Fields), len(wantFields))
	}
	for i, want := range wantFields {
		if rec.Fields[i].Name != want.name || rec.Fields[i].Type.Primitive != want.prim {
			t.Errorf("field[%d]: got {%q,%q}, want {%q,%q}", i, rec.Fields[i].Name, rec.Fields[i].Type.Primitive, want.name, want.prim)
		}
	}

	// type1: tuple<u32, string, bool>
	tup, ok := c.Types[1].Descriptor.(TupleDesc)
	if !ok {
		t.Fatalf("type[1] descriptor: got %T, want TupleDesc", c.Types[1].Descriptor)
	}
	wantTuple := []string{"u32", "string", "bool"}
	if len(tup.Elements) != len(wantTuple) {
		t.Fatalf("tuple elements: got %d, want %d", len(tup.Elements), len(wantTuple))
	}
	for i, want := range wantTuple {
		if tup.Elements[i].Primitive != want {
			t.Errorf("tuple[%d]: got %q, want %q", i, tup.Elements[i].Primitive, want)
		}
	}

	// type2: list<u8> (the inner, anonymous list referenced by type3).
	innerList, ok := c.Types[2].Descriptor.(ListDesc)
	if !ok || innerList.Element.Primitive != "u8" {
		t.Errorf("type[2]: got %+v, want ListDesc{u8}", c.Types[2].Descriptor)
	}

	// type3: list<list<u8>> -- the outer list's element is a TypeIndex referencing type2.
	outerList, ok := c.Types[3].Descriptor.(ListDesc)
	if !ok {
		t.Fatalf("type[3] descriptor: got %T, want ListDesc", c.Types[3].Descriptor)
	}
	if outerList.Element.TypeIndex == nil || *outerList.Element.TypeIndex != 2 {
		t.Errorf("nested list element: got %+v, want TypeIndex=2", outerList.Element)
	}

	// type4: component type (consumed structurally; not fully represented).
	if _, ok := c.Types[4].Descriptor.(ComponentDesc); !ok {
		t.Errorf("type[4] descriptor: got %T, want ComponentDesc", c.Types[4].Descriptor)
	}

	// type5: resource with a destructor.
	res, ok := c.Types[5].Descriptor.(ResourceDesc)
	if !ok {
		t.Fatalf("type[5] descriptor: got %T, want ResourceDesc", c.Types[5].Descriptor)
	}
	if res.Rep.Primitive != "bool" {
		// Per `wasm-tools print`, "(rep i32)" is on the wire as the raw
		// primvaltype byte 0x7f -- the same byte this grammar assigns to
		// "bool" -- since i32 is not itself a distinct primvaltype code;
		// resource reps are always encoded this way in the current spec.
		t.Errorf("resource rep: got %q, want bool (wire byte 0x7f)", res.Rep.Primitive)
	}
	if res.Dtor == nil {
		t.Error("resource dtor: got nil, want a destructor function index")
	}

	// type6/7: own<5>/borrow<5>.
	own, ok := c.Types[6].Descriptor.(OwnDesc)
	if !ok || own.ResourceType != 5 {
		t.Errorf("type[6]: got %+v, want OwnDesc{ResourceType:5}", c.Types[6].Descriptor)
	}
	borrow, ok := c.Types[7].Descriptor.(BorrowDesc)
	if !ok || borrow.ResourceType != 5 {
		t.Errorf("type[7]: got %+v, want BorrowDesc{ResourceType:5}", c.Types[7].Descriptor)
	}

	// type8: func with 6 params (4 plain u32, 1 own, 1 borrow), unnamed u32 result.
	fn, ok := c.Types[8].Descriptor.(FuncDesc)
	if !ok {
		t.Fatalf("type[8] descriptor: got %T, want FuncDesc", c.Types[8].Descriptor)
	}
	if len(fn.Params) != 6 {
		t.Fatalf("func params: got %d, want 6", len(fn.Params))
	}
	if fn.Params[4].Name != "e" || fn.Params[4].Type.TypeIndex == nil || *fn.Params[4].Type.TypeIndex != 6 {
		t.Errorf("param[4] (own): got %+v", fn.Params[4])
	}
	if fn.Params[5].Name != "f" || fn.Params[5].Type.TypeIndex == nil || *fn.Params[5].Type.TypeIndex != 7 {
		t.Errorf("param[5] (borrow): got %+v", fn.Params[5])
	}
	if fn.Results.Unnamed == nil || fn.Results.Unnamed.Primitive != "u32" {
		t.Errorf("func result: got %v, want u32", fn.Results.Unnamed)
	}

	// The single import is the component-type import.
	if len(c.Imports) != 1 || c.Imports[0].Name != "test:pkg/comp" || c.Imports[0].ExternType != 0x04 {
		t.Errorf("imports: got %+v, want one test:pkg/comp (component sort 0x04)", c.Imports)
	}

	// 5 exports: allprims, t, nested, r, many.
	wantExports := []string{"allprims", "t", "nested", "r", "many"}
	if len(c.Exports) != len(wantExports) {
		t.Fatalf("exports: got %d, want %d (%+v)", len(c.Exports), len(wantExports), c.Exports)
	}
	for i, want := range wantExports {
		if c.Exports[i].Name != want {
			t.Errorf("export[%d]: got %q, want %q", i, c.Exports[i].Name, want)
		}
	}
}

// TestResultVariantsFixture verifies all four shapes of the result type
// (bare, err-only, ok-only, both) against a real component assembled by
// wasm-tools from testdata/result_variants.wat.
func TestResultVariantsFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "result_variants.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Types) != 4 {
		t.Fatalf("types: got %d, want 4", len(c.Types))
	}

	bare, ok := c.Types[0].Descriptor.(ResultDesc)
	if !ok || bare.Ok != nil || bare.Err != nil {
		t.Errorf("type[0] bare result: got %+v, want {nil,nil}", c.Types[0].Descriptor)
	}

	errOnly, ok := c.Types[1].Descriptor.(ResultDesc)
	if !ok || errOnly.Ok != nil || errOnly.Err == nil || errOnly.Err.Primitive != "string" {
		t.Errorf("type[1] err-only result: got %+v", c.Types[1].Descriptor)
	}

	okOnly, ok := c.Types[2].Descriptor.(ResultDesc)
	if !ok || okOnly.Ok == nil || okOnly.Ok.Primitive != "u32" || okOnly.Err != nil {
		t.Errorf("type[2] ok-only result: got %+v", c.Types[2].Descriptor)
	}

	both, ok := c.Types[3].Descriptor.(ResultDesc)
	if !ok || both.Ok == nil || both.Ok.Primitive != "u32" || both.Err == nil || both.Err.Primitive != "string" {
		t.Errorf("type[3] both result: got %+v", c.Types[3].Descriptor)
	}

	if len(c.Exports) != 4 {
		t.Fatalf("exports: got %d, want 4", len(c.Exports))
	}
}

// ---------------------------------------------------------------------
// Dump() output assertions
// ---------------------------------------------------------------------

// TestDumpAllBranches exercises every branch of Component.Dump: the Types
// header, Imports, Exports, and the "Skipped Sections" branch (RawSections),
// plus every sectionIDName and externTypeName case (including their
// "Unknown"/"unknown" default branches).
func TestDumpAllBranches(t *testing.T) {
	c := &Component{
		Types: []Type{{Index: 0, Kind: "record"}},
		Imports: []Import{
			{Name: "imp-func", ExternType: 0x01, ExternIndex: 0},
			{Name: "imp-unknown", ExternType: 0xff, ExternIndex: 1},
		},
		Exports: []Export{
			{Name: "exp-value", ExternType: 0x02, ExternIndex: 0},
			{Name: "exp-unknown", ExternType: 0xfe, ExternIndex: 1},
		},
		RawSections: []RawSection{
			{ID: 0, Size: 3},
			{ID: 1, Size: 5},
			{ID: 2, Size: 1},
			{ID: 3, Size: 1},
			{ID: 4, Size: 1},
			{ID: 5, Size: 1},
			{ID: 6, Size: 1},
			{ID: 8, Size: 1},
			{ID: 9, Size: 1},
			{ID: 12, Size: 1},
			{ID: 200, Size: 1}, // triggers sectionIDName's "Unknown" default
		},
	}

	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Component Model Binary", "Types:", "record",
		"Imports:", "imp-func", "func", "imp-unknown", "unknown",
		"Exports:", "exp-value", "value", "exp-unknown", "unknown",
		"Skipped Sections:",
		"Custom", "CoreModule", "CoreInstance", "CoreType", "Component",
		"Instance", "Alias", "Canonical", "Start", "Value", "Unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump output missing %q\n---\n%s", want, out)
		}
	}
}

// TestDumpEmptyComponent verifies Dump() on a Component with no types,
// imports, exports, or raw sections: only the header is printed, none of
// the section-specific branches fire.
func TestDumpEmptyComponent(t *testing.T) {
	c := &Component{}
	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Component Model Binary") {
		t.Errorf("missing header: %s", out)
	}
	for _, notWant := range []string{"Types:", "Imports:", "Exports:", "Skipped Sections:"} {
		if strings.Contains(out, notWant) {
			t.Errorf("unexpected section header %q in empty dump: %s", notWant, out)
		}
	}
}

// TestDumpWriteError verifies Dump() propagates a write error from the
// underlying io.Writer (exercising every "if _, err := ...; err != nil"
// early-return branch, since a single always-failing writer will trip the
// very first write).
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestDumpWriteError(t *testing.T) {
	c := &Component{
		Types:       []Type{{Kind: "u32"}},
		Imports:     []Import{{Name: "x", ExternType: 0x01}},
		Exports:     []Export{{Name: "y", ExternType: 0x01}},
		RawSections: []RawSection{{ID: 0, Size: 1}},
	}
	if err := c.Dump(errWriter{}); err == nil {
		t.Fatal("expected Dump to propagate the writer error")
	}
}

// TestRawSectionRecordedForSkippedSection verifies that a genuinely unknown
// section id is recorded in RawSections and skipped without decoding, via a
// hand-crafted component with a single unknown section (id=200).
func TestRawSectionRecordedForSkippedSection(t *testing.T) {
	buf := preamble()
	buf = append(buf, 200, 0x02, 0xaa, 0xbb) // section id=200, size=2, body=[0xaa,0xbb]
	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.RawSections) != 1 || c.RawSections[0].ID != 200 || c.RawSections[0].Size != 2 {
		t.Errorf("RawSections: got %+v, want one {ID:200, Size:2}", c.RawSections)
	}
}

// ---------------------------------------------------------------------
// Byte-level error-path tests (hand-crafted, no wasm-tools needed)
// ---------------------------------------------------------------------

// preamble returns a valid, minimal component preamble (magic + version +
// layer) with no sections following.
func preamble() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}
}

func TestPreambleOnlyComponentIsValid(t *testing.T) {
	c, err := Decode(bytes.NewReader(preamble()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Types) != 0 || len(c.Imports) != 0 || len(c.Exports) != 0 || len(c.RawSections) != 0 {
		t.Errorf("expected an entirely empty component, got %+v", c)
	}
}

func TestPreambleTruncation(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		wantErr error
	}{
		{"empty", nil, ErrInvalidMagicNumber},
		{"one byte", []byte{0x00}, ErrInvalidMagicNumber},
		{"two bytes", []byte{0x00, 0x61}, ErrInvalidMagicNumber},
		{"three bytes", []byte{0x00, 0x61, 0x73}, ErrInvalidMagicNumber},
		{"wrong magic", []byte{0x01, 0x02, 0x03, 0x04}, ErrInvalidMagicNumber},
		{"magic only", []byte{0x00, 0x61, 0x73, 0x6d}, ErrInvalidVersion},
		{"magic plus one version byte", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d}, ErrInvalidVersion},
		{"core module version (0x01 0x00)", []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, ErrInvalidVersion},
		{"wrong version bytes", []byte{0x00, 0x61, 0x73, 0x6d, 0xff, 0xff}, ErrInvalidVersion},
		{"magic+version only", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00}, ErrInvalidLayer},
		{"magic+version plus one layer byte", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01}, ErrInvalidLayer},
		{"wrong layer bytes", []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x00, 0x00}, ErrInvalidLayer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.buf))
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("got err=%v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestSectionSizeTruncated(t *testing.T) {
	// Section id byte present, but the LEB128 size is entirely missing.
	buf := append(preamble(), 0x00)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: missing section size")
	}
}

func TestCustomSectionTruncatedBody(t *testing.T) {
	// Custom section (id=0) claims size 127 but supplies 0 bytes.
	buf := append(preamble(), 0x00, 0x7f)
	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestUnknownSectionTruncatedBody(t *testing.T) {
	// Unknown section (id=200) claims size 50 but supplies 0 bytes.
	buf := append(preamble(), 200, 50)
	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestTypeSectionTruncated(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"missing count", append(preamble(), 7, 0x00)},
		{"count present, no type body", append(preamble(), 7, 0x01, 0x01)},
		{"truncated mid-compound-type", append(preamble(), 7, 0x02, 0x01, 0x70)}, // count=1, list tag but no element byte
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.buf))
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestTypeSectionInvalidTag(t *testing.T) {
	// count=1, then an invalid deftype tag (0x00 is not a valid tag in this position).
	buf := append(preamble(), 7, 0x02, 0x01, 0x00)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error for invalid deftype tag")
	}
}

func TestTypeSectionOverrun(t *testing.T) {
	// count=1, one valid primitive type (u32, tag 0x79, 1 byte), but the
	// section claims size=1 even though reading the type consumes 2 bytes
	// total (count byte + tag byte) -- actually count is read first and not
	// counted toward sectionSize in this decoder's bookkeeping; use a
	// section size smaller than what decodeTypeSection actually reads to
	// trigger the over-run check.
	buf := append(preamble(), 7, 0x02, 0x01, 0x79) // section claims 2 bytes: count(1) + tag(1) -- exactly matches, so make it under-claim
	buf[len(buf)-3] = 0x01                         // shrink claimed section size to 1 (only the count byte)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: type section read more bytes than its claimed size")
	}
}

func TestImportSectionTruncated(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"missing count", append(preamble(), 10, 0x00)},
		{"count present, no name", append(preamble(), 10, 0x01, 0x01)},
		{"name present, no externdesc", append(preamble(), 10, 0x03, 0x01, 0x00, 0x00, 0x00)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.buf))
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestImportSectionOverrun(t *testing.T) {
	// One import: name kind=0x00, len=1, "x" (1 byte), externdesc sort=0x01
	// (func) typeidx=0 -- 6 bytes total after the count. Claim section size
	// 1 byte less than that to trigger the over-run check.
	body := []byte{0x01, 0x00, 0x01, 'x', 0x01, 0x00}
	buf := append(preamble(), 10, byte(len(body)-1))
	buf = append(buf, body...)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: import section read more bytes than its claimed size")
	}
}

func TestExportSectionTruncated(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"missing count", append(preamble(), 11, 0x00)},
		{"count present, no name", append(preamble(), 11, 0x01, 0x01)},
		{"name present, no sortidx", append(preamble(), 11, 0x03, 0x01, 0x00, 0x00, 0x00)},
		{"sortidx present, no ascription tag", append(preamble(), 11, 0x06, 0x01, 0x00, 0x01, 'x', 0x01, 0x00)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.buf))
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestExportSectionInvalidAscriptionTag(t *testing.T) {
	// One export: name kind=0x00 len=1 "x", sortidx sort=0x01(func) idx=0,
	// ascription tag=0x02 (invalid: must be 0x00 or 0x01).
	body := []byte{0x01, 0x00, 0x01, 'x', 0x01, 0x00, 0x02}
	buf := append(preamble(), 11, byte(len(body)))
	buf = append(buf, body...)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error for invalid export type-ascription tag")
	}
}

func TestExportSectionWithAscribedType(t *testing.T) {
	// One export: name "x", sortidx func/0, ascription tag=0x01 followed by
	// a valid externdesc (sort=0x01 func, typeidx=0). Exercises the
	// "hasType == 0x01" branch that reads the ascribed externdesc.
	body := []byte{0x01, 0x00, 0x01, 'x', 0x01, 0x00, 0x01, 0x01, 0x00}
	buf := append(preamble(), 11, byte(len(body)))
	buf = append(buf, body...)
	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Exports) != 1 || c.Exports[0].Name != "x" {
		t.Errorf("exports: got %+v", c.Exports)
	}
}

func TestExportSectionAscribedTypeError(t *testing.T) {
	// name "x", sortidx func/0, ascription tag=0x01 followed by an invalid
	// externdesc sort byte (0xff), exercising the ascribed-type error
	// propagation branch distinct from a successfully-decoded ascription.
	body := []byte{0x01, 0x00, 0x01, 'x', 0x01, 0x00, 0x01, 0xff}
	buf := append(preamble(), 11, byte(len(body)))
	buf = append(buf, body...)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: invalid ascribed externdesc")
	}
}

func TestExportSectionOverrun(t *testing.T) {
	body := []byte{0x01, 0x00, 0x01, 'x', 0x01, 0x00, 0x00} // name+sortidx+ascription(none)
	buf := append(preamble(), 11, byte(len(body)-1))        // under-claim by 1 byte
	buf = append(buf, body...)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: export section read more bytes than its claimed size")
	}
}

func TestSectionSizeMismatch(t *testing.T) {
	// A custom section that claims a larger size than what its dispatch
	// consumes is impossible (custom sections always consume exactly
	// sectionSize by construction), so exercise the exact-match check via
	// a type section that reads *fewer* bytes than claimed: count=1, a
	// 1-byte primitive type, but the section claims size=3.
	buf := append(preamble(), 7, 0x03, 0x01, 0x79)
	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error: section size mismatch (claimed 3, type section only consumed 2)")
	}
}

// ---------------------------------------------------------------------
// Extern name / externdesc / sort / alias byte-level error paths
// ---------------------------------------------------------------------

func TestReadExternNameAttributes(t *testing.T) {
	// Kind byte 0x02 (name + attributes) is explicitly unsupported (M1).
	buf := []byte{0x02}
	_, _, err := readExternName(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "unsupported (M1)") {
		t.Errorf("got err=%v, want unsupported (M1) extern name attributes error", err)
	}
}

func TestReadExternNameInvalidKind(t *testing.T) {
	buf := []byte{0xff}
	_, _, err := readExternName(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid kind byte") {
		t.Errorf("got err=%v, want invalid kind byte error", err)
	}
}

func TestReadExternNameTruncated(t *testing.T) {
	_, _, err := readExternName(nil, 0)
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestPrimNameUnrecognizedByte(t *testing.T) {
	// primName's default branch is only reachable by calling it with a
	// byte outside 0x73..0x7f directly (every caller in this package
	// already gates on isPrimValtype first).
	got := primName(0x00)
	if got != "prim(0x0)" {
		t.Errorf("primName(0x00) = %q, want %q", got, "prim(0x0)")
	}
}

func TestReadExterndescSorts(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		wantErr bool
	}{
		{"core module (0x00 0x11)", []byte{0x00, 0x11, 0x00}, false},
		{"core module missing 0x11 prefix", []byte{0x00, 0x22, 0x00}, true},
		{"core module truncated after sort", []byte{0x00}, true},
		{"core module truncated typeidx", []byte{0x00, 0x11}, true},
		{"func (0x01)", []byte{0x01, 0x00}, false},
		{"component (0x04)", []byte{0x04, 0x00}, false},
		{"instance (0x05)", []byte{0x05, 0x00}, false},
		{"type bound eq (0x03 0x00)", []byte{0x03, 0x00, 0x00}, false},
		{"type bound sub (0x03 0x01)", []byte{0x03, 0x01}, false},
		{"type bound truncated", []byte{0x03}, true},
		{"type bound eq truncated index", []byte{0x03, 0x00}, true},
		{"unsupported sort (value, 0x02)", []byte{0x02}, true},
		{"unknown sort", []byte{0xff}, true},
		{"empty buffer", nil, true},
		{"typeidx truncated (func)", []byte{0x01}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := readExterndesc(tt.buf, 0)
			if (err != nil) != tt.wantErr {
				t.Errorf("readExterndesc(%v) err=%v, wantErr=%v", tt.buf, err, tt.wantErr)
			}
		})
	}
}

func TestReadSort(t *testing.T) {
	tests := []struct {
		name    string
		buf     []byte
		wantOff int
		wantErr bool
	}{
		{"core sort", []byte{0x00, 0x01}, 2, false},
		{"core sort truncated", []byte{0x00}, 0, true},
		{"non-core sort", []byte{0x01}, 1, false},
		{"empty", nil, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			off, err := readSort(tt.buf, 0)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}
			if err == nil && off != tt.wantOff {
				t.Errorf("off=%d, want %d", off, tt.wantOff)
			}
		})
	}
}

func TestReadAlias(t *testing.T) {
	t.Run("core export target", func(t *testing.T) {
		// sort=core(0x00)+discriminator(0x01=func), target=0x00 (export),
		// instance idx=0x00, name len=1 "x".
		buf := []byte{0x00, 0x01, 0x00, 0x00, 0x01, 'x'}
		off, err := readAlias(buf, 0)
		if err != nil {
			t.Fatalf("readAlias: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("outer target", func(t *testing.T) {
		// sort=type(0x03)+bound eq(0x00)+typeidx(0x00), target=0x02
		// (outer), outer-count=0x01, index=0x00.
		buf := []byte{0x03, 0x00, 0x00, 0x02, 0x01, 0x00}
		off, err := readAlias(buf, 0)
		if err != nil {
			t.Fatalf("readAlias: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("invalid target kind", func(t *testing.T) {
		buf := []byte{0x01, 0xff}
		_, err := readAlias(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "invalid target kind") {
			t.Errorf("got err=%v, want invalid target kind error", err)
		}
	})

	t.Run("truncated before target", func(t *testing.T) {
		buf := []byte{0x01}
		_, err := readAlias(buf, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})

	t.Run("export target truncated instance idx", func(t *testing.T) {
		buf := []byte{0x01, 0x00}
		_, err := readAlias(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("export target truncated name", func(t *testing.T) {
		buf := []byte{0x01, 0x00, 0x00}
		_, err := readAlias(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("outer target truncated", func(t *testing.T) {
		buf := []byte{0x01, 0x02}
		_, err := readAlias(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("outer target truncated second index", func(t *testing.T) {
		buf := []byte{0x01, 0x02, 0x00}
		_, err := readAlias(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad sort propagates", func(t *testing.T) {
		buf := []byte{}
		_, err := readAlias(buf, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})
}

func TestReadComponentDecl(t *testing.T) {
	t.Run("import decl", func(t *testing.T) {
		// tag=0x03, name kind=0x00 len=1 "x", externdesc sort=0x01(func) typeidx=0.
		buf := []byte{0x03, 0x00, 0x01, 'x', 0x01, 0x00}
		off, err := readComponentDecl(buf, 0)
		if err != nil {
			t.Fatalf("readComponentDecl: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("import decl bad name", func(t *testing.T) {
		buf := []byte{0x03, 0xff}
		_, err := readComponentDecl(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("import decl bad externdesc", func(t *testing.T) {
		buf := []byte{0x03, 0x00, 0x01, 'x', 0xff}
		_, err := readComponentDecl(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("falls through to instancedecl", func(t *testing.T) {
		// tag=0x04 (export decl, handled by readInstanceDeclDesc): name +
		// externdesc.
		buf := []byte{0x04, 0x00, 0x01, 'x', 0x01, 0x00}
		off, err := readComponentDecl(buf, 0)
		if err != nil {
			t.Fatalf("readComponentDecl: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("truncated", func(t *testing.T) {
		_, err := readComponentDecl(nil, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})
}

func TestReadComponenttype(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		buf := []byte{0x00} // count=0
		off, err := readComponenttype(buf, 0)
		if err != nil || off != 1 {
			t.Errorf("off=%d err=%v, want 1,nil", off, err)
		}
	})

	t.Run("one import decl", func(t *testing.T) {
		buf := []byte{0x01, 0x03, 0x00, 0x01, 'x', 0x01, 0x00}
		off, err := readComponenttype(buf, 0)
		if err != nil {
			t.Fatalf("readComponenttype: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("bad count", func(t *testing.T) {
		_, err := readComponenttype(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("propagates componentdecl error with index", func(t *testing.T) {
		buf := []byte{0x02, 0x03, 0x00, 0x01, 'x', 0x01, 0x00, 0xff}
		_, err := readComponenttype(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "componentdecl[1]") {
			t.Errorf("got err=%v, want componentdecl[1] error", err)
		}
	})
}

// ---------------------------------------------------------------------
// Descriptor-level byte errors: variant refines, resourcetype, instance
// decls, valtype vectors.
// ---------------------------------------------------------------------

func TestReadValTypeRefInvalidPrimitive(t *testing.T) {
	// Negative s33 whose low 7 bits aren't a valid primvaltype code.
	// 0x40 as a single-byte signed LEB128 is -64 (0xffffffc0), low byte
	// 0xc0 is outside 0x73..0x7f.
	buf := []byte{0x40}
	_, _, err := readValTypeRef(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid primitive code") {
		t.Errorf("got err=%v, want invalid primitive code error", err)
	}
}

func TestReadValTypeRefTruncated(t *testing.T) {
	_, _, err := readValTypeRef(nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadOptValTypeRefInvalidTag(t *testing.T) {
	buf := []byte{0x02}
	_, _, err := readOptValTypeRef(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "invalid tag") {
		t.Errorf("got err=%v, want invalid tag error", err)
	}
}

func TestReadOptValTypeRefTruncated(t *testing.T) {
	_, _, err := readOptValTypeRef(nil, 0)
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestReadVariantCaseDescRefines(t *testing.T) {
	t.Run("refines present", func(t *testing.T) {
		// name len=1 "x", opt-valtype tag=0x00 (none), refines tag=0x01,
		// refines index=0x00.
		buf := []byte{0x01, 'x', 0x00, 0x01, 0x00}
		c, off, err := readVariantCaseDesc(buf, 0)
		if err != nil {
			t.Fatalf("readVariantCaseDesc: %v", err)
		}
		if c.Name != "x" || c.Type != nil {
			t.Errorf("case: got %+v", c)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("invalid refines tag", func(t *testing.T) {
		buf := []byte{0x01, 'x', 0x00, 0xff}
		_, _, err := readVariantCaseDesc(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "invalid tag") {
			t.Errorf("got err=%v, want invalid tag error", err)
		}
	})

	t.Run("truncated before refines tag", func(t *testing.T) {
		buf := []byte{0x01, 'x', 0x00}
		_, _, err := readVariantCaseDesc(buf, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})

	t.Run("truncated refines index", func(t *testing.T) {
		buf := []byte{0x01, 'x', 0x00, 0x01}
		_, _, err := readVariantCaseDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad name propagates", func(t *testing.T) {
		_, _, err := readVariantCaseDesc(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad type propagates", func(t *testing.T) {
		buf := []byte{0x01, 'x', 0x02} // invalid opt-valtype tag
		_, _, err := readVariantCaseDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadResourcetypeDesc(t *testing.T) {
	t.Run("no destructor", func(t *testing.T) {
		buf := []byte{0x7f, 0x00} // rep=bool, dtor tag=0x00 (none)
		d, off, err := readResourcetypeDesc(buf, 0)
		if err != nil {
			t.Fatalf("readResourcetypeDesc: %v", err)
		}
		if d.Dtor != nil {
			t.Errorf("dtor: got %v, want nil", d.Dtor)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("with destructor", func(t *testing.T) {
		buf := []byte{0x7f, 0x01, 0x05} // rep=bool, dtor tag=0x01, funcidx=5
		d, off, err := readResourcetypeDesc(buf, 0)
		if err != nil {
			t.Fatalf("readResourcetypeDesc: %v", err)
		}
		if d.Dtor == nil || *d.Dtor != 5 {
			t.Errorf("dtor: got %v, want 5", d.Dtor)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("invalid destructor tag", func(t *testing.T) {
		buf := []byte{0x7f, 0x02}
		_, _, err := readResourcetypeDesc(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "invalid destructor tag") {
			t.Errorf("got err=%v, want invalid destructor tag error", err)
		}
	})

	t.Run("truncated rep", func(t *testing.T) {
		_, _, err := readResourcetypeDesc(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("truncated before dtor tag", func(t *testing.T) {
		buf := []byte{0x7f}
		_, _, err := readResourcetypeDesc(buf, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})

	t.Run("truncated destructor index", func(t *testing.T) {
		buf := []byte{0x7f, 0x01}
		_, _, err := readResourcetypeDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadInstanceDeclDesc(t *testing.T) {
	t.Run("nested type decl", func(t *testing.T) {
		// tag=0x01 (type), then a deftype: primitive u32 (0x79).
		buf := []byte{0x01, 0x79}
		off, err := readInstanceDeclDesc(buf, 0)
		if err != nil {
			t.Fatalf("readInstanceDeclDesc: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("export decl", func(t *testing.T) {
		buf := []byte{0x04, 0x00, 0x01, 'x', 0x01, 0x00}
		off, err := readInstanceDeclDesc(buf, 0)
		if err != nil {
			t.Fatalf("readInstanceDeclDesc: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("export decl bad name propagates", func(t *testing.T) {
		buf := []byte{0x04, 0xff}
		_, err := readInstanceDeclDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("export decl bad externdesc propagates", func(t *testing.T) {
		buf := []byte{0x04, 0x00, 0x01, 'x', 0xff}
		_, err := readInstanceDeclDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("alias decl", func(t *testing.T) {
		buf := []byte{0x02, 0x01, 0x00, 0x00, 0x01, 'x'}
		off, err := readInstanceDeclDesc(buf, 0)
		if err != nil {
			t.Fatalf("readInstanceDeclDesc: %v", err)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("core type unsupported", func(t *testing.T) {
		buf := []byte{0x00}
		_, err := readInstanceDeclDesc(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "unsupported (M1): core type in instance type") {
			t.Errorf("got err=%v, want core-type-in-instance-type unsupported error", err)
		}
	})

	t.Run("invalid tag", func(t *testing.T) {
		buf := []byte{0xff}
		_, err := readInstanceDeclDesc(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "invalid tag") {
			t.Errorf("got err=%v, want invalid tag error", err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		_, err := readInstanceDeclDesc(nil, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})
}

func TestReadInstancetypeDescPropagatesIndexedError(t *testing.T) {
	// count=2, first instancedecl valid (export decl), second invalid tag.
	buf := []byte{0x02, 0x04, 0x00, 0x01, 'x', 0x01, 0x00, 0xff}
	_, _, err := readInstancetypeDesc(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "instancedecl[1]") {
		t.Errorf("got err=%v, want instancedecl[1] error", err)
	}
}

func TestReadInstancetypeDescTruncatedCount(t *testing.T) {
	_, _, err := readInstancetypeDesc(nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadValtypeVecDesc(t *testing.T) {
	t.Run("two elements", func(t *testing.T) {
		buf := []byte{0x02, 0x7f, 0x7e} // count=2, bool, s8
		refs, off, err := readValtypeVecDesc(buf, 0)
		if err != nil {
			t.Fatalf("readValtypeVecDesc: %v", err)
		}
		if len(refs) != 2 || refs[0].Primitive != "bool" || refs[1].Primitive != "s8" {
			t.Errorf("refs: got %+v", refs)
		}
		if off != len(buf) {
			t.Errorf("off=%d, want %d", off, len(buf))
		}
	})

	t.Run("truncated count", func(t *testing.T) {
		_, _, err := readValtypeVecDesc(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad element propagates", func(t *testing.T) {
		buf := []byte{0x01, 0x40} // count=1, invalid primitive code
		_, _, err := readValtypeVecDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadLabelTruncated(t *testing.T) {
	t.Run("missing length", func(t *testing.T) {
		_, _, err := readLabel(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("length exceeds buffer", func(t *testing.T) {
		buf := []byte{0x05, 'a', 'b'} // claims 5 bytes, only 2 follow
		_, _, err := readLabel(buf, 0)
		if !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("got err=%v, want ErrTruncatedBinary", err)
		}
	})
}

// ---------------------------------------------------------------------
// readDeftypeDesc: unsupported-construct and truncation paths not
// otherwise reachable through a real wasm-tools fixture.
// ---------------------------------------------------------------------

func TestReadDeftypeDescUnsupportedTag(t *testing.T) {
	// 0x67 is not a recognized defvaltype tag (valid tags are documented as
	// 0x68..0x72 plus the primvaltype range and the 0x3f/0x40/0x41/0x42
	// aggregate tags).
	buf := []byte{0x67}
	_, _, err := readDeftypeDesc(buf, 0)
	if err == nil || !strings.Contains(err.Error(), "unsupported (M1)") {
		t.Errorf("got err=%v, want unsupported (M1) defvaltype tag error", err)
	}
}

func TestReadDefvaltypeDescPrimitiveTagDirect(t *testing.T) {
	// readDeftypeDesc dispatches primitives before calling
	// readDefvaltypeDesc, so this branch (isPrimValtype(tag) inside
	// readDefvaltypeDesc itself) is only reached by calling it directly
	// with a primitive tag -- exercised here for completeness/defensiveness.
	d, off, err := readDefvaltypeDesc([]byte{}, 0, 0x7f)
	if err != nil {
		t.Fatalf("readDefvaltypeDesc: %v", err)
	}
	if off != 0 {
		t.Errorf("off=%d, want 0 (primitive consumes no further bytes)", off)
	}
	prim, ok := d.(PrimitiveDesc)
	if !ok || prim.Prim != "bool" {
		t.Errorf("got %+v, want PrimitiveDesc{bool}", d)
	}
}

func TestReadDeftypeDescTruncated(t *testing.T) {
	_, _, err := readDeftypeDesc(nil, 0)
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestReadDefvaltypeDescResultErrorPropagation(t *testing.T) {
	t.Run("bad ok type", func(t *testing.T) {
		buf := []byte{0x02} // invalid opt-valtype tag for the ok slot
		_, _, err := readDefvaltypeDesc(buf, 0, 0x6a)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad err type", func(t *testing.T) {
		buf := []byte{0x00, 0x02} // ok=none, err has invalid opt-valtype tag
		_, _, err := readDefvaltypeDesc(buf, 0, 0x6a)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("own truncated index", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x69)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("borrow truncated index", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x68)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("record error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x72)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("variant error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x71)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("list error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x70)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("tuple error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x6f)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("flags error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x6e)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("enum error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x6d)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("option error propagates", func(t *testing.T) {
		_, _, err := readDefvaltypeDesc(nil, 0, 0x6b)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadFunctypeDescErrorPropagation(t *testing.T) {
	t.Run("bad params", func(t *testing.T) {
		_, _, err := readFunctypeDesc(nil, 0)
		if err == nil || !strings.Contains(err.Error(), "functype params") {
			t.Errorf("got err=%v, want functype params error", err)
		}
	})

	t.Run("bad results", func(t *testing.T) {
		buf := []byte{0x00, 0xff} // 0 params, invalid result-list tag
		_, _, err := readFunctypeDesc(buf, 0)
		if err == nil || !strings.Contains(err.Error(), "functype results") {
			t.Errorf("got err=%v, want functype results error", err)
		}
	})
}

func TestReadResultListDescTruncated(t *testing.T) {
	_, _, err := readResultListDesc(nil, 0)
	if !errors.Is(err, ErrTruncatedBinary) {
		t.Errorf("got err=%v, want ErrTruncatedBinary", err)
	}
}

func TestReadResultListDescNamedErrorPropagates(t *testing.T) {
	buf := []byte{0x01} // tag=named, but the vec(label valtype) is truncated
	_, _, err := readResultListDesc(buf, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadResultListDescUnnamedErrorPropagates(t *testing.T) {
	buf := []byte{0x00, 0x40} // tag=unnamed, invalid primitive code
	_, _, err := readResultListDesc(buf, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadResultListDescNamedWithElements(t *testing.T) {
	// tag=named, count=2, ("x", u32), ("y", s32).
	buf := []byte{0x01, 0x02, 0x01, 'x', 0x79, 0x01, 'y', 0x7a}
	results, off, err := readResultListDesc(buf, 0)
	if err != nil {
		t.Fatalf("readResultListDesc: %v", err)
	}
	if off != len(buf) {
		t.Errorf("off=%d, want %d", off, len(buf))
	}
	if len(results.Named) != 2 || results.Named[0].Name != "x" || results.Named[0].Type.Primitive != "u32" ||
		results.Named[1].Name != "y" || results.Named[1].Type.Primitive != "s32" {
		t.Errorf("named results: got %+v", results.Named)
	}
	if results.Unnamed != nil {
		t.Errorf("unnamed should be nil, got %v", results.Unnamed)
	}
}

func TestReadRecordDescErrorPropagates(t *testing.T) {
	_, _, err := readRecordDesc(nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadVariantDescTruncatedCount(t *testing.T) {
	_, _, err := readVariantDesc(nil, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadVariantDescElementErrorPropagates(t *testing.T) {
	buf := []byte{0x01} // count=1, but no case bytes follow
	_, _, err := readVariantDesc(buf, 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestReadLabelValtypeVecDescErrors(t *testing.T) {
	t.Run("truncated count", func(t *testing.T) {
		_, _, err := readLabelValtypeVecDesc(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad name", func(t *testing.T) {
		buf := []byte{0x01} // count=1, no name bytes
		_, _, err := readLabelValtypeVecDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad type", func(t *testing.T) {
		buf := []byte{0x01, 0x01, 'x'} // count=1, name="x", no type byte
		_, _, err := readLabelValtypeVecDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestReadLabelVecDescErrors(t *testing.T) {
	t.Run("truncated count", func(t *testing.T) {
		_, _, err := readLabelVecDesc(nil, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("bad name", func(t *testing.T) {
		buf := []byte{0x01}
		_, _, err := readLabelVecDesc(buf, 0)
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

// ---------------------------------------------------------------------
// isTypeDesc marker methods: zero-width, only invoked directly (mirrors
// the same pattern in the wit package's AST marker interfaces).
// ---------------------------------------------------------------------

func TestIsTypeDescMarkers(t *testing.T) {
	PrimitiveDesc{}.isTypeDesc()
	RecordDesc{}.isTypeDesc()
	VariantDesc{}.isTypeDesc()
	ListDesc{}.isTypeDesc()
	TupleDesc{}.isTypeDesc()
	FlagsDesc{}.isTypeDesc()
	EnumDesc{}.isTypeDesc()
	OptionDesc{}.isTypeDesc()
	ResultDesc{}.isTypeDesc()
	OwnDesc{}.isTypeDesc()
	BorrowDesc{}.isTypeDesc()
	FuncDesc{}.isTypeDesc()
	InstanceDesc{}.isTypeDesc()
	ComponentDesc{}.isTypeDesc()
	ResourceDesc{}.isTypeDesc()
}

// TestKindStrings exercises every TypeDesc.Kind() implementation directly.
func TestKindStrings(t *testing.T) {
	tests := []struct {
		name string
		d    TypeDesc
		want string
	}{
		{"primitive", PrimitiveDesc{Prim: "u32"}, "u32"},
		{"record", RecordDesc{}, "record"},
		{"variant", VariantDesc{}, "variant"},
		{"list", ListDesc{}, "list"},
		{"tuple", TupleDesc{}, "tuple"},
		{"flags", FlagsDesc{}, "flags"},
		{"enum", EnumDesc{}, "enum"},
		{"option", OptionDesc{}, "option"},
		{"result", ResultDesc{}, "result"},
		{"own", OwnDesc{}, "own"},
		{"borrow", BorrowDesc{}, "borrow"},
		{"func", FuncDesc{}, "func"},
		{"instance", InstanceDesc{}, "instance"},
		{"component", ComponentDesc{}, "component"},
		{"resource", ResourceDesc{}, "resource"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.d.Kind(); got != tt.want {
				t.Errorf("Kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// readSortidx direct coverage (core-sort discriminator + truncation).
// ---------------------------------------------------------------------

func TestReadSortidxCoreSort(t *testing.T) {
	// core sort (0x00) + discriminator byte + u32 index.
	buf := []byte{0x00, 0x01, 0x05}
	sort, idx, off, err := readSortidx(buf, 0)
	if err != nil {
		t.Fatalf("readSortidx: %v", err)
	}
	if sort != 0x00 || idx != 5 || off != len(buf) {
		t.Errorf("got sort=%#x idx=%d off=%d, want 0x00,5,%d", sort, idx, off, len(buf))
	}
}

// ---------------------------------------------------------------------
// Direct externTypeName / sectionIDName coverage (all branches, including
// their "unknown"/"Unknown" defaults) -- more reliable than threading every
// value through Dump().
// ---------------------------------------------------------------------

func TestExternTypeNameAllValues(t *testing.T) {
	tests := []struct {
		b    byte
		want string
	}{
		{0x00, "module"},
		{0x01, "func"},
		{0x02, "value"},
		{0x03, "type"},
		{0x04, "component"},
		{0x05, "instance"},
		{0xff, "unknown"},
	}
	for _, tt := range tests {
		if got := externTypeName(tt.b); got != tt.want {
			t.Errorf("externTypeName(%#x) = %q, want %q", tt.b, got, tt.want)
		}
	}
}

func TestSectionIDNameAllValues(t *testing.T) {
	tests := []struct {
		id   byte
		want string
	}{
		{0, "Custom"}, {1, "CoreModule"}, {2, "CoreInstance"}, {3, "CoreType"},
		{4, "Component"}, {5, "Instance"}, {6, "Alias"}, {7, "Type"},
		{8, "Canonical"}, {9, "Start"}, {10, "Import"}, {11, "Export"},
		{12, "Value"}, {200, "Unknown"},
	}
	for _, tt := range tests {
		if got := sectionIDName(tt.id); got != tt.want {
			t.Errorf("sectionIDName(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// TestDumpWriteErrorAtEveryCallsite forces the underlying writer to fail at
// each successive Write call, one at a time, to exercise every one of
// Dump's "if _, err := ...; err != nil { return err }" early-return
// branches -- not just the very first one, which is all a single
// always-failing writer can reach.
type failAfterNWriter struct {
	n     int
	calls int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.n {
		return 0, errors.New("boom")
	}
	return len(p), nil
}

func TestDumpWriteErrorAtEveryCallsite(t *testing.T) {
	c := &Component{
		Types: []Type{{Kind: "record"}, {Kind: "func"}},
		Imports: []Import{
			{Name: "imp1", ExternType: 0x01},
			{Name: "imp2", ExternType: 0x05},
		},
		Exports: []Export{
			{Name: "exp1", ExternType: 0x01},
			{Name: "exp2", ExternType: 0x05},
		},
		RawSections: []RawSection{
			{ID: 1, Size: 2},
			{ID: 2, Size: 3},
		},
	}
	// 20 is comfortably more than the total number of Write/Fprintf calls
	// Dump makes for this component; failing at any n in range forces a
	// different early-return branch (or, once past the last call, no error
	// at all -- both are valid outcomes we just don't assert further on).
	sawError := false
	for n := 1; n <= 20; n++ {
		w := &failAfterNWriter{n: n}
		err := c.Dump(w)
		if err != nil {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected at least one failAfterN value to trigger a Dump error")
	}
}

func TestDecodeReaderError(t *testing.T) {
	_, err := Decode(errReader{})
	if err == nil {
		t.Fatal("expected error from a failing io.Reader")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }

// Tests for core module, core instance, instance, alias, canon, and start sections

func TestDecodeCoreInstance_Instantiate(t *testing.T) {
	// Construct a component with a core module and core instance.
	buf := preamble()
	// Section 1: core module (just a minimal core wasm module)
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00} // minimal core module
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	// Section 2: core instance with instantiate form
	// 0x00 moduleIdx vec(args)
	// instantiatearg: 0x00 len(name) name 0x12 instanceidx
	coreInstBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = instantiate
		0x00,       // moduleIdx = 0
		0x00,       // argCount = 0
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if len(c.CoreModules) != 1 {
		t.Errorf("CoreModules: got %d, want 1", len(c.CoreModules))
	}
	if len(c.CoreInstances) != 1 {
		t.Errorf("CoreInstances: got %d, want 1", len(c.CoreInstances))
	}
	ci := c.CoreInstances[0]
	if ci.Kind != 0x00 {
		t.Errorf("kind: got %#x, want 0x00", ci.Kind)
	}
	if ci.ModuleIdx != 0 {
		t.Errorf("moduleIdx: got %d, want 0", ci.ModuleIdx)
	}
	if len(ci.Args) != 0 {
		t.Errorf("args: got %d, want 0", len(ci.Args))
	}
}

func TestDecodeCoreInstance_InlineExports(t *testing.T) {
	buf := preamble()
	// Minimal core module
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	// Section 2: core instance with inline exports
	coreInstBody := []byte{
		0x01,                   // count = 1
		0x01,                   // kind = inline exports
		0x00,                   // exportCount = 0
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.CoreInstances) != 1 || c.CoreInstances[0].Kind != 0x01 {
		t.Errorf("inline exports not decoded correctly")
	}
}

func TestDecodeCoreInstance_InvalidKind(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	coreInstBody := []byte{
		0x01,       // count = 1
		0xff,       // kind = invalid
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error for invalid core instance kind")
	}
}

func TestDecodeInstance(t *testing.T) {
	buf := preamble()
	// Section 5: instance
	instBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = instantiate
		0x00,       // componentIdx = 0
		0x00,       // argCount = 0
	}
	buf = append(buf, 0x05, byte(len(instBody)))
	buf = append(buf, instBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Instances) != 1 || c.Instances[0].Kind != 0x00 {
		t.Errorf("instance not decoded")
	}
}

func TestDecodeAlias(t *testing.T) {
	buf := preamble()
	// Section 6: alias (export target)
	// sort=0x01 (func), target=0x00 (export), instanceidx=0, name="f"
	aliasBody := []byte{
		0x01,               // count = 1
		0x01,               // sort = func
		0x00,               // target kind = export
		0x00,               // instance index = 0
		0x01, 'f',          // label: len=1, "f"
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Aliases) != 1 {
		t.Errorf("alias not decoded")
	}
	alias := c.Aliases[0]
	if alias.Sort != 0x01 || alias.TargetKind != 0x00 || alias.Name != "f" {
		t.Errorf("alias decoded incorrectly: %+v", alias)
	}
}

func TestDecodeAlias_CoreExport(t *testing.T) {
	buf := preamble()
	// Section 6: alias with core export
	// sort=0x00 (core) + discriminator, target=0x01 (core export)
	aliasBody := []byte{
		0x01,               // count = 1
		0x00,               // sort = core
		0x01,               // discriminator = func
		0x01,               // target kind = core export
		0x00,               // instance index = 0
		0x01, 'f',          // label
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Aliases) != 1 {
		t.Fatalf("alias not decoded")
	}
	alias := c.Aliases[0]
	if alias.TargetKind != 0x01 {
		t.Errorf("core export target not decoded")
	}
}

func TestDecodeAlias_Outer(t *testing.T) {
	buf := preamble()
	// Section 6: alias with outer target
	// sort=0x01 (func), target=0x02 (outer)
	aliasBody := []byte{
		0x01,               // count = 1
		0x01,               // sort = func
		0x02,               // target kind = outer
		0x01,               // outer count = 1
		0x00,               // outer index = 0
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Aliases) != 1 || c.Aliases[0].TargetKind != 0x02 {
		t.Errorf("outer alias not decoded")
	}
}

func TestDecodeCanon_Lift(t *testing.T) {
	buf := preamble()
	// Add a type first (for the canon to reference)
	buf = append(buf, 0x07, 0x01, 0x00) // section 7, size 1, count=0

	// Section 8: canon with lift
	// 0x00 0x00 coreidx opts typeidx
	canonBody := []byte{
		0x01,           // count = 1
		0x00,           // kind = lift
		0x00,           // prefix
		0x00,           // core func idx = 0
		0x00,           // opt count = 0
		0x00,           // type idx = 0
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 {
		t.Errorf("canon not decoded")
	}
	canon := c.Canons[0]
	if canon.Kind != 0x00 || canon.CoreFuncIdx != 0 || canon.TypeIdx != 0 {
		t.Errorf("lift canon decoded incorrectly: %+v", canon)
	}
}

func TestDecodeCanon_Lower(t *testing.T) {
	buf := preamble()
	// Section 8: canon with lower
	canonBody := []byte{
		0x01,           // count = 1
		0x01,           // kind = lower
		0x00,           // prefix
		0x00,           // func idx = 0
		0x00,           // opt count = 0
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 || c.Canons[0].Kind != 0x01 {
		t.Errorf("lower canon not decoded")
	}
}

func TestDecodeCanon_Resource(t *testing.T) {
	buf := preamble()
	// Section 8: canon with resource.new
	canonBody := []byte{
		0x01,           // count = 1
		0x02,           // kind = resource.new
		0x00,           // type idx = 0
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 || c.Canons[0].Kind != 0x02 {
		t.Errorf("resource canon not decoded")
	}
}

func TestDecodeCanon_UnsupportedKind(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,           // count = 1
		0xff,           // kind = unsupported
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil || !strings.Contains(err.Error(), "unsupported (M1)") {
		t.Fatalf("expected unsupported kind error, got: %v", err)
	}
}

func TestDecodeStart(t *testing.T) {
	buf := preamble()
	// Section 9: start
	// funcidx vec(valueidx) resultcount
	startBody := []byte{
		0x00,           // func idx = 0
		0x00,           // arg count = 0
		0x00,           // result count = 0
	}
	buf = append(buf, 0x09, byte(len(startBody)))
	buf = append(buf, startBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Start == nil || c.Start.FuncIdx != 0 {
		t.Errorf("start section not decoded")
	}
}

func TestDecodeStart_WithArgsAndResults(t *testing.T) {
	buf := preamble()
	// Section 9: start with args and results
	startBody := []byte{
		0x00,           // func idx = 0
		0x02,           // arg count = 2
		0x00, 0x01,     // args: 0, 1
		0x02,           // result count = 2
	}
	buf = append(buf, 0x09, byte(len(startBody)))
	buf = append(buf, startBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Start == nil || len(c.Start.Args) != 2 || c.Start.ResultCount != 2 {
		t.Errorf("start with args/results not decoded correctly: %+v", c.Start)
	}
}

func TestDecodeTruncated_CoreInstance(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	coreInstBody := []byte{0x01, 0x00} // missing moduleIdx
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestDecodeTruncated_Alias(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{0x01, 0x01} // count=1, sort=1, but missing target kind
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error in alias")
	}
}

func TestDecodeTruncated_Canon(t *testing.T) {
	buf := preamble()
	canonBody := []byte{0x01, 0x00} // count=1, kind=lift, but missing prefix
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error in canon")
	}
}

func TestDecodeTruncated_Start(t *testing.T) {
	buf := preamble()
	buf = append(buf, 0x09, 0x00) // section 9, empty size

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error in start")
	}
}

func TestDecodeCanonWithOptions(t *testing.T) {
	buf := preamble()
	// Section 8: canon with options
	canonBody := []byte{
		0x01,           // count = 1
		0x00,           // kind = lift
		0x00,           // prefix
		0x00,           // core func idx = 0
		0x02,           // opt count = 2
		0x00,           // opt[0] kind = utf8 (no data)
		0x03, 0x00,     // opt[1] kind = memory, idx = 0
		0x00,           // type idx = 0
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 || len(c.Canons[0].Opts) != 2 {
		t.Errorf("canon options not decoded")
	}
}

func TestHOSTComponentWithCanonAndAlias(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "host_component.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// host_component has 1 core module, 1 core instance, 1 alias, 1 canon, 1 export
	if len(c.CoreModules) != 1 {
		t.Errorf("CoreModules: got %d, want 1", len(c.CoreModules))
	}
	if len(c.CoreInstances) != 1 {
		t.Errorf("CoreInstances: got %d, want 1", len(c.CoreInstances))
	}
	if len(c.Aliases) != 1 {
		t.Errorf("Aliases: got %d, want 1", len(c.Aliases))
	}
	if len(c.Canons) != 1 {
		t.Errorf("Canons: got %d, want 1", len(c.Canons))
	}
	if len(c.Exports) != 1 {
		t.Errorf("Exports: got %d, want 1", len(c.Exports))
	}

	// Verify canon details
	canon := c.Canons[0]
	if canon.Kind != 0x00 {
		t.Errorf("canon kind: got %#x, want 0x00 (lift)", canon.Kind)
	}

	// Verify alias details
	alias := c.Aliases[0]
	if alias.TargetKind != 0x01 {
		t.Errorf("alias target: got %#x, want 0x01 (core export)", alias.TargetKind)
	}
}

func TestExtendedComponentWithCanonAndAliases(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "extended_component.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// extended_component has core module, core instance, aliases, canon
	if len(c.CoreModules) < 1 {
		t.Errorf("CoreModules: got %d, want >=1", len(c.CoreModules))
	}
	if len(c.CoreInstances) < 1 {
		t.Errorf("CoreInstances: got %d, want >=1", len(c.CoreInstances))
	}
	if len(c.Aliases) < 1 {
		t.Errorf("Aliases: got %d, want >=1", len(c.Aliases))
	}
	if len(c.Canons) < 1 {
		t.Errorf("Canons: got %d, want >=1", len(c.Canons))
	}
}

// More thorough error path coverage

func TestDecodeCoreInstance_TruncatedArgName(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	// Core instance with arg that has truncated name
	coreInstBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = instantiate
		0x00,       // moduleIdx = 0
		0x01,       // argCount = 1
		0x00, 0x05, // name: kind=0x00, len=5 (but only provide 0 bytes)
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for core instance arg name")
	}
}

func TestDecodeCoreInstance_MissingPrefix(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	// Core instance instantiate arg missing the 0x12 prefix
	coreInstBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = instantiate
		0x00,       // moduleIdx = 0
		0x01,       // argCount = 1
		0x00, 0x01, 'x',   // name: kind=0x00, len=1, "x"
		0x13,       // WRONG: should be 0x12
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil || !strings.Contains(err.Error(), "0x12 prefix") {
		t.Fatalf("expected 0x12 prefix error, got: %v", err)
	}
}

func TestDecodeCoreInstance_TruncatedInlineExportName(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	coreInstBody := []byte{
		0x01,       // count = 1
		0x01,       // kind = inline exports
		0x01,       // exportCount = 1
		0x00, 0x05, // name: kind=0x00, len=5 (but only provide 0 bytes)
	}
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for core instance export name")
	}
}

func TestDecodeInstance_Truncated(t *testing.T) {
	buf := preamble()
	instBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = instantiate
		0x00,       // componentIdx = 0
		// missing argCount
	}
	buf = append(buf, 0x05, byte(len(instBody)))
	buf = append(buf, instBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for instance argCount")
	}
}

func TestDecodeAlias_InvalidTargetKind(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{
		0x01,       // count = 1
		0x01,       // sort = func
		0x03,       // target kind = invalid
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error for invalid alias target kind")
	}
}

func TestDecodeAlias_TruncatedInstanceIndex(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{
		0x01,       // count = 1
		0x01,       // sort = func
		0x00,       // target kind = export
		// missing instance index
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias instance index")
	}
}

func TestDecodeAlias_TruncatedName(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{
		0x01,       // count = 1
		0x01,       // sort = func
		0x00,       // target kind = export
		0x00,       // instance index = 0
		0x05, 'a',  // label: len=5, but only provide 1 byte
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias name")
	}
}

func TestDecodeAlias_TruncatedOuterCount(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{
		0x01,       // count = 1
		0x01,       // sort = func
		0x02,       // target kind = outer
		// missing outer count
	}
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias outer count")
	}
}

func TestDecodeCanon_LiftInvalidPrefix(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = lift
		0x01,       // WRONG: should be 0x00
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil || !strings.Contains(err.Error(), "0x00 prefix") {
		t.Fatalf("expected 0x00 prefix error for lift, got: %v", err)
	}
}

func TestDecodeCanon_LowerInvalidPrefix(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,       // count = 1
		0x01,       // kind = lower
		0x01,       // WRONG: should be 0x00
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil || !strings.Contains(err.Error(), "0x00 prefix") {
		t.Fatalf("expected 0x00 prefix error for lower, got: %v", err)
	}
}

func TestDecodeCanonOpts_UnsupportedKind(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = lift
		0x00,       // prefix
		0x00,       // core func idx = 0
		0x01,       // opt count = 1
		0xff,       // opt kind = unsupported
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil || !strings.Contains(err.Error(), "unsupported (M1)") {
		t.Fatalf("expected unsupported canon opt kind error, got: %v", err)
	}
}

func TestDecodeCanonOpts_TruncatedIndex(t *testing.T) {
	buf := preamble()
	canonBody := []byte{
		0x01,       // count = 1
		0x00,       // kind = lift
		0x00,       // prefix
		0x00,       // core func idx = 0
		0x01,       // opt count = 1
		0x03,       // opt kind = memory (needs index)
		// missing index
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for canon opt index")
	}
}

func TestDumpCoreInstancesAndCanons(t *testing.T) {
	c := &Component{
		CoreModules: []CoreModule{{Offset: 100, Size: 50}},
		CoreInstances: []CoreInstance{
			{Kind: 0x00, ModuleIdx: 0, Args: []CoreInstantiateArg{{Name: "test", InstanceIdx: 0}}},
			{Kind: 0x01, Exports: []CoreInlineExport{{Name: "run", Sort: 0x01, CoreSortIdx: 0}}},
		},
		Instances: []Instance{
			{Kind: 0x00, ComponentIdx: 0},
		},
		Aliases: []AliasDef{
			{Sort: 0x01, TargetKind: 0x00, InstanceIdx: 0, Name: "f"},
			{Sort: 0x01, TargetKind: 0x02, OuterCount: 1, OuterIndex: 0},
		},
		Canons: []Canon{
			{Kind: 0x00, CoreFuncIdx: 0, TypeIdx: 1},
			{Kind: 0x01, FuncIdx: 0},
			{Kind: 0x02, TypeIdx: 2},
			{Kind: 0x03, TypeIdx: 2},
			{Kind: 0x04, TypeIdx: 2},
		},
		Start: &Start{FuncIdx: 0, Args: []uint32{0, 1}, ResultCount: 2},
	}

	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out := buf.String()

	// Check that all new sections are in the dump
	for _, want := range []string{
		"Core Modules:", "Core Instances:", "Instances:", "Aliases:", "Canons:", "Start:",
		"instantiate module", "inline exports", "instantiate component",
		"lift core func", "lower func", "resource.new", "resource.drop", "resource.rep",
		"export instance", "outer count",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n---\n%s", want, out)
		}
	}
}

func TestCanonWithAllOptTypes(t *testing.T) {
	buf := preamble()
	// Section 8: canon with all supported opt types
	canonBody := []byte{
		0x01,           // count = 1
		0x00,           // kind = lift
		0x00,           // prefix
		0x00,           // core func idx = 0
		0x07,           // opt count = 7
		0x00,           // utf8
		0x01,           // utf16
		0x02,           // latin1+utf16
		0x03, 0x00,     // memory, idx=0
		0x04, 0x00,     // realloc, idx=0
		0x05, 0x00,     // post-return, idx=0
		0x06,           // async (no index)
		0x00,           // type idx = 0
	}
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Canons) != 1 || len(c.Canons[0].Opts) != 7 {
		t.Errorf("canon with all opts not decoded: %+v", c.Canons[0])
	}
}

func TestDecodeStartWithTruncatedArgs(t *testing.T) {
	buf := preamble()
	startBody := []byte{
		0x00,           // func idx = 0
		0x02,           // arg count = 2
		0x00,           // arg[0] = 0
		// missing arg[1]
	}
	buf = append(buf, 0x09, byte(len(startBody)))
	buf = append(buf, startBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for start args")
	}
}

func TestDecodeStartWithTruncatedResultCount(t *testing.T) {
	buf := preamble()
	startBody := []byte{
		0x00,           // func idx = 0
		0x00,           // arg count = 0
		// missing result count
	}
	buf = append(buf, 0x09, byte(len(startBody)))
	buf = append(buf, startBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for start result count")
	}
}

func TestDecodeCoreInstanceTruncatedCount(t *testing.T) {
	buf := preamble()
	coreModuleBody := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, 0x01, byte(len(coreModuleBody)))
	buf = append(buf, coreModuleBody...)

	coreInstBody := []byte{} // empty, missing count
	buf = append(buf, 0x02, byte(len(coreInstBody)))
	buf = append(buf, coreInstBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for core instance count")
	}
}

func TestDecodeInstanceTruncatedCount(t *testing.T) {
	buf := preamble()
	instBody := []byte{} // empty, missing count
	buf = append(buf, 0x05, byte(len(instBody)))
	buf = append(buf, instBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for instance count")
	}
}

func TestDecodeAliasTruncatedCount(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{} // empty, missing count
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias count")
	}
}

func TestDecodeAliasTruncatedSort(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{0x01} // count=1, missing sort
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias sort")
	}
}

func TestDecodeAliasTruncatedCoreDiscriminator(t *testing.T) {
	buf := preamble()
	aliasBody := []byte{0x01, 0x00} // count=1, sort=core, missing discriminator
	buf = append(buf, 0x06, byte(len(aliasBody)))
	buf = append(buf, aliasBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for alias core discriminator")
	}
}

func TestDecodeCanonTruncatedCount(t *testing.T) {
	buf := preamble()
	canonBody := []byte{} // empty, missing count
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for canon count")
	}
}

func TestDecodeCanonTruncatedKind(t *testing.T) {
	buf := preamble()
	canonBody := []byte{0x01} // count=1, missing kind
	buf = append(buf, 0x08, byte(len(canonBody)))
	buf = append(buf, canonBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected truncation error for canon kind")
	}
}

func TestInstance_InvalidKind(t *testing.T) {
	buf := preamble()
	instBody := []byte{
		0x01,       // count = 1
		0xff,       // kind = invalid
	}
	buf = append(buf, 0x05, byte(len(instBody)))
	buf = append(buf, instBody...)

	_, err := Decode(bytes.NewReader(buf))
	if err == nil {
		t.Fatal("expected error for invalid instance kind")
	}
}

func TestInstanceWithArgs(t *testing.T) {
	buf := preamble()
	// Section 5: instance with arguments
	instBody := []byte{
		0x01,               // count = 1
		0x00,               // kind = instantiate
		0x00,               // componentIdx = 0
		0x01,               // argCount = 1
		0x00, 0x01, 'x',    // arg name: kind=0x00, len=1, "x"
		0x01, 0x00,         // arg sortidx: sort=func, idx=0
	}
	buf = append(buf, 0x05, byte(len(instBody)))
	buf = append(buf, instBody...)

	c, err := Decode(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.Instances) != 1 || len(c.Instances[0].Args) != 1 {
		t.Errorf("instance with args not decoded correctly")
	}
}

func TestDumpWithNilStart(t *testing.T) {
	// Verify that Dump doesn't print Start section when it's nil
	c := &Component{}
	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "Start:") {
		t.Errorf("dump should not include Start: when Start is nil")
	}
}

func TestReadSortidxTruncated(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"core sort, missing discriminator", []byte{0x00}},
		{"non-core sort, missing index", []byte{0x01}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := readSortidx(tt.buf, 0)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
