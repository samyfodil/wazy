package wasip2

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/componenttest"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// fsConfigDir materializes files (keyed by the absolute path the guest sees,
// e.g. "/sub/a.txt") into a fresh temp directory and returns an FSConfig
// mounting it at "/", plus the host directory itself so a test can assert on
// disk what the guest wrote. It is how every fixture that used to declare a
// map[string][]byte host filesystem now declares its starting tree.
func fsConfigDir(t *testing.T, files map[string][]byte) (wazy.FSConfig, string) {
	t.Helper()
	dir := t.TempDir()
	for guestPath, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(guestPath, "/")))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", guestPath, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", guestPath, err)
		}
	}
	return wazy.NewFSConfig().WithDirMount(dir, "/"), dir
}

// wasiFSConfigDir is wasiFSConfig over a single "/" mount holding files --
// the shape almost every test in this file wants.
func wasiFSConfigDir(t *testing.T, files map[string][]byte) (*componenttest.Harness, *component.HandleTable, string) {
	t.Helper()
	fsc, dir := fsConfigDir(t, files)
	h, resources := wasiFSConfig(t, WASIConfig{FS: fsc})
	return h, resources, dir
}

// hostRead reads back what the guest wrote at the guest-visible path p under
// the mount rooted at dir.
func hostRead(t *testing.T, dir, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "/"))))
	if err != nil {
		t.Fatalf("reading %s back from the mount: %v", p, err)
	}
	return string(b)
}

// requireAbsent fails unless the guest-visible path p is gone from the mount
// rooted at dir.
func requireAbsent(t *testing.T, dir, p string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "/")))
	if _, err := os.Lstat(full); !os.IsNotExist(err) {
		t.Fatalf("%s still present on the mount (Lstat err = %v), want removed", p, err)
	}
}

// wasiFSConfig builds a WithWASI config the same way wasiHostFunc does, but
// returns the whole *config plus the *component.HandleTable runResourceHooks handed
// to it, rather than a single extracted component.HostFunc, so a test can chain
// calls across multiple funcs that share the same underlying wasiFS state
// (e.g. get-directories then open-at against the descriptor it returned)
// -- something wasiHostFunc's one-shot extraction can't do, since each
// call to it builds an entirely independent config/wasiFS pair -- and
// resolve a borrow<T>/own<T> handle to its rep itself (rootHandleRep),
// mirroring what liftHostArgs (host_import.go) does automatically for a
// real guest call, since these tests invoke the extracted component.HostFunc
// directly, bypassing that generic lift step.
func wasiFSConfig(t *testing.T, cfg WASIConfig) (*componenttest.Harness, *component.HandleTable) {
	t.Helper()
	h := componenttest.New(WithWASI(cfg)...)
	return h, h.Resources()
}

func wasiFSFn(t *testing.T, h *componenttest.Harness, iface, name string) component.HostFunc {
	t.Helper()
	fn := h.Func(iface, name)
	if fn == nil {
		t.Fatalf("WithWASI did not register %q %q", iface, name)
	}
	return fn
}

// rootDescriptorHandle drives get-directories and returns the own<
// descriptor> handle for the one preopened root directory it names.
func rootDescriptorHandle(t *testing.T, h *componenttest.Harness) uint32 {
	t.Helper()
	getDirectories := wasiFSFn(t, h, wasiIfacePreopens, "get-directories")
	results, err := getDirectories(context.Background(), nil)
	if err != nil {
		t.Fatalf("get-directories: %v", err)
	}
	dirs := results[0].([]abi.Value)
	entry := dirs[0].([]abi.Value)
	return entry[0].(uint32)
}

