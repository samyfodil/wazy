package binary

import (
	"bytes"
	"embed"
	"encoding/json"
	"path"
	"strings"
	"testing"
)

//go:embed testdata/wast
var validationFS embed.FS

// valueSizeGaps are the assert_invalid components of the vendored
// max-value-size suites that still decode here, with why. The test fails
// when one starts being refused, so the list has to shrink with it.
var valueSizeGaps = map[string]string{
	// The oversized type comes in as an export of an imported instance's
	// type, which this decoder does not resolve structurally (see
	// Component.ResolveType), so its size is not known.
	"wasm-tools-max-value-size/max-value-size.3.wasm": "imported instance's type export",
	// The oversized type is declared inside a component type, whose
	// declarations this decoder does not represent (see ComponentDesc).
	"wasm-tools-max-value-size/max-value-size.5.wasm": "component type declaration",
}

// TestMaxValueSize runs the component-model's and wasm-tools'
// max-value-size suites (testdata/wast): each module decodes, and each
// assert_invalid is refused as exceeding the maximum byte size, but for
// valueSizeGaps.
func TestMaxValueSize(t *testing.T) {
	for _, suite := range []string{"component-model-max-value-size", "wasm-tools-max-value-size"} {
		t.Run(suite, func(t *testing.T) {
			dir := path.Join("testdata/wast", suite)
			raw, err := validationFS.ReadFile(path.Join(dir, "max-value-size.json"))
			if err != nil {
				t.Fatal(err)
			}
			var manifest struct {
				Commands []struct {
					Type     string `json:"type"`
					Line     int    `json:"line"`
					Filename string `json:"filename"`
					Text     string `json:"text"`
				} `json:"commands"`
			}
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			for _, c := range manifest.Commands {
				wasm, err := validationFS.ReadFile(path.Join(dir, c.Filename))
				if err != nil {
					t.Fatal(err)
				}
				_, err = Decode(bytes.NewReader(wasm))
				key := suite + "/" + c.Filename
				_, gap := valueSizeGaps[key]
				switch {
				case c.Type == "module" && err != nil:
					t.Errorf("line %d: %v", c.Line, err)
				case c.Type != "assert_invalid":
				case gap && err == nil:
				case gap:
					t.Errorf("line %d: now refused (%v); drop %s from valueSizeGaps", c.Line, err, key)
				case err == nil:
					t.Errorf("line %d: decoded; want it refused (%s)", c.Line, c.Text)
				case !strings.Contains(err.Error(), c.Text):
					t.Errorf("line %d: %v, want %q", c.Line, err, c.Text)
				}
			}
		})
	}
}
