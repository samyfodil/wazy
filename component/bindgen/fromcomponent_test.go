package bindgen

import (
	"embed"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/component/wit"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// testdata/fromcomponent holds components, and for each the WIT
// `wasm-tools component wit` (1.253, or 1.262 for implements, which 1.253
// does not write) prints for it: the oracle.
//
//go:embed testdata/fromcomponent internal/kitchen/kitchen.wasm internal/storebind/store.wasm
var fromComponent embed.FS

// The WIT read from a compiled component binds exactly as the WIT
// wasm-tools reads from it does: every interface, func, type and resource,
// with WASI bound too.
func TestFromComponentMatchesWasmTools(t *testing.T) {
	for _, c := range []struct{ name, wasm string }{
		{"kitchen", "internal/kitchen/kitchen.wasm"},
		{"store", "internal/storebind/store.wasm"},
		{"hello", "testdata/fromcomponent/hello.wasm"},
		{"otelguest", "testdata/fromcomponent/otelguest.wasm"},
		{"http_root", "testdata/fromcomponent/http_root.wasm"},
		{"fixedlistguest", "testdata/fromcomponent/fixedlistguest.wasm"},
		{"async", "testdata/fromcomponent/async.wasm"},
		{"shapes", "testdata/fromcomponent/shapes.wasm"},
		{"implements", "testdata/fromcomponent/implements.wasm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			wasm, err := fromComponent.ReadFile(c.wasm)
			require.NoError(t, err)
			oracle, err := fromComponent.ReadFile("testdata/fromcomponent/" + c.name + ".wit")
			require.NoError(t, err)
			ours, err := wit.FromComponent(wasm)
			require.NoError(t, err)
			theirs, err := wit.Parse(c.name+".wit", string(oracle))
			require.NoError(t, err)
			for _, skip := range [][]string{nil, {}} {
				opts := Options{Package: "p", World: "root", Skip: skip}
				want, wantErr := Generate(theirs, opts)
				got, gotErr := Generate(ours, opts)
				if wantErr != nil {
					require.EqualError(t, gotErr, wantErr.Error())
					continue
				}
				require.NoError(t, gotErr)
				if string(got) != string(want) {
					t.Fatalf("skip %q: bindings differ from wasm-tools' WIT's:\n%s", skip, firstDiff(string(want), string(got)))
				}
			}
		})
	}
}

// firstDiff shows where two texts first differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "line " + strconv.Itoa(i+1) + ":\n- " + al[i] + "\n+ " + bl[i]
		}
	}
	return "one is a prefix of the other"
}
