package instance

import (
	"context"
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/sys"
)

// Branch tests for the mutating wasi:filesystem/types methods --
// create-directory-at, remove-directory-at, rename-at, link-at,
// unlink-file-at -- driven through the registered HostFuncs against a real
// mount. The real_fsops guest covers the happy path end to end; these pin
// the error branches one guest run does not reach, and the two cases only a
// multi-mount configuration can produce (cross-device rename and link).

// fsOpsFixture is a config over a single "/" mount plus the resolved rep of
// its preopened root descriptor, the starting point of every test below.
func fsOpsFixture(t *testing.T, files map[string][]byte) (c *config, resources *handleTable, rootRep uint32, dir string) {
	t.Helper()
	c, resources, dir = wasiFSConfigDir(t, files)
	rootRep = rootHandleRep(t, resources, rootDescriptorHandle(t, c))
	return
}

// callFS invokes one registered wasi:filesystem/types method and returns its
// single result<...> value.
func callFS(t *testing.T, c *config, name string, args ...abi.Value) abi.ResultValue {
	t.Helper()
	results, err := wasiFSFn(t, c, wasiIfaceFilesystemTypes, name)(context.Background(), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return results[0].(abi.ResultValue)
}

func requireErrCode(t *testing.T, rv abi.ResultValue, want uint32, what string) {
	t.Helper()
	if !rv.IsErr {
		t.Fatalf("%s: got Ok(%#v), want Err(%d)", what, rv.Payload, want)
	}
	if got := rv.Payload.(uint32); got != want {
		t.Fatalf("%s: got error-code %d, want %d", what, got, want)
	}
}

func requireOk(t *testing.T, rv abi.ResultValue, what string) abi.Value {
	t.Helper()
	if rv.IsErr {
		t.Fatalf("%s: got Err(%v), want Ok", what, rv.Payload)
	}
	return rv.Payload
}

func TestWasiFS_CreateDirectoryAt(t *testing.T) {
	c, resources, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/file": []byte("x")})

	requireOk(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "d"), "create-directory-at(d)")
	if st, err := os.Stat(filepath.Join(dir, "d")); err != nil || !st.IsDir() {
		t.Fatalf("d after create-directory-at: stat err=%v, isDir=%v; want a real directory", err, err == nil && st.IsDir())
	}

	// Creating over an existing directory, or over an existing file, is the
	// mount's own EEXIST.
	requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "d"),
		wasiErrorCodeExist, "create-directory-at(d) twice")
	requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "file"),
		wasiErrorCodeExist, "create-directory-at over a file")

	// An absolute path is rejected before the mount is ever touched.
	requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "/abs"),
		wasiErrorCodeNotPermitted, `create-directory-at("/abs")`)

	// Against a regular-file descriptor: not-directory.
	fileRep := openRep(t, c, resources, rootRep, "file", 0, 0)
	requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", fileRep, "x"),
		wasiErrorCodeNotDirectory, "create-directory-at under a file descriptor")
}

func TestWasiFS_RemoveDirectoryAt(t *testing.T) {
	c, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/d/inner": []byte("x"), "/file": []byte("f")})

	requireErrCode(t, callFS(t, c, "[method]descriptor.remove-directory-at", rootRep, "nope"),
		wasiErrorCodeNoEntry, "remove-directory-at(missing)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.remove-directory-at", rootRep, "d"),
		wasiErrorCodeNotEmpty, "remove-directory-at(non-empty)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.remove-directory-at", rootRep, "file"),
		wasiErrorCodeNotDirectory, "remove-directory-at(a file)")

	// An empty directory -- the case the old synthetic-directory model could
	// not represent at all -- removes cleanly.
	requireOk(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "e"), "create-directory-at(e)")
	requireOk(t, callFS(t, c, "[method]descriptor.remove-directory-at", rootRep, "e"), "remove-directory-at(e)")
	requireAbsent(t, dir, "/e")
}

func TestWasiFS_UnlinkFileAt(t *testing.T) {
	c, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/a": []byte("A"), "/d/inner": []byte("x")})

	requireOk(t, callFS(t, c, "[method]descriptor.unlink-file-at", rootRep, "a"), "unlink-file-at(a)")
	requireAbsent(t, dir, "/a")

	requireErrCode(t, callFS(t, c, "[method]descriptor.unlink-file-at", rootRep, "a"),
		wasiErrorCodeNoEntry, "unlink-file-at(already gone)")
	// unlink(2) refuses a directory; sysfs normalizes the platform's EPERM to
	// EISDIR, which must surface as error-code::is-directory.
	requireErrCode(t, callFS(t, c, "[method]descriptor.unlink-file-at", rootRep, "d"),
		wasiErrorCodeIsDirectory, "unlink-file-at(a directory)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.unlink-file-at", rootRep, "/abs"),
		wasiErrorCodeNotPermitted, `unlink-file-at("/abs")`)
}

