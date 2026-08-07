package wasip2

import (
	"context"
	iofs "io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/componenttest"
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
func fsOpsFixture(t *testing.T, files map[string][]byte) (h *componenttest.Harness, resources *component.HandleTable, rootRep uint32, dir string) {
	t.Helper()
	h, resources, dir = wasiFSConfigDir(t, files)
	rootRep = rootHandleRep(t, resources, rootDescriptorHandle(t, h))
	return
}

// callFS invokes one registered wasi:filesystem/types method and returns its
// single result<...> value.
func callFS(t *testing.T, h *componenttest.Harness, name string, args ...abi.Value) abi.ResultValue {
	t.Helper()
	results, err := wasiFSFn(t, h, wasiIfaceFilesystemTypes, name)(context.Background(), args)
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
	h, resources, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/file": []byte("x")})

	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "d"), "create-directory-at(d)")
	if st, err := os.Stat(filepath.Join(dir, "d")); err != nil || !st.IsDir() {
		t.Fatalf("d after create-directory-at: stat err=%v, isDir=%v; want a real directory", err, err == nil && st.IsDir())
	}

	// Creating over an existing directory, or over an existing file, is the
	// mount's own EEXIST.
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "d"),
		wasiErrorCodeExist, "create-directory-at(d) twice")
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "file"),
		wasiErrorCodeExist, "create-directory-at over a file")

	// An absolute path is rejected before the mount is ever touched.
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "/abs"),
		wasiErrorCodeNotPermitted, `create-directory-at("/abs")`)

	// Against a regular-file descriptor: not-directory.
	fileRep := openRep(t, h, resources, rootRep, "file", 0, 0)
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", fileRep, "x"),
		wasiErrorCodeNotDirectory, "create-directory-at under a file descriptor")
}

func TestWasiFS_RemoveDirectoryAt(t *testing.T) {
	h, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/d/inner": []byte("x"), "/file": []byte("f")})

	requireErrCode(t, callFS(t, h, "[method]descriptor.remove-directory-at", rootRep, "nope"),
		wasiErrorCodeNoEntry, "remove-directory-at(missing)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.remove-directory-at", rootRep, "d"),
		wasiErrorCodeNotEmpty, "remove-directory-at(non-empty)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.remove-directory-at", rootRep, "file"),
		wasiErrorCodeNotDirectory, "remove-directory-at(a file)")

	// An empty directory -- the case the old synthetic-directory model could
	// not represent at all -- removes cleanly.
	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "e"), "create-directory-at(e)")
	requireOk(t, callFS(t, h, "[method]descriptor.remove-directory-at", rootRep, "e"), "remove-directory-at(e)")
	requireAbsent(t, dir, "/e")
}

func TestWasiFS_UnlinkFileAt(t *testing.T) {
	h, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/a": []byte("A"), "/d/inner": []byte("x")})

	requireOk(t, callFS(t, h, "[method]descriptor.unlink-file-at", rootRep, "a"), "unlink-file-at(a)")
	requireAbsent(t, dir, "/a")

	requireErrCode(t, callFS(t, h, "[method]descriptor.unlink-file-at", rootRep, "a"),
		wasiErrorCodeNoEntry, "unlink-file-at(already gone)")
	// unlink(2) refuses a directory; sysfs normalizes the platform's EPERM to
	// EISDIR, which must surface as error-code::is-directory.
	requireErrCode(t, callFS(t, h, "[method]descriptor.unlink-file-at", rootRep, "d"),
		wasiErrorCodeIsDirectory, "unlink-file-at(a directory)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.unlink-file-at", rootRep, "/abs"),
		wasiErrorCodeNotPermitted, `unlink-file-at("/abs")`)
}

