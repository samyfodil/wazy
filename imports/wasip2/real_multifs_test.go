package wasip2

import (
	"bytes"
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
)

// real_multifs.component.wasm is a genuine rustc wasm32-wasip2 component
// (rustc --target wasm32-wasip2 -O) built from:
//
//	println!("root={}", fs::read_to_string("/a.txt").unwrap());
//	println!("tmp={}", fs::read_to_string("/tmp/a.txt").unwrap());
//	println!("pkg={}", fs::read_to_string("/site-packages/a.txt").unwrap());
//	fs::write("/tmp/written.txt", b"scratch").unwrap();
//	println!("wrote_tmp={}", fs::read_to_string("/tmp/written.txt").unwrap());
//	fs::create_dir("/tmp/sub").unwrap();
//	fs::write("/tmp/sub/deep.txt", b"nested").unwrap();
//	println!("nested={}", fs::read_to_string("/tmp/sub/deep.txt").unwrap());
//	let mut root_entries: Vec<String> = fs::read_dir("/")...collect();
//	root_entries.sort();
//	println!("root_ls={:?}", root_entries);
//	println!("escape_sideways_err={}", fs::read_to_string("/tmp/../a.txt").is_err());
//	println!("escape_host_err={}", fs::read_to_string("/tmp/../../etc/passwd").is_err());
//
// The same basename ("a.txt") lives in all three mounts, holding different
// bytes, so only correct per-mount resolution can tell them apart.
//
//go:embed testdata/real_multifs.component.wasm
var realMultiFSWasm []byte

// realMultiFSGolden is the exact stdout `wasmtime run --dir root::/ --dir
// tmp::/tmp --dir pkg::/site-packages` produces for the fixture above
// (wasmtime-cli 23.0.1), captured against three temp directories seeded
// exactly as TestRealMultiFS seeds them.
const realMultiFSGolden = `root=from-root
tmp=from-tmp
pkg=from-pkg
wrote_tmp=scratch
nested=nested
root_ls=["a.txt"]
escape_sideways_err=true
escape_host_err=true
`

// TestRealMultiFS is the real-guest proof for multi-mount: an off-the-shelf
// rustc component reads from three separate preopens, writes into the one
// that owns the path, and is refused when it tries to leave a preopen --
// byte-for-byte identical to wasmtime.
//
// Two properties of the golden are worth naming, because they are the whole
// point of the feature and wasmtime is what pins them:
//
//   - root_ls lists only "a.txt". "tmp" and "site-packages" are separate
//     preopens, NOT entries in the root mount, so a host that flattened the
//     mounts into one tree (as the old map[string][]byte model effectively
//     did) would print three names here and fail.
//   - escape_sideways_err is true. "/tmp/../a.txt" cleans to a path that
//     exists in the root mount, and wasmtime still refuses it, because the
//     guest may not walk out of the preopen it resolved against. wazy
//     answers not-permitted for the same reason (wasiJoinFSPath) -- this
//     line is the reference implementation agreeing with that choice, rather
//     than wazy agreeing with itself.
func TestRealMultiFS(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	rootDir, tmpDir, pkgDir := t.TempDir(), t.TempDir(), t.TempDir()
	for dir, content := range map[string]string{
		rootDir: "from-root",
		tmpDir:  "from-tmp",
		pkgDir:  "from-pkg",
	} {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", dir, err)
		}
	}

	var stdout, stderr bytes.Buffer
	inst, err := component.Instantiate(ctx, r, realMultiFSWasm, WithWASI(WASIConfig{
		Stdout: &stdout,
		Stderr: &stderr,
		FS: wazy.NewFSConfig().
			WithDirMount(rootDir, "/").
			WithDirMount(tmpDir, "/tmp").
			WithDirMount(pkgDir, "/site-packages"),
	})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		t.Fatalf("Call run(): %v (stdout: %q, stderr: %q)", err, stdout.String(), stderr.String())
	}
	if stdout.String() != realMultiFSGolden {
		t.Fatalf("guest stdout does not match the wasmtime golden:\ngot:\n%s\nwant:\n%s\nstderr: %q",
			stdout.String(), realMultiFSGolden, stderr.String())
	}

	// The writes really landed on the host, in the mount that owned each
	// path -- and nowhere else.
	if got, err := os.ReadFile(filepath.Join(tmpDir, "written.txt")); err != nil || string(got) != "scratch" {
		t.Errorf("tmp mount written.txt = (%q, %v), want %q", got, err, "scratch")
	}
	if got, err := os.ReadFile(filepath.Join(tmpDir, "sub", "deep.txt")); err != nil || string(got) != "nested" {
		t.Errorf("tmp mount sub/deep.txt = (%q, %v), want %q", got, err, "nested")
	}
	for _, dir := range []string{rootDir, pkgDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("listing %s: %v", dir, err)
		}
		if len(entries) != 1 || entries[0].Name() != "a.txt" {
			t.Errorf("mount %s = %v, want only a.txt; a write leaked into the wrong mount", dir, entries)
		}
	}
}
