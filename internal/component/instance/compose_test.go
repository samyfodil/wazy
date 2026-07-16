package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// TestMultiComponentComposition wires three real components together on one
// Runtime: A (the adder) is loaded; B is loaded with its import bound to call A;
// C is loaded with its import bound to call B (which itself calls A) AND to call
// A directly. Driving C therefore runs the full chain
//
//	C -> C's host import -> { B -> B's host import -> A } + { A directly }
//
// which is guest->host->guest->host->guest re-entrancy across three live,
// independent component instances. This is the composition property in full:
// many components coexist on one Runtime (anonymous internals, no name
// collisions) and one can call another, transitively, with the host bridging
// each component's import to another's export (wazy has no cross-component
// linker, so the host wires the calls -- the standard mechanism).
func TestMultiComponentComposition(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	// A: the adder, exports component:adder/calc.add(u32,u32)->u32.
	a, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	defer a.Close(ctx)
	aCalls := 0
	callA := func() {
		aCalls++
		res, err := a.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
		if err != nil {
			t.Fatalf("A.add: %v", err)
		}
		if got, ok := res[0].(uint32); !ok || got != 5 {
			t.Fatalf("A.add(2,3) = %v, want 5", res[0])
		}
	}

	// B imports A: B's host import calls A. B exports "run".
	bRan := false
	b, err := Instantiate(ctx, r, logHelloWasm, stringLogOpt(func(context.Context, []abi.Value) ([]abi.Value, error) {
		callA()
		bRan = true
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("load B (imports A): %v", err)
	}
	defer b.Close(ctx)

	// C imports A and B: C's host import calls B.run (which calls A), then A.
	cCalledB, cCalledA := false, false
	c, err := Instantiate(ctx, r, logHelloWasm, stringLogOpt(func(context.Context, []abi.Value) ([]abi.Value, error) {
		if _, err := b.Call(ctx, "run"); err != nil {
			t.Fatalf("C -> B.run: %v", err)
		}
		cCalledB = true
		callA()
		cCalledA = true
		return nil, nil
	}))
	if err != nil {
		t.Fatalf("load C (imports A and B): %v", err)
	}
	defer c.Close(ctx)

	if _, err := c.Call(ctx, "run"); err != nil {
		t.Fatalf("C.run: %v", err)
	}

	if !bRan || !cCalledB || !cCalledA {
		t.Fatalf("chain incomplete: B ran=%v, C called B=%v, C called A=%v", bRan, cCalledB, cCalledA)
	}
	if aCalls != 2 {
		t.Fatalf("A must be called exactly twice (via B, and directly by C), got %d", aCalls)
	}
}