func TestWasiFS_RenameAt(t *testing.T) {
	h, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{
		"/a":             []byte("A"),
		"/src/f1":        []byte("1"),
		"/src/deep/f2":   []byte("2"),
		"/untouched.txt": []byte("u"),
	})

	requireErrCode(t, callFS(t, h, "[method]descriptor.rename-at", rootRep, "missing", rootRep, "x"),
		wasiErrorCodeNoEntry, "rename-at(missing source)")

	requireOk(t, callFS(t, h, "[method]descriptor.rename-at", rootRep, "a", rootRep, "b"), "rename-at(a -> b)")
	requireAbsent(t, dir, "/a")
	if got := hostRead(t, dir, "/b"); got != "A" {
		t.Fatalf("/b = %q, want \"A\"", got)
	}

	// A whole directory subtree moves in one call -- the mount's own rename,
	// no per-entry re-keying.
	requireOk(t, callFS(t, h, "[method]descriptor.rename-at", rootRep, "src", rootRep, "dst"), "rename-at(src -> dst)")
	requireAbsent(t, dir, "/src")
	if got := hostRead(t, dir, "/dst/deep/f2"); got != "2" {
		t.Fatalf("/dst/deep/f2 = %q, want \"2\"", got)
	}
	if got := hostRead(t, dir, "/untouched.txt"); got != "u" {
		t.Fatalf("/untouched.txt = %q, want \"u\" (an unrelated file was disturbed)", got)
	}

	requireErrCode(t, callFS(t, h, "[method]descriptor.rename-at", rootRep, "/abs", rootRep, "x"),
		wasiErrorCodeNotPermitted, `rename-at("/abs")`)

	// Renaming onto a non-empty directory is not-empty, not exist. POSIX
	// lets the host report that as either errno and platforms disagree
	// (illumos says EEXIST, Linux and the BSDs say ENOTEMPTY), so sysfs
	// normalizes it -- this asserts the guest sees one answer everywhere.
	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "occupied"), "create-directory-at(occupied)")
	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "occupied/child"), "create-directory-at(occupied/child)")
	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "mover"), "create-directory-at(mover)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.rename-at", rootRep, "mover", rootRep, "occupied"),
		wasiErrorCodeNotEmpty, "rename-at onto a non-empty directory")
}

func TestWasiFS_LinkAt(t *testing.T) {
	h, _, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/a": []byte("A")})

	requireOk(t, callFS(t, h, "[method]descriptor.link-at", rootRep, uint32(0), "a", rootRep, "hard"), "link-at(a -> hard)")
	if got := hostRead(t, dir, "/hard"); got != "A" {
		t.Fatalf("/hard = %q, want \"A\"", got)
	}
	// A real hard link, not a copy: both names share one inode, which is
	// exactly what metadata-hash-at reports.
	hashA := requireOk(t, callFS(t, h, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "a"), "metadata-hash-at(a)")
	hashHard := requireOk(t, callFS(t, h, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "hard"), "metadata-hash-at(hard)")
	if a, h := hashA.([]abi.Value), hashHard.([]abi.Value); a[0] != h[0] || a[1] != h[1] {
		t.Fatalf("metadata-hash-at: a = %v, hard = %v; a hard link must share the source's identity", a, h)
	}

	requireErrCode(t, callFS(t, h, "[method]descriptor.link-at", rootRep, uint32(0), "a", rootRep, "hard"),
		wasiErrorCodeExist, "link-at onto an existing name")
	requireErrCode(t, callFS(t, h, "[method]descriptor.link-at", rootRep, uint32(0), "missing", rootRep, "x"),
		wasiErrorCodeNoEntry, "link-at(missing source)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.link-at", rootRep, uint32(0), "/abs", rootRep, "x"),
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
	h, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().
		WithDirMount(dirA, "/").
		WithDirMount(dirB, "/other")})

	results, err := wasiFSFn(t, h, wasiIfacePreopens, "get-directories")(context.Background(), nil)
	if err != nil {
		t.Fatalf("get-directories: %v", err)
	}
	dirs := results[0].([]abi.Value)
	if len(dirs) != 2 {
		t.Fatalf("get-directories: got %d preopens, want 2", len(dirs))
	}
	repA := rootHandleRep(t, resources, dirs[0].([]abi.Value)[0].(uint32))
	repB := rootHandleRep(t, resources, dirs[1].([]abi.Value)[0].(uint32))

	requireErrCode(t, callFS(t, h, "[method]descriptor.rename-at", repA, "a.txt", repB, "a.txt"),
		wasiErrorCodeCrossDevice, "rename-at across mounts")
	requireErrCode(t, callFS(t, h, "[method]descriptor.link-at", repA, uint32(0), "a.txt", repB, "a.txt"),
		wasiErrorCodeCrossDevice, "link-at across mounts")
}