func TestWasiFS_RenameAt(t *testing.T) {
	c, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{
		"/a":             []byte("A"),
		"/src/f1":        []byte("1"),
		"/src/deep/f2":   []byte("2"),
		"/untouched.txt": []byte("u"),
	})

	requireErrCode(t, callFS(t, c, "[method]descriptor.rename-at", rootRep, "missing", rootRep, "x"),
		wasiErrorCodeNoEntry, "rename-at(missing source)")

	requireOk(t, callFS(t, c, "[method]descriptor.rename-at", rootRep, "a", rootRep, "b"), "rename-at(a -> b)")
	requireAbsent(t, dir, "/a")
	if got := hostRead(t, dir, "/b"); got != "A" {
		t.Fatalf("/b = %q, want \"A\"", got)
	}

	// A whole directory subtree moves in one call -- the mount's own rename,
	// no per-entry re-keying.
	requireOk(t, callFS(t, c, "[method]descriptor.rename-at", rootRep, "src", rootRep, "dst"), "rename-at(src -> dst)")
	requireAbsent(t, dir, "/src")
	if got := hostRead(t, dir, "/dst/deep/f2"); got != "2" {
		t.Fatalf("/dst/deep/f2 = %q, want \"2\"", got)
	}
	if got := hostRead(t, dir, "/untouched.txt"); got != "u" {
		t.Fatalf("/untouched.txt = %q, want \"u\" (an unrelated file was disturbed)", got)
	}

	requireErrCode(t, callFS(t, c, "[method]descriptor.rename-at", rootRep, "/abs", rootRep, "x"),
		wasiErrorCodeNotPermitted, `rename-at("/abs")`)
}

func TestWasiFS_LinkAt(t *testing.T) {
	c, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/a": []byte("A")})

	requireOk(t, callFS(t, c, "[method]descriptor.link-at", rootRep, uint32(0), "a", rootRep, "hard"), "link-at(a -> hard)")
	if got := hostRead(t, dir, "/hard"); got != "A" {
		t.Fatalf("/hard = %q, want \"A\"", got)
	}
	// A real hard link, not a copy: both names share one inode, which is
	// exactly what metadata-hash-at reports.
	hashA := requireOk(t, callFS(t, c, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "a"), "metadata-hash-at(a)")
	hashHard := requireOk(t, callFS(t, c, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "hard"), "metadata-hash-at(hard)")
	if a, h := hashA.([]abi.Value), hashHard.([]abi.Value); a[0] != h[0] || a[1] != h[1] {
		t.Fatalf("metadata-hash-at: a = %v, hard = %v; a hard link must share the source's identity", a, h)
	}

	requireErrCode(t, callFS(t, c, "[method]descriptor.link-at", rootRep, uint32(0), "a", rootRep, "hard"),
		wasiErrorCodeExist, "link-at onto an existing name")
	requireErrCode(t, callFS(t, c, "[method]descriptor.link-at", rootRep, uint32(0), "missing", rootRep, "x"),
		wasiErrorCodeNoEntry, "link-at(missing source)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.link-at", rootRep, uint32(0), "/abs", rootRep, "x"),
		wasiErrorCodeNotPermitted, `link-at("/abs")`)
}

// TestWasiFS_CrossMount proves rename-at and link-at refuse to span two
// mounts with cross-device, the errno std turns back into EXDEV (and, for
// rename, the signal for its copy-then-delete fallback) -- the one thing a
// single flat filesystem could never get wrong, and a multi-mount host must.
func TestWasiFS_CrossMount(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().
		WithDirMount(dirA, "/").
		WithDirMount(dirB, "/other")})

	results, err := wasiFSFn(t, c, wasiIfacePreopens, "get-directories")(context.Background(), nil)
	if err != nil {
		t.Fatalf("get-directories: %v", err)
	}
	dirs := results[0].([]abi.Value)
	if len(dirs) != 2 {
		t.Fatalf("get-directories: got %d preopens, want 2", len(dirs))
	}
	repA := rootHandleRep(t, resources, dirs[0].([]abi.Value)[0].(uint32))
	repB := rootHandleRep(t, resources, dirs[1].([]abi.Value)[0].(uint32))

	requireErrCode(t, callFS(t, c, "[method]descriptor.rename-at", repA, "a.txt", repB, "a.txt"),
		wasiErrorCodeCrossDevice, "rename-at across mounts")
	requireErrCode(t, callFS(t, c, "[method]descriptor.link-at", repA, uint32(0), "a.txt", repB, "a.txt"),
		wasiErrorCodeCrossDevice, "link-at across mounts")
}

// TestWasiFS_ReadDirectory lists a directory through the real
// read-directory -> read-directory-entry iterator, including an empty
// directory (which the previous flat-map model could not represent).
func TestWasiFS_ReadDirectory(t *testing.T) {
	c, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{
		"/a.txt":     []byte("a"),
		"/sub/b.txt": []byte("b"),
	})
	requireOk(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "empty"), "create-directory-at(empty)")

	got := listDir(t, c, resources, rootRep)
	want := map[string]bool{"a.txt": false, "sub": true, "empty": true}
	if len(got) != len(want) {
		t.Fatalf("read-directory(/): got %v, want %v", got, want)
	}
	for name, isDir := range want {
		if gotIsDir, ok := got[name]; !ok || gotIsDir != isDir {
			t.Fatalf("read-directory(/): entry %q = (isDir %v, present %v), want isDir %v", name, gotIsDir, ok, isDir)
		}
	}

	// An empty directory lists as zero entries -- not "." and "..", which
	// wasi:filesystem omits and which would make a guest recurse forever.
	emptyRep := openRep(t, c, resources, rootRep, "empty", wasiOpenFlagDirectory, 0)
	if entries := listDir(t, c, resources, emptyRep); len(entries) != 0 {
		t.Fatalf("read-directory(/empty): got %v, want no entries", entries)
	}

	// read-directory against a regular file is not-directory.
	fileRep := openRep(t, c, resources, rootRep, "a.txt", 0, 0)
	requireErrCode(t, callFS(t, c, "[method]descriptor.read-directory", fileRep),
		wasiErrorCodeNotDirectory, "read-directory(a regular file)")
}

