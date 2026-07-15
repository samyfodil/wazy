package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// wasiFSConfig builds a WithWASI config the same way wasiHostFunc does, but
// returns the whole *config plus the *handleTable runResourceHooks handed
// to it, rather than a single extracted HostFunc, so a test can chain
// calls across multiple funcs that share the same underlying wasiFS state
// (e.g. get-directories then open-at against the descriptor it returned)
// -- something wasiHostFunc's one-shot extraction can't do, since each
// call to it builds an entirely independent config/wasiFS pair -- and
// resolve a borrow<T>/own<T> handle to its rep itself (rootHandleRep),
// mirroring what liftHostArgs (host_import.go) does automatically for a
// real guest call, since these tests invoke the extracted HostFunc
// directly, bypassing that generic lift step.
func wasiFSConfig(t *testing.T, cfg WASIConfig) (*config, *handleTable) {
	t.Helper()
	c := newConfig(WithWASI(cfg))
	resources := newHandleTable()
	runResourceHooks(c, resources)
	return c, resources
}

func wasiFSFn(t *testing.T, c *config, iface, name string) HostFunc {
	t.Helper()
	hi, ok := c.imports[importKey{iface: iface, name: name}]
	if !ok {
		t.Fatalf("WithWASI did not register %q %q", iface, name)
	}
	return hi.fn
}

