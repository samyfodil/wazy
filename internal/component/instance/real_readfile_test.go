package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_readfile.component.wasm is a genuine rustc wasm32-wasip2
// wasi:cli/command component (built by the real Rust/wit-component
// toolchain, not a synthetic .wat fixture) whose main is:
//
//	let s = std::fs::read_to_string("/greeting.txt").unwrap();
//	print!("{s}");
//
// Unlike real_hello/real_args (stdio + environment only), this exercises
// wasi:filesystem/preopens + wasi:filesystem/types' `descriptor` resource +
// wasi:io/streams' `input-stream` -- the largest WASI 0.2 interface, backed
// here by an in-memory host filesystem (WASIConfig.FS) -- see wasi_fs.go
// for the host implementation and its package doc for the exact ordered
// list of WASI calls this fixture's read_to_string reaches (discovered by
// running it with an empty WASIConfig.FS under the graph engine's
// automatic trap-stub fallback, one unimplemented call at a time).
//
//go:embed testdata/real_readfile.component.wasm
var realReadfileWasm []byte

// runRealReadFile instantiates real_readfile.component.wasm with fs backing
// wasi:filesystem/preopens' one preopened root directory, calls run(), and
// returns (stdout, run()'s result, the Call error). t.Fatal on an
// Instantiate error (a harness failure, not part of what any individual
// test is proving).
func runRealReadFile(t *testing.T, fs map[string][]byte) (string, []abi.Value, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realReadfileWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		FS:     fs,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, callErr := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	t.Logf("stdout: %q stderr: %q", stdout.String(), stderr.String())
	return stdout.String(), results, callErr
}

// TestRealReadFile is THE milestone: a genuine, off-the-shelf rustc
// wasm32-wasip2 wasi:cli/command component really reads a file's contents
// through wazy's WASI filesystem layer -- not a Go literal -- proven by
// std::fs::read_to_string("/greeting.txt") returning exactly what
// WASIConfig.FS holds under that path, echoed back via a real WASI write.
func TestRealReadFile(t *testing.T) {
	const want = "hello from the host filesystem"
	out, results, err := runRealReadFile(t, map[string][]byte{"/greeting.txt": []byte(want)})
	if err != nil {
		t.Fatalf("Call run(): %v", err)
	}
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
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
		t.Fatal("run() returned Err, want Ok")
	}
}

// TestRealReadFile_DifferentContent re-runs with entirely different file
// contents to rule out a hardcoded result: a hardcoded (or accidentally
// cached) implementation would still echo the first test's string.
func TestRealReadFile_DifferentContent(t *testing.T) {
	const want = "a totally different second payload, read on the second run\n"
	out, _, err := runRealReadFile(t, map[string][]byte{"/greeting.txt": []byte(want)})
	if err != nil {
		t.Fatalf("Call run(): %v", err)
	}
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if out == "hello from the host filesystem" {
		t.Fatal("stdout matches TestRealReadFile's string; looks hardcoded/cached rather than genuinely read from WASIConfig.FS")
	}
}

// TestRealReadFile_MissingFile proves the error path is real too: reading
// a path absent from WASIConfig.FS resolves, through open-at's
// error-code::no-entry and the guest's own Rust std::io translation, to a
// genuine Result::unwrap() panic -- which surfaces as an unreachable trap
// (Rust panic=abort under wasm32-wasip2), not a silently-empty read.
func TestRealReadFile_MissingFile(t *testing.T) {
	out, _, err := runRealReadFile(t, nil) // no "/greeting.txt" entry at all
	if err == nil {
		t.Fatal("expected an error reading a file absent from WASIConfig.FS")
	}
	t.Logf("run() error (expected): %v", err)
	requireErrContains(t, err, "unreachable")
	if out != "" {
		t.Fatalf("stdout = %q, want empty (the guest panicked before printing)", out)
	}
}
