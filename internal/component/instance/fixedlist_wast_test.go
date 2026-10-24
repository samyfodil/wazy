package instance

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// TestWasmToolsFixedLengthLists runs wasm-tools' fixed-length-list cases
// (testdata/wast/wasm-tools-fixed-length-lists): each module instantiates and
// its export, whose core func is `unreachable`, gets as far as the trap --
// so the list<u32, 4> was lifted from memory, or lowered to four i32s, on
// the way -- and each component a validator must refuse is refused for the
// reason the suite gives. The last two instantiate an IMPORTED component with
// a type argument that does not match its import's (eq (list s32 10))
// bound; they are refused by the instantiation type check (ctenv.go) before
// anything is looked up for the import, as a validator refuses them.
func TestWasmToolsFixedLengthLists(t *testing.T) {
	const dir = "testdata/wast/wasm-tools-fixed-length-lists"
	fetched(t, dir)
	raw, err := wastFS.ReadFile(path.Join(dir, "fixed-length-lists.json"))
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
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	calls := map[string][]abi.Value{
		"fixed-length-lists.0.wasm": nil,
		"fixed-length-lists.1.wasm": {[]uint32{1, 2, 3, 4}},
	}
	exports := map[string]string{
		"fixed-length-lists.0.wasm": "ret-list",
		"fixed-length-lists.1.wasm": "param-list",
	}
	seen := 0
	for _, c := range manifest.Commands {
		if c.Type == "assert_malformed" {
			continue // a text-format case: there is no binary to decode
		}
		wasm, err := wastFS.ReadFile(path.Join(dir, c.Filename))
		if err != nil {
			t.Fatal(err)
		}
		seen++
		in, err := Instantiate(ctx, r, wasm)
		switch c.Type {
		case "module":
			if err != nil {
				t.Fatalf("line %d: %v", c.Line, err)
			}
			_, err := in.Call(ctx, exports[c.Filename], calls[c.Filename]...)
			if err == nil || !strings.Contains(err.Error(), "unreachable") {
				t.Errorf("line %d: %s: %v, want the core func's unreachable", c.Line, exports[c.Filename], err)
			}
			in.Close(ctx)
		case "assert_invalid":
			if err == nil {
				in.Close(ctx)
				t.Errorf("line %d: instantiated; want it refused (%s)", c.Line, c.Text)
				continue
			}
			want := c.Text
			if !strings.Contains(want, "type mismatch") {
				// wasm-tools words these its own way; what matters is the
				// reason: no elements, or too many bytes.
				want = map[bool]string{true: "more than zero elements", false: "exceeds maximum byte size"}[strings.Contains(c.Text, "zero")]
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("line %d: %v, want %q", c.Line, err, want)
			}
		}
	}
	if seen != 6 {
		t.Fatalf("ran %d commands, want 6", seen)
	}
}