// rootDescriptorHandle drives get-directories and returns the own<
// descriptor> handle for the one preopened root directory it names.
func rootDescriptorHandle(t *testing.T, c *config) uint32 {
	t.Helper()
	getDirectories := wasiFSFn(t, c, wasiIfacePreopens, "get-directories")
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
		{"/", "greeting.txt", "/greeting.txt", true},
		{"/", "sub/greeting.txt", "/sub/greeting.txt", true},
		{"/sub", "greeting.txt", "/sub/greeting.txt", true},
		{"/", "/greeting.txt", "", false}, // absolute rel: rejected
	}
	for _, tt := range tests {
		got, ok := wasiJoinFSPath(tt.dir, tt.rel)
		if ok != tt.wantOK || (ok && got != tt.want) {
			t.Errorf("wasiJoinFSPath(%q, %q) = (%q, %v), want (%q, %v)", tt.dir, tt.rel, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestWasiFS_GetDirectories_RootIsDirectory(t *testing.T) {
	c, resources := wasiFSConfig(t, WASIConfig{})
	rootHandle := rootDescriptorHandle(t, c)

	getType := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
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
// host_import.go) before a [method]descriptor.* HostFunc ever sees it --
// these unit tests call the extracted HostFunc directly, bypassing that
// generic lift step, so they must do the resolution themselves.
func rootHandleRep(t *testing.T, resources *handleTable, handle uint32) uint32 {
	t.Helper()
	rep, err := resources.Rep(wasiDescriptorResType, handle)
	if err != nil {
		t.Fatalf("resolve descriptor handle %d: %v", handle, err)
	}
	return rep
}

func TestWasiFS_OpenAt_FullChain(t *testing.T) {
	const content = "chained open-at contents"
	c, resources := wasiFSConfig(t, WASIConfig{FS: map[string][]byte{"/greeting.txt": []byte(content)}})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
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

	getType := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	gtResults, err := getType(context.Background(), []abi.Value{fileRep})
	if err != nil {
		t.Fatalf("get-type: %v", err)
	}
	gtrv := gtResults[0].(abi.ResultValue)
	if gtrv.IsErr || gtrv.Payload.(uint32) != wasiDescriptorTypeRegularFile {
		t.Fatalf("get-type: got %#v, want Ok(regular-file)", gtrv)
	}

	stat := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.stat")
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

	readViaStream := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream")
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

	read := wasiFSFn(t, c, wasiIfaceStreams, "[method]input-stream.read")
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
	c, resources := wasiFSConfig(t, WASIConfig{})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
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
	c, resources := wasiFSConfig(t, WASIConfig{FS: map[string][]byte{"/x": []byte("x")}})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
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

func TestWasiFS_OpenAt_CreateUnsupported(t *testing.T) {
	c, resources := wasiFSConfig(t, WASIConfig{})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
	results, err := openAt(context.Background(), []abi.Value{
		rootRep, uint32(0), "new.txt", wasiOpenFlagCreate, uint32(0),
	})
	if err != nil {
		t.Fatalf("open-at: %v", err)
	}
	rv := results[0].(abi.ResultValue)
	if !rv.IsErr || rv.Payload.(uint32) != wasiErrorCodeReadOnly {
		t.Fatalf("open-at(create): got %#v, want Err(read-only)", rv)
	}
}

func TestWasiFS_OpenAt_OnNonDirectory(t *testing.T) {
	c, resources := wasiFSConfig(t, WASIConfig{FS: map[string][]byte{"/f": []byte("f")}})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	openAt := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.open-at")
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
	readViaStream := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream")
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
	c, _ := wasiFSConfig(t, WASIConfig{})
	getType := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.get-type")
	_, err := getType(context.Background(), []abi.Value{uint32(99999)})
	requireErrContains(t, err, "does not name a live descriptor")
}

func TestWasiFS_UnknownStreamRep(t *testing.T) {
	c, _ := wasiFSConfig(t, WASIConfig{})
	read := wasiFSFn(t, c, wasiIfaceStreams, "[method]input-stream.read")
	_, err := read(context.Background(), []abi.Value{uint32(99999), uint64(1)})
	requireErrContains(t, err, "does not name a live stream")
}

func TestWasiFS_FilesystemErrorCode_AlwaysNone(t *testing.T) {
	c, _ := wasiFSConfig(t, WASIConfig{})
	fn := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "filesystem-error-code")
	results, err := fn(context.Background(), []abi.Value{uint32(1)})
	if err != nil {
		t.Fatalf("filesystem-error-code: %v", err)
	}
	if results[0] != nil {
		t.Fatalf("filesystem-error-code: got %#v, want none", results[0])
	}
}

func TestWasiFS_MetadataHash(t *testing.T) {
	c, resources := wasiFSConfig(t, WASIConfig{})
	rootHandle := rootDescriptorHandle(t, c)
	rootRep := rootHandleRep(t, resources, rootHandle)

	fn := wasiFSFn(t, c, wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash")
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
	c, _ := wasiFSConfig(t, WASIConfig{})
	for _, tc := range []struct{ iface, name string }{
		{wasiIfaceTerminalStdin, "get-terminal-stdin"},
		{wasiIfaceTerminalStdout, "get-terminal-stdout"},
		{wasiIfaceTerminalStderr, "get-terminal-stderr"},
	} {
		fn := wasiFSFn(t, c, tc.iface, tc.name)
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
	c, _ := wasiFSConfig(t, WASIConfig{FS: map[string][]byte{"/x": []byte("x")}})

	tests := []struct {
		name    string
		iface   string
		funcN   string
		args    []abi.Value
		wantErr string
	}{
		{"open-at wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1)}, "expected 5 args"},
		{"open-at bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{"not-a-rep", uint32(0), "p", uint32(0), uint32(0)}, "self: expected uint32"},
		{"open-at bad path type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1), uint32(0), uint32(0), uint32(0), uint32(0)}, "path: expected string"},
		{"open-at bad open-flags type", wasiIfaceFilesystemTypes, "[method]descriptor.open-at",
			[]abi.Value{uint32(1), uint32(0), "p", "bad", uint32(0)}, "open-flags: expected uint32"},
		{"get-type wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.get-type",
			nil, "expected 1 arg"},
		{"get-type bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.get-type",
			[]abi.Value{"bad"}, "self: expected uint32"},
		{"stat wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.stat",
			nil, "expected 1 arg"},
		{"stat bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.stat",
			[]abi.Value{"bad"}, "self: expected uint32"},
		{"read-via-stream wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{uint32(1)}, "expected 2 args"},
		{"read-via-stream bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{"bad", uint64(0)}, "self: expected uint32"},
		{"read-via-stream bad offset type", wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream",
			[]abi.Value{uint32(1), "bad"}, "offset: expected uint64"},
		{"input-stream.read wrong arg count", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{uint32(1)}, "expected 2 args"},
		{"input-stream.read bad self type", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{"bad", uint64(0)}, "self: expected uint32"},
		{"input-stream.read bad len type", wasiIfaceStreams, "[method]input-stream.read",
			[]abi.Value{uint32(1), "bad"}, "len: expected uint64"},
		{"metadata-hash wrong arg count", wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash",
			nil, "expected 1 arg"},
		{"metadata-hash bad self type", wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash",
			[]abi.Value{"bad"}, "self: expected uint32"},
		{"filesystem-error-code wrong arg count", wasiIfaceFilesystemTypes, "filesystem-error-code",
			nil, "expected 1 arg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fn := wasiFSFn(t, c, tt.iface, tt.funcN)
			_, err := fn(context.Background(), tt.args)
			requireErrContains(t, err, tt.wantErr)
		})
	}
}

func TestWasiFS_GetResources_NotInitialized(t *testing.T) {
	// Build the config WITHOUT running resource hooks -- get-directories
	// (and any func minting an own<T>) must fail loud rather than
	// dereference a nil resources table.
	c := newConfig(WithWASI(WASIConfig{}))
	getDirectories := wasiFSFn(t, c, wasiIfacePreopens, "get-directories")
	_, err := getDirectories(context.Background(), nil)
	requireErrContains(t, err, "resources handle table not yet initialized")
}