// TestWasiFS_ReadDirectory lists a directory through the real
// read-directory -> read-directory-entry iterator, including an empty
// directory (which the previous flat-map model could not represent).
func TestWasiFS_ReadDirectory(t *testing.T) {
	h, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{
		"/a.txt":     []byte("a"),
		"/sub/b.txt": []byte("b"),
	})
	requireOk(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "empty"), "create-directory-at(empty)")

	got := listDir(t, h, resources, rootRep)
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
	emptyRep := openRep(t, h, resources, rootRep, "empty", wasiOpenFlagDirectory, 0)
	if entries := listDir(t, h, resources, emptyRep); len(entries) != 0 {
		t.Fatalf("read-directory(/empty): got %v, want no entries", entries)
	}

	// read-directory against a regular file is not-directory.
	fileRep := openRep(t, h, resources, rootRep, "a.txt", 0, 0)
	requireErrCode(t, callFS(t, h, "[method]descriptor.read-directory", fileRep),
		wasiErrorCodeNotDirectory, "read-directory(a regular file)")
}

// openRep drives open-at and returns the resolved rep of the descriptor it
// hands back.
func openRep(t *testing.T, h *componenttest.Harness, resources *component.HandleTable, dirRep uint32, rel string, openFlags, descFlags uint32) uint32 {
	t.Helper()
	rv := callFS(t, h, "[method]descriptor.open-at", dirRep, uint32(0), rel, openFlags, descFlags)
	return rootHandleRep(t, resources, requireOk(t, rv, "open-at("+rel+")").(uint32))
}

// listDir reads a directory descriptor's whole listing through the
// read-directory -> read-directory-entry iterator, as name -> isDir.
func listDir(t *testing.T, h *componenttest.Harness, resources *component.HandleTable, dirRep uint32) map[string]bool {
	t.Helper()
	streamHandle := requireOk(t, callFS(t, h, "[method]descriptor.read-directory", dirRep), "read-directory").(uint32)
	streamRep, err := resources.Rep(wasiDirEntryStreamResType, streamHandle)
	if err != nil {
		t.Fatalf("resolve directory-entry-stream handle: %v", err)
	}
	out := map[string]bool{}
	for {
		rv := callFS(t, h, "[method]directory-entry-stream.read-directory-entry", streamRep)
		entry := requireOk(t, rv, "read-directory-entry")
		if entry == nil {
			return out // option::none: exhausted
		}
		e := entry.([]abi.Value)
		out[e[1].(string)] = e[0].(uint32) == wasiDescriptorTypeDirectory
	}
}

// wasiDirDescFlags is the descriptor-flags bitset get-flags reports for
// every directory descriptor, on any mount: readable (a guest lists, stats
// and opens through it), not writable (write means write-via-stream, which
// every directory descriptor refuses with is-directory), and mutable as a
// directory.
const wasiDirDescFlags = wasiDescFlagRead | wasiDescFlagMutateDirectory

// descFlagsOf drives [method]descriptor.get-flags against descRep and
// returns the raw descriptor-flags bitset it reports.
func descFlagsOf(t *testing.T, h *componenttest.Harness, descRep uint32, what string) uint32 {
	t.Helper()
	return requireOk(t, callFS(t, h, "[method]descriptor.get-flags", descRep), "get-flags("+what+")").(uint32)
}

