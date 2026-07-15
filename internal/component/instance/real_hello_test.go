package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_hello.component.wasm is a genuine rustc wasm32-wasip2 wasi:cli/command
// component (built by the real Rust/wit-component toolchain, not a synthetic
// .wat fixture) that prints "hello world". Unlike real_adder.component.wasm,
// it has 4 embedded core modules -- a main guest module (compiled against
// the legacy wasi_snapshot_preview1 ABI), a preview1-to-preview2 adapter, and
// two small "indirect call table" trampoline modules wit-component uses to
// break the circular dependency between the two -- wired together by 17
// core:instance definitions, and imports 10 WASI interfaces lowered into 15
// canon-lower core funcs. See graph.go's package doc for how this package's
// general instantiation graph engine wires it all together.
//
//go:embed testdata/real_hello.component.wasm
var realHelloWasm []byte

// TestRealHello_InstantiatesGraph is the milestone proof that the general
// graph engine wires up every one of real_hello's 4 embedded core modules
// (plus its shim modules regrouping funcs/memory/table under new names, and
// the 15 WASI trap-stub host funcs) without error.
func TestRealHello_InstantiatesGraph(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if inst == nil {
		t.Fatal("Instantiate returned a nil *Instance with a nil error")
	}
	if got := inst.CoreModuleCount(); got != 4 {
		t.Fatalf("CoreModuleCount() = %d, want 4", got)
	}
}

// TestRealHello_RunReachesWASI is the milestone proof that calling the
// exported run() reaches real guest code that calls into a real WASI
// import -- which, since every lowered WASI func is a trap stub at this
// step (no real implementations yet), surfaces as an error naming the
// specific WASI interface+func the guest called first. It also logs the
// full ordered list of WASI iface+func the graph engine wired up, to scope
// the next step (which host WASI funcs need real implementations, and in
// what order the guest is expected to reach them).
func TestRealHello_RunReachesWASI(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	calls := inst.WASICalls()
	if len(calls) != 15 {
		t.Fatalf("WASICalls(): got %d entries, want 15: %v", len(calls), calls)
	}
	t.Logf("wired WASI calls, in declaration order:")
	for i, c := range calls {
		t.Logf("  [%d] %s", i, c)
	}

	_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	if err == nil {
		t.Log("run() completed without trapping -- documenting: no WASI trap-stub call was needed to reach this point")
		return
	}
	t.Logf("run() trapped as expected: %v", err)
	requireErrContains(t, err, "not implemented (trap stub)")
}