func TestWasiFS_JoinPath(t *testing.T) {
	tests := []struct {
		dir, rel string
		want     string
		wantOK   bool
	}{
		{".", "greeting.txt", "greeting.txt", true},
		{".", "sub/greeting.txt", "sub/greeting.txt", true},
		{"sub", "greeting.txt", "sub/greeting.txt", true},
		{".", ".", ".", true},    // "." names the directory itself
		{"sub", "", "sub", true}, // so does ""
		{"sub", "a/../b", "sub/b", true},
		{"sub", "./a", "sub/a", true},
		{"sub", "file/", "sub/file/", true}, // trailing slash survives

		// Escapes, all rejected -- see wasiJoinFSPath's "# Escaping".
		{".", "/greeting.txt", "", false}, // rooted
		{".", "..", "", false},
		{".", "../escape", "", false},
		{"sub", "..", "", false},           // up out of the descriptor...
		{"sub", "../sibling", "", false},   // ...even when it stays on the mount
		{".", "a/../../escape", "", false}, // escapes only after cleaning
		{".", "sub/../../escape", "", false},
	}
	for _, tt := range tests {
		got, ok := wasiJoinFSPath(tt.dir, tt.rel)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("wasiJoinFSPath(%q, %q) = (%q, %v), want (%q, %v)", tt.dir, tt.rel, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestWasiFS_GetDirectories_RootIsDirectory(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)

	getType := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	results, err := getType(context.Background(), []abi.Value{rootHandleRep(t, resources, rootHandle)})
	if err != nil {
		t.Fatalf("get-type: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if rv.IsErr {
		t.Fatalf("get-type: unexpected Err: %#v", rv.Payload)
	}
	if rv.Payload.(uint32) != wasiDescriptorTypeDirectory {
		t.Fatalf("get-type: got case %v, want directory (%d)", rv.Payload, wasiDescriptorTypeDirectory)
	}
}

// rootHandleRep resolves handle back to its host rep the same way a real
// borrow<descriptor> self argument would be resolved (liftHostArgs,
// host_import.go) before a [method]descriptor.* component.HostFunc ever sees it --
// these unit tests call the extracted component.HostFunc directly, bypassing that
// generic lift step, so they must do the resolution themselves.
func rootHandleRep(t *testing.T, resources *component.HandleTable, handle uint32) uint32 {
	t.Helper()
	rep, err := resources.Rep(wasiDescriptorResType, handle)
	if err != nil {
		t.Fatalf("resolve descriptor handle %d: %v", handle, err)
	}
	return rep
}

func TestWasiFS_OpenAt_FullChain(t *testing.T) {
	const content = "chained open-at contents"
	h, resources, _ := wasiFSConfigDir(t, map[string][]byte{"/greeting.txt": []byte(content)})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "greeting.txt", uint32(0), uint32(0),
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if rv.IsErr {
		t.Fatalf("open-at: unexpected Err: %#v", rv.Payload)
	}
	fileHandle := rv.Payload.(uint32)
	fileRep, err := resources.Rep(wasiDescriptorResType, fileHandle)
	if err != nil {
		t.Fatalf("resolve opened file handle: %v", err)
	}

	getType := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	gtResults, err := getType(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("get-type: %v", err)
	}
	gtrv := gtResults[0].(abi.ResultValue)
	if gtrv.IsErr || gtrv.Payload.(uint32) != wasiDescriptorTypeRegularFile {
		t.Fatalf("get-type: got %#v, want Ok(regular-file)", gtrv)
	}

	stat := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.stat")
	stResults, err := stat(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	strv := stResults[0].(abi.ResultValue)
	if strv.IsErr {
		t.Fatalf("stat: unexpected Err: %#v", strv.Payload)
	}
	rec := strv.Payload.([]abi.Value)
	if got := rec[2].(uint64); got != uint64(len(content)) {
		t.Fatalf("stat: size = %d, want %d", got, len(content))
	}

	readViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream")
	rvsResults, err := readViaStream(context.Background(), []abi.Value{fileRep, uint64(0)})
	if err != nil {
		t.Fatalf("read-via-stream: %v", err)
	}
	rvsrv := rvsResults[0].(abi.ResultValue)
	if rvsrv.IsErr {
		t.Fatalf("read-via-stream: unexpected Err: %#v", rvsrv.Payload)
	}
	streamHandle := rvsrv.Payload.(uint32)
	streamRep, err := resources.Rep(wasiInputStreamResType, streamHandle)
	if err != nil {
		t.Fatalf("resolve stream handle: %v", err)
	}

	read := wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")
	rdResults, err := read(context.Background(), []abi.Value{streamRep, uint64(1024)})
	if err != nil {
		t.Fatalf("input-stream.read: %v", err)
	}
	rdrv := rdResults[0].(abi.ResultValue)
	if rdrv.IsErr {
		t.Fatalf("input-stream.read: unexpected Err: %#v", rdrv.Payload)
	}
	got := string(wasiBytesFromListT(t, rdrv.Payload))
	if got != content {
		t.Fatalf("input-stream.read: got %q, want %q", got, content)
	}

	// A second read at EOF must report stream-error::closed (case 1), not
	// an empty Ok list -- see streamRead's doc.
	rdResults2, err := read(context.Background(), []abi.Value{streamRep, uint64(1024)})
	if err != nil {
		t.Fatalf("input-stream.read (EOF): %v", err)
	}
	rdrv2 := rdResults2[0].(abi.ResultValue)
	if !rdrv2.IsErr {
		t.Fatalf("input-stream.read (EOF): got Ok(%#v), want Err(closed)", rdrv2.Payload)
	}
	vv := rdrv2.Payload.(abi.VariantValue)
	if vv.Disc != wasiStreamErrClosed {
		t.Fatalf("input-stream.read (EOF): variant case %d, want closed (%d)", vv.Disc, wasiStreamErrClosed)
	}
}

func wasiBytesFromListT(t *testing.T, v abi.Value) []byte {
	t.Helper()
	buf, err := wasiBytesFromList(v)
	if err != nil {
		t.Fatalf("wasiBytesFromList: %v", err)
	}
	return buf
}

func TestWasiFS_OpenAt_NoEntry(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "missing.txt", uint32(0), uint32(0),
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if !rv.IsErr || rv.Payload.(uint32) != wasiErrorCodeNoEntry {
		t.Fatalf("open-at(missing): got %#v, want Err(no-entry)", rv)
	}
}

func TestWasiFS_OpenAt_AbsolutePathRejected(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, map[string][]byte{"/x": []byte("x")})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "/x", uint32(0), uint32(0),
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if !rv.IsErr || rv.Payload.(uint32) != wasiErrorCodeNotPermitted {
		t.Fatalf("open-at(\"/x\"): got %#v, want Err(not-permitted)", rv)
	}
}

// TestWasiFS_OpenAt_Create_CreatesEntry proves open-at honors the create
// open-flag against a path WASIConfig.FS doesn't already have: the call
// succeeds (not error-code::read-only, this package's old, now-superseded
// behavior before write support existed) and the new path becomes visible
// in the same host fs map, as an empty regular file, immediately -- mirrors
// a real open(2) with O_CREAT making the directory entry exist right away.
func TestWasiFS_OpenAt_Create_CreatesEntry(t *testing.T) {
	h, resources, dir := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "new.txt", wasiOpenFlagCreate, wasiDescFlagWrite,
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if rv.IsErr {
		t.Fatalf("open-at(create): got %#v, want Ok", rv)
	}
	if got := hostRead(t, dir, "/new.txt"); got != "" {
		t.Fatalf("open-at(create): /new.txt = %q, want a new empty file", got)
	}

	fileHandle := rv.Payload.(uint32)
	fileRep, err := resources.Rep(wasiDescriptorResType, fileHandle)
	if err != nil {
		t.Fatalf("resolve opened file handle: %v", err)
	}
	getType := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	gtResults, err := getType(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("get-type: %v", err)
	}
	gtrv := gtResults[0].(abi.ResultValue)
	if gtrv.IsErr || gtrv.Payload.(uint32) != wasiDescriptorTypeRegularFile {
		t.Fatalf("get-type: got %#v, want Ok(regular-file)", gtrv)
	}
}

// TestWasiFS_OpenAt_Truncate proves the truncate open-flag resets an
// existing, writably-opened entry's content to empty -- and that a
// truncate request against a descriptor NOT opened for writing is not
// honored (matching a real OS's O_TRUNC|O_RDONLY combination doing
// nothing).
func TestWasiFS_OpenAt_Truncate(t *testing.T) {
	h, resources, dir := wasiFSConfigDir(t, map[string][]byte{"/f": []byte("original contents")})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)
	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")

	// Truncate without the write descriptor-flag: not honored.
	_, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "f", wasiOpenFlagTruncate, uint32(0),
	})
	if err != nil {
		t.Fatalf("open-at(truncate, read-only): %v", err)
	}
	if got := hostRead(t, dir, "/f"); got != "original contents" {
		t.Fatalf("open-at(truncate, read-only): /f = %q, want unchanged", got)
	}

	// Truncate with the write descriptor-flag: content resets to empty.
	_, err = openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "f", wasiOpenFlagTruncate, wasiDescFlagWrite,
	})
	if err != nil {
		t.Fatalf("open-at(truncate, write): %v", err)
	}
	if got := hostRead(t, dir, "/f"); got != "" {
		t.Fatalf("open-at(truncate, write): /f = %q, want empty", got)
	}
}

