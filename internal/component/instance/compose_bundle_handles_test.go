package instance

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// compose_nested_handles' crossings, with P's and Q's interfaces passed to
// their importers as inline bundles and as instances of pure re-export shims:
// own and borrow handles in composites, into P, which defines r, and into Q,
// which does not. The same crossings through the instances themselves are
// the control, and are repeated with Q's interface passed before P's, whose
// resource Q's funcs take. See the .wat for the shape.
//
//go:embed testdata/compose_bundle_handles.wasm
var composeBundleHandlesWasm []byte

func TestComposeBundleHandles(t *testing.T) {
	cases := []struct {
		export string
		want   uint32
		err    string
	}{
		{export: "own-args", want: (11+20+70)*100 + 5},
		{export: "own-results", want: (15+7+9)*100 + 4},
		{export: "borrow-owner", want: (18+5)*100 + 3},
		{export: "borrow-other", want: (100+11+12+11)*100 + 2},
		{export: "own-other", want: (100+21+22)*100 + 2},
		{export: "borrow-top-other", want: (1000+7)*100 + 1},
		{export: "async", want: (3+5+4)*1000 + 2*uint32(subtaskReturned)*10 + 3},
		{export: "leak", err: "borrow handles still remain"},
	}
	for _, via := range []string{"direct", "reversed", "bundle", "shim"} {
		for _, tc := range cases {
			export := via + "-" + tc.export
			t.Run(export, func(t *testing.T) {
				ctx := context.Background()
				r := wazy.NewRuntime(ctx)
				defer r.Close(ctx)
				in, err := Instantiate(ctx, r, composeBundleHandlesWasm)
				if err != nil {
					t.Fatalf("Instantiate: %v", err)
				}
				defer in.Close(ctx) //nolint:errcheck // test cleanup
				out, err := in.Call(ctx, export)
				if tc.err != "" {
					if err == nil || !strings.Contains(err.Error(), tc.err) {
						t.Fatalf("%s = %v, %v; want an error containing %q", export, out, err, tc.err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s: %v", export, err)
				}
				if got := out[0].(uint32); got != tc.want {
					t.Fatalf("%s = %d, want %d", export, got, tc.want)
				}
			})
		}
	}
}

// A resource and the funcs using it passed to a nested component as separate
// type and func args, the type also aliased back out of a bundle and out of a
// shim instance: the handle $C gets from $P is in $C's table under $C's tag,
// and dropping it runs $P's destructor.
//
//go:embed testdata/compose_type_arg_handles.wasm
var composeTypeArgHandlesWasm []byte

func TestComposeTypeArgHandles(t *testing.T) {
	for _, export := range []string{"run", "run-bundle", "run-shim"} {
		t.Run(export, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)
			in, err := Instantiate(ctx, r, composeTypeArgHandlesWasm)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close(ctx) //nolint:errcheck // test cleanup
			if out, err := in.Call(ctx, export); err != nil || len(out) != 1 || out[0] != uint32(501) {
				t.Fatalf("%s = %v, %v; want [501]", export, out, err)
			}
		})
	}
}

// A host resource passed on in an inline bundle and in a shim instance: the
// handle the host mints for $C carries the host's tag in $C's table, so $C's
// borrow reaches the host as the rep and its drop runs the host destructor.
//
//go:embed testdata/compose_host_bundle.wasm
var composeHostBundleWasm []byte

func TestComposeHostResourceThroughBundle(t *testing.T) {
	const tag uint32 = 0x000F_0001
	u32 := binary.PrimitiveDesc{Prim: "u32"}
	for _, export := range []string{"bundle-run", "shim-run"} {
		t.Run(export, func(t *testing.T) {
			var dropped []uint32
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)
			in, err := Instantiate(ctx, r, composeHostBundleWasm,
				WithResourceTag("acme:api/host", "thing", tag),
				WithImport("acme:api/host", "make", func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
					return []abi.Value{args[0]}, nil
				}, []binary.TypeDesc{u32}, []binary.TypeDesc{binary.OwnDesc{ResourceType: tag}}),
				WithImport("acme:api/host", "peek", func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
					return []abi.Value{args[0].(uint32) + 1}, nil
				}, []binary.TypeDesc{binary.BorrowDesc{ResourceType: tag}}, []binary.TypeDesc{u32}),
				WithHostResourceDtor(tag, func(_ context.Context, rep uint32) error {
					dropped = append(dropped, rep)
					return nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close(ctx) //nolint:errcheck // test cleanup
			if out, err := in.Call(ctx, export); err != nil || len(out) != 1 || out[0] != uint32(10) {
				t.Fatalf("%s = %v, %v; want [10]", export, out, err)
			}
			if !slices.Equal(dropped, []uint32{9}) {
				t.Fatalf("host destructor ran for %v, want [9]", dropped)
			}
		})
	}
}