// openRep drives open-at and returns the resolved rep of the descriptor it
// hands back.
func openRep(t *testing.T, c *config, resources *handleTable, dirRep uint32, rel string, openFlags, descFlags uint32) uint32 {
	t.Helper()
	rv := callFS(t, c, "[method]descriptor.open-at", dirRep, uint32(0), rel, openFlags, descFlags)
	return rootHandleRep(t, resources, requireOk(t, rv, "open-at("+rel+")").(uint32))
}

// listDir reads a directory descriptor's whole listing through the
// read-directory -> read-directory-entry iterator, as name -> isDir.
func listDir(t *testing.T, c *config, resources *handleTable, dirRep uint32) map[string]bool {
	t.Helper()
	streamHandle := requireOk(t, callFS(t, c, "[method]descriptor.read-directory", dirRep), "read-directory").(uint32)
	streamRep, err := resources.Rep(wasiDirEntryStreamResType, streamHandle)
	if err != nil {
		t.Fatalf("resolve directory-entry-stream handle: %v", err)
	}
	out := map[string]bool{}
	for {
		rv := callFS(t, c, "[method]directory-entry-stream.read-directory-entry", streamRep)
		entry := requireOk(t, rv, "read-directory-entry")
		if entry == nil {
			return out // option::none: exhausted
		}
		e := entry.([]abi.Value)
		out[e[1].(string)] = e[0].(uint32) == wasiDescriptorTypeDirectory
	}
}

// TestFSErrorCode pins the errno -> error-code mapping, including the
// catch-all: an errno with no specific counterpart must report `io` rather
// than silently claiming something more specific.
func TestFSErrorCode(t *testing.T) {
	for _, tc := range []struct {
		errno sys.Errno
		want  uint32
	}{
		{sys.ENOENT, wasiErrorCodeNoEntry},
		{sys.EEXIST, wasiErrorCodeExist},
		{sys.ENOTDIR, wasiErrorCodeNotDirectory},
		{sys.EISDIR, wasiErrorCodeIsDirectory},
		{sys.ENOTEMPTY, wasiErrorCodeNotEmpty},
		{sys.EPERM, wasiErrorCodeNotPermitted},
		{sys.EACCES, wasiErrorCodeAccess},
		{sys.EBADF, wasiErrorCodeBadDescriptor},
		{sys.EROFS, wasiErrorCodeReadOnly},
		{sys.ENOSYS, wasiErrorCodeUnsupported},
		{sys.EINVAL, wasiErrorCodeInvalid},
		{sys.EIO, wasiErrorCodeIO},
		{sys.ELOOP, wasiErrorCodeLoop},
		{sys.ENAMETOOLONG, wasiErrorCodeNameTooLong},
		{sys.EINTR, wasiErrorCodeInterrupted},
		{sys.EAGAIN, wasiErrorCodeWouldBlock},
		{sys.ENOTSUP, wasiErrorCodeUnsupported},
		{sys.ERANGE, wasiErrorCodeIO}, // no counterpart: the catch-all
	} {
		if got := fsErrorCode(tc.errno); got != tc.want {
			t.Errorf("fsErrorCode(%v) = %d, want %d", tc.errno, got, tc.want)
		}
	}
}

// TestFSMountsFromConfig covers guest-path normalization (every spelling of
// the root coerces to "/") and the non-wazy FSConfig fallback.
func TestFSMountsFromConfig(t *testing.T) {
	dir := t.TempDir()
	for _, guestPath := range []string{"", ".", "/", "./"} {
		mounts := fsMountsFromConfig(wazy.NewFSConfig().WithDirMount(dir, guestPath))
		if len(mounts) != 1 || mounts[0].guestPath != "/" {
			t.Errorf("WithDirMount(dir, %q): got %v, want one mount at %q", guestPath, mounts, "/")
		}
	}
	for _, guestPath := range []string{"tmp", "/tmp", "/tmp/"} {
		mounts := fsMountsFromConfig(wazy.NewFSConfig().WithDirMount(dir, guestPath))
		if len(mounts) != 1 || mounts[0].guestPath != "/tmp" {
			t.Errorf("WithDirMount(dir, %q): got %v, want one mount at %q", guestPath, mounts, "/tmp")
		}
	}
	if mounts := fsMountsFromConfig(nil); mounts != nil {
		t.Errorf("fsMountsFromConfig(nil) = %v, want nil", mounts)
	}
	if mounts := fsMountsFromConfig(notAWazyFSConfig{}); mounts != nil {
		t.Errorf("fsMountsFromConfig(foreign impl) = %v, want nil", mounts)
	}
}