// TestWasiFS_WriteViaStream_WritesAndCommits drives open-at(create,write)
// -> write-via-stream -> [method]output-stream.write end to end, proving
// the bytes land in the host fs map (not just some internal buffer) and
// that blocking-flush against the resulting stream succeeds as a no-op
// (this package has no internal buffering to actually flush -- see
// writeStreamWrite's doc).
func TestWasiFS_WriteViaStream_WritesAndCommits(t *testing.T) {
	h, resources, dir := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	openResults, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "out.txt", wasiOpenFlagCreate, wasiDescFlagWrite,
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	fileRep := rootHandleRep(t, resources, openResults[0].(abi.ResultValue).Payload.(uint32))

	writeViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream")
	wvsResults, err := writeViaStream(context.Background(), []abi.Value{fileRep, uint64(0)})
	if err != nil {
		t.Fatalf("write-via-stream: %v", err)
	}
	wvsrv := wvsResults[0].(abi.ResultValue)
	if wvsrv.IsErr {
		t.Fatalf("write-via-stream: unexpected Err: %#v", wvsrv.Payload)
	}
	streamHandle := wvsrv.Payload.(uint32)
	streamRep, err := resources.Rep(wasiOutputStreamResType, streamHandle)
	if err != nil {
		t.Fatalf("resolve output-stream handle: %v", err)
	}

	write := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.write")
	wResults, err := write(context.Background(), []abi.Value{streamRep, wasiListFromBytes([]byte("hello "))})
	if err != nil {
		t.Fatalf("output-stream.write: %v", err)
	}
	if wResults[0].(abi.ResultValue).IsErr {
		t.Fatalf("output-stream.write: unexpected Err: %#v", wResults[0])
	}
	// A second write, at the stream's now-advanced cursor, must append
	// rather than overwrite from position 0.
	_, err = write(context.Background(), []abi.Value{streamRep, wasiListFromBytes([]byte("world"))})
	if err != nil {
		t.Fatalf("output-stream.write (2nd): %v", err)
	}
	if got := hostRead(t, dir, "/out.txt"); got != "hello world" {
		t.Fatalf("/out.txt = %q, want \"hello world\"", got)
	}

	blockingFlush := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.blocking-flush")
	bfResults, err := blockingFlush(context.Background(), []abi.Value{streamRep})
	if err != nil {
		t.Fatalf("blocking-flush: %v", err)
	}
	if bfResults[0].(abi.ResultValue).IsErr {
		t.Fatalf("blocking-flush: unexpected Err: %#v", bfResults[0])
	}

	checkWrite := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.check-write")
	cwResults, err := checkWrite(context.Background(), []abi.Value{streamRep})
	if err != nil {
		t.Fatalf("check-write: %v", err)
	}
	if cwResults[0].(abi.ResultValue).IsErr {
		t.Fatalf("check-write: unexpected Err: %#v", cwResults[0])
	}
}

