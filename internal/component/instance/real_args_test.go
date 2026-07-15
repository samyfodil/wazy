package instance

import (
	"bytes"
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_args.component.wasm is a genuine rustc wasm32-wasip2 wasi:cli/command
// component (built by the real Rust/wit-component toolchain, not a synthetic
// .wat fixture) whose main does:
//
//	for a in std::env::args().skip(1) { println!("arg: {a}"); }
//	for (k, v) in std::env::vars() { println!("env: {k}={v}"); }
//
// It imports the same WASI surface as real_hello.component.wasm (see
// real_hello_test.go) plus wasi:cli/environment's get-arguments -- see
// TestRealArgs_InstantiatesGraph, which documents the wired WASI call count.
//
//go:embed testdata/real_args.component.wasm
var realArgsWasm []byte

// TestRealArgs_InstantiatesGraph is the sanity check that the general graph
// engine wires up real_args.component.wasm without error, mirroring
// TestRealHello_InstantiatesGraph.
func TestRealArgs_InstantiatesGraph(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realArgsWasm)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if inst == nil {
		t.Fatal("Instantiate returned a nil *Instance with a nil error")
	}
}

// runRealArgs instantiates real_args.component.wasm with the given Args/Env,
// calls run(), and returns stdout's contents. It fails the test on any
// instantiate/call error or a non-Ok run() result.
func runRealArgs(t *testing.T, args []string, env []string) string {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realArgsWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		Args:   args,
		Env:    env,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	if err != nil {
		t.Fatalf("Call run(): %v (stdout so far: %q, stderr so far: %q)", err, stdout.String(), stderr.String())
	}
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
	return stdout.String()
}

// TestRealArgs_EchoesArgsAndEnv is THE milestone: a genuine, off-the-shelf
// rustc wasm32-wasip2 wasi:cli/command component really receives real
// command-line arguments and environment variables through wazy's WASI
// layer, not a Go literal -- proven by echoing them back through the guest's
// own std::env::args()/std::env::vars() and a real WASI write.
func TestRealArgs_EchoesArgsAndEnv(t *testing.T) {
	out := runRealArgs(t,
		[]string{"hello", "from", "wazy"},
		[]string{"GREETING=hi", "LANG=en"},
	)
	t.Logf("stdout: %q", out)

	for _, want := range []string{
		"arg: hello\n",
		"arg: from\n",
		"arg: wazy\n",
		"env: GREETING=hi\n",
		"env: LANG=en\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q does not contain %q", out, want)
		}
	}
}

// TestRealArgs_EchoesArgsAndEnv_DifferentInput re-runs with entirely
// different Args/Env to rule out a hardcoded result: it asserts the new
// values are present AND the first run's values are absent, which a
// hardcoded (or accidentally-cached) implementation would fail.
func TestRealArgs_EchoesArgsAndEnv_DifferentInput(t *testing.T) {
	out := runRealArgs(t,
		[]string{"second", "run"},
		[]string{"COLOR=blue"},
	)

	for _, want := range []string{
		"arg: second\n",
		"arg: run\n",
		"env: COLOR=blue\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q does not contain %q", out, want)
		}
	}
	for _, notWant := range []string{
		"arg: hello\n",
		"arg: from\n",
		"arg: wazy\n",
		"env: GREETING=hi\n",
		"env: LANG=en\n",
	} {
		if strings.Contains(out, notWant) {
			t.Fatalf("stdout %q unexpectedly contains %q from a different test's input (result looks hardcoded/cached)", out, notWant)
		}
	}
}

// TestRealArgs_EmptyArgsAndEnv proves the zero-value case (no Args/Env
// configured) doesn't panic and simply echoes nothing -- both loops in main
// run zero times.
func TestRealArgs_EmptyArgsAndEnv(t *testing.T) {
	out := runRealArgs(t, nil, nil)
	if strings.Contains(out, "arg: ") || strings.Contains(out, "env: ") {
		t.Fatalf("stdout = %q, want no arg:/env: lines for empty Args/Env", out)
	}
}