// TestWasiFS_GetFlags pins the exact descriptor-flags bitset get-flags
// reports for every way a descriptor can come into existence: the access
// mode the open actually used, plus mutate-directory for a directory --
// never a guess from the path's own permissions, and never one of the three
// sync bits this package does not claim (see wasiDescFlagRead's doc).
func TestWasiFS_GetFlags(t *testing.T) {
	h, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f"), "/sub/x": []byte("x")})

	// A preopened root is readable, not writable, and mutable as a directory.
	if got := descFlagsOf(t, h, rootRep, "the preopened root"); got != wasiDirDescFlags {
		t.Errorf("get-flags(the preopened root) = %#b, want read|mutate-directory (%#b)", got, wasiDirDescFlags)
	}

	for _, tt := range []struct {
		name      string
		descFlags uint32
		want      uint32
	}{
		{"read-only", wasiDescFlagRead, wasiDescFlagRead},
		{"write-only", wasiDescFlagWrite, wasiDescFlagWrite},
		{"read-write", wasiDescFlagRead | wasiDescFlagWrite, wasiDescFlagRead | wasiDescFlagWrite},
		// Empty descriptor-flags still open O_RDONLY, so the descriptor
		// really is readable -- see openAt's switch.
		{"neither bit requested", 0, wasiDescFlagRead},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rep := openRep(t, h, resources, rootRep, "f", 0, tt.descFlags)
			got := descFlagsOf(t, h, rep, tt.name)
			if got != tt.want {
				t.Fatalf("get-flags(a %s file descriptor) = %#b, want %#b", tt.name, got, tt.want)
			}
			// mutate-directory is directory-scoped by definition: a
			// regular-file descriptor must never carry it.
			if got&wasiDescFlagMutateDirectory != 0 {
				t.Fatalf("get-flags(a %s file descriptor) = %#b, want no mutate-directory bit", tt.name, got)
			}
		})
	}

	// A subdirectory descriptor reports read|mutate-directory and never
	// write, however it was asked for: the open is forced read-only either
	// way, and what a guest may do *inside* it is mutate-directory's job.
	for _, tt := range []struct {
		name      string
		descFlags uint32
	}{
		{"opened for reading", wasiDescFlagRead},
		{"asking for write anyway", wasiDescFlagRead | wasiDescFlagWrite},
	} {
		t.Run("directory "+tt.name, func(t *testing.T) {
			rep := openRep(t, h, resources, rootRep, "sub", wasiOpenFlagDirectory, tt.descFlags)
			if got := descFlagsOf(t, h, rep, "sub"); got != wasiDirDescFlags {
				t.Fatalf("get-flags(a directory descriptor %s) = %#b, want read|mutate-directory (%#b)", tt.name, got, wasiDirDescFlags)
			}
		})
	}

	// get-flags never touches the mount: it answers for a descriptor whose
	// path has since been removed exactly as it did before, which is what
	// makes it a report about the descriptor and not about the file.
	c2, resources2, rootRep2, dir2 := fsOpsFixture(t, map[string][]byte{"/gone": []byte("g")})
	goneRep := openRep(t, c2, resources2, rootRep2, "gone", 0, wasiDescFlagRead|wasiDescFlagWrite)
	if err := os.Remove(filepath.Join(dir2, "gone")); err != nil {
		t.Fatalf("removing gone: %v", err)
	}
	if got := descFlagsOf(t, c2, goneRep, "a vanished path"); got != wasiDescFlagRead|wasiDescFlagWrite {
		t.Fatalf("get-flags(a vanished path) = %#b, want read|write (%#b)", got, wasiDescFlagRead|wasiDescFlagWrite)
	}
}

// TestWasiFS_Sync drives [method]descriptor.sync and its sibling
// [method]descriptor.sync-data -- which share one implementation, so both
// run the whole table -- across every descriptor shape fsSyncFunc
// distinguishes: a directory (a real fsync of the directory), a file opened
// for writing (a real fsync of the file, in the mode it was opened with),
// and a file not opened for writing (Ok with no effect, per types.wit).
func TestWasiFS_Sync(t *testing.T) {
	for _, method := range []string{"[method]descriptor.sync", "[method]descriptor.sync-data"} {
		t.Run(method, func(t *testing.T) {
			h, resources, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/f": []byte("payload"), "/d/x": []byte("x")})

			rwRep := openRep(t, h, resources, rootRep, "f", 0, wasiDescFlagRead|wasiDescFlagWrite)
			woRep := openRep(t, h, resources, rootRep, "f", 0, wasiDescFlagWrite)
			roRep := openRep(t, h, resources, rootRep, "f", 0, wasiDescFlagRead)
			dirRep := openRep(t, h, resources, rootRep, "d", wasiOpenFlagDirectory, wasiDescFlagRead)

			for _, tt := range []struct {
				name string
				rep  uint32
			}{
				{"a read-write file descriptor", rwRep},
				{"a write-only file descriptor", woRep},
				{"a read-only file descriptor", roRep},
				{"a directory descriptor", dirRep},
				{"the preopened root", rootRep},
			} {
				requireOk(t, callFS(t, h, method, tt.rep), method+"("+tt.name+")")
			}

			// Bytes written through the descriptor are still there after the
			// sync: syncing through a freshly opened handle is a flush, never
			// a truncate or a reopen that could lose them (see fsSyncFunc).
			writeVia(t, h, resources, rwRep, "synced!")
			requireOk(t, callFS(t, h, method, rwRep), method+"(after a write)")
			if got := hostRead(t, dir, "/f"); got != "synced!" {
				t.Fatalf("/f after %s = %q, want %q", method, got, "synced!")
			}

			// The mount's own failure surfaces: with the file gone, the open
			// this func has to do reports no-entry rather than claiming a
			// successful sync of nothing.
			if err := os.Remove(filepath.Join(dir, "f")); err != nil {
				t.Fatalf("removing f: %v", err)
			}
			requireErrCode(t, callFS(t, h, method, rwRep), wasiErrorCodeNoEntry, method+"(a vanished file)")
			// A descriptor not opened for writing never touches the mount, so
			// it cannot report the mount's failure either -- it still answers
			// Ok, which is the same "no effect" it always answered.
			requireOk(t, callFS(t, h, method, roRep), method+"(a vanished file, read-only descriptor)")

			if err := os.RemoveAll(filepath.Join(dir, "d")); err != nil {
				t.Fatalf("removing d: %v", err)
			}
			requireErrCode(t, callFS(t, h, method, dirRep), wasiErrorCodeNoEntry, method+"(a vanished directory)")
		})
	}
}