// TestWasiFS_AppendViaStream proves append-via-stream seeds its stream's
// write cursor at the file's current length, not 0 -- distinct from
// write-via-stream(0), and exercised directly since no fixture this
// package runs actually calls it (std::fs::write always truncates instead
// -- see this file's package doc).
func TestWasiFS_AppendViaStream(t *testing.T) {
	h, resources, dir := wasiFSConfigDir(t, map[string][]byte{"/f": []byte("existing-")})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	openResults, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "f", uint32(0), wasiDescFlagWrite,
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	fileRep := rootHandleRep(t, resources, openResults[0].(abi.ResultValue).Payload.(uint32))

	appendViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream")
	avsResults, err := appendViaStream(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("append-via-stream: %v", err)
	}
	avsrv := avsResults[0].(abi.ResultValue)
	if avsrv.IsErr {
		t.Fatalf("append-via-stream: unexpected Err: %#v", avsrv.Payload)
	}
	streamRep, err := resources.Rep(wasiOutputStreamResType, avsrv.Payload.(uint32))
	if err != nil {
		t.Fatalf("resolve output-stream handle: %v", err)
	}

	write := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.write")
	_, err = write(context.Background(), []abi.Value{streamRep, wasiListFromBytes([]byte("appended"))})
	if err != nil {
		t.Fatalf("output-stream.write: %v", err)
	}
	if got := hostRead(t, dir, "/f"); got != "existing-appended" {
		t.Fatalf("/f = %q, want \"existing-appended\"", got)
	}
}

// TestWasiFS_WriteViaStream_ReadOnlyDescriptor proves write-via-stream (and
// append-via-stream) refuse a descriptor that wasn't opened with the write
// descriptor-flag, rather than silently allowing the write anyway.
func TestWasiFS_WriteViaStream_ReadOnlyDescriptor(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, map[string][]byte{"/f": []byte("x")})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	openResults, err := openAt(context.Background(), []abi.Value{rootRep, uint32(0), "f", uint32(0), uint32(0)})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	fileRep := rootHandleRep(t, resources, openResults[0].(abi.ResultValue).Payload.(uint32))

	writeViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream")
	wvsResults, err := writeViaStream(context.Background(), []abi.Value{fileRep, uint64(0)})
	if err != nil {
		t.Fatalf("write-via-stream: %v", err)
	}
	wvsrv := wvsResults[0].(abi.ResultValue)
	if !wvsrv.IsErr || wvsrv.Payload.(uint32) != wasiErrorCodeReadOnly {
		t.Fatalf("write-via-stream (read-only descriptor): got %#v, want Err(read-only)", wvsrv)
	}

	appendViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream")
	avsResults, err := appendViaStream(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("append-via-stream: %v", err)
	}
	avsrv := avsResults[0].(abi.ResultValue)
	if !avsrv.IsErr || avsrv.Payload.(uint32) != wasiErrorCodeReadOnly {
		t.Fatalf("append-via-stream (read-only descriptor): got %#v, want Err(read-only)", avsrv)
	}
}

