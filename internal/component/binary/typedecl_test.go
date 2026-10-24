package binary

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/component/testfixtures"
)

// Every declaration kind, parsed from a component type's body: a core type,
// a type, an outer type alias, an import of a func and an export of an
// abstract resource.
func TestComponentDescDecls(t *testing.T) {
	body := []byte{
		0x05,                   // five declarations
		0x00, 0x60, 0x00, 0x00, // core:type (func)
		0x01, 0x40, 0x00, 0x01, 0x00, // type (func) -- no params, no result
		0x02, 0x03, 0x02, 0x01, 0x00, // alias outer 1 0 (type)
		0x03, 0x00, 0x01, 'f', 0x01, 0x00, // import "f" (func (type 0))
		0x04, 0x00, 0x01, 'r', 0x03, 0x01, // export "r" (type (sub resource))
	}
	d, off, err := readComponenttypeDesc(body, 0)
	if err != nil || off != len(body) {
		t.Fatalf("read %d of %d bytes: %v", off, len(body), err)
	}
	decls := d.Decls
	want := []TypeDeclKind{TypeDeclCoreType, TypeDeclType, TypeDeclAlias, TypeDeclImport, TypeDeclExport}
	if len(decls) != len(want) {
		t.Fatalf("%d declarations, want %d", len(decls), len(want))
	}
	for i, d := range decls {
		if d.Kind != want[i] {
			t.Errorf("declaration %d is %#x, want %#x", i, d.Kind, want[i])
		}
	}
	if _, ok := decls[1].Type.(FuncDesc); !ok {
		t.Errorf("type declaration: %T, want a FuncDesc", decls[1].Type)
	}
	if a := decls[2].Alias; a.Sort != 0x03 || a.TargetKind != 0x02 || a.OuterCount != 1 || a.OuterIndex != 0 {
		t.Errorf("alias: %+v", a)
	}
	if d := decls[3]; d.Name != "f" || d.Desc != (ExternDesc{Sort: 0x01}) {
		t.Errorf("import: %+v", d)
	}
	if d := decls[4]; d.Name != "r" || d.Desc != (ExternDesc{Sort: 0x03}) {
		t.Errorf("export: %+v", d)
	}
}

func TestTypeDeclsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      []byte
		component bool
		want      string
	}{
		{"an instance type importing", []byte{0x01, 0x03, 0x00, 0x01, 'f', 0x01, 0x00}, false, "instancedecl[0]: instancedecl: invalid tag 0x3"},
		{"an unknown tag", []byte{0x01, 0x09}, true, "componentdecl[0]: instancedecl: invalid tag 0x9"},
		{"a missing declaration", []byte{0x01}, true, ErrTruncatedBinary.Error()},
		{"no count", nil, true, "EOF"},
		{"a bad deftype", []byte{0x01, 0x01, 0x6e}, true, "componentdecl[0]"},
		{"a bad export name", []byte{0x01, 0x04, 0x07}, true, "componentdecl[0]"},
		{"a bad externdesc", []byte{0x01, 0x04, 0x00, 0x01, 'f', 0x02}, false, "instancedecl[0]"},
		{"a bad alias", []byte{0x01, 0x02, 0x03, 0x09}, false, "instancedecl[0]: invalid target kind 0x9"},
		{"a bad core type", []byte{0x01, 0x00, 0x00}, true, "componentdecl[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readTypeBody(tc.body, 0, tc.component)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, _, err := readInstancetypeDesc([]byte{0x01, 0x09}, 0); err == nil {
		t.Error("an instance type with a bad declaration decoded")
	}
	if _, _, err := readComponenttypeDesc([]byte{0x01, 0x09}, 0); err == nil {
		t.Error("a component type with a bad declaration decoded")
	}
}

