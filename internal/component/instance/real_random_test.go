package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_random.component.wasm is a genuine rustc wasm32-wasip2 component (built
// with the wasi 0.14 crate) that calls every wasi:random func:
//
//	get_random_bytes(16); get_random_u64();
//	get_insecure_random_bytes(8); get_insecure_random_u64();
//	insecure_seed();
//
// and prints structural facts about the results (byte lengths + ok flags, not
// the random values themselves, so stdout is deterministic). Confirmed under
// `wasmtime run` to print the golden below.
//
//go:embed testdata/real_random.component.wasm
var realRandomWasm []byte

// TestRealRandom proves wazy implements the full wasi:random surface --
// random.get-random-bytes/get-random-u64, insecure.get-insecure-random-bytes/
// get-insecure-random-u64, and insecure-seed.insecure-seed -- by running a real
// guest that calls all of them; its (deterministic, value-independent) stdout
// must match wasmtime's byte for byte, proving none of the calls trapped.
func TestRealRandom(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realRandomWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
	}

	// Byte-identical to wasmtime's golden (all fields are deterministic:
	// lengths + ok flags, never the random values themselves).
	want := "bytes_len=16\nu64_nonzero_likely=true\ninsecure_bytes_len=8\ninsecure_u64_ok=true\nseed_ok=true\n"
	if stdout.String() != want {
		t.Fatalf("guest stdout = %q, want %q (stderr: %q)", stdout.String(), want, stderr.String())
	}
}
