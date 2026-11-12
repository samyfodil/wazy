package binary

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Core types decode in full: a module type's every declaration, every core
// extern sort, and a rectype kept as the core encoding it is.
func TestReadCoreTypeDef(t *testing.T) {
	moduleType := []byte{
		0x50, 0x08, // module type, 8 declarations
		0x01, 0x60, 0x01, 0x7f, 0x00, // type 0: (func (param i32))
		0x02, 0x10, 0x01, 0x01, 0x03, // alias outer 1 3 (type)
		0x00, 0x01, 'a', 0x01, 'f', 0x00, 0x00, // import "a" "f" (func 0)
		0x00, 0x00, 0x01, 't', 0x01, 0x70, 0x01, 0x01, 0x02, // import "" "t" (table 1 2 funcref)
		0x00, 0x00, 0x01, 'm', 0x02, 0x03, 0x01, 0x02, // import "" "m" (memory 1 2 shared)
		0x00, 0x00, 0x01, 'g', 0x03, 0x64, 0x00, 0x01, // import "" "g" (global (mut (ref 0)))
		0x03, 0x01, 'e', 0x04, 0x00, 0x00, // export "e" (tag (type 0))
		0x03, 0x01, 'x', 0x02, 0x0c, 0x01, 0x10, // export "x" (memory i64 1, page size 2^16)
	}
	d, off, err := readCoreTypeDef(moduleType, 0)
	if err != nil || off != len(moduleType) {
		t.Fatalf("readCoreTypeDef = %d, %v", off, err)
	}
	if d.Module == nil || d.Count != 1 || len(d.Module.Decls) != 8 {
		t.Fatalf("got %+v", d)
	}
	decls := d.Module.Decls
	if decls[0].Kind != CoreModuleDeclType || decls[0].RecCount != 1 || !bytes.Equal(decls[0].Rec, []byte{0x60, 0x01, 0x7f, 0x00}) {
		t.Errorf("type: %+v", decls[0])
	}
	if decls[1].Kind != CoreModuleDeclAlias || decls[1].AliasCount != 1 || decls[1].AliasIndex != 3 {
		t.Errorf("alias: %+v", decls[1])
	}
	if im := decls[2]; im.Kind != CoreModuleDeclImport || im.Module != "a" || im.Name != "f" || im.Extern.Kind != 0x00 || im.Extern.TypeIndex != 0 {
		t.Errorf("func import: %+v", im)
	}
	if x := decls[3].Extern; x.Kind != 0x01 || !bytes.Equal(x.Bytes, []byte{0x70, 0x01, 0x01, 0x02}) {
		t.Errorf("table: %+v", x)
	}
	if x := decls[4].Extern; x.Kind != 0x02 || !bytes.Equal(x.Bytes, []byte{0x03, 0x01, 0x02}) {
		t.Errorf("memory: %+v", x)
	}
	if x := decls[5].Extern; x.Kind != 0x03 || !bytes.Equal(x.Bytes, []byte{0x64, 0x00, 0x01}) {
		t.Errorf("global: %+v", x)
	}
	if ex := decls[6]; ex.Kind != CoreModuleDeclExport || ex.Name != "e" || ex.Extern.Kind != 0x04 || ex.Extern.TypeIndex != 0 {
		t.Errorf("tag export: %+v", ex)
	}
	if x := decls[7].Extern; x.Kind != 0x02 || !bytes.Equal(x.Bytes, []byte{0x0c, 0x01, 0x10}) {
		t.Errorf("memory64 export: %+v", x)
	}

	// A recursion group of a struct, an array and a final sub of a func --
	// three types -- kept as the bytes a core type section has.
	rec := []byte{
		0x4e, 0x03,
		0x5f, 0x02, 0x78, 0x00, 0x63, 0x6e, 0x01, // struct (field i8) (field (mut anyref))
		0x5e, 0x77, 0x01, // array (mut i16)
		0x4f, 0x01, 0x00, 0x60, 0x00, 0x01, 0x7b, // sub final 0 (func (result v128))
	}
	d, off, err = readCoreTypeDef(rec, 0)
	if err != nil || off != len(rec) || d.Module != nil || d.Count != 3 || !bytes.Equal(d.Rec, rec) {
		t.Fatalf("rec group: %+v, %d, %v", d, off, err)
	}
	// A top-level non-final sub is prefixed 0x00, which the core encoding
	// does not have.
	sub := []byte{0x00, 0x50, 0x00, 0x60, 0x00, 0x00}
	d, off, err = readCoreTypeDef(sub, 0)
	if err != nil || off != len(sub) || d.Count != 1 || !bytes.Equal(d.Rec, sub[1:]) {
		t.Fatalf("prefixed sub: %+v, %d, %v", d, off, err)
	}
}

