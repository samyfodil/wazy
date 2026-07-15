package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
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

// TestRealHello_PrintsHelloWorld is THE milestone: a genuine, off-the-shelf
// rustc wasm32-wasip2 wasi:cli/command component -- not a synthetic .wat
// fixture -- really executes on wazy and prints "hello world" through the
// real WASI 0.2 host surface WithWASI registers (see wasi.go): the guest's
// println! goes through the preview1-to-preview2 adapter's fd_write, which
// calls wasi:cli/stdout.get-stdout for an own<output-stream> handle and then
// [method]output-stream.write on it, landing the guest-computed bytes in
// the *bytes.Buffer this test owns.
func TestRealHello_PrintsHelloWorld(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realHelloWasm, WithWASI(WASIConfig{Stdout: &stdout, Stderr: &stderr})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	if err != nil {
		t.Fatalf("Call run(): %v (stdout so far: %q, stderr so far: %q)", err, stdout.String(), stderr.String())
	}

	if got := stdout.String(); got != "hello world\n" {
		t.Fatalf("stdout = %q, want %q (stderr: %q)", got, "hello world\n", stderr.String())
	}

	// run() -> result<_, _> per the decoded WIT (wasi:cli/run's `run: func()
	// -> result;`); a successful run lifts to Ok (IsErr == false).
	if len(results) != 1 {
		t.Fatalf("run() returned %d result(s), want 1", len(results))
	}
	rv, ok := results[0].(abi.ResultValue)
	if !ok {
		t.Fatalf("run() result: expected abi.ResultValue, got %T (%v)", results[0], results[0])
	}
	if rv.IsErr {
		t.Fatalf("run() returned Err, want Ok (stderr: %q)", stderr.String())
	}
}

// TestRealHello_ReinstantiateAfterCloseOnSameRuntime is the regression proof
// for a real leak found while profiling the call path: instantiateGraph's
// inline-export core instances build a private host module (via
// buildCanonHostModule) per canon-produced core func (a WASI trap stub or
// resource canon) and register it on the Runtime under a unique
// "wazy:component/privN" name, but that module was never appended to
// Instance.closers -- only the passthrough shim that imports from it was
// (see BenchmarkInstantiateHello's doc, which worked around this same bug by
// paying for a fresh Runtime every iteration). So Instance.Close never freed
// those private names, and a second Instantiate of the same component on the
// same Runtime reliably failed with "already instantiated" the moment it
// tried to reuse "wazy:component/priv1". Fixed by threading the private
// module through to instantiateGraph's closers slice; this proves a full
// Instantiate+Close+Instantiate+Close cycle on one Runtime now succeeds.
func TestRealHello_ReinstantiateAfterCloseOnSameRuntime(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst1, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("first Instantiate: %v", err)
	}
	if err := inst1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	inst2, err := Instantiate(ctx, r, realHelloWasm)
	if err != nil {
		t.Fatalf("second Instantiate on the same Runtime: %v", err)
	}
	if err := inst2.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
