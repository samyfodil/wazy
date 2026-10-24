package wit_test

import (
	"bufio"
	"embed"
	"encoding/base64"
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/component/wit"
	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// The lexer and parser are checked against wit-parser itself: each input
// below has wit-parser's outcome (wasm-tools 1.262.0; see
// testdata/gen-probes.sh) -- accepted, or its error and where -- and must
// have the same one here. What parses is then resolved by component/wit,
// for the errors wit-parser makes past parsing.

// testdata is embedded: some CI jobs run a test binary from the module
// root, where a relative path would not find it.
//
//go:embed testdata
var testdata embed.FS

// TestParse_Probes runs testdata/probes, inputs written to reach the
// lexer's and parser's errors.
func TestParse_Probes(t *testing.T) {
	ran := 0
	for _, line := range readLines(t, "testdata/probes.txt") {
		name, want, _ := strings.Cut(line, "|")
		src, err := fs.ReadFile(testdata, path.Join("testdata/probes", name+".wit"))
		if err != nil {
			t.Fatal(err)
		}
		ran++
		t.Run(name, func(t *testing.T) { compare(t, name+".wit", string(src), want) })
	}
	if ran == 0 {
		t.Fatal("no probes")
	}
}

// TestParse_Mutations runs testdata/mutations.txt: a source that uses every
// construct, cut short and with a stray character put in at each token (see
// testdata/gen-mutations.py).
func TestParse_Mutations(t *testing.T) {
	results := map[string]string{}
	for _, line := range readLines(t, "testdata/mutations-result.txt") {
		name, want, _ := strings.Cut(line, "|")
		results[name] = want
	}
	ran := 0
	for _, line := range readLines(t, "testdata/mutations.txt") {
		name, b64, _ := strings.Cut(line, "|")
		src, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := results[name]
		if !ok {
			t.Fatalf("%s: no result (run testdata/gen-probes.sh)", name)
		}
		ran++
		t.Run(name, func(t *testing.T) { compare(t, "m.wit", string(src), want) })
	}
	if ran == 0 {
		t.Fatal("no mutations")
	}
}

// compare parses src and checks the outcome against wit-parser's: "OK", or
// "message|file:line:col" -- or "file:line:col: message|" when wit-parser
// shows the line no snippet.
func compare(t *testing.T, file, src, want string) {
	t.Helper()
	_, perr := ast.Parse(file, src)
	err := perr
	if err == nil {
		_, err = wit.Parse(file, src)
	}
	if want == "OK" {
		if err != nil {
			t.Fatalf("wit-parser accepts it: %v", err)
		}
		return
	}
	text, pos, _ := strings.Cut(want, "|")
	msg := text
	if pos != "" {
		msg = pos + ": " + text
	}
	var e *ast.Error
	switch {
	case err == nil:
		t.Fatalf("accepted; wit-parser rejects it: %s", msg)
	case errors.As(perr, &e):
		// A parse error: the same, at the same place.
		if perr.Error() != msg {
			t.Fatalf("error %q\nwant %q", perr, msg)
		}
	case !strings.Contains(err.Error(), text):
		t.Fatalf("error %q\nwant it to contain %q", err, text)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	f, err := testdata.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