func TestReadCoreTypeDef_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  []byte
		want string
	}{
		{"empty", nil, "unexpected end-of-file"},
		{"bad prefix", []byte{0x00, 0x60}, "0x00 must prefix a non-final sub"},
		{"bad comptype", []byte{0x7f}, "invalid leading byte (0x7f) for core composite type"},
		{"module count", []byte{0x50, 0x80}, "core:moduletype"},
		{"module count past the input", []byte{0x50, 0x01}, "count 1 exceeds remaining input"},
		{"module truncated", []byte{0x50, 0x02, 0x03, 0x00, 0x00, 0x00}, "unexpected end-of-file"},
		{"bad decl", []byte{0x50, 0x01, 0x7f}, "invalid leading byte (0x7f) for type definition"},
		{"import module name", []byte{0x50, 0x01, 0x00, 0x05}, "core:name"},
		{"import field name", []byte{0x50, 0x01, 0x00, 0x00, 0x05}, "core:name"},
		{"import desc", []byte{0x50, 0x01, 0x00, 0x00, 0x00}, "unexpected end-of-file"},
		{"export name", []byte{0x50, 0x01, 0x03, 0x05}, "core:name"},
		{"bad sort", []byte{0x50, 0x01, 0x03, 0x00, 0x09}, "core:externtype: invalid sort 0x9"},
		{"func index", []byte{0x50, 0x01, 0x03, 0x00, 0x00}, "unexpected end-of-file"},
		{"table reftype", []byte{0x50, 0x01, 0x03, 0x00, 0x01, 0x40}, "core:valtype: invalid type 0x40"},
		{"table limits", []byte{0x50, 0x01, 0x03, 0x00, 0x01, 0x70}, "unexpected end-of-file"},
		{"limits flags", []byte{0x50, 0x01, 0x03, 0x00, 0x02, 0x10}, "core:limits: invalid flags 0x10"},
		{"limits min", []byte{0x50, 0x01, 0x03, 0x00, 0x02, 0x00}, "unexpected end-of-file"},
		{"limits max", []byte{0x50, 0x01, 0x03, 0x00, 0x02, 0x01, 0x00}, "unexpected end-of-file"},
		{"limits page size", []byte{0x50, 0x01, 0x03, 0x00, 0x02, 0x08, 0x00}, "unexpected end-of-file"},
		{"limits min overflow", []byte{0x50, 0x01, 0x03, 0x00, 0x02, 0x00, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "overflow"},
		{"global valtype", []byte{0x50, 0x01, 0x03, 0x00, 0x03}, "unexpected end-of-file"},
		{"global mut missing", []byte{0x50, 0x01, 0x03, 0x00, 0x03, 0x7f}, "unexpected end-of-file"},
		{"global mut", []byte{0x50, 0x01, 0x03, 0x00, 0x03, 0x7f, 0x02}, "core:globaltype: invalid mutability 0x2"},
		{"tag attribute missing", []byte{0x50, 0x01, 0x03, 0x00, 0x04}, "unexpected end-of-file"},
		{"tag attribute", []byte{0x50, 0x01, 0x03, 0x00, 0x04, 0x01}, "core:tagtype: invalid attribute 0x1"},
		{"alias truncated", []byte{0x50, 0x01, 0x02, 0x10}, "unexpected end-of-file"},
		{"alias not outer type", []byte{0x50, 0x01, 0x02, 0x00, 0x01}, "invalid leading byte (0x0) for outer alias kind"},
		{"alias count", []byte{0x50, 0x01, 0x02, 0x10, 0x01}, "unexpected end-of-file"},
		{"alias index", []byte{0x50, 0x01, 0x02, 0x10, 0x01, 0x00}, "unexpected end-of-file"},
		{"type decl", []byte{0x50, 0x01, 0x01, 0x50, 0x00, 0x00}, "invalid leading byte (0x0) for core composite type"},
		{"rec group count", []byte{0x4e}, "unexpected end-of-file"},
		{"rec group member missing", []byte{0x4e, 0x01}, "unexpected end-of-file"},
		{"type decl missing", []byte{0x50, 0x01, 0x01}, "unexpected end-of-file"},
		{"rec group count overflow", []byte{0x4e, 0xff, 0xff, 0xff, 0xff, 0xff}, "overflow"},
		{"prefixed sub body", []byte{0x00, 0x50, 0x00, 0x7f}, "invalid leading byte (0x7f) for core composite type"},
		{"sub supertype count overflow", []byte{0x4f, 0xff, 0xff, 0xff, 0xff, 0xff}, "overflow"},
		{"sub supertype overflow", []byte{0x4f, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff}, "overflow"},
		{"rec group member", []byte{0x4e, 0x01, 0x5e}, "rec group member 0"},
		{"sub supertypes", []byte{0x4f}, "unexpected end-of-file"},
		{"sub supertype", []byte{0x4f, 0x01}, "unexpected end-of-file"},
		{"sub comptype", []byte{0x4f, 0x00}, "unexpected end-of-file"},
		{"func params", []byte{0x60}, "unexpected end-of-file"},
		{"func param", []byte{0x60, 0x01, 0x40}, "core:valtype: invalid type 0x40"},
		{"struct count", []byte{0x5f}, "unexpected end-of-file"},
		{"struct field", []byte{0x5f, 0x01, 0x40}, "core:valtype"},
		{"field truncated", []byte{0x5e}, "unexpected end-of-file"},
		{"field mut missing", []byte{0x5e, 0x78}, "unexpected end-of-file"},
		{"field mut", []byte{0x5e, 0x78, 0x02}, "core:fieldtype: invalid mutability 0x2"},
		{"valtype truncated", []byte{0x60, 0x01}, "unexpected end-of-file"},
		{"heap type truncated", []byte{0x60, 0x01, 0x63}, "unexpected end-of-file"},
		{"heap type negative", []byte{0x60, 0x01, 0x63, 0x40}, "core:heaptype: invalid type -64"},
		{"heap type cut short", []byte{0x60, 0x01, 0x63, 0x80}, "core:heaptype: EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readCoreTypeDef(tc.buf, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The core type section fills the core type index space: one index per type
// a definition defines, and one per core type alias, in order.
func TestDecodeCoreTypeSection(t *testing.T) {
	// A component: core type section (a rec group of 2, then a module type),
	// then an alias section with an outer core type alias.
	comp := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00,
		0x03, 0x0b, 0x02, // core type section, 11 bytes, 2 types
		0x4e, 0x02, 0x60, 0x00, 0x00, 0x60, 0x00, 0x00, // rec (func) (func)
		0x50, 0x00, // (module)
		0x06, 0x05, 0x01, 0x00, 0x10, 0x02, 0x00, // alias section: core type, outer... see below
	}
	// The alias section's alias names a core type of an enclosing component
	// that a standalone component does not have; only the space matters here.
	comp = comp[:len(comp)-7]
	comp = append(comp, 0x06, 0x06, 0x01, 0x00, 0x10, 0x02, 0x00, 0x00)
	c, err := Decode(bytes.NewReader(comp))
	if err != nil {
		t.Fatal(err)
	}
	want := []CoreTypeSpaceEntry{
		{Kind: CoreTypeFromDefinition, Def: 0, Member: 0},
		{Kind: CoreTypeFromDefinition, Def: 0, Member: 1},
		{Kind: CoreTypeFromDefinition, Def: 1, Member: 0},
		{Kind: CoreTypeFromAlias, Alias: 0},
	}
	if len(c.CoreTypes) != 2 || c.CoreTypes[1].Module == nil || len(c.CoreTypeSpace) != len(want) {
		t.Fatalf("CoreTypes %+v, CoreTypeSpace %+v", c.CoreTypes, c.CoreTypeSpace)
	}
	for i := range want {
		if c.CoreTypeSpace[i] != want[i] {
			t.Errorf("CoreTypeSpace[%d] = %+v, want %+v", i, c.CoreTypeSpace[i], want[i])
		}
	}

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"count", []byte{0x80}, "count"},
		{"definition", []byte{0x01, 0x7f}, "core type 0"},
		{"trailing", []byte{0x01, 0x60, 0x00, 0x00, 0x00}, "1 bytes left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeCoreTypeSection(tc.body, 0, uint32(len(tc.body)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, _, err := decodeCoreTypeSection([]byte{0x01}, 0, 5); err != ErrTruncatedBinary {
		t.Fatalf("a section past the end: %v", err)
	}
}

// A core type declared inside a component or instance type is decoded too.
func TestTypeDeclCoreType(t *testing.T) {
	// vec(instancedecl): one core:type, an empty module type.
	b, _, err := readTypeBody([]byte{0x01, 0x00, 0x50, 0x00}, 0, false)
	if decls := b.decls; err != nil || len(decls) != 1 || decls[0].CoreType == nil || decls[0].CoreType.Module == nil {
		t.Fatalf("decls %+v, %v", decls, err)
	}
	if _, _, err := readTypeBody([]byte{0x01, 0x00, 0x50, 0x01}, 0, false); err == nil {
		t.Fatal("a truncated module type was accepted")
	}
}

// A core module's exports are read off its export section alone; a module
// cut short anywhere on the way is refused.
func TestCoreModuleExports(t *testing.T) {
	head := []byte{0, 'a', 's', 'm', 1, 0, 0, 0}
	module := append(append([]byte{}, head...),
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section, stepped over
		0x07, 0x0a, 0x02, // export section: 2 exports
		0x01, 'f', 0x00, 0x00, // "f" func 0
		0x02, 'm', 'm', 0x02, // "mm" memory ...
		0x00, // ... 0
	)
	var got []string
	if err := CoreModuleExports(module, func(name []byte, kind byte) { got = append(got, string(name)+string('0'+kind)) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "f0,mm2" {
		t.Fatalf("exports %v", got)
	}
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"section size", []byte{0x07}, "section 7"},
		{"section past the end", []byte{0x07, 0x05, 0x00}, "section 7: unexpected end-of-file"},
		{"count", []byte{0x07, 0x01, 0x80}, "section 7: "},
		{"name length", []byte{0x07, 0x02, 0x01, 0x80}, "section 7 entry 0"},
		{"name past the end", []byte{0x07, 0x03, 0x01, 0x05, 'f'}, "section 7 entry 0: unexpected end-of-file"},
		{"index", []byte{0x07, 0x04, 0x01, 0x01, 'f', 0x00}, "section 7 entry 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CoreModuleExports(append(append([]byte{}, head...), tc.body...), func([]byte, byte) {})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if err := CoreModuleExports(head[:4], nil); err != ErrTruncatedBinary {
		t.Fatalf("a module shorter than its header: %v", err)
	}
}

func TestCoreModuleImports(t *testing.T) {
	// (module (import "m" "f" (func)) (import "" "g" (global i32)))
	mod := []byte{
		0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00, // type section: () -> ()
		0x02, 0x0d, 0x02,
		0x01, 'm', 0x01, 'f', 0x00, 0x00,
		0x00, 0x01, 'g', 0x03, 0x7f, 0x00,
	}
	var got []string
	if err := CoreModuleImports(mod, func(m, n []byte) { got = append(got, string(m)+":"+string(n)) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "m:f,:g" {
		t.Fatalf("imports = %v", got)
	}
	if err := CoreModuleImports(mod[:8], func([]byte, []byte) { t.Fatal("an import of a module with none") }); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mod  []byte
	}{
		{"count past the input", []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00, 0x02, 0x01, 0x80}},
		{"name past the input", []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00, 0x02, 0x03, 0x01, 0x05, 'm'}},
		{"desc past the input", []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00, 0x02, 0x05, 0x01, 0x01, 'm', 0x01, 'f'}},
		{"module name missing", []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00, 0x02, 0x01, 0x01}},
		{"section past the input", []byte{0x00, 'a', 's', 'm', 0x01, 0x00, 0x00, 0x00, 0x02, 0x09}},
	} {
		if err := CoreModuleImports(tc.mod, func([]byte, []byte) {}); err == nil {
			t.Errorf("%s: no error", tc.name)
		}
	}
}

