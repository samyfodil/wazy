package binary

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// log_hello.wasm exercises the core-instance decode paths that a lowered import
// requires: the instantiate form (moduleidx + core:name instantiate args) and
// the inline-export form (vec of core:name -> core:sortidx). These are the
// paths whose decoders were fixed to use core-level name/sortidx readers.
func TestDecodeInstantiationGraph_CoreInstanceForms(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "log_hello.wasm"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(c.CoreInstances) == 0 {
		t.Fatal("expected core instances")
	}
	var sawInstantiate, sawInlineExport, sawInstArg bool
	for _, ci := range c.CoreInstances {
		switch ci.Kind {
		case 0x00: // instantiate form
			sawInstantiate = true
			if len(ci.Args) > 0 {
				sawInstArg = true
			}
		case 0x01: // inline export form
			sawInlineExport = true
			for _, e := range ci.Exports {
				if e.Name == "" {
					t.Error("inline export decoded with empty core:name")
				}
			}
		}
	}
	if !sawInstantiate || !sawInstArg {
		t.Errorf("expected an instantiate core instance with args; got %+v", c.CoreInstances)
	}
	if !sawInlineExport {
		t.Errorf("expected an inline-export core instance; got %+v", c.CoreInstances)
	}
	// A lowered import must be present (canon lower) for this fixture.
	var sawLower bool
	for _, cn := range c.Canons {
		if cn.Kind == 0x01 {
			sawLower = true
		}
	}
	if !sawLower {
		t.Error("expected a canon lower in log_hello")
	}
}
