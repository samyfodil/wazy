package component_test

import (
	"context"
	_ "embed"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/wit"
)

// A real guest that passes fixed-length lists both ways. It is a Rust
// wasm32-wasip2 component built with wit-bindgen 0.63.0 in the pinned build
// image (testdata/fixedlistguest), against testdata/fixedlistguest/wit; the
// host below registers every import from that same WIT, parsed, so the
// guest's generated bindings meet descriptors no one wrote by hand.
//
//go:embed testdata/fixedlistguest.wasm
var fixedListGuestWasm []byte

//go:embed testdata/fixedlistguest/wit/fixed.wit
var fixedListGuestWIT string

// fixedListHost implements wazy:fixed/host: reverse a list<u32, 4>, greet
// a name with a list<string, 2>, shift a list<point, 2>.
var fixedListHost = map[string]component.HostFunc{
	"reverse": func(_ context.Context, args []component.Value) ([]component.Value, error) {
		v, err := component.ListOf[uint32](args[0])
		if err != nil {
			return nil, err
		}
		out := slices.Clone(v)
		slices.Reverse(out)
		return []component.Value{out}, nil
	},
	"greet": func(_ context.Context, args []component.Value) ([]component.Value, error) {
		name := args[0].(string)
		return []component.Value{[]component.Value{"hello, " + name, "bye, " + name}}, nil
	},
	"shift": func(_ context.Context, args []component.Value) ([]component.Value, error) {
		points, err := component.ListOf[component.Value](args[0])
		if err != nil {
			return nil, err
		}
		by := args[1].(int32)
		out := make([]component.Value, len(points))
		for i, p := range points {
			xy := p.([]component.Value)
			out[i] = []component.Value{xy[0].(int32) + by, xy[1].(int32) + by}
		}
		return []component.Value{out}, nil
	},
}

func TestFixedListGuest(t *testing.T) {
	pkg, err := wit.Parse("fixed.wit", fixedListGuestWIT)
	if err != nil {
		t.Fatal(err)
	}
	host := pkg.Interface("host")
	var opts []component.Option
	for _, f := range host.Funcs {
		sig, err := f.Signature(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, component.WithImportCustom(host.Name, f.Name, fixedListHost[f.Name], sig.Desc, sig.Types.Resolver()))
	}

	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)
	inst, err := component.Instantiate(ctx, r, fixedListGuestWasm, opts...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	const api = "wazy:fixed/api@0.1.0"
	wide := make([]uint64, 20)
	var wantWide uint64
	for i := range wide {
		wide[i] = uint64(i+1) << 40
		wantWide = wantWide*31 + wide[i]
	}
	for _, tc := range []struct {
		fn   string
		args []component.Value
		want component.Value
	}{
		// Reversed by the host, each plus one by the guest.
		{"run", []component.Value{[]uint32{1, 2, 3, 0xfffffffe}}, []uint32{0xffffffff, 4, 3, 2}},
		// Either shape lowers: the []Value one as well as the typed one.
		{"run", []component.Value{[]component.Value{uint32(7), uint32(8), uint32(9), uint32(10)}}, []uint32{11, 10, 9, 8}},
		// The guest greets the first name and bids the second farewell.
		{"names", []component.Value{[]component.Value{"wazy", "wit"}}, []component.Value{"hello, wazy", "bye, wit"}},
		{"points", []component.Value{[]component.Value{
			[]component.Value{int32(1), int32(2)},
			[]component.Value{int32(-30), int32(4)},
		}}, []component.Value{
			[]component.Value{int32(11), int32(12)},
			[]component.Value{int32(-20), int32(14)},
		}},
		{"matrix", []component.Value{[]component.Value{[]byte{1, 2, 3}, []byte{4, 5, 6}}}, []component.Value{[]byte{6, 5, 4}, []byte{3, 2, 1}}},
		// Twenty u64s spill the params to memory.
		{"wide", []component.Value{wide}, wantWide},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			got, err := inst.CallExport(ctx, api, tc.fn, tc.args...)
			if err != nil {
				t.Fatalf("%s: %v", tc.fn, err)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("%s = %s, want %s", tc.fn, show(got), show([]component.Value{tc.want}))
			}
		})
	}

	// A list of the wrong length is the caller's error, not a trap.
	if _, err := inst.CallExport(ctx, api, "run", []uint32{1, 2, 3}); err == nil {
		t.Error("run took a list of 3 as a list<u32, 4>")
	}
}

func show(v []component.Value) string { return fmt.Sprintf("%#v", v) }
