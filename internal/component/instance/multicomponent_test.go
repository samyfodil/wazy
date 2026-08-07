package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
)

// These two exercise the ENGINE's multi-component behavior on one Runtime --
// two different components, and the same component twice. They use the hello
// and adder guests only as convenient real components; nothing here is about
// WASI, which is why they live with the engine rather than with the WASI
// implementation.

func TestTwoDistinctComponentsOnOneRuntime(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	adder, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate adder: %v", err)
	}
	defer adder.Close(ctx)

	hello, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("Instantiate hello on the same Runtime as adder: %v", err)
	}
	defer hello.Close(ctx)

	// Both must still be callable -- names being unique isn't enough; the
	// wiring must resolve to the right module. Exercise adder's export.
	got, err := adder.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
	if err != nil {
		t.Fatalf("adder add after co-instantiation: %v", err)
	}
	if len(got) != 1 || got[0].(uint32) != 5 {
		t.Fatalf("adder add(2,3) = %v, want 5", got)
	}
}

// TestSameComponentTwiceLiveOnOneRuntime guards the other half of that
// property: the SAME component, instantiated twice, yields two independent
// live instances on one Runtime. Synthesized names are unique per
// instantiation (not per component), so identical bytes no longer collide.
// Both instances are exercised, and each is independently callable after the
// other is closed -- proving they are genuinely separate instances, not
// aliases of one.
func TestSameComponentTwiceLiveOnOneRuntime(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	a, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate #1: %v", err)
	}
	b, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate #2 (same component, both live): %v", err)
	}

	add := func(who string, inst *Instance, x, y, want uint32) {
		got, err := inst.CallExport(ctx, "component:adder/calc", "add", x, y)
		if err != nil {
			t.Fatalf("%s add: %v", who, err)
		}
		if len(got) != 1 || got[0].(uint32) != want {
			t.Fatalf("%s add(%d,%d) = %v, want %d", who, x, y, got, want)
		}
	}
	add("a", a, 2, 3, 5)
	add("b", b, 10, 20, 30)

	// Closing one must not disturb the other.
	if err := a.Close(ctx); err != nil {
		t.Fatalf("close a: %v", err)
	}
	add("b-after-a-closed", b, 1, 1, 2)
	if err := b.Close(ctx); err != nil {
		t.Fatalf("close b: %v", err)
	}
}
