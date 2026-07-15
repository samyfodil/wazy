package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_transform.component.wasm is a genuine rustc wasm32-wasip2
// wasi:cli/command component (built by the real Rust/wit-component
// toolchain, not a synthetic .wat fixture) whose main is:
//
//	let s = std::fs::read_to_string("/input.txt").unwrap();
//	std::fs::write("/output.txt", s.to_uppercase()).unwrap();
//
// This is the capstone WASI filesystem milestone: a realistic program that
// both READS a file (the same wasi:filesystem/types + wasi:io/streams
// input-stream path real_readfile.component.wasm exercises -- see
// wasi_fs.go's package doc) and WRITES a new one, completing the write
// half. std::fs::write's own additional call beyond the read path is
// [method]descriptor.write-via-stream, followed by
// [method]output-stream.write against the own<output-stream> it returns
// (wasi_fs.go's writeViaStream/wasi.go's writeSink); open-at's create/
// truncate open-flags and write descriptor-flag (also exercised here, since
// "/output.txt" does not exist beforehand) are handled the same place
// read_to_string's plain open-at is, in wasi_fs.go's openAt.
//
//go:embed testdata/real_transform.component.wasm
var realTransformWasm []byte

// runRealTransform instantiates real_transform.component.wasm with fs
// backing wasi:filesystem/preopens' one preopened root directory, calls
// run(), and returns (fs, run()'s result, the Call error). fs is the exact
// map passed in: open-at(create) and every subsequent write commit straight
// into it (see wasi_fs.go's fsFileSet), so the caller can read
// fs["/output.txt"] back after this returns without any extra plumbing --
// see WASIConfig.FS's doc for why a non-nil map is required for that to
// work. t.Fatal on an Instantiate error (a harness failure, not part of
// what any individual test is proving).
func runRealTransform(t *testing.T, fs map[string][]byte) (map[string][]byte, []abi.Value, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realTransformWasm, WithWASI(WASIConfig{
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
	return fs, results, callErr
}

// TestRealTransform is THE capstone milestone: a genuine, off-the-shelf
// rustc wasm32-wasip2 wasi:cli/command component really reads a file
// through wazy's WASI filesystem layer, transforms its contents in real
// guest code (Rust's own str::to_uppercase, not anything this package
// does), and really writes the result to a new file -- proven by
// WASIConfig.FS containing "/output.txt" == the uppercased input after
// run() returns, read back through the exact same host map the guest's
// writes committed into (see runRealTransform's doc).
func TestRealTransform(t *testing.T) {
	const in = "hello world"
	const want = "HELLO WORLD"
	fs, results, err := runRealTransform(t, map[string][]byte{"/input.txt": []byte(in)})
	if err != nil {
		t.Fatalf("Call run(): %v", err)
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

	got, ok := fs["/output.txt"]
	if !ok {
		t.Fatal(`fs["/output.txt"] absent after run(); the guest's std::fs::write never landed`)
	}
	if string(got) != want {
		t.Fatalf(`fs["/output.txt"] = %q, want %q`, got, want)
	}

	// The input entry must survive untouched -- writing "/output.txt" must
	// not clobber "/input.txt" (e.g. via a shared-slice aliasing bug between
	// the two fs.files entries).
	if string(fs["/input.txt"]) != in {
		t.Fatalf(`fs["/input.txt"] = %q after run(), want unchanged %q`, fs["/input.txt"], in)
	}
}

// TestRealTransform_DifferentInput re-runs with entirely different input to
// rule out a hardcoded/coincidental result: a hardcoded (or accidentally
// cached) implementation would still produce "HELLO WORLD" regardless of
// what "/input.txt" actually holds. Mixed-case input additionally proves
// the guest's own str::to_uppercase runs (not e.g. a host-side transform
// this package might have silently substituted): digits and punctuation
// pass through unchanged, only letters change case.
func TestRealTransform_DifferentInput(t *testing.T) {
	const in = "MixedCase 123, already Loud!"
	const want = "MIXEDCASE 123, ALREADY LOUD!"
	fs, _, err := runRealTransform(t, map[string][]byte{"/input.txt": []byte(in)})
	if err != nil {
		t.Fatalf("Call run(): %v", err)
	}
	got, ok := fs["/output.txt"]
	if !ok {
		t.Fatal(`fs["/output.txt"] absent after run()`)
	}
	if string(got) != want {
		t.Fatalf(`fs["/output.txt"] = %q, want %q`, got, want)
	}
	if string(got) == "HELLO WORLD" {
		t.Fatal(`fs["/output.txt"] matches TestRealTransform's string; looks hardcoded rather than genuinely transformed from WASIConfig.FS`)
	}
}

// TestRealTransform_MissingInput proves the read half's error path still
// works end-to-end through the write-capable host: reading a path absent
// from WASIConfig.FS resolves to a genuine Result::unwrap() panic (same as
// TestRealReadFile_MissingFile), surfacing as an unreachable trap before
// std::fs::write is ever reached -- so "/output.txt" must never appear.
func TestRealTransform_MissingInput(t *testing.T) {
	fs := map[string][]byte{} // no "/input.txt" entry at all
	_, _, err := runRealTransform(t, fs)
	if err == nil {
		t.Fatal("expected an error reading a file absent from WASIConfig.FS")
	}
	t.Logf("run() error (expected): %v", err)
	requireErrContains(t, err, "unreachable")
	if _, ok := fs["/output.txt"]; ok {
		t.Fatalf(`fs["/output.txt"] = %q present despite the guest panicking before ever writing it`, fs["/output.txt"])
	}
}