// An instance type keeps every declaration, the export of a func included,
// beside what the ABI resolves: its `eq` type export in Exports, and an
// outer type alias in Outer.
func TestInstancetypeDescDecls(t *testing.T) {
	body := []byte{
		0x04,                         // four declarations
		0x02, 0x03, 0x02, 0x01, 0x00, // alias outer 1 0 (type) -- local type 0
		0x01, 0x40, 0x00, 0x01, 0x00, // type (func) -- local type 1
		0x04, 0x00, 0x01, 't', 0x03, 0x00, 0x00, // export "t" (type (eq 0)) -- local type 2
		0x04, 0x00, 0x01, 'f', 0x01, 0x01, // export "f" (func (type 1))
	}
	d, off, err := readInstancetypeDesc(body, 0)
	if err != nil || off != len(body) {
		t.Fatalf("read %d of %d bytes: %v", off, len(body), err)
	}
	if len(d.Decls) != 4 || d.Decls[3].Name != "f" || d.Decls[3].Desc != (ExternDesc{Sort: 0x01, Index: 1}) {
		t.Fatalf("decls: %+v", d.Decls)
	}
	if len(d.Types) != 3 || d.Types[0] != nil || d.Types[2] != nil {
		t.Errorf("types: %#v", d.Types)
	}
	if ref, ok := d.Exports["t"]; !ok || ref.TypeIndex == nil || *ref.TypeIndex != 0 || len(d.Exports) != 1 {
		t.Errorf("exports: %+v", d.Exports)
	}
	if want := (OuterAlias{1, 0}); d.Outer[0] != want || d.Outer[2] != want {
		t.Errorf("outer: %+v", d.Outer)
	}
}

// A component import keeps the index of its declared component type, and a
// component type its declarations.
func TestDecode_ComponentImportType(t *testing.T) {
	b := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00,
		0x07, 0x05, 0x02, 0x42, 0x00, 0x41, 0x00, // type section: (instance) (component)
		0x0a, 0x06, 0x01, 0x00, 0x01, 'c', 0x04, 0x01, // import "c" (component (type 1))
	}
	c, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if im := c.Imports[0]; im.ExternType != 0x04 || im.ExternIndex != 1 {
		t.Fatalf("import: %+v, want a component import of type 1", im)
	}
	if d, ok := c.Types[1].Descriptor.(ComponentDesc); !ok || len(d.Decls) != 0 {
		t.Fatalf("type 1: %#v, want a ComponentDesc with an empty declaration list", c.Types[1].Descriptor)
	}
	if d, ok := c.Types[0].Descriptor.(InstanceDesc); !ok || len(d.Decls) != 0 {
		t.Errorf("type 0: %#v, want an InstanceDesc with an empty declaration list", c.Types[0].Descriptor)
	}
}

// wit-component ascribes a type to every export of the shims it nests in a
// component; the decoder keeps it.
func TestDecode_ExportAscription(t *testing.T) {
	c, err := Decode(bytes.NewReader(testfixtures.RealHello))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range c.NestedComponents {
		for _, ex := range n.Exports {
			if ex.Ascribed != nil && ex.Ascribed.Sort == ex.ExternType {
				return
			}
		}
	}
	t.Fatal("no export of a nested shim carries its ascribed type")
}

// An invalid ascription is refused, naming the export.
func TestDecode_ExportAscriptionInvalid(t *testing.T) {
	b := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00,
		0x0b, 0x06, 0x01, 0x00, 0x01, 'f', 0x01, 0x00, // export "f" (func 0) -- then:
	}
	b = append(b, 0x01, 0x02) // an ascription of the value sort
	b[9] = 0x08
	if _, err := Decode(bytes.NewReader(b)); err == nil || !strings.Contains(err.Error(), "export[0] ascribed type") {
		t.Fatalf("got %v, want the ascription refused", err)
	}
}

// readAliasDef's errors, through the alias section.
func TestReadAliasDefErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  []byte
	}{
		{"no sort", nil},
		{"no core sort", []byte{0x00}},
		{"no target", []byte{0x03}},
		{"no outer index", []byte{0x03, 0x02, 0x01}},
	} {
		if _, _, err := readAliasDef(tc.buf, 0); err == nil {
			t.Errorf("%s: no error", tc.name)
		} else if tc.buf == nil && !errors.Is(err, ErrTruncatedBinary) {
			t.Errorf("%s: %v, want ErrTruncatedBinary", tc.name, err)
		}
	}
}

// readInstanceDeclDesc and readComponentDecl read one declaration of an
// instance or a component type, for the tests of the grammar of one.
func readInstanceDeclDesc(buf []byte, off int) (int, error) {
	var b typeBody
	return b.read(buf, off, false)
}

func readComponentDecl(buf []byte, off int) (int, error) {
	var b typeBody
	return b.read(buf, off, true)
}