// TestWasiFS_Sync_ReadOnlyMount covers the two answers a mount with no write
// surface gives sync. A file descriptor there can never have been opened for
// writing (the mount refuses a write-mode open outright), so sync is the
// spec's "succeeds with no effect". A directory descriptor does genuinely
// ask the mount to sync, and wazy's read-only layer answers EBADF for that
// -- the same thing wazy's own preview1 fd_sync reports for the same mount,
// so the two runtimes cannot disagree about it.
//
// It also pins what get-flags says about that mount, which is the opposite
// posture on purpose: a descriptor-flags bit is advisory, so the directories
// still advertise mutate-directory and let the mutation itself carry the
// refusal.
func TestWasiFS_Sync_ReadOnlyMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("f"), 0o644); err != nil {
		t.Fatalf("seeding the mount: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("seeding the mount: %v", err)
	}
	h, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().WithReadOnlyDirMount(dir, "/")})
	rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, h))
	fileRep := openRep(t, h, resources, rootRep, "f", 0, wasiDescFlagRead)

	for _, method := range []string{"[method]descriptor.sync", "[method]descriptor.sync-data"} {
		requireOk(t, callFS(t, h, method, fileRep), method+"(a read-only mount's file)")
		requireErrCode(t, callFS(t, h, method, rootRep), wasiErrorCodeBadDescriptor,
			method+"(a read-only mount's directory)")
	}

	// get-flags agrees no file on such a mount is ever writable...
	if got := descFlagsOf(t, h, fileRep, "a read-only mount's file"); got != wasiDescFlagRead {
		t.Fatalf("get-flags(a read-only mount's file) = %#b, want read (%#b)", got, wasiDescFlagRead)
	}
	// ...but its directory descriptor still advertises mutate-directory. A
	// capability flag is advisory: the guest is told it may try, and the
	// mutation itself reports the mount's real refusal (below), exactly as
	// wazy's preview1 hands out dirRightsBase for this same mount. Hiding
	// the flag would make a guest that gates on it skip the call entirely
	// and see a read-only filesystem with no errno to explain it.
	if got := descFlagsOf(t, h, rootRep, "a read-only mount's root"); got != wasiDirDescFlags {
		t.Fatalf("get-flags(a read-only mount's root) = %#b, want read|mutate-directory (%#b)", got, wasiDirDescFlags)
	}
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "d"),
		wasiErrorCodeReadOnly, "create-directory-at on a read-only mount")

	subRep := openRep(t, h, resources, rootRep, "sub", wasiOpenFlagDirectory, wasiDescFlagRead)
	if got := descFlagsOf(t, h, subRep, "a read-only mount's subdirectory"); got != wasiDirDescFlags {
		t.Fatalf("get-flags(a read-only mount's subdirectory) = %#b, want read|mutate-directory (%#b)", got, wasiDirDescFlags)
	}
}

