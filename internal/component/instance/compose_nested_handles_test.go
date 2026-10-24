package instance

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

// Resource handles nested inside composites crossing guest-to-guest imports:
// own and borrow, as args and results, sync and async lowered, in a record, a
// tuple, an option<result<..>> and lists. See the .wat for the shape and the
// checksums.
//
//go:embed testdata/compose_nested_handles.wasm
var composeNestedHandlesWasm []byte

func TestComposeNestedHandles(t *testing.T) {
	for _, tc := range []struct {
		export string
		want   uint32
		err    string
	}{
		{export: "own-args", want: (11+20+70)*100 + 5},
		{export: "own-results", want: (15+7+9)*100 + 4},
		{export: "borrow-owner", want: (18+5)*100 + 3},
		{export: "borrow-other", want: (100+11+12+11)*100 + 2},
		{export: "async", want: (3+5+4)*1000 + 2*uint32(subtaskReturned)*10 + 3},
		{export: "bad-arg", err: "param 0: unknown handle index 999"},
		{export: "bad-result", err: "unknown handle index 99 "},
		{export: "leak", err: "borrow handles still remain"},
		{export: "bad-arg-async", err: "param 0: unknown handle index 999"},
		{export: "bad-result-async", err: "unknown handle index 99 "},
	} {
		t.Run(tc.export, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)
			in, err := Instantiate(ctx, r, composeNestedHandlesWasm)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer in.Close(ctx) //nolint:errcheck // test cleanup
			out, err := in.Call(ctx, tc.export)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("%s = %v, %v; want an error containing %q", tc.export, out, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: %v", tc.export, err)
			}
			if got := out[0].(uint32); got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.export, got, tc.want)
			}
		})
	}
}
