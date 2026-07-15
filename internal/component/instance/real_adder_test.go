package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// real_adder.component.wasm is a genuine wasm32-wasip2 reactor component
// (built by the real Rust/wit-bindgen toolchain, not a synthetic .wat
// fixture) whose WIT world exports the interface `component:adder/calc`:
//
//	add: func(a: u32, b: u32) -> u32
//	greet: func(name: string) -> string
//
// Because the world exports an *interface* rather than bare functions,
// wit-component packages the two lifted funcs into a nested "re-export
// shim" component and an Instance that instantiates it, with the top-level
// export naming that instance (sort 0x05) -- see the package doc and
// bindInstanceExport in host_import.go for how this is resolved back to
// ordinary canon lifts.
//
//go:embed testdata/real_adder.component.wasm
var realAdderWasm []byte

// TestRealAdder_Add is the milestone proof for the plain (non-string) member
// of an exported instance: add(2,3) really executes inside the guest core
// module and returns 5, reached through the instance -> shim -> outer canon
// lift chain rather than any hardcoded value.
func TestRealAdder_Add(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
	if err != nil {
		t.Fatalf("CallExport add: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d (%v)", len(results), results)
	}
	got, ok := results[0].(uint32)
	if !ok {
		t.Fatalf("expected uint32 result, got %T (%v)", results[0], results[0])
	}
	if got != 5 {
		t.Fatalf("add(2,3) = %d, want 5", got)
	}
}

// TestRealAdder_AddViaComposedCallKey proves the "instance#member" spelling
// documented on CallExport also works, since both route through the same
// exports map entry.
func TestRealAdder_AddViaComposedCallKey(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "component:adder/calc#add", uint32(9), uint32(33))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got, ok := results[0].(uint32); !ok || got != 42 {
		t.Fatalf("add(9,33) = %v, want 42", results[0])
	}
}

// TestRealAdder_Greet is the milestone proof for the string round-trip: a
// string parameter is lowered into guest memory (allocated via the guest's
// own cabi_realloc, exercised through the canon lift's realloc option), the
// guest computes "hello, " + name + "!" and returns a new string, which is
// lifted back out of guest memory, and finally the canon lift's post-return
// option is invoked (via the "cabi_post_..." core func) so the guest can
// free the result buffer. None of this is hardcoded: a different input name
// than any literal appearing in this test file is used, and the assertion
// checks the exact guest-computed value.
func TestRealAdder_Greet(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.CallExport(ctx, "component:adder/calc", "greet", "wazy")
	if err != nil {
		t.Fatalf("CallExport greet: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d (%v)", len(results), results)
	}
	got, ok := results[0].(string)
	if !ok {
		t.Fatalf("expected string result, got %T (%v)", results[0], results[0])
	}
	if got != "hello, wazy!" {
		t.Fatalf("greet(%q) = %q, want %q", "wazy", got, "hello, wazy!")
	}
}

// TestRealAdder_GreetDifferentInput re-runs greet with a second input to
// further rule out a hardcoded result: a fixed-string implementation would
// fail this (either by returning the same string as TestRealAdder_Greet, or
// by returning garbage, since a hardcoded lift wouldn't correctly compute a
// second realloc'd buffer).
func TestRealAdder_GreetDifferentInput(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.CallExport(ctx, "component:adder/calc", "greet", "component-model")
	if err != nil {
		t.Fatalf("CallExport greet: %v", err)
	}
	if got, ok := results[0].(string); !ok || got != "hello, component-model!" {
		t.Fatalf("greet(%q) = %v, want %q", "component-model", results[0], "hello, component-model!")
	}
}

// TestRealAdder_GreetPostReturnWired is a white-box check (same package)
// that greet's boundExport really was wired to the guest's post-return core
// func -- "cabi_post_component:adder/calc#greet", per the canon lift's
// post-return option -- and not left empty. A passing TestRealAdder_Greet
// alone can't distinguish "post-return executed" from "post-return silently
// skipped" (both leave no external trace, since this particular post-return
// body happens to be a no-op in the compiled guest), so this asserts the
// wiring directly.
func TestRealAdder_GreetPostReturnWired(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	be, ok := inst.exports["component:adder/calc#greet"]
	if !ok {
		t.Fatal("no bound export for component:adder/calc#greet")
	}
	const want = "cabi_post_component:adder/calc#greet"
	if be.postReturnFuncName != want {
		t.Fatalf("postReturnFuncName = %q, want %q", be.postReturnFuncName, want)
	}

	// add has no post-return option in the canon lift.
	if be2 := inst.exports["component:adder/calc#add"]; be2.postReturnFuncName != "" {
		t.Fatalf("add postReturnFuncName = %q, want empty (add has no post-return)", be2.postReturnFuncName)
	}
}

// TestRealAdder_GreetPostReturnActuallyCalled proves invoke() really
// executes the post-return call during a live Call (not dead/skipped code):
// with the cached post-return handle corrupted to nil (simulating a name the
// guest doesn't export -- postReturnFn is resolved once at bind time by
// finalizeBoundExport, see boundExport's doc, so invoke() itself no longer
// does a fresh lookup to corrupt), the call must fail with the "post-return
// core func ... not found" error -- which only fires from inside the
// post-return branch of invoke(), after a successful lift.
func TestRealAdder_GreetPostReturnActuallyCalled(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	be, ok := inst.exports["component:adder/calc#greet"]
	if !ok {
		t.Fatal("no bound export for component:adder/calc#greet")
	}
	be.postReturnFuncName = "does-not-exist"
	be.postReturnFn = nil

	_, err = inst.CallExport(ctx, "component:adder/calc", "greet", "wazy")
	requireErrContains(t, err, "post-return core func \"does-not-exist\" not found")
}

// TestRealAdder_CallExportUnknownInstance proves an unrecognized instance
// name in CallExport fails loud with the plain "export not found" message
// (the same one Call gives), rather than panicking.
func TestRealAdder_CallExportUnknownInstance(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realAdderWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	_, err = inst.CallExport(ctx, "does-not-exist", "add", uint32(1), uint32(2))
	requireErrContains(t, err, "not found")
}

// TestRealAdder_NestedComponentDecoded is a decode-layer sanity check (not
// duplicating binary package tests): real_adder's single nested component
// (the wit-component re-export shim) decodes to exactly the pure-passthrough
// shape bindInstanceExport requires -- 2 func imports, 2 func exports, and
// nothing else that would produce a func-sort index.
func TestRealAdder_NestedComponentDecoded(t *testing.T) {
	comp, err := binary.Decode(bytes.NewReader(realAdderWasm))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(comp.NestedComponents) != 1 {
		t.Fatalf("NestedComponents: got %d, want 1", len(comp.NestedComponents))
	}
	shim := comp.NestedComponents[0]
	if err := validateShimComponent(shim); err != nil {
		t.Fatalf("validateShimComponent: %v", err)
	}
	if len(shim.Imports) != 2 || len(shim.Exports) != 2 {
		t.Fatalf("shim shape: imports=%d exports=%d, want 2 and 2", len(shim.Imports), len(shim.Exports))
	}
}