// writeVia writes s through fileRep's own write-via-stream, the way a guest
// does, so a test can assert on what a following sync did not disturb.
func writeVia(t *testing.T, h *componenttest.Harness, resources *component.HandleTable, fileRep uint32, s string) {
	t.Helper()
	streamHandle := requireOk(t, callFS(t, h, "[method]descriptor.write-via-stream", fileRep, uint64(0)), "write-via-stream").(uint32)
	streamRep, err := resources.Rep(wasiOutputStreamResType, streamHandle)
	if err != nil {
		t.Fatalf("resolve output-stream handle: %v", err)
	}
	results, err := wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.write")(
		context.Background(), []abi.Value{streamRep, wasiListFromBytes([]byte(s))})
	if err != nil {
		t.Fatalf("output-stream.write: %v", err)
	}
	if rv := results[0].(abi.ResultValue); rv.IsErr {
		t.Fatalf("output-stream.write: unexpected Err: %#v", rv.Payload)
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
	h, _ := wasiFSConfig(t, WASIConfig{})

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

		{"get-flags count", "[method]descriptor.get-flags", []abi.Value{uint32(1), uint32(2)}, "expected 1 arg"},
		{"get-flags self", "[method]descriptor.get-flags", []abi.Value{"bad"}, "self: expected uint32"},

		{"sync count", "[method]descriptor.sync", nil, "expected 1 arg"},
		{"sync self", "[method]descriptor.sync", []abi.Value{"bad"}, "self: expected uint32"},
		{"sync-data count", "[method]descriptor.sync-data", []abi.Value{uint32(1), uint32(2)}, "expected 1 arg"},
		{"sync-data self", "[method]descriptor.sync-data", []abi.Value{"bad"}, "self: expected uint32"},

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
			_, err := wasiFSFn(t, h, wasiIfaceFilesystemTypes, tt.funcN)(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestWasiFS_UnknownReps proves every method that resolves a descriptor or a
// directory-entry-stream rep fails loud on one that names nothing live,
// rather than dereferencing a missing map entry.
func TestWasiFS_UnknownReps(t *testing.T) {
	h, _, rootRep, _ := fsOpsFixture(t, nil)
	const dead = uint32(99999)

	for _, tt := range []struct {
		name    string
		funcN   string
		args    []abi.Value
		wantErr string
	}{
		{"open-at", "[method]descriptor.open-at", []abi.Value{dead, uint32(0), "p", uint32(0), uint32(0)}, "does not name a live descriptor"},
		{"stat", "[method]descriptor.stat", []abi.Value{dead}, "does not name a live descriptor"},
		{"get-flags", "[method]descriptor.get-flags", []abi.Value{dead}, "does not name a live descriptor"},
		{"sync", "[method]descriptor.sync", []abi.Value{dead}, "does not name a live descriptor"},
		{"sync-data", "[method]descriptor.sync-data", []abi.Value{dead}, "does not name a live descriptor"},
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
			_, err := wasiFSFn(t, h, wasiIfaceFilesystemTypes, tt.funcN)(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

// TestWasiFS_NotDirectorySelf proves every *-at method rejects a
// regular-file descriptor as its directory argument (either argument, for
// the two that take two).
func TestWasiFS_NotDirectorySelf(t *testing.T) {
	h, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	fileRep := openRep(t, h, resources, rootRep, "f", 0, 0)

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
			requireErrCode(t, callFS(t, h, tt.funcN, tt.args...), wasiErrorCodeNotDirectory, tt.name)
		})
	}
}

// TestWasiFS_AbsolutePathRejected covers the wasiJoinFSPath guard on the
// methods wasi_fs_test.go's open-at case does not reach.
func TestWasiFS_AbsolutePathRejected(t *testing.T) {
	h, _, rootRep, _ := fsOpsFixture(t, nil)
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
			requireErrCode(t, callFS(t, h, tt.funcN, tt.args...), wasiErrorCodeNotPermitted, tt.name)
		})
	}
}

// TestWasiFS_StatAt covers stat-at's success and no-entry paths, including
// the real timestamps a mount now supplies where the old flat map could only
// answer `none`.
func TestWasiFS_StatAt(t *testing.T) {
	h, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("12345")})

	rec := requireOk(t, callFS(t, h, "[method]descriptor.stat-at", rootRep, uint32(0), "f"), "stat-at(f)").([]abi.Value)
	if rec[0].(uint32) != wasiDescriptorTypeRegularFile {
		t.Errorf("stat-at(f): type = %v, want regular-file", rec[0])
	}
	if rec[2].(uint64) != 5 {
		t.Errorf("stat-at(f): size = %v, want 5", rec[2])
	}
	if rec[4] == nil {
		t.Error("stat-at(f): data-modification-timestamp is none; a real mount has one")
	}

	dirRec := requireOk(t, callFS(t, h, "[method]descriptor.stat-at", rootRep, uint32(0), "."), "stat-at(.)").([]abi.Value)
	if dirRec[0].(uint32) != wasiDescriptorTypeDirectory {
		t.Errorf("stat-at(.): type = %v, want directory", dirRec[0])
	}

	requireErrCode(t, callFS(t, h, "[method]descriptor.stat-at", rootRep, uint32(0), "missing"),
		wasiErrorCodeNoEntry, "stat-at(missing)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.metadata-hash-at", rootRep, uint32(0), "missing"),
		wasiErrorCodeNoEntry, "metadata-hash-at(missing)")
}

