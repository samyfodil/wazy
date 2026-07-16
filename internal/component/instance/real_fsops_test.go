package instance

import (
	"bytes"
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
)

// real_fsops.component.wasm is a genuine rustc wasm32-wasip2 component built
// from:
//
//	fs::rename("/a.txt", "/b.txt")?;
//	println!("renamed={}", fs::read_to_string("/b.txt")?);
//	fs::create_dir("/sub")?;
//	fs::write("/sub/c.txt", b"in-sub")?;
//	println!("sub_file={}", fs::read_to_string("/sub/c.txt")?);
//	fs::remove_file("/sub/c.txt")?;
//	fs::remove_dir("/sub")?;
//	println!("removed_dir={}", !Path::new("/sub").exists());
//
// It exercises wasi:filesystem/types rename-at, create-directory-at, and
// remove-directory-at (the three methods this milestone adds). Confirmed under
// `wasmtime run --dir` to print renamed=hello-a / sub_file=in-sub /
// removed_dir=true before wazy's host implemented them.
//
//go:embed testdata/real_fsops.component.wasm
var realFSOpsWasm []byte

// TestRealFSOps proves rename-at + create-directory-at + remove-directory-at
// end to end through a real guest, and (via the mutated FS map) that the moves
// really committed: "/a.txt" is gone and "/b.txt" holds its bytes; the created
// "/sub" and its file are both absent again after removal.
func TestRealFSOps(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	fs := map[string][]byte{"/a.txt": []byte("hello-a")}
	var stdout, stderr bytes.Buffer
	inst, err := Instantiate(ctx, r, realFSOpsWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		FS:     fs,
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
	}

	want := "renamed=hello-a\nsub_file=in-sub\nremoved_dir=true\n"
	if stdout.String() != want {
		t.Fatalf("guest stdout = %q, want %q (stderr: %q)", stdout.String(), want, stderr.String())
	}

	// The rename really moved the map entry.
	if _, present := fs["/a.txt"]; present {
		t.Fatalf(`fs["/a.txt"] still present after rename; rename-at did not remove the old key`)
	}
	if string(fs["/b.txt"]) != "hello-a" {
		t.Fatalf(`fs["/b.txt"] = %q, want "hello-a" (renamed bytes)`, fs["/b.txt"])
	}
	// The created file was removed again.
	if _, present := fs["/sub/c.txt"]; present {
		t.Fatalf(`fs["/sub/c.txt"] still present; remove_file did not commit`)
	}
}

// real_hardlink.component.wasm is a genuine rustc guest built from:
//
//	fs::hard_link("/a.txt", "/hard.txt")?;
//	println!("hard={}", fs::read_to_string("/hard.txt")?);
//
// std::fs::hard_link drives wasi:filesystem/types link-at. Confirmed under
// `wasmtime run --dir` to print hard=<content>.
//
//go:embed testdata/real_hardlink.component.wasm
var realHardlinkWasm []byte

// TestRealHardLink proves link-at end to end: the guest hard-links "/a.txt" to
// "/hard.txt" and reads the linked path back, and the FS map shows "/hard.txt"
// now holds "/a.txt"'s bytes (two distinct contents prove real data flow).
func TestRealHardLink(t *testing.T) {
	for _, content := range []string{"hardcontent", "another payload"} {
		t.Run(content, func(t *testing.T) {
			ctx := context.Background()
			r := wazy.NewRuntime(ctx)
			defer r.Close(ctx)

			fs := map[string][]byte{"/a.txt": []byte(content)}
			var stdout, stderr bytes.Buffer
			inst, err := Instantiate(ctx, r, realHardlinkWasm, WithWASI(WASIConfig{
				Stdout: &stdout,
				Stderr: &stderr,
				FS:     fs,
			})...)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			defer inst.Close(ctx)

			if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
				t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
			}
			if want := "hard=" + content + "\n"; stdout.String() != want {
				t.Fatalf("guest stdout = %q, want %q", stdout.String(), want)
			}
			if string(fs["/hard.txt"]) != content {
				t.Fatalf(`fs["/hard.txt"] = %q, want %q (linked bytes)`, fs["/hard.txt"], content)
			}
		})
	}
}
