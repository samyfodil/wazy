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

// TestMaxValueSize runs the component-model's and wasm-tools'
// max-value-size suites (testdata/wast): each module decodes, and each
// assert_invalid is refused as exceeding the maximum byte size.
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
				switch {
				case c.Type == "module" && err != nil:
					t.Errorf("line %d: %v", c.Line, err)
				case c.Type != "assert_invalid":
				case err == nil:
					t.Errorf("line %d: decoded; want it refused (%s)", c.Line, c.Text)
				case !strings.Contains(err.Error(), c.Text):
					t.Errorf("line %d: %v, want %q", c.Line, err, c.Text)
				}
			}
		})
	}
}
