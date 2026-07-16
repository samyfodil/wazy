package instance

import (
	"bytes"
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_mega.component.wasm is a genuine rustc wasm32-wasip2 wasi:cli/command
// component that exercises a broad slice of the WASI 0.2 surface in one run:
// args, environment, a HashMap (which seeds itself via wasi:random), stdin,
// filesystem read+write+readback, and wall-clock. It is the compliance
// capstone -- one real guest crossing cli / io-streams / environment /
// filesystem / random / clocks at once, asserted byte-for-byte against
// wasmtime's golden.
//
//go:embed testdata/real_mega.component.wasm
var realMegaWasm []byte

// TestRealMega runs the multi-interface guest through wazy's own host and
// asserts its (fully deterministic) stdout matches wasmtime, plus the FS map
// shows the guest's write committed -- proving args, env, random-seeded
// HashMap, stdin, filesystem, and clocks all work together in a single run.
func TestRealMega(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	fs := map[string][]byte{"/in.txt": []byte("hello mega world")}
	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realMegaWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("stdin data here\n"),
		Args:   []string{"alpha", "beta"},
		Env:    []string{"GREETING=hi"},
		FS:     fs,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
	}

	want := strings.Join([]string{
		`args=["alpha", "beta"]`,
		"env_GREETING=hi",
		`wordcounts=[("brown", 1), ("fox", 2), ("quick", 1), ("the", 2)]`,
		"stdin_upper=STDIN DATA HERE",
		"fs_roundtrip=HELLO MEGA WORLD",
		"clock_positive=true",
		"",
	}, "\n")
	if stdout.String() != want {
		t.Fatalf("stdout mismatch:\ngot:  %q\nwant: %q\nstderr: %q", stdout.String(), want, stderr.String())
	}
	if string(fs["/out.txt"]) != "HELLO MEGA WORLD" {
		t.Fatalf(`fs["/out.txt"] = %q, want "HELLO MEGA WORLD" (guest write did not commit)`, fs["/out.txt"])
	}
}