// TestWasiFS_OpenAt_Exclusive proves the exclusive open-flag reaches the
// mount as O_EXCL: creating an existing path fails with exist rather than
// silently reopening it.
func TestWasiFS_OpenAt_Exclusive(t *testing.T) {
	h, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	requireErrCode(t,
		callFS(t, h, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagCreate|wasiOpenFlagExclusive, wasiDescFlagWrite),
		wasiErrorCodeExist, "open-at(create|exclusive) over an existing file")

	// Without exclusive, the same open succeeds.
	requireOk(t, callFS(t, h, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagCreate, wasiDescFlagWrite),
		"open-at(create) over an existing file")
}

// TestWasiFS_OpenAt_DirectoryFlagOnFile proves open-flags::directory against
// a regular file is not-directory. Some platforms' open(2) rejects it with
// ENOTDIR outright; where it does not, the IsDir check after the open does.
func TestWasiFS_OpenAt_DirectoryFlagOnFile(t *testing.T) {
	h, _, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("f")})
	requireErrCode(t,
		callFS(t, h, "[method]descriptor.open-at", rootRep, uint32(0), "f", wasiOpenFlagDirectory, uint32(0)),
		wasiErrorCodeNotDirectory, "open-at(directory) on a regular file")
}

// TestWasiFS_VanishedPaths proves the methods that touch the mount after a
// descriptor already exists surface the mount's errno instead of assuming
// the path is still there -- the failure mode a descriptor holding a cached
// snapshot could never report at all.
func TestWasiFS_VanishedPaths(t *testing.T) {
	h, resources, rootRep, dir := fsOpsFixture(t, map[string][]byte{"/f": []byte("payload"), "/d/x": []byte("x")})
	fileRep := openRep(t, h, resources, rootRep, "f", 0, wasiDescFlagWrite)
	dirRep := openRep(t, h, resources, rootRep, "d", wasiOpenFlagDirectory, 0)

	// A write stream outlives its file: the next write reports the failure.
	wStream := requireOk(t, callFS(t, h, "[method]descriptor.write-via-stream", fileRep, uint64(0)), "write-via-stream").(uint32)
	wRep, err := resources.Rep(wasiOutputStreamResType, wStream)
	if err != nil {
		t.Fatalf("resolve output-stream handle: %v", err)
	}
	// A read stream, likewise.
	rStream := requireOk(t, callFS(t, h, "[method]descriptor.read-via-stream", fileRep, uint64(0)), "read-via-stream").(uint32)
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

	_, err = wasiFSFn(t, h, wasiIfaceStreams, "[method]output-stream.write")(
		context.Background(), []abi.Value{wRep, wasiListFromBytes([]byte("x"))})
	requireErrContains(t, err, "opening")

	_, err = wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")(
		context.Background(), []abi.Value{rRep, uint64(4)})
	requireErrContains(t, err, "opening")

	requireErrCode(t, callFS(t, h, "[method]descriptor.stat", fileRep), wasiErrorCodeNoEntry, "stat(vanished)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.get-type", fileRep), wasiErrorCodeNoEntry, "get-type(vanished)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.metadata-hash", fileRep), wasiErrorCodeNoEntry, "metadata-hash(vanished)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.append-via-stream", fileRep), wasiErrorCodeNoEntry, "append-via-stream(vanished)")
	requireErrCode(t, callFS(t, h, "[method]descriptor.read-directory", dirRep), wasiErrorCodeNoEntry, "read-directory(vanished)")
}

