package binary_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/binary"
	"github.com/samyfodil/wazy/internal/component/testfixtures"
	"github.com/samyfodil/wazy/internal/testing/dwarftestdata"
)

func TestDecode_WASIP1Module(t *testing.T) {
	m, err := binary.Decode(dwarftestdata.ZigCCWasm, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.ImportSection) != 1 {
		t.Fatalf("%d imports, want 1", len(m.ImportSection))
	}
	imp := m.ImportSection[0]
	if imp.Module != "wasi_snapshot_preview1" || imp.Name != "proc_exit" || imp.Type != api.ExternTypeFunc {
		t.Errorf("import %+v, want func wasi_snapshot_preview1.proc_exit", imp)
	}
	if ft := m.TypeSection[imp.DescFunc]; len(ft.Params) != 1 || len(ft.Results) != 0 {
		t.Errorf("proc_exit type %s, want one param and no result", ft.String())
	}
	var exports []string
	for _, e := range m.ExportSection {
		exports = append(exports, e.Name)
	}
	if fmt.Sprint(exports) != "[memory _start]" {
		t.Errorf("exports %v, want [memory _start]", exports)
	}
	if m.NameSection == nil || len(m.CustomSections) == 0 {
		t.Errorf("custom sections were dropped: name=%v, others=%d", m.NameSection != nil, len(m.CustomSections))
	}
}

func TestDecode_ZeroFeaturesMeansV2(t *testing.T) {
	// A data count section, which bulk memory (a 2.0 feature) introduced.
	dataCount := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x0c, 0x01, 0x00}
	if _, err := binary.Decode(dataCount, api.CoreFeaturesV1); err == nil {
		t.Fatal("decoded a data count section with only 1.0 features enabled")
	}
	if _, err := binary.Decode(dataCount, 0); err != nil {
		t.Fatalf("default features: %v", err)
	}
}

func TestDecode_Rejects(t *testing.T) {
	if _, err := binary.Decode(testfixtures.RealHello, 0); !errors.Is(err, binary.ErrInvalidVersion) {
		t.Errorf("component: err %v, want ErrInvalidVersion", err)
	}
	if _, err := binary.Decode([]byte("not wasm"), 0); !errors.Is(err, binary.ErrInvalidMagicNumber) {
		t.Errorf("garbage: err %v, want ErrInvalidMagicNumber", err)
	}
}

// Decode a wasip1 program and list what it imports and exports.
func Example() {
	wasm := dwarftestdata.ZigCCWasm // the bytes of a .wasm file

	m, err := binary.Decode(wasm, 0)
	if err != nil {
		panic(err)
	}
	for _, imp := range m.ImportSection {
		fmt.Println("import", imp.Module, imp.Name)
	}
	for _, exp := range m.ExportSection {
		fmt.Println("export", exp.Name)
	}
	// Output:
	// import wasi_snapshot_preview1 proc_exit
	// export memory
	// export _start
}