func TestCoreModuleSectionHeader(t *testing.T) {
	section := func(mod ...byte) []byte {
		return append(append(preamble(), 0x01, byte(len(mod))), mod...)
	}
	for _, tc := range []struct {
		name, want string
		buf        []byte
	}{
		{"no magic", "magic header not detected", section(0x01, 0x02, 0x03, 0x04, 0x01, 0x00, 0x00, 0x00)},
		{"too short", "magic header not detected", section(0x00, 'a', 's', 'm')},
		{"a component", "expected a version header for a module", section(0x00, 'a', 's', 'm', 0x0d, 0x00, 0x01, 0x00)},
		{"version 2", "unknown binary version: 0x2", section(0x00, 'a', 's', 'm', 0x02, 0x00, 0x00, 0x00)},
	} {
		_, err := Decode(bytes.NewReader(tc.buf))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name, want string
		header     []byte
	}{
		{"a core module", "expected a version header for a component", []byte{0x01, 0x00, 0x00, 0x00}},
		{"version 0xc", "unknown binary version: 0xc", []byte{0x0c, 0x00, 0x01, 0x00}},
	} {
		_, err := Decode(bytes.NewReader(append([]byte{0x00, 'a', 's', 'm'}, tc.header...)))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := Decode(bytes.NewReader(append(preamble(), 0x00, 0x02, 0x01, 0xff))); err == nil || !strings.Contains(err.Error(), "malformed UTF-8 encoding") {
		t.Errorf("custom section name: err = %v", err)
	}
}
