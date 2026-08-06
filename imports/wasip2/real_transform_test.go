package wasip2

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
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
// run(), and returns (the host directory that mount is rooted at, run()'s
// result, the Call error). The guest's writes go straight to that directory,
// so the caller reads "/output.txt" back off disk with hostRead. t.Fatal on
// an Instantiate error (a harness failure, not part of what any individual
// test is proving).
func runRealTransform(t *testing.T, files map[string][]byte) (string, []abi.Value, error) {
	t.Helper()
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	fsConfig, dir := fsConfigDir(t, files)
	var stdout, stderr bytes.Buffer
	inst, err := component.Instantiate(ctx, r, realTransformWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		FS:     fsConfig,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	results, callErr := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	t.Logf("stdout: %q stderr: %q", stdout.String(), stderr.String())
	return dir, results, callErr
}

// TestRealTransform is THE capstone milestone: a genuine, off-the-shelf
// rustc wasm32-wasip2 wasi:cli/command component really reads a file
// through wazy's WASI filesystem layer, transforms its contents in real
// guest code (Rust's own str::to_uppercase, not anything this package
// does), and really writes the result to a new file -- proven by
// the mounted directory containing "/output.txt" == the uppercased input
// after run() returns, read back off the very disk the guest's writes
// committed to (see runRealTransform's doc).
func TestRealTransform(t *testing.T) {
	const in = "hello world"
	const want = "HELLO WORLD"
	dir, results, err := runRealTransform(t, map[string][]byte{"/input.txt": []byte(in)})
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

	if got := hostRead(t, dir, "/output.txt"); got != want {
		t.Fatalf("/output.txt = %q, want %q", got, want)
	}

	// The input file must survive untouched -- writing "/output.txt" must
	// not clobber "/input.txt".
	if got := hostRead(t, dir, "/input.txt"); got != in {
		t.Fatalf("/input.txt = %q after run(), want unchanged %q", got, in)
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
	dir, _, err := runRealTransform(t, map[string][]byte{"/input.txt": []byte(in)})
	if err != nil {
		t.Fatalf("Call run(): %v", err)
	}
	got := hostRead(t, dir, "/output.txt")
	if got != want {
		t.Fatalf("/output.txt = %q, want %q", got, want)
	}
	if got == "HELLO WORLD" {
		t.Fatal(`/output.txt matches TestRealTransform's string; looks hardcoded rather than genuinely transformed from WASIConfig.FS`)
	}
}

// TestRealTransform_MissingInput proves the read half's error path still
// works end-to-end through the write-capable host: reading a path absent
// from WASIConfig.FS resolves to a genuine Result::unwrap() panic (same as
// TestRealReadFile_MissingFile), surfacing as an unreachable trap before
// std::fs::write is ever reached -- so "/output.txt" must never appear.
func TestRealTransform_MissingInput(t *testing.T) {
	dir, _, err := runRealTransform(t, map[string][]byte{}) // an empty mount: no "/input.txt" at all
	if err == nil {
		t.Fatal("expected an error reading a file absent from WASIConfig.FS")
	}
	t.Logf("run() error (expected): %v", err)
	requireErrContains(t, err, "unreachable")
	requireAbsent(t, dir, "/output.txt")
}