// TestWasiFS_WriteViaStream_OnDirectory proves write-via-stream/
// append-via-stream against the root directory descriptor fail with
// is-directory, mirroring read-via-stream's own directory guard
// (TestWasiFS_OpenAt_OnNonDirectory).
func TestWasiFS_WriteViaStream_OnDirectory(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	writeViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream")
	wvsResults, err := writeViaStream(context.Background(), []abi.Value{rootRep, uint64(0)})
	if err != nil {
		t.Fatalf("write-via-stream (on root dir): %v", err)
	}
	wvsrv := wvsResults[0].(abi.ResultValue)
	if !wvsrv.IsErr || wvsrv.Payload.(uint32) != wasiErrorCodeIsDirectory {
		t.Fatalf("write-via-stream (on root dir): got %#v, want Err(is-directory)", wvsrv)
	}

	appendViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream")
	avsResults, err := appendViaStream(context.Background(), []abi.Value{rootRep})
	if err != nil {
		t.Fatalf("append-via-stream (on root dir): %v", err)
	}
	avsrv := avsResults[0].(abi.ResultValue)
	if !avsrv.IsErr || avsrv.Payload.(uint32) != wasiErrorCodeIsDirectory {
		t.Fatalf("append-via-stream (on root dir): got %#v, want Err(is-directory)", avsrv)
	}
}

// TestWasiFS_NilFS_PreopensNothing proves a nil WASIConfig.FS grants the
// guest no filesystem at all: get-directories returns an empty list, the
// same thing wasmtime does without --dir (and the same thing this package
// did before wasi_fs.go existed). The guest then fails on its own
// "failed to find a pre-opened file descriptor" path -- there is no
// descriptor for it to try anything against, which is the point.
func TestWasiFS_NilFS_PreopensNothing(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{FS: nil})

	getDirectories := wasiFSFn(t, h, wasiIfacePreopens, "get-directories")
	results, err := getDirectories(context.Background(), nil)
	if err != nil {
		t.Fatalf("get-directories: %v", err)
	}
	if dirs := results[0].([]abi.Value); len(dirs) != 0 {
		t.Fatalf("get-directories with a nil FS: got %d preopens, want none", len(dirs))
	}
}

// TestWasiFS_MultipleMounts proves each FSConfig mount becomes its own
// preopened descriptor, reported under its own guest path and resolving into
// its own filesystem -- the pycage shape (a root, a writable scratch, a
// read-only package tree), where a bare "a.txt" must find a different file
// depending on which preopen the guest resolved it against.
func TestWasiFS_MultipleMounts(t *testing.T) {
	rootDir, tmpDir, pkgDir := t.TempDir(), t.TempDir(), t.TempDir()
	for dir, content := range map[string]string{rootDir: "from-root", tmpDir: "from-tmp", pkgDir: "from-pkg"} {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", dir, err)
		}
	}
	fsc := wazy.NewFSConfig().
		WithDirMount(rootDir, "/").
		WithDirMount(tmpDir, "/tmp").
		WithReadOnlyDirMount(pkgDir, "/site-packages")
	h, resources := wasiFSConfig(t, WASIConfig{FS: fsc})

	getDirectories := wasiFSFn(t, h, wasiIfacePreopens, "get-directories")
	results, err := getDirectories(context.Background(), nil)
	if err != nil {
		t.Fatalf("get-directories: %v", err)
	}
	dirs := results[0].([]abi.Value)
	if len(dirs) != 3 {
		t.Fatalf("get-directories: got %d preopens, want 3", len(dirs))
	}

	wantPaths := []string{"/", "/tmp", "/site-packages"}
	wantContent := []string{"from-root", "from-tmp", "from-pkg"}
	for i, entry := range dirs {
		e := entry.([]abi.Value)
		if got := e[1].(string); got != wantPaths[i] {
			t.Errorf("preopen %d: guest path = %q, want %q", i, got, wantPaths[i])
		}
		descRep := rootHandleRep(t, resources, e[0].(uint32))
		if got := readFileVia(t, h, resources, descRep, "a.txt"); got != wantContent[i] {
			t.Errorf("preopen %d (%s): a.txt = %q, want %q", i, wantPaths[i], got, wantContent[i])
		}
	}

	// The read-only mount refuses a write: open-at with the write flag never
	// gets far enough to hand back a writable descriptor. sysfs.ReadFS
	// answers a write-mode open with ENOSYS (the same errno preview1 reports
	// for one), which fsErrorCode carries through as error-code::unsupported.
	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	pkgRep := rootHandleRep(t, resources, dirs[2].([]abi.Value)[0].(uint32))
	roResults, err := openAt(context.Background(), []abi.Value{
		pkgRep, uint32(0), "new.txt", wasiOpenFlagCreate, wasiDescFlagWrite,
	})
	if err != nil {
		t.Fatalf("open-at(create) on a read-only mount: %v", err)
	}
	rv := roResults[0].(abi.ResultValue)
	if !rv.IsErr {
		t.Fatalf("open-at(create) on a read-only mount: got Ok(%#v), want Err", rv.Payload)
	}
	if code := rv.Payload.(uint32); code != wasiErrorCodeUnsupported {
		t.Fatalf("open-at(create) on a read-only mount: got error-code %d, want unsupported (%d)", code, wasiErrorCodeUnsupported)
	}

	// mkdir on the same mount is refused too, as EROFS -> read-only.
	mkdirResults, err := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.create-directory-at")(
		context.Background(), []abi.Value{pkgRep, "newdir"})
	if err != nil {
		t.Fatalf("create-directory-at on a read-only mount: %v", err)
	}
	mrv := mkdirResults[0].(abi.ResultValue)
	if !mrv.IsErr || mrv.Payload.(uint32) != wasiErrorCodeReadOnly {
		t.Fatalf("create-directory-at on a read-only mount: got %#v, want Err(read-only)", mrv)
	}
}

