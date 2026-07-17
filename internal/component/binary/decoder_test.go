package binary

import (
	"bytes"
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
	data, err := fixtureFS.ReadFile("testdata/host_component.wasm")
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
	data, err := fixtureFS.ReadFile("testdata/rich_component.wasm")
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

// real_hello.component.wasm is a genuine wasm32-wasip2 `wasi:cli/command`
// component produced by rustc/cargo-component (prints "hello world"), not a
// synthetic fixture assembled from a hand-written .wat. A copy is vendored in
// binary/testdata (embedded via fixtureFS) so the compiled test binary can read
// it from the repo root, as the scratch/BSD CI jobs run it.
//
// Ground truth per `wasm-tools component wit` / `wasm-tools objdump`:
//   - 10 component imports, all instance-sort (0x05): wasi:cli/{environment,
//     exit,stdin,stdout,stderr}, wasi:io/{error,streams},
//     wasi:clocks/wall-clock, wasi:filesystem/{types,preopens} (each @0.2.3).
//   - 1 component export, instance-sort (0x05): wasi:cli/run@0.2.3.
//   - 4 core modules (the main guest module plus three adapter/shim modules
//     produced by the component tooling), each starting with the core-wasm
//     magic number.
//   - 20 canonical-function definitions: 1 lift (kind 0x00), 15 lowers (kind
//     0x01), 4 resource.drop (kind 0x03).
//   - A trailing nested component definition (section id 4, the adapter
//     shim) that this decoder does not fully parse and records as a
//     RawSection with an exact byte range instead.
func TestDecodeRealGuest(t *testing.T) {
	data, err := fixtureFS.ReadFile("testdata/real_hello.component.wasm")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode real_hello.component.wasm: %v", err)
	}

	wantImports := []string{
		"wasi:cli/environment@0.2.3",
		"wasi:cli/exit@0.2.3",
		"wasi:io/error@0.2.3",
		"wasi:io/streams@0.2.3",
		"wasi:cli/stdin@0.2.3",
		"wasi:cli/stdout@0.2.3",
		"wasi:cli/stderr@0.2.3",
		"wasi:clocks/wall-clock@0.2.3",
		"wasi:filesystem/types@0.2.3",
		"wasi:filesystem/preopens@0.2.3",
	}
	if len(c.Imports) != len(wantImports) {
		t.Fatalf("imports: got %d, want %d (%+v)", len(c.Imports), len(wantImports), c.Imports)
	}
	for i, want := range wantImports {
		if c.Imports[i].Name != want {
			t.Errorf("import[%d] name: got %q, want %q", i, c.Imports[i].Name, want)
		}
		if c.Imports[i].ExternType != 0x05 { // instance
			t.Errorf("import[%d] %q sort: got %#x, want 0x05 (instance)", i, c.Imports[i].Name, c.Imports[i].ExternType)
		}
	}

	if len(c.Exports) != 1 {
		t.Fatalf("exports: got %d, want 1 (%+v)", len(c.Exports), c.Exports)
	}
	if c.Exports[0].Name != "wasi:cli/run@0.2.3" {
		t.Errorf("export name: got %q, want %q", c.Exports[0].Name, "wasi:cli/run@0.2.3")
	}
	if c.Exports[0].ExternType != 0x05 { // instance
		t.Errorf("export sort: got %#x, want 0x05 (instance)", c.Exports[0].ExternType)
	}

	if len(c.CoreModules) != 4 {
		t.Fatalf("core modules: got %d, want 4 (%+v)", len(c.CoreModules), c.CoreModules)
	}
	coreMagic := []byte{0x00, 0x61, 0x73, 0x6d}
	for i, m := range c.CoreModules {
		if m.Offset+4 > len(data) || !bytes.Equal(data[m.Offset:m.Offset+4], coreMagic) {
			t.Errorf("core module[%d] at offset %d does not start with core-wasm magic", i, m.Offset)
		}
	}

	var lifts, lowers, resourceDrops int
	for _, cn := range c.Canons {
		switch cn.Kind {
		case 0x00:
			lifts++
		case 0x01:
			lowers++
		case 0x03:
			resourceDrops++
		}
	}
	if lifts != 1 {
		t.Errorf("canon lifts: got %d, want 1", lifts)
	}
	if lowers != 15 {
		t.Errorf("canon lowers: got %d, want 15", lowers)
	}
	if resourceDrops != 4 {
		t.Errorf("canon resource.drops: got %d, want 4", resourceDrops)
	}
	if len(c.Canons) != lifts+lowers+resourceDrops {
		t.Errorf("canons: got %d total, want exactly lift+lower+resource.drop (%d)", len(c.Canons), lifts+lowers+resourceDrops)
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