// TestWasiFS_ReadStream_CapsLength proves a guest-chosen read length is
// capped (see wasiMaxStreamRead): a u64 the guest picked must never become
// the size of a host allocation. A short read is a legal answer, so the
// call still succeeds.
func TestWasiFS_ReadStream_CapsLength(t *testing.T) {
	h, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/f": []byte("payload")})
	fileRep := openRep(t, h, resources, rootRep, "f", 0, 0)
	stream := requireOk(t, callFS(t, h, "[method]descriptor.read-via-stream", fileRep, uint64(0)), "read-via-stream").(uint32)
	streamRep, err := resources.Rep(wasiInputStreamResType, stream)
	if err != nil {
		t.Fatalf("resolve input-stream handle: %v", err)
	}

	results, err := wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")(
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
	h, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().WithFSMount(fstest.MapFS{
		"greeting.txt": {Data: []byte("from an io/fs.FS")},
		"sub/deep.txt": {Data: []byte("nested")},
	}, "/")})
	rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, h))

	if got := readFileVia(t, h, resources, rootRep, "greeting.txt"); got != "from an io/fs.FS" {
		t.Fatalf("greeting.txt = %q, want %q", got, "from an io/fs.FS")
	}

	entries := listDir(t, h, resources, rootRep)
	if isDir, ok := entries["sub"]; !ok || !isDir {
		t.Fatalf("read-directory(/): got %v, want a directory entry \"sub\"", entries)
	}
	if isDir, ok := entries["greeting.txt"]; !ok || isDir {
		t.Fatalf("read-directory(/): got %v, want a file entry \"greeting.txt\"", entries)
	}

	subRep := openRep(t, h, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)
	if got := readFileVia(t, h, resources, subRep, "deep.txt"); got != "nested" {
		t.Fatalf("sub/deep.txt = %q, want %q", got, "nested")
	}

	// io/fs.FS has no write surface at all, so every mutation is refused
	// rather than silently dropped. A create-open reports no-entry (io/fs.FS
	// can only ever open what already exists, so the path stays missing);
	// the mutating methods report unsupported outright.
	requireErrCode(t, callFS(t, h, "[method]descriptor.open-at", rootRep, uint32(0), "new.txt", wasiOpenFlagCreate, wasiDescFlagWrite),
		wasiErrorCodeNoEntry, "open-at(create) on an io/fs.FS mount")
	requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", rootRep, "newdir"),
		wasiErrorCodeUnsupported, "create-directory-at on an io/fs.FS mount")
	requireErrCode(t, callFS(t, h, "[method]descriptor.unlink-file-at", rootRep, "greeting.txt"),
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

	h, resources := wasiFSConfig(t, WASIConfig{FS: wazy.NewFSConfig().WithDirMount(mount, "/")})
	rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, h))
	subRep := openRep(t, h, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)

	// Sanity: the descriptors do work for paths that stay inside.
	if got := readFileVia(t, h, resources, subRep, "ok.txt"); got != "in-mount" {
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
			requireErrCode(t, callFS(t, h, "[method]descriptor.open-at", tt.dirRep, uint32(0), tt.relPath, uint32(0), uint32(0)),
				wasiErrorCodeNotPermitted, "open-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.open-at", tt.dirRep, uint32(0), tt.relPath, wasiOpenFlagCreate, wasiDescFlagWrite),
				wasiErrorCodeNotPermitted, "open-at(create, "+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.stat-at", tt.dirRep, uint32(0), tt.relPath),
				wasiErrorCodeNotPermitted, "stat-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.metadata-hash-at", tt.dirRep, uint32(0), tt.relPath),
				wasiErrorCodeNotPermitted, "metadata-hash-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.unlink-file-at", tt.dirRep, tt.relPath),
				wasiErrorCodeNotPermitted, "unlink-file-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.create-directory-at", tt.dirRep, tt.relPath),
				wasiErrorCodeNotPermitted, "create-directory-at("+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.rename-at", tt.dirRep, tt.relPath, rootRep, "landing"),
				wasiErrorCodeNotPermitted, "rename-at(from "+tt.relPath+")")
			requireErrCode(t, callFS(t, h, "[method]descriptor.link-at", tt.dirRep, uint32(0), tt.relPath, rootRep, "landing"),
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
	h, resources, rootRep, _ := fsOpsFixture(t, map[string][]byte{"/sub/deep/x.txt": []byte("reached")})

	if got := readFileVia(t, h, resources, rootRep, "sub/deep/../deep/x.txt"); got != "reached" {
		t.Fatalf("sub/deep/../deep/x.txt = %q, want %q", got, "reached")
	}
	subRep := openRep(t, h, resources, rootRep, "sub", wasiOpenFlagDirectory, 0)
	if got := readFileVia(t, h, resources, subRep, "./deep/x.txt"); got != "reached" {
		t.Fatalf("./deep/x.txt under sub = %q, want %q", got, "reached")
	}
}