// readFileVia opens rel under the descriptor named by dirRep and reads it
// whole, through the same open-at -> read-via-stream -> input-stream.read
// chain a guest walks.
func readFileVia(t *testing.T, h *componenttest.Harness, resources *component.HandleTable, dirRep uint32, rel string) string {
	t.Helper()
	ctx := context.Background()

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	openResults, err := openAt(ctx, []abi.Value{dirRep, uint32(0), rel, uint32(0), uint32(0)})
	if err != nil {
		t.Fatalf("open-at(%q): %v", rel, err)
	}
	orv := openResults[0].(abi.ResultValue)
	if orv.IsErr {
		t.Fatalf("open-at(%q): Err(%v)", rel, orv.Payload)
	}
	fileRep := rootHandleRep(t, resources, orv.Payload.(uint32))

	readViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream")
	rvsResults, err := readViaStream(ctx, []abi.Value{fileRep, uint64(0)})
	if err != nil {
		t.Fatalf("read-via-stream(%q): %v", rel, err)
	}
	rvsrv := rvsResults[0].(abi.ResultValue)
	if rvsrv.IsErr {
		t.Fatalf("read-via-stream(%q): Err(%v)", rel, rvsrv.Payload)
	}
	streamRep, err := resources.Rep(wasiInputStreamResType, rvsrv.Payload.(uint32))
	if err != nil {
		t.Fatalf("resolve input-stream handle: %v", err)
	}

	read := wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")
	var out []byte
	for {
		rdResults, err := read(ctx, []abi.Value{streamRep, uint64(1024)})
		if err != nil {
			t.Fatalf("input-stream.read(%q): %v", rel, err)
		}
		rdrv := rdResults[0].(abi.ResultValue)
		if rdrv.IsErr {
			return string(out) // stream-error::closed == EOF
		}
		out = append(out, wasiBytesFromListT(t, rdrv.Payload)...)
	}
}

// TestWasiFS_UnknownWriteStreamRep proves [method]output-stream.write,
// check-write, and blocking-flush fail loud on a rep that names neither a
// stdio stream nor a live file-write stream, rather than silently no-oping
// -- all three report it the same way (writerForRep's "does not name a
// stdout/stderr stream", see wasi.go's writeSink doc for why write's
// dispatch preserves that wording instead of fs.writeStreamWrite's own).
func TestWasiFS_UnknownWriteStreamRep(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})

	write := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.write")
	_, err := write(context.Background(), []abi.Value{uint32(99999), wasiListFromBytes([]byte("x"))})
	requireErrContains(t, err, "does not name a stdout/stderr stream")

	checkWrite := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.check-write")
	_, err = checkWrite(context.Background(), []abi.Value{uint32(99999)})
	requireErrContains(t, err, "does not name a stdout/stderr stream")

	blockingFlush := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.blocking-flush")
	_, err = blockingFlush(context.Background(), []abi.Value{uint32(99999)})
	requireErrContains(t, err, "does not name a stdout/stderr stream")
}

func TestWasiFS_OpenAt_OnNonDirectory(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, map[string][]byte{"/f": []byte("f")})
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{rootRep, uint32(0), "f", uint32(0), uint32(0)})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	fileHandle := results[0].(abi.ResultValue).Payload.(uint32)
	fileRep, err := resources.Rep(wasiDescriptorResType, fileHandle)
	if err != nil {
		t.Fatalf("resolve file handle: %v", err)
	}

	// Opening "anything" under a regular-file descriptor must fail with
	// not-directory, not silently treat it as a directory.
	results2, err := openAt(context.Background(), []abi.Value{fileRep, uint32(0), "anything", uint32(0), uint32(0)})
	if err != nil {
		t.Fatalf("open-at (on a file): %v", err)
	}
	rv2 := results2[0].(abi.ResultValue)
	if !rv2.IsErr || rv2.Payload.(uint32) != wasiErrorCodeNotDirectory {
		t.Fatalf("open-at (on a file): got %#v, want Err(not-directory)", rv2)
	}

	// read-via-stream on a directory descriptor must fail with is-directory.
	readViaStream := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream")
	rvsResults, err := readViaStream(context.Background(), []abi.Value{rootRep, uint64(0)})
	if err != nil {
		t.Fatalf("read-via-stream (on root dir): %v", err)
	}
	rvsrv := rvsResults[0].(abi.ResultValue)
	if !rvsrv.IsErr || rvsrv.Payload.(uint32) != wasiErrorCodeIsDirectory {
		t.Fatalf("read-via-stream (on root dir): got %#v, want Err(is-directory)", rvsrv)
	}
}

