package wasip2

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// real_gowrite.component.wasm is a STANDARD-Go (not TinyGo, not Rust) wasip1
// guest run through the wasi_snapshot_preview1 component adapter:
//
//	for _, n := range []int{1, 8, 4096, 70000} {
//	    f, _ := os.Create(fmt.Sprintf("/w%d.bin", n))
//	    nw, err := f.Write(want)          // want is n bytes of 'a'..'z'
//	    f.Close()
//	    got, _ := os.ReadFile(path)
//	    fmt.Printf("size=%d ok\n", n)     // or the failure detail
//	}
//
// It exists to cover a path none of the other fixtures reach. Go's
// internal/poll.FD.Write goes through the adapter's fd_write, which calls
// wasi:io/streams check-write and then write; Rust's fs::write uses
// blocking-write-and-flush and never calls check-write at all. Every one of the
// other real_* and conformance fixtures is Rust, so a check-write budget that
// was nonzero as a u64 but ZERO once narrowed to a wasm32 usize passed all 35
// of them while silently writing nothing here -- the adapter computes its write
// length as bytes.len().min(permit as usize) (an i32.wrap_i64) and its "no
// budget" guard tests the full u64, so the wrapped-to-zero budget slipped past
// it and every write returned nwritten = 0. See wasiMaxWriteBudget.
//
//go:embed testdata/real_gowrite.component.wasm
var realGoWriteWasm []byte

// TestRealGoWrite proves os.File.Write works end to end for a standard-Go guest
// through the preview1 adapter, at sizes bracketing the adapter's buffering.
// With a budget whose low 32 bits are zero this fails at EVERY size, reporting
// a short write of 0 bytes (which surfaces in the guest as io.ErrUnexpectedEOF
// from internal/poll.FD.Write's n == 0 arm).
func TestRealGoWrite(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	fsConfig, dir := fsConfigDir(t, nil)
	var stdout, stderr bytes.Buffer
	inst, err := component.Instantiate(ctx, r, realGoWriteWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		FS:     fsConfig,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	// A standard-Go command guest ends in runtime.exit -> proc_exit, which the
	// adapter routes to wasi:cli/exit.exit; wazy has no process to terminate so
	// that call always fails (see wasiExit). exit(ok) is the success signal here
	// -- it means main returned normally rather than panicking mid-write.
	_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
	if err == nil {
		t.Fatalf("Call run(): expected the guest to reach exit(ok), got no error (stdout: %q)", stdout.String())
	}
	requireErrContains(t, err, "guest called exit(ok)")

	sizes := []int{1, 8, 4096, 70000}
	var want bytes.Buffer
	for _, n := range sizes {
		fmt.Fprintf(&want, "size=%d ok\n", n)
	}
	if stdout.String() != want.String() {
		t.Fatalf("guest stdout = %q, want %q (stderr: %q)", stdout.String(), want.String(), stderr.String())
	}

	// The bytes really landed on the mount, at full length.
	for _, n := range sizes {
		got := hostRead(t, dir, fmt.Sprintf("/w%d.bin", n))
		if len(got) != n {
			t.Fatalf("/w%d.bin: host sees %d bytes, want %d", n, len(got), n)
		}
	}
}
