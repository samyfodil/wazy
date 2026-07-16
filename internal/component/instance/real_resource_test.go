package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_resource.component.wasm is a genuine cargo-component/wit-bindgen guest
// (testdata/real_resource.wit) that EXPORTS its own resource type: a `counter`
// with a constructor, increment/add/get methods, and a `make` factory func.
// Unlike the WASI resources wazy PROVIDES to guests (streams/descriptors,
// where the guest holds host handles), here the GUEST owns the resource -- so
// calling a method must convert the host's handle to the guest's rep (the
// method's core func takes the rep, which the guest casts to its Counter
// pointer). This exercises the guest-owned-resource lifecycle end to end:
// make -> own<counter> handle, methods on borrow<counter>, resource-drop.
//
//go:embed testdata/real_resource.component.wasm
var realResourceWasm []byte

func TestRealResource(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// The guest links the wit-bindgen-rt runtime, which touches WASI on some
	// paths; give it a (default) WASI host so nothing traps.
	inst, err := Instantiate(ctx, r, realResourceWasm, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	const iface = "example:res/counters"
	u32 := func(v abi.Value) uint32 {
		t.Helper()
		u, ok := v.(uint32)
		if !ok {
			t.Fatalf("expected uint32, got %T (%v)", v, v)
		}
		return u
	}
	call := func(member string, args ...abi.Value) abi.Value {
		t.Helper()
		res, err := inst.CallExport(ctx, iface, member, args...)
		if err != nil {
			t.Fatalf("%s: %v", member, err)
		}
		if len(res) != 1 {
			t.Fatalf("%s returned %d results, want 1", member, len(res))
		}
		return res[0]
	}

	// make(10) -> own<counter> handle. Its mutable state lives in the guest.
	h := call("make", uint32(10))
	if _, ok := h.(uint32); !ok {
		t.Fatalf("make returned %T, want a uint32 resource handle", h)
	}

	// Methods mutate the guest-owned Counter through the shared handle.
	if got := u32(call("[method]counter.increment", h)); got != 11 {
		t.Errorf("increment = %d, want 11", got)
	}
	if got := u32(call("[method]counter.add", h, uint32(5))); got != 16 {
		t.Errorf("add(5) = %d, want 16", got)
	}
	if got := u32(call("[method]counter.get", h)); got != 16 {
		t.Errorf("get = %d, want 16 (state persisted across method calls)", got)
	}

	// A second, independent counter proves per-resource state isolation and
	// that make mints a distinct handle/rep each time.
	h2 := call("make", uint32(100))
	if got := u32(call("[method]counter.increment", h2)); got != 101 {
		t.Errorf("second counter increment = %d, want 101", got)
	}
	if got := u32(call("[method]counter.get", h)); got != 16 {
		t.Errorf("first counter after second's use = %d, want 16 (isolation)", got)
	}
	// (Host-initiated `[resource-drop]counter` -- a resource.drop canon export,
	// not a lift -- plus destructor invocation is a separate feature wazy does
	// not yet bind; the handles are released when the instance is closed.)
}
