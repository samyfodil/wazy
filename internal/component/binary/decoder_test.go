package binary

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// host_component.wasm is a real component assembled by wasm-tools from
// testdata/host_component.wat. Ground truth (per `wasm-tools print`):
//
//	(type (;0;) (instance (func "log"(string)) (func "level"()->u32)))
//	(import "test:pkg/host" (instance (type 0)))
//	(type (;1;) (func (result u32)))
//	(export "run" (func ...))
//
// i.e. 2 component types (an instance type then a func type), 1 import
// (instance sort 0x05), 1 export (func sort 0x01).
func loadFixture(t *testing.T) *Component {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "host_component.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode real component: %v", err)
	}
	return c
}

func TestDecodeRealComponent_Preamble(t *testing.T) {
	// A core module must be rejected: same magic, but version 0x01 layer 0x00.
	core := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if _, err := Decode(bytes.NewReader(core)); err == nil {
		t.Fatal("expected core module (version 0x01) to be rejected")
	}
}

func TestDecodeRealComponent_Imports(t *testing.T) {
	c := loadFixture(t)
	if len(c.Imports) != 1 {
		t.Fatalf("imports: got %d, want 1", len(c.Imports))
	}
	imp := c.Imports[0]
	if imp.Name != "test:pkg/host" {
		t.Errorf("import name: got %q, want %q", imp.Name, "test:pkg/host")
	}
	if imp.ExternType != 0x05 { // instance
		t.Errorf("import sort: got %#x, want 0x05 (instance)", imp.ExternType)
	}
}

func TestDecodeRealComponent_Exports(t *testing.T) {
	c := loadFixture(t)
	if len(c.Exports) != 1 {
		t.Fatalf("exports: got %d, want 1", len(c.Exports))
	}
	exp := c.Exports[0]
	if exp.Name != "run" {
		t.Errorf("export name: got %q, want %q", exp.Name, "run")
	}
	if exp.ExternType != 0x01 { // func
		t.Errorf("export sort: got %#x, want 0x01 (func)", exp.ExternType)
	}
}

func TestDecodeRealComponent_Types(t *testing.T) {
	c := loadFixture(t)
	// Two component type sections: an instance type, then a func type.
	if len(c.Types) != 2 {
		t.Fatalf("types: got %d, want 2 (%+v)", len(c.Types), c.Types)
	}
	if c.Types[0].Kind != "instance" {
		t.Errorf("type[0] kind: got %q, want instance", c.Types[0].Kind)
	}
	if c.Types[1].Kind != "func" {
		t.Errorf("type[1] kind: got %q, want func", c.Types[1].Kind)
	}
	// Global indices must be assigned across sections.
	if c.Types[0].Index != 0 || c.Types[1].Index != 1 {
		t.Errorf("type indices: got %d,%d want 0,1", c.Types[0].Index, c.Types[1].Index)
	}
}

func TestDecodeRealComponent_Dump(t *testing.T) {
	c := loadFixture(t)
	var buf bytes.Buffer
	if err := c.Dump(&buf); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"test:pkg/host", "run", "instance", "func"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n---\n%s", want, out)
		}
	}
}

// rich_component.wasm exercises the full defvaltype grammar plus outer aliases
// inside an instance type. Ground truth: top-level types enum, record, option,
// result, list, variant, flags, then an instance type (with aliases) and a func.
func TestDecodeRichComponent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rich_component.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode rich component: %v", err)
	}
	wantKinds := []string{"enum", "record", "option", "result", "list", "variant", "flags", "instance", "func"}
	if len(c.Types) != len(wantKinds) {
		t.Fatalf("types: got %d, want %d (%+v)", len(c.Types), len(wantKinds), c.Types)
	}
	for i, want := range wantKinds {
		if c.Types[i].Kind != want {
			t.Errorf("type[%d] kind: got %q, want %q", i, c.Types[i].Kind, want)
		}
	}
	if len(c.Imports) != 1 || c.Imports[0].Name != "test:pkg/api" {
		t.Errorf("imports: got %+v, want one test:pkg/api", c.Imports)
	}
	if len(c.Exports) != 1 || c.Exports[0].Name != "run" {
		t.Errorf("exports: got %+v, want one run", c.Exports)
	}
}

func TestDecodeInvalidMagic(t *testing.T) {
	if _, err := Decode(bytes.NewReader([]byte{0x00, 0x01, 0x02, 0x03})); err == nil {
		t.Fatal("expected invalid magic to be rejected")
	}
}

func TestDecodeTruncated(t *testing.T) {
	if _, err := Decode(bytes.NewReader([]byte{0x00, 0x61, 0x73})); err == nil {
		t.Fatal("expected truncated preamble to be rejected")
	}
}