func TestWasiFS_UnknownDescriptorRep(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})
	getType := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	_, err := getType(context.Background(), []abi.Value{uint32(99999)})
	requireErrContains(t, err, "does not name a live descriptor")
}

func TestWasiFS_UnknownStreamRep(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})
	read := wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")
	_, err := read(context.Background(), []abi.Value{uint32(99999), uint64(1)})
	requireErrContains(t, err, "does not name a live stream")
}

func TestWasiFS_FilesystemErrorCode_AlwaysNone(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})
	fn := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "filesystem-error-code")
	results, err := fn(context.Background(), []abi.Value{uint32(1)})
	if err != nil {
		t.Fatalf("filesystem-error-code: %v", err)
	}
	if results[0] != nil {
		t.Fatalf("filesystem-error-code: got %#v, want none", results[0])
	}
}

func TestWasiFS_MetadataHash(t *testing.T) {
	h, resources, _ := wasiFSConfigDir(t, nil)
	rootHandle := rootDescriptorHandle(t, h)
	rootRep := rootHandleRep(t, resources, rootHandle)

	fn := wasiFSFn(t, h, wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash")
	results, err := fn(context.Background(), []abi.Value{rootRep})
	if err != nil {
		t.Fatalf("metadata-hash: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if rv.IsErr {
		t.Fatalf("metadata-hash: unexpected Err: %#v", rv.Payload)
	}
	rec := rv.Payload.([]abi.Value)
	if len(rec) != 2 {
		t.Fatalf("metadata-hash: got %d fields, want 2 (lower, upper)", len(rec))
	}
}

func TestWasiFS_GetTerminals_AlwaysNone(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})
	for _, tc := range []struct{ iface, name string }{
		{wasiIfaceTerminalStdin, "get-terminal-stdin"},
		{wasiIfaceTerminalStdout, "get-terminal-stdout"},
		{wasiIfaceTerminalStderr, "get-terminal-stderr"},
	} {
		fn := wasiFSFn(t, h, tc.iface, tc.name)
		results, err := fn(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s.%s: %v", tc.iface, tc.name, err)
		}
		if results[0] != nil {
			t.Fatalf("%s.%s: got %#v, want none", tc.iface, tc.name, results[0])
		}
	}
}