// notAWazyFSConfig is an FSConfig implementation from outside wazy -- which
// FSConfig's own doc says cannot exist, so fsMountsFromConfig treats it as
// having no mounts rather than panicking on the Preopens assertion.
type notAWazyFSConfig struct{ wazy.FSConfig }

// TestWasiFS_ArgValidation_AtMethods extends wasi_fs_test.go's arg-shape
// table to the *-at methods and the directory iterator it did not reach:
// each must fail loud on the wrong arg count/type rather than panicking on a
// bad type assertion.
func TestWasiFS_ArgValidation_AtMethods(t *testing.T) {
	c, _ := wasiFSConfig(t, WASIConfig{})

	for _, tt := range []struct {
		name    string
		funcN   string
		args    []abi.Value
		wantErr string
	}{
		{"stat-at count", "[method]descriptor.stat-at", []abi.Value{uint32(1)}, "expected 3 args"},
		{"stat-at self", "[method]descriptor.stat-at", []abi.Value{"bad", uint32(0), "p"}, "self: expected uint32"},
		{"stat-at path", "[method]descriptor.stat-at", []abi.Value{uint32(1), uint32(0), uint32(0)}, "path: expected string"},

		{"read-directory count", "[method]descriptor.read-directory", nil, "expected 1 arg"},
		{"read-directory self", "[method]descriptor.read-directory", []abi.Value{"bad"}, "self: expected uint32"},

		{"read-directory-entry count", "[method]directory-entry-stream.read-directory-entry", nil, "expected 1 arg"},
		{"read-directory-entry self", "[method]directory-entry-stream.read-directory-entry", []abi.Value{"bad"}, "self: expected uint32"},

		{"unlink-file-at count", "[method]descriptor.unlink-file-at", []abi.Value{uint32(1)}, "expected 2 args"},
		{"unlink-file-at self", "[method]descriptor.unlink-file-at", []abi.Value{"bad", "p"}, "self: expected uint32"},
		{"unlink-file-at path", "[method]descriptor.unlink-file-at", []abi.Value{uint32(1), uint32(0)}, "path: expected string"},

		{"create-directory-at count", "[method]descriptor.create-directory-at", []abi.Value{uint32(1)}, "expected 2 args"},
		{"create-directory-at self", "[method]descriptor.create-directory-at", []abi.Value{"bad", "p"}, "self: expected uint32"},
		{"create-directory-at path", "[method]descriptor.create-directory-at", []abi.Value{uint32(1), uint32(0)}, "path: expected string"},
		{"remove-directory-at count", "[method]descriptor.remove-directory-at", []abi.Value{uint32(1)}, "expected 2 args"},

		{"rename-at count", "[method]descriptor.rename-at", []abi.Value{uint32(1)}, "expected 4 args"},
		{"rename-at self", "[method]descriptor.rename-at", []abi.Value{"bad", "o", uint32(1), "n"}, "self: expected uint32"},
		{"rename-at old-path", "[method]descriptor.rename-at", []abi.Value{uint32(1), uint32(0), uint32(1), "n"}, "old-path: expected string"},
		{"rename-at new-descriptor", "[method]descriptor.rename-at", []abi.Value{uint32(1), "o", "bad", "n"}, "new-descriptor: expected uint32"},
		{"rename-at new-path", "[method]descriptor.rename-at", []abi.Value{uint32(1), "o", uint32(1), uint32(0)}, "new-path: expected string"},

		{"link-at count", "[method]descriptor.link-at", []abi.Value{uint32(1)}, "expected 5 args"},
		{"link-at self", "[method]descriptor.link-at", []abi.Value{"bad", uint32(0), "o", uint32(1), "n"}, "self: expected uint32"},
		{"link-at old-path", "[method]descriptor.link-at", []abi.Value{uint32(1), uint32(0), uint32(0), uint32(1), "n"}, "old-path: expected string"},
		{"link-at new-descriptor", "[method]descriptor.link-at", []abi.Value{uint32(1), uint32(0), "o", "bad", "n"}, "new-descriptor: expected uint32"},
		{"link-at new-path", "[method]descriptor.link-at", []abi.Value{uint32(1), uint32(0), "o", uint32(1), uint32(0)}, "new-path: expected string"},

		{"metadata-hash-at count", "[method]descriptor.metadata-hash-at", []abi.Value{uint32(1)}, "expected 3 args"},
		{"metadata-hash-at self", "[method]descriptor.metadata-hash-at", []abi.Value{"bad", uint32(0), "p"}, "self: expected uint32"},
		{"metadata-hash-at path", "[method]descriptor.metadata-hash-at", []abi.Value{uint32(1), uint32(0), uint32(0)}, "path: expected string"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := wasiFSFn(t, c, wasiIfaceFilesystemTypes, tt.funcN)(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestWasiFS_UnknownReps proves every method that resolves a descriptor or a
// directory-entry-stream rep fails loud on one that names nothing live,
// rather than dereferencing a missing map entry.
func TestWasiFS_UnknownReps(t *testing.T) {
	c, _, rootRep, _ := fsOpsFixture(t, nil)
	const dead = uint32(99999)

	for _, tt := range []struct {
		name    string
		funcN   string
		args    []abi.Value
		wantErr string
	}{
		{"open-at", "[method]descriptor.open-at", []abi.Value{dead, uint32(0), "p", uint32(0), uint32(0)}, "does not name a live descriptor"},
		{"stat", "[method]descriptor.stat", []abi.Value{dead}, "does not name a live descriptor"},
		{"stat-at", "[method]descriptor.stat-at", []abi.Value{dead, uint32(0), "p"}, "does not name a live descriptor"},
		{"read-directory", "[method]descriptor.read-directory", []abi.Value{dead}, "does not name a live descriptor"},
		{"unlink-file-at", "[method]descriptor.unlink-file-at", []abi.Value{dead, "p"}, "does not name a live descriptor"},
		{"create-directory-at", "[method]descriptor.create-directory-at", []abi.Value{dead, "p"}, "does not name a live descriptor"},
		{"rename-at self", "[method]descriptor.rename-at", []abi.Value{dead, "o", rootRep, "n"}, "does not name a live descriptor"},
		{"rename-at new-descriptor", "[method]descriptor.rename-at", []abi.Value{rootRep, "o", dead, "n"}, "new-descriptor: wasi:filesystem/types: descriptor rep"},
		{"link-at self", "[method]descriptor.link-at", []abi.Value{dead, uint32(0), "o", rootRep, "n"}, "does not name a live descriptor"},
		{"link-at new-descriptor", "[method]descriptor.link-at", []abi.Value{rootRep, uint32(0), "o", dead, "n"}, "new-descriptor: wasi:filesystem/types: descriptor rep"},
		{"read-via-stream", "[method]descriptor.read-via-stream", []abi.Value{dead, uint64(0)}, "does not name a live descriptor"},
		{"write-via-stream", "[method]descriptor.write-via-stream", []abi.Value{dead, uint64(0)}, "does not name a live descriptor"},
		{"append-via-stream", "[method]descriptor.append-via-stream", []abi.Value{dead}, "does not name a live descriptor"},
		{"metadata-hash", "[method]descriptor.metadata-hash", []abi.Value{dead}, "does not name a live descriptor"},
		{"metadata-hash-at", "[method]descriptor.metadata-hash-at", []abi.Value{dead, uint32(0), "p"}, "does not name a live descriptor"},
		{"read-directory-entry", "[method]directory-entry-stream.read-directory-entry", []abi.Value{dead}, "does not name a live stream"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := wasiFSFn(t, c, wasiIfaceFilesystemTypes, tt.funcN)(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestWasiFS_NotDirectorySelf proves every *-at method rejects a
// regular-file descriptor as its directory argument (either argument, for
// the two that take two).
func TestWasiFS_NotDirectorySelf(t *testing.T) {
	c, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	fileRep := openRep(t, c, resources, rootRep, "f", 0, 0)

	for _, tt := range []struct {
		name  string
		funcN string
		args  []abi.Value
	}{
		{"stat-at", "[method]descriptor.stat-at", []abi.Value{fileRep, uint32(0), "p"}},
		{"unlink-file-at", "[method]descriptor.unlink-file-at", []abi.Value{fileRep, "p"}},
		{"remove-directory-at", "[method]descriptor.remove-directory-at", []abi.Value{fileRep, "p"}},
		{"read-directory", "[method]descriptor.read-directory", []abi.Value{fileRep}},
		{"metadata-hash-at", "[method]descriptor.metadata-hash-at", []abi.Value{fileRep, uint32(0), "p"}},
		{"rename-at self", "[method]descriptor.rename-at", []abi.Value{fileRep, "o", rootRep, "n"}},
		{"rename-at new-descriptor", "[method]descriptor.rename-at", []abi.Value{rootRep, "o", fileRep, "n"}},
		{"link-at self", "[method]descriptor.link-at", []abi.Value{fileRep, uint32(0), "o", rootRep, "n"}},
		{"link-at new-descriptor", "[method]descriptor.link-at", []abi.Value{rootRep, uint32(0), "o", fileRep, "n"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireErrCode(t, callFS(t, c, tt.funcN, tt.args...), wasiErrorCodeNotDirectory, tt.name)
		})
	}
}

// TestWasiFS_AbsolutePathRejected covers the wasiJoinFSPath guard on the
// methods wasi_fs_test.go's open-at case does not reach.
func TestWasiFS_AbsolutePathRejected(t *testing.T) {
	c, _, rootRep, _ := fsOpsFixture(t, nil)
	for _, tt := range []struct {
		name  string
		funcN string
		args  []abi.Value
	}{
		{"stat-at", "[method]descriptor.stat-at", []abi.Value{rootRep, uint32(0), "/abs"}},
		{"remove-directory-at", "[method]descriptor.remove-directory-at", []abi.Value{rootRep, "/abs"}},
		{"metadata-hash-at", "[method]descriptor.metadata-hash-at", []abi.Value{rootRep, uint32(0), "/abs"}},
		{"rename-at new-path", "[method]descriptor.rename-at", []abi.Value{rootRep, "o", rootRep, "/abs"}},
		{"link-at new-path", "[method]descriptor.link-at", []abi.Value{rootRep, uint32(0), "o", rootRep, "/abs"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireErrCode(t, callFS(t, c, tt.funcN, tt.args...), wasiErrorCodeNotPermitted, tt.name)
		})
	}
}

// TestWasiFS_StatAt covers stat-at's success and no-entry paths, including
// the real timestamps a mount now supplies where the old flat map could only
// answer `none`.
func TestWasiFS_StatAt(t *testing.T) {
	c, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("12345")})

	rec := requireOk(t, callFS(t, c, "[method]descriptor.stat-at", rootRep, uint32(0), "f"), "stat-at(f)").([]abi.Value)
	if rec[0].(uint32) != wasiDescriptorTypeRegularFile {
		t.Errorf("stat-at(f): type = %v, want regular-file", rec[0])
	}
	if rec[2].(uint64) != 5 {
		t.Errorf("stat-at(f): size = %v, want 5", rec[2])
	}
	if rec[4] == nil {
		t.Error("stat-at(f): data-modification-timestamp is none; a real mount has one")
	}

	dirRec := requireOk(t, callFS(t, c, "[method]descriptor.stat-at", rootRep, uint32(0), "."), "stat-at(.)").([]abi.Value)
	if dirRec[0].(uint32) != wasiDescriptorTypeDirectory {
		t.Errorf("stat-at(.): type = %v, want directory", dirRec[0])
	}

	requireErrCode(t, callFS(t, c, "[method]descriptor.stat-at", rootRep, uint32(0), "missing"),
		wasiErrorCodeNoEntry, "stat-at(missing)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "missing"),
		wasiErrorCodeNoEntry, "metadata-hash-at(missing)")
}

// TestWasiFS_OpenAt_Exclusive proves the exclusive open-flag reaches the
// mount as O_EXCL: creating an existing path fails with exist rather than
// silently reopening it.
func TestWasiFS_OpenAt_Exclusive(t *testing.T) {
	c, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	requireErrCode(t,
		callFS(t, c, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagCreate|wasiOpenFlagExclusive, wasiDescFlagWrite),
		wasiErrorCodeExist, "open-at(create|exclusive) over an existing file")

	// Without exclusive, the same open succeeds.
	requireOk(t, callFS(t, c, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagCreate, wasiDescFlagWrite),
		"open-at(create) over an existing file")
}

// TestWasiFS_OpenAt_DirectoryFlagOnFile proves open-flags::directory against
// a regular file is not-directory. Some platforms' open(2) rejects it with
// ENOTDIR outright; where it does not, the IsDir check after the open does.
func TestWasiFS_OpenAt_DirectoryFlagOnFile(t *testing.T) {
	c, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	requireErrCode(t,
		callFS(t, c, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagDirectory, uint32(0)),
		wasiErrorCodeNotDirectory, "open-at(directory) on a regular file")
}

// TestWasiFS_VanishedPaths proves the methods that touch the mount after a
// descriptor already exists surface the mount's errno instead of assuming
// the path is still there -- the failure mode a descriptor holding a cached
// snapshot could never report at all.
func TestWasiFS_VanishedPaths(t *testing.T) {
	c, resources, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/f": []byte("payload"), "/d/x": []byte("x")})
	fileRep := openRep(t, c, resources, rootRep, "f", 0, wasiDescFlagWrite)
	dirRep := openRep(t, c, resources, rootRep, "d", wasiOpenFlagDirectory, 0)

	// A write stream outlives its file: the next write reports the failure.
	wStream := requireOk(t, callFS(t, c, "[method]descriptor.write-via-stream", fileRep, uint64(0)), "write-via-stream").(uint32)
	wRep, err := resources.Rep(wasiOutputStreamResType, wStream)
	if err != nil {
		t.Fatalf("resolve output-stream handle: %v", err)
	}
	// A read stream, likewise.
	rStream := requireOk(t, callFS(t, c, "[method]descriptor.read-via-stream", fileRep, uint64(0)), "read-via-stream").(uint32)
	rRep, err := resources.Rep(wasiInputStreamResType, rStream)
	if err != nil {
		t.Fatalf("resolve input-stream handle: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "f")); err != nil {
		t.Fatalf("removing f: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
		t.Fatalf("removing d: %v", err)
	}

	_, err = wasiFSFn(t, c, wasiIfaceStreams, "[method]output-stream.write")(
		context.Background(), []abi.Value{wRep, wasiListFromBytes([]byte("x"))})
	requireErrContains(t, err, "opening")

	_, err = wasiFSFn(t, c, wasiIfaceStreams, "[method]input-stream.read")(
		context.Background(), []abi.Value{rRep, uint64(4)})
	requireErrContains(t, err, "opening")

	requireErrCode(t, callFS(t, c, "[method]descriptor.stat", fileRep), wasiErrorCodeNoEntry, "stat(vanished)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.get-type", fileRep), wasiErrorCodeNoEntry, "get-type(vanished)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.metadata-hash", fileRep), wasiErrorCodeNoEntry, "metadata-hash(vanished)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.append-via-stream", fileRep), wasiErrorCodeNoEntry, "append-via-stream(vanished)")
	requireErrCode(t, callFS(t, c, "[method]descriptor.read-directory", dirRep), wasiErrorCodeNoEntry, "read-directory(vanished)")
}

// TestWasiFS_ReadStream_CapsLength proves a guest-chosen read length is
// capped (see wasiMaxStreamRead): a u64 the guest picked must never become
// the size of a host allocation. A short read is a legal answer, so the
// call still succeeds.
func TestWasiFS_ReadStream_CapsLength(t *testing.T) {
	c, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("payload")})
	fileRep := openRep(t, c, resources, rootRep, "f", 0, 0)
	stream := requireOk(t, callFS(t, c, "[method]descriptor.read-via-stream", fileRep, uint64(0)), "read-via-stream").(uint32)
	streamRep, err := resources.Rep(wasiInputStreamResType, stream)
	if err != nil {
		t.Fatalf("resolve input-stream handle: %v", err)
	}

	results, err := wasiFSFn(t, c, wasiIfaceStreams, "[method]input-stream.read")(
		context.Background(), []abi.Value{streamRep, ^uint64(0)})
	if err != nil {
		t.Fatalf("input-stream.read(u64 max): %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if rv.IsErr {
		t.Fatalf("input-stream.read(u64 max): Err(%v), want the file's bytes", rv.Payload)
	}
	if got := string(wasiBytesFromListT(t, rv.Payload)); got != "payload" {
		t.Fatalf("input-stream.read(u64 max) = %q, want %q", got, "payload")
	}
}

func TestFSDescriptorType(t *testing.T) {
	for _, tc := range []struct {
		mode iofs.FileMode
		want uint32
	}{
		{0o644, wasiDescriptorTypeRegularFile},
		{iofs.ModeDir | 0o755, wasiDescriptorTypeDirectory},
		{iofs.ModeSymlink | 0o777, wasiDescriptorTypeSymbolicLink},
		{iofs.ModeNamedPipe | 0o644, wasiDescriptorTypeFIFO},
		{iofs.ModeSocket | 0o644, wasiDescriptorTypeSocket},
		{iofs.ModeDevice | iofs.ModeCharDevice | 0o644, wasiDescriptorTypeCharacterDevice},
		{iofs.ModeDevice | 0o644, wasiDescriptorTypeBlockDevice},
	} {
		if got := fsDescriptorType(tc.mode); got != tc.want {
			t.Errorf("fsDescriptorType(%v) = %d, want %d", tc.mode, got, tc.want)
		}
	}
}

func TestFSDatetime(t *testing.T) {
	// A mount that keeps no times reports 0, which must lower to `none`
	// rather than to an epoch-zero datetime.
	if got := fsDatetime(0); got != nil {
		t.Errorf("fsDatetime(0) = %v, want nil (option::none)", got)
	}
	if got := fsDatetime(-1); got != nil {
		t.Errorf("fsDatetime(-1) = %v, want nil (option::none)", got)
	}
	got, ok := fsDatetime(1_500_000_123).([]abi.Value)
	if !ok || len(got) != 2 {
		t.Fatalf("fsDatetime(1500000123) = %#v, want a 2-field datetime record", got)
	}
	if got[0].(uint64) != 1 || got[1].(uint32) != 500_000_123 {
		t.Errorf("fsDatetime(1500000123) = {%v, %v}, want {1, 500000123}", got[0], got[1])
	}
}

// TestWasiFS_FSMount proves a plain io/fs.FS mounts read-only through
// WithFSMount: the guest reads files and lists directories out of it, and
// every write is refused. Any io/fs.FS works the same way -- os.DirFS,
// embed.FS, or a third-party adapter such as afero.NewIOFS -- so this uses
// the stdlib's own fstest.MapFS rather than pulling in a dependency to prove
// the same seam.
func TestWasiFS_FSMount(t *testing.T) {
	c, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().WithFSMount(fstest.MapFS{
		"greeting.txt": {Data: []byte("from an io/fs.FS")},
		"sub/deep.txt": {Data: []byte("nested")},
	}, "/")})
	rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, c))

	if got := readFileVia(t, c, resources, rootRep, "greeting.txt"); got != "from an io/fs.FS" {
		t.Fatalf("greeting.txt = %q, want %q", got, "from an io/fs.FS")
	}

	entries := listDir(t, c, resources, rootRep)
	if isDir, ok := entries["sub"]; !ok || !isDir {
		t.Fatalf("read-directory(/): got %v, want a directory entry \"sub\"", entries)
	}
	if isDir, ok := entries["greeting.txt"]; !ok || isDir {
		t.Fatalf("read-directory(/): got %v, want a file entry \"greeting.txt\"", entries)
	}

	subRep := openRep(t, c, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)
	if got := readFileVia(t, c, resources, subRep, "deep.txt"); got != "nested" {
		t.Fatalf("sub/deep.txt = %q, want %q", got, "nested")
	}

	// io/fs.FS has no write surface at all, so every mutation is refused
	// rather than silently dropped. A create-open reports no-entry (io/fs.FS
	// can only ever open what already exists, so the path stays missing);
	// the mutating methods report unsupported outright.
	requireErrCode(t, callFS(t, c, "[method]descriptor.open-at", rootRep, uint32(0), "new.txt", wasiOpenFlagCreate, wasiDescFlagWrite),
		wasiErrorCodeNoEntry, "open-at(create) on an io/fs.FS mount")
	requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", rootRep, "newdir"),
		wasiErrorCodeUnsupported, "create-directory-at on an io/fs.FS mount")
	requireErrCode(t, callFS(t, c, "[method]descriptor.unlink-file-at", rootRep, "greeting.txt"),
		wasiErrorCodeUnsupported, "unlink-file-at on an io/fs.FS mount")
}