// Argument-shape validation: each closure fails loud on the wrong arg
// count/type rather than panicking on a bad type assertion.
func TestWasiFS_ArgValidation(t *testing.T) {
	h, _ := wasiFSConfig(t, WASIConfig{})

	tests := []struct {
		name    string
		iface   string
		funcN   string
		args    []abi.Value
		wantErr string
	}{
		{
			"open-at wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1)},
			"expected 5 args",
		},
		{
			"open-at bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{"not-a-rep", uint32(0), "p", uint32(0), uint32(0)},
			"self: expected uint32",
		},
		{
			"open-at bad path type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1), uint32(0), uint32(0), uint32(0), uint32(0)},
			"path: expected string",
		},
		{
			"open-at bad open-flags type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1), uint32(0), "p", "bad", uint32(0)},
			"open-flags: expected uint32",
		},
		{
			"open-at bad flags type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1), uint32(0), "p", uint32(0), "bad"},
			"flags: expected uint32",
		},
		{
			"write-via-stream wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream",
			[]abi.Value{uint32(1)},
			"expected 2 args",
		},
		{
			"write-via-stream bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream",
			[]abi.Value{"bad", uint64(0)},
			"self: expected uint32",
		},
		{
			"write-via-stream bad offset type", wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream",
			[]abi.Value{uint32(1), "bad"},
			"offset: expected uint64",
		},
		{
			"append-via-stream wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream",
			nil, "expected 1 arg",
		},
		{
			"append-via-stream bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream",
			[]abi.Value{"bad"},
			"self: expected uint32",
		},
		{
			"output-stream.write wrong arg count", wasiIfaceStreams, "[method]output-stream.write",
			[]abi.Value{uint32(1)},
			"expected 2 args",
		},
		{
			"output-stream.write bad self type", wasiIfaceStreams, "[method]output-stream.write",
			[]abi.Value{"bad", wasiListFromBytes(nil)},
			"self: expected uint32",
		},
		{
			"get-type wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.get-type",
			nil, "expected 1 arg",
		},
		{
			"get-type bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.get-type",
			[]abi.Value{"bad"},
			"self: expected uint32",
		},
		{
			"stat wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.stat",
			nil, "expected 1 arg",
		},
		{
			"stat bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.stat",
			[]abi.Value{"bad"},
			"self: expected uint32",
		},
		{
			"read-via-stream wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{uint32(1)},
			"expected 2 args",
		},
		{
			"read-via-stream bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{"bad", uint64(0)},
			"self: expected uint32",
		},
		{
			"read-via-stream bad offset type", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{uint32(1), "bad"},
			"offset: expected uint64",
		},
		{
			"input-stream.read wrong arg count", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{uint32(1)},
			"expected 2 args",
		},
		{
			"input-stream.read bad self type", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{"bad", uint64(0)},
			"self: expected uint32",
		},
		{
			"input-stream.read bad len type", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{uint32(1), "bad"},
			"len: expected uint64",
		},
		{
			"metadata-hash wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash",
			nil, "expected 1 arg",
		},
		{
			"metadata-hash bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash",
			[]abi.Value{"bad"},
			"self: expected uint32",
		},
		{
			"filesystem-error-code wrong arg count", wasiIfaceFilesystemTypes, "filesystem-error-code",
			nil, "expected 1 arg",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := wasiFSFn(t, h, tt.iface, tt.funcN)
			_, err := fn(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestWasiFS_WriteStreamWrite_UnknownRep exercises wasiFS.writeStreamWrite's
// own "does not name a live stream" guard directly: wasi.go's writeSink
// (WithWASI's [method]output-stream.write dispatch) always checks
// writeStreamNode itself first, so that guard is otherwise unreachable
// through the registered component.HostFunc -- see writeSink's doc for why it
// deliberately keeps writerForRep's wording instead of surfacing this one.
func TestWasiFS_WriteStreamWrite_UnknownRep(t *testing.T) {
	fs := newWasiFS(nil)
	err := fs.writeStreamWrite(99999, []byte("x"))
	requireErrContains(t, err, "does not name a live stream")
}

func TestWasiFS_GetResources_NotInitialized(t *testing.T) {
	// getResources must fail loud when the resource hook has not run, rather
	// than dereference a nil table. Tested against wasiFS directly: a real
	// registration always runs the hooks before any host func can execute
	// (componenttest.New does too), so this state is not reachable through a
	// harness -- which is the point of the guard.
	fs := newWasiFS(nil)
	_, err := fs.getResources()
	requireErrContains(t, err, "resources handle table not yet initialized")
}

// TestDatetimeDesc pins the exported datetime record.
//
// It is public API that other host packages intern into their own tables
// (wasi:otel's spans, logs and metrics all carry a datetime), and a record's
// field names never reach the wire -- only order and types do. So a reordering
// here would silently change the layout of every interface that reuses it, and
// no guest test elsewhere would necessarily say why.
func TestDatetimeDesc(t *testing.T) {
	d := DatetimeDesc()

	if len(d.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(d.Fields))
	}
	for i, want := range []struct{ name, prim string }{
		{"seconds", "u64"},
		{"nanoseconds", "u32"},
	} {
		if got := d.Fields[i].Name; got != want.name {
			t.Errorf("field %d name = %q, want %q", i, got, want.name)
		}
		if got := d.Fields[i].Type.Primitive; got != want.prim {
			t.Errorf("field %d type = %q, want %q", i, got, want.prim)
		}
	}

	// DatetimeType interns that same record and returns a ref to it.
	tbl := component.NewTypeTable()
	ref := DatetimeType(tbl)
	if ref.TypeIndex == nil {
		t.Fatal("DatetimeType should return a table ref, not an inline primitive")
	}
	got, ok := tbl.Resolver()(*ref.TypeIndex).(component.RecordDesc)
	if !ok {
		t.Fatalf("interned entry = %T, want component.RecordDesc", tbl.Resolver()(*ref.TypeIndex))
	}
	if len(got.Fields) != len(d.Fields) {
		t.Fatalf("interned record has %d fields, want %d", len(got.Fields), len(d.Fields))
	}

	// This package's own table interns the identical shape, so the two entry
	// points cannot drift apart.
	priv := &typeTable{}
	privRef := wasiDatetimeType(priv)
	if privRef.TypeIndex == nil {
		t.Fatal("wasiDatetimeType should return a table ref")
	}
	privDesc, ok := priv.entries[*privRef.TypeIndex].(component.RecordDesc)
	if !ok {
		t.Fatalf("private entry = %T, want component.RecordDesc", priv.entries[*privRef.TypeIndex])
	}
	for i := range d.Fields {
		if privDesc.Fields[i] != d.Fields[i] {
			t.Errorf("private table field %d = %+v, want %+v", i, privDesc.Fields[i], d.Fields[i])
		}
	}
}