// TestWasiFS_EscapeRejected is the security test for the mount boundary: a
// guest must not reach anything outside the directory it was given, whether
// it walks up from the preopen root or from a subdirectory descriptor it
// holds. The host file planted next to the mount is what a successful escape
// would read, so an assertion that the call fails is also an assertion that
// its contents never reached the guest.
func TestWasiFS_EscapeRejected(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("host secret"), 0o600); err != nil {
		t.Fatalf("planting the host file: %v", err)
	}
	mount := filepath.Join(parent, "mount")
	if err := os.MkdirAll(filepath.Join(mount, "sub"), 0o755); err != nil {
		t.Fatalf("creating the mount: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mount, "sub", "ok.txt"), []byte("in-mount"), 0o644); err != nil {
		t.Fatalf("seeding the mount: %v", err)
	}

	c, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().WithDirMount(mount, "/")})
	rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, c))
	subRep := openRep(t, c, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)

	// Sanity: the descriptors do work for paths that stay inside.
	if got := readFileVia(t, c, resources, subRep, "ok.txt"); got != "in-mount" {
		t.Fatalf("sub/ok.txt = %q, want %q", got, "in-mount")
	}

	for _, tt := range []struct {
		name    string
		dirRep  uint32
		relPath string
	}{
		{"up from the preopen root", rootRep, "../secret.txt"},
		{"up twice from the root", rootRep, "../../etc/passwd"},
		{"up from a subdirectory descriptor", subRep, "../../secret.txt"},
		{"up to the preopen from a subdirectory", subRep, ".."},
		{"escaping only after cleaning", rootRep, "sub/../../secret.txt"},
		{"rooted host path", rootRep, "/etc/passwd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Every method that resolves a guest path must refuse it, not
			// just open-at: a reader, a writer, a metadata probe, and a
			// destructive one.
			requireErrCode(t, callFS(t, c, "[method]descriptor.open-at", tt.dirRep, uint32(0), tt.relPath, uint32(0), uint32(0)),
				wasiErrorCodeNotPermitted, "open-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.open-at", tt.dirRep, uint32(0), tt.relPath, wasiOpenFlagCreate, wasiDescFlagWrite),
				wasiErrorCodeNotPermitted, "open-at(create, "+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.stat-at", tt.dirRep, uint32(0), tt.relPath),
				wasiErrorCodeNotPermitted, "stat-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.metadata-hash-at", tt.dirRep, uint32(0), tt.relPath),
				wasiErrorCodeNotPermitted, "metadata-hash-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.unlink-file-at", tt.dirRep, tt.relPath),
				wasiErrorCodeNotPermitted, "unlink-file-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.create-directory-at", tt.dirRep, tt.relPath),
				wasiErrorCodeNotPermitted, "create-directory-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.rename-at", tt.dirRep, tt.relPath, rootRep, "landing"),
				wasiErrorCodeNotPermitted, "rename-at(from "+tt.relPath+")")
			requireErrCode(t, callFS(t, c, "[method]descriptor.link-at", tt.dirRep, uint32(0), tt.relPath, rootRep, "landing"),
				wasiErrorCodeNotPermitted, "link-at(from "+tt.relPath+")")
		})
	}

	// Nothing above created, moved, or removed anything outside the mount.
	if got, err := os.ReadFile(filepath.Join(parent, "secret.txt")); err != nil || string(got) != "host secret" {
		t.Fatalf("the host file outside the mount = (%q, %v), want it untouched", got, err)
	}
}

// TestWasiFS_RelativeLinksThatStayInside proves the clamp is not blunt: a
// path that walks up and back down without ever leaving the descriptor
// resolves normally, which is what the WASI testsuite's interesting_paths
// cases require.
func TestWasiFS_RelativeLinksThatStayInside(t *testing.T) {
	c, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/sub/deep/x.txt": []byte("reached")})

	if got := readFileVia(t, c, resources, rootRep, "sub/deep/../deep/x.txt"); got != "reached" {
		t.Fatalf("sub/deep/../deep/x.txt = %q, want %q", got, "reached")
	}
	subRep := openRep(t, c, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)
	if got := readFileVia(t, c, resources, subRep, "./deep/x.txt"); got != "reached" {
		t.Fatalf("./deep/x.txt under sub = %q, want %q", got, "reached")
	}
}
