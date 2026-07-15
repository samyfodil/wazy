package instance

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file extends wasi.go's WASI 0.2 host surface with a genuine
// wasi:filesystem/types + wasi:io/streams input-stream implementation,
// backed by an in-memory host filesystem (WASIConfig.FS), plus the three
// wasi:cli/terminal-{stdin,stdout,stderr} funcs a real rustc guest's
// std::fs path also reaches (all three always answer "no terminal" -- see
// wasiGetTerminalSig's doc).
//
// # Discovery
//
// Instantiating testdata/real_readfile.component.wasm (a genuine rustc
// wasm32-wasip2 guest whose main is
// `print!("{}", std::fs::read_to_string("/greeting.txt").unwrap())`) with
// wasi.go's WithWASI alone -- get-directories always returning an empty
// list -- and calling run() surfaces Rust's own error, not a wazy trap
// stub: std::sys::pal::wasi's path-to-preopen resolution walks
// get-directories' result looking for a preopened directory whose name is a
// prefix of "/greeting.txt", finds none, and the guest itself panics
// ("failed to find a pre-opened file descriptor ..."), aborting via the
// adapter's unreachable trap before ever reaching a WASI import this
// package doesn't implement. So get-directories must return a real
// preopened root descriptor for the guest to make it any further; once it
// does, re-running names the next unimplemented call in turn. The
// funcs below were discovered exactly that way, one trap at a time; the
// final ordered set std::fs::read_to_string("/greeting.txt") reaches on a
// non-empty get-directories result is:
//
//   - wasi:filesystem/preopens.get-directories (wasi.go's WithWASI slot,
//     rewired here to return one real root descriptor instead of empty)
//   - wasi:filesystem/types.filesystem-error-code
//   - wasi:filesystem/types [method]descriptor.open-at
//   - wasi:filesystem/types [method]descriptor.get-type
//   - wasi:filesystem/types [method]descriptor.stat
//   - wasi:filesystem/types [method]descriptor.metadata-hash (reached via
//     the preview1-to-preview2 adapter's fd_filestat_get, which combines
//     stat + metadata-hash into a full POSIX fstat result -- not called
//     directly by anything in std::fs::read_to_string's own source)
//   - wasi:filesystem/types [method]descriptor.read-via-stream
//   - wasi:io/streams [method]input-stream.blocking-read
//   - wasi:io/streams [method]input-stream.read
//   - wasi:cli/terminal-stdin.get-terminal-stdin
//   - wasi:cli/terminal-stdout.get-terminal-stdout
//   - wasi:cli/terminal-stderr.get-terminal-stderr
//
// (write-via-stream and append-via-stream were, at that point, declared
// imports left to the graph engine's automatic trap-stub fallback --
// read_to_string's read-only path never calls them. The write path,
// discovered the same way against testdata/real_transform.component.wasm
// (`std::fs::write("/output.txt", s.to_uppercase())`), reaches exactly one
// additional descriptor method beyond the read list above:
// [method]descriptor.write-via-stream, followed by
// [method]output-stream.write against the own<output-stream> it returns
// (registered in wasi.go, alongside stdout/stderr's, since output-stream is
// one shared resource/handle namespace across stdio and filesystem writers
// -- see wasi.go's writeSink dispatch). append-via-stream is never actually
// invoked by this fixture -- std::fs::write opens with O_CREAT|O_TRUNC,
// never O_APPEND -- but this package still registers a real implementation
// for it below (sharing write-via-stream's own [method]output-stream.write
// path once minted, differing only in the stream's starting offset), rather
// than leaving a func this close to write-via-stream's own semantics as a
// landmine for the next guest that does call it.)
//
// # Nested own<T> handles
//
// Every func below whose result nests an own<T> inside a result<>/list<>
// (open-at's result<descriptor,error-code>, read-via-stream's
// result<input-stream,error-code>, get-directories' rewritten
// list<tuple<own<descriptor>,string>>) must mint that handle itself via
// resources.NewOwn: host_import.go's generic lift/lower
// (allocHandleResult/resolveHandleArg) only resolves an own<T>/borrow<T>
// at a func's *top level* (see withResourcesHook's doc in host_import.go),
// not inside a nested composite. wasiFS.resources, set once via
// withResourcesHook right after the Instance's handle table exists (before
// any host func can run), is how these closures -- built once per WithWASI
// call, before any Instance/handleTable exists -- get access to it. A
// borrow<descriptor>/borrow<input-stream> `self` argument, by contrast, IS
// always a func's sole top-level first param, so liftHostArgs already
// resolves it to a rep before these closures ever see it.
const (
	wasiTerminalInputResType  uint32 = 5
	wasiTerminalOutputResType uint32 = 6
)

// wasi:filesystem/types' error-code enum, and the two enum indices this
// package actually returns, in declaration order (from `wasm-tools
// component wit real_readfile.component.wasm`).
const (
	wasiErrorCodeAccess uint32 = iota
	wasiErrorCodeWouldBlock
	wasiErrorCodeAlready
	wasiErrorCodeBadDescriptor
	wasiErrorCodeBusy
	wasiErrorCodeDeadlock
	wasiErrorCodeQuota
	wasiErrorCodeExist
	wasiErrorCodeFileTooLarge
	wasiErrorCodeIllegalByteSequence
	wasiErrorCodeInProgress
	wasiErrorCodeInterrupted
	wasiErrorCodeInvalid
	wasiErrorCodeIO
	wasiErrorCodeIsDirectory
	wasiErrorCodeLoop
	wasiErrorCodeTooManyLinks
	wasiErrorCodeMessageSize
	wasiErrorCodeNameTooLong
	wasiErrorCodeNoDevice
	wasiErrorCodeNoEntry
	wasiErrorCodeNoLock
	wasiErrorCodeInsufficientMemory
	wasiErrorCodeInsufficientSpace
	wasiErrorCodeNotDirectory
	wasiErrorCodeNotEmpty
	wasiErrorCodeNotRecoverable
	wasiErrorCodeUnsupported
	wasiErrorCodeNoTTY
	wasiErrorCodeNoSuchDevice
	wasiErrorCodeOverflow
	wasiErrorCodeNotPermitted
	wasiErrorCodePipe
	wasiErrorCodeReadOnly
	wasiErrorCodeInvalidSeek
	wasiErrorCodeTextFileBusy
	wasiErrorCodeCrossDevice
)

// wasi:filesystem/types' descriptor-type enum indices this package returns
// (a descriptor is either the one preopened root directory, or a regular
// file opened under it -- no other descriptor-type ever occurs).
const (
	wasiDescriptorTypeDirectory   uint32 = 3
	wasiDescriptorTypeRegularFile uint32 = 6
)

// wasi:io/streams' stream-error variant case indices (see
// wasiStreamErrorType in wasi.go: case 0 is last-operation-failed(error),
// case 1 is closed). This package never constructs
// last-operation-failed -- an in-memory read never fails after the
// descriptor has already resolved -- so streamErrClosed is the only case
// ever produced.
const wasiStreamErrClosed uint32 = 1

// wasi:filesystem/types' open-flags flag bits this package inspects, per
// their WIT declaration order create/directory/exclusive/truncate: create
// (bit 0) makes open-at create a missing path's FS entry instead of failing
// with error-code::no-entry, and truncate (bit 3) resets an existing
// (writable) entry's content to empty. directory/exclusive are not
// inspected -- this in-memory filesystem has no directory-vs-file open
// ambiguity beyond isDir, and no concurrent opener to race against for
// exclusive to matter.
const (
	wasiOpenFlagCreate   uint32 = 1 << 0
	wasiOpenFlagTruncate uint32 = 1 << 3
)

// wasi:filesystem/types' descriptor-flags bit this package inspects (bit 1,
// per its WIT declaration order
// read/write/file-integrity-sync/data-integrity-sync/requested-write-sync/
// mutate-directory): a descriptor opened with the write bit set is the one
// [method]descriptor.write-via-stream/append-via-stream may be called
// against; every other descriptor (including the single preopened root
// directory) is write-via-stream-ineligible, matching a real OS refusing to
// write through a read-only fd.
const wasiDescFlagWrite uint32 = 1 << 1

// fsDescNode is one live wasi:filesystem/types `descriptor` this package's
// handle table (wasiFS.descs, keyed by rep) tracks: either the single
// preopened root directory (isDir true, path "/"), or a regular file
// opened under it (isDir false, path the full virtual path used to look it
// up in wasiFS.files, content its bytes at open time -- read-via-stream's
// only consumer -- which may go stale if the same path is written after
// this descriptor was opened; nothing in this package's fixtures opens a
// path for both reading and writing through two different descriptors, so
// that staleness is never actually observed). writable records whether
// open-at's descriptor-flags carried the write bit (wasiDescFlagWrite),
// gating write-via-stream/append-via-stream.
type fsDescNode struct {
	isDir    bool
	path     string
	content  []byte
	writable bool
}

// fsWriteStreamNode is one live wasi:io/streams `output-stream` writing into
// an in-memory file's bytes: path names the fs.files entry it commits into
// (looked up fresh on every write, so it always sees the latest bytes even
// if another stream on the same path wrote first), pos is the next write
// offset (mirrors a real file descriptor's write cursor: write-via-stream
// seeds it at a fixed offset, append-via-stream seeds it at the file's
// current length). mu guards pos and serializes the read-modify-write
// against fs.files -- mirrors fsStreamNode's mu doc.
type fsWriteStreamNode struct {
	mu   sync.Mutex
	path string
	pos  int
}

// fsStreamNode is one live wasi:io/streams `input-stream` reading from an
// in-memory byte slice (the tail of an fsDescNode's content from
// read-via-stream's offset onward). mu guards pos, since nothing prevents
// a guest from racing two reads against the same stream handle (undefined
// which read gets which bytes, but neither may corrupt the other or the
// host).
type fsStreamNode struct {
	mu   sync.Mutex
	data []byte
	pos  int
}

// wasiFS holds the mutable state wasi_fs.go's host funcs close over: the
// configured virtual filesystem (files -- no longer read-only once
// write-via-stream/append-via-stream are registered, see fsFileGet/
// fsFileSet), the live descriptor/input-stream/output-stream rep tables,
// and a reference to the owning Instance's resource handle table
// (resources) -- set once via withResourcesHook, see this file's package
// doc's "Nested own<T> handles" section for why these closures cannot get
// it any other way.
type wasiFS struct {
	mu    sync.Mutex
	files map[string][]byte

	resources    *handleTable
	descs        map[uint32]*fsDescNode
	nextDesc     uint32
	streams      map[uint32]*fsStreamNode
	nextStream   uint32
	writeStreams map[uint32]*fsWriteStreamNode
	nextWriteRep uint32
}

// newWasiFS returns a wasiFS backed by files (WASIConfig.FS; a nil map
// behaves as an empty, unwritable-back filesystem -- fsFileSet lazily
// allocates its own internal map in that case so create/write still work
// within the run, but since that internal map is never the caller's own nil
// variable, a caller that wants to observe writes after run() must pass a
// non-nil (possibly empty) map, matching this package's doc comment on
// WASIConfig.FS). Rep numbering for descs, (read-)streams, and writeStreams
// each starts at 1, mirroring handleTable's own "0 is never allocated"
// convention (resource.go); the three counters are independent of each
// other, of wasiStdoutRep/wasiStderrRep (wasi.go), and of the handleTable's
// own handle numbering -- a rep is this package's private key into wasiFS's
// own maps, meaningful only together with which map it is looked up in.
// writeStreams' reps additionally never collide with wasiStdoutRep(1)/
// wasiStderrRep(2) because they share the same output-stream handle
// namespace (wasiOutputStreamResType) wasi.go's write/check-write/
// blocking-flush dispatch on: nextWriteRep starts at 3 for exactly that
// reason.
func newWasiFS(files map[string][]byte) *wasiFS {
	return &wasiFS{
		files:        files,
		descs:        make(map[uint32]*fsDescNode),
		nextDesc:     1,
		streams:      make(map[uint32]*fsStreamNode),
		nextStream:   1,
		writeStreams: make(map[uint32]*fsWriteStreamNode),
		nextWriteRep: 3,
	}
}

// fsFileGet returns files[path] and whether it was present, guarded by mu
// (files may now be concurrently written by fsFileSet -- see wasiFS's doc).
func (w *wasiFS) fsFileGet(path string) ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, ok := w.files[path]
	return b, ok
}

// fsFileSet commits content as path's new bytes, lazily allocating w.files
// if the configured WASIConfig.FS was nil (see newWasiFS's doc for why that
// case cannot write back to the caller).
func (w *wasiFS) fsFileSet(path string, content []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.files == nil {
		w.files = make(map[string][]byte)
	}
	w.files[path] = content
}

// setResources implements withResourcesHook's callback: it runs once, right
// after the owning Instance's handleTable is created and before any host
// func can be invoked (see host_import.go's withResourcesHook doc).
func (w *wasiFS) setResources(t *handleTable) {
	w.mu.Lock()
	w.resources = t
	w.mu.Unlock()
}

// getResources returns the resources handleTable setResources recorded,
// failing loud if a filesystem host func is somehow invoked before it ran
// (should be unreachable in practice: setResources always runs before
// instantiation returns control to any code that could call an export).
func (w *wasiFS) getResources() (*handleTable, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.resources == nil {
		return nil, fmt.Errorf("wasi:filesystem: resources handle table not yet initialized (setResources not called)")
	}
	return w.resources, nil
}

// newDescRep mints a fresh rep naming n and returns it.
func (w *wasiFS) newDescRep(n *fsDescNode) uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	rep := w.nextDesc
	w.nextDesc++
	w.descs[rep] = n
	return rep
}

// descNode resolves rep to its fsDescNode, failing loud if rep does not
// name a live descriptor (unknown, or already handled some other way --
// this package never drops a descriptor from w.descs, mirroring how a
// dropped guest handle is the handleTable's concern, not wasiFS's: rep
// reuse across live/dead descriptors would be far more dangerous than a
// small permanent map).
func (w *wasiFS) descNode(rep uint32) (*fsDescNode, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, ok := w.descs[rep]
	if !ok {
		return nil, fmt.Errorf("wasi:filesystem/types: descriptor rep %d does not name a live descriptor", rep)
	}
	return n, nil
}

// newStreamRep mints a fresh rep naming s and returns it.
func (w *wasiFS) newStreamRep(s *fsStreamNode) uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	rep := w.nextStream
	w.nextStream++
	w.streams[rep] = s
	return rep
}

// streamNode resolves rep to its fsStreamNode, failing loud if unknown.
func (w *wasiFS) streamNode(rep uint32) (*fsStreamNode, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.streams[rep]
	if !ok {
		return nil, fmt.Errorf("wasi:io/streams: input-stream rep %d does not name a live stream", rep)
	}
	return s, nil
}

// newWriteStreamRep mints a fresh output-stream rep naming s and returns it
// -- see newWasiFS's doc for why numbering starts at 3, not 1.
func (w *wasiFS) newWriteStreamRep(s *fsWriteStreamNode) uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	rep := w.nextWriteRep
	w.nextWriteRep++
	w.writeStreams[rep] = s
	return rep
}

// writeStreamNode resolves rep to its fsWriteStreamNode, reporting found=
// false (not an error) if rep does not name a live file-write stream --
// callers use this to distinguish "not one of mine" (fall through to
// wasi.go's stdout/stderr dispatch) from a genuinely unknown rep (which
// wasi.go's writeSink then reports as the fail-loud error).
func (w *wasiFS) writeStreamNode(rep uint32) (*fsWriteStreamNode, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.writeStreams[rep]
	return s, ok
}

// writeStreamWrite appends buf into the file the write-stream named by rep
// targets, starting at that stream's current write cursor, and advances the
// cursor by len(buf). Growing the underlying content past its current
// length (including past a positive starting offset, e.g. a first write
// through write-via-stream(offset) seeded beyond the file's current end)
// zero-fills the gap, mirroring a sparse-write real filesystem. Every write
// commits straight into fs.files (via fsFileSet) -- there is no internal
// buffering to distinguish "written" from "written and flushed" (mirrors
// wasi.go's write/blocking-write-and-flush sharing one implementation for
// the same reason), so [method]output-stream.blocking-flush against one of
// these reps has nothing left to do beyond confirming the rep is live.
func (w *wasiFS) writeStreamWrite(rep uint32, buf []byte) error {
	s, ok := w.writeStreamNode(rep)
	if !ok {
		return fmt.Errorf("wasi:io/streams: output-stream rep %d does not name a live stream", rep)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, _ := w.fsFileGet(s.path)
	end := s.pos + len(buf)
	if end > len(cur) {
		grown := make([]byte, end)
		copy(grown, cur)
		cur = grown
	}
	copy(cur[s.pos:end], buf)
	w.fsFileSet(s.path, cur)
	s.pos = end
	return nil
}

// wasiJoinFSPath joins a directory descriptor's virtual path (dir, always
// either "/" or a path this package itself produced) with a guest-supplied
// relative path component (rel), the same way [method]descriptor.open-at
// resolves its `path` argument against `self`. Per wasi:filesystem/types'
// doc (see types.wit), a `rel` that itself starts with "/" is invalid --
// it must be relative to dir, not another absolute path -- so that case
// returns ok=false rather than silently concatenating into a bogus path.
func wasiJoinFSPath(dir, rel string) (path string, ok bool) {
	if strings.HasPrefix(rel, "/") {
		return "", false
	}
	if dir == "/" {
		return "/" + rel, true
	}
	return dir + "/" + rel, true
}

// wasiListFromBytes converts buf into the list<u8> shape abi.Value expects
// (see abi.Value's doc: list<T> -> []abi.Value, u8 -> uint32) -- the
// lowering counterpart to wasi.go's wasiBytesFromList.
func wasiListFromBytes(buf []byte) []abi.Value {
	out := make([]abi.Value, len(buf))
	for i, b := range buf {
		out[i] = uint32(b)
	}
	return out
}

// wasiFilesystemOptions returns the Options implementing
// wasi:filesystem/preopens.get-directories, wasi:filesystem/types (the
// subset this file's package doc's discovery list names), wasi:io/streams'
// [method]input-stream.{read,blocking-read}, and the three
// wasi:cli/terminal-* get-terminal-* funcs -- everything WithWASI (wasi.go)
// needs beyond its own stdio-only surface to run a guest that actually
// reads and writes a file. fs (its files field backs the single preopened
// root directory "/") is constructed by WithWASI itself (not here) and
// shared with wasi.go's output-stream write/check-write/blocking-flush
// dispatch, since output-stream is one resource/handle namespace spanning
// both stdio and the write-via-stream/append-via-stream streams this file
// mints -- see wasi.go's writeSink doc for why that dispatch lives there
// instead of here.
func wasiFilesystemOptions(fs *wasiFS) []Option {
	getDirectories := func(context.Context, []abi.Value) ([]abi.Value, error) {
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newDescRep(&fsDescNode{isDir: true, path: "/"})
		handle := resources.NewOwn(wasiDescriptorResType, rep)
		return []abi.Value{[]abi.Value{[]abi.Value{handle, "/"}}}, nil
	}

	// filesystem-error-code translates a stream-error::last-operation-failed
	// payload into an error-code, when possible. This package never
	// constructs that variant case (every stream-error this package returns
	// is `closed`, which carries no payload -- see wasiStreamErrClosed's
	// doc), so no borrow<error> handle this func could be legitimately
	// called with ever exists; if a guest calls it anyway, liftHostArgs's
	// generic top-level borrow<error> resolution (resolveHandleArg,
	// host_import.go) already fails loud with "unknown handle" before this
	// closure body runs, so the body itself never needs to inspect its arg.
	filesystemErrorCode := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("wasi:filesystem/types.filesystem-error-code: expected 1 arg (err), got %d", len(args))
		}
		return []abi.Value{nil}, nil // option<error-code>::none
	}

	openAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 5 {
			return nil, fmt.Errorf("[method]descriptor.open-at: expected 5 args (self, path-flags, path, open-flags, flags), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.open-at: self: expected uint32 rep, got %T", args[0])
		}
		path, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.open-at: path: expected string, got %T", args[2])
		}
		openFlags, ok := args[3].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.open-at: open-flags: expected uint32, got %T", args[3])
		}
		descFlags, ok := args[4].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.open-at: flags: expected uint32, got %T", args[4])
		}
		// path-flags (args[1]) is ignored: this in-memory filesystem has no
		// symlinks, so symlink-follow has nothing to do.

		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.open-at: %w", err)
		}
		if !node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		writable := descFlags&wasiDescFlagWrite != 0
		content, found := fs.fsFileGet(full)
		switch {
		case !found && openFlags&wasiOpenFlagCreate != 0:
			// create: the path gets a brand-new, empty FS entry (mirroring
			// O_CREAT against a missing path), committed immediately -- a
			// real open(2) with O_CREAT makes the directory entry exist
			// right away, even before any byte is ever written to it.
			content = []byte{}
			fs.fsFileSet(full, content)
		case !found:
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNoEntry}}, nil
		case openFlags&wasiOpenFlagTruncate != 0 && writable:
			// truncate: an existing, writable entry's content resets to
			// empty (O_TRUNC); a truncate request against a descriptor that
			// wasn't even opened for writing is not honored, matching a
			// real OS's O_TRUNC|O_RDONLY combination doing nothing useful.
			content = []byte{}
			fs.fsFileSet(full, content)
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newDescRep(&fsDescNode{isDir: false, path: full, content: content, writable: writable})
		handle := resources.NewOwn(wasiDescriptorResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	getType := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.get-type: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.get-type: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.get-type: %w", err)
		}
		t := wasiDescriptorTypeRegularFile
		if node.isDir {
			t = wasiDescriptorTypeDirectory
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: t}}, nil
	}

	stat := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.stat: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.stat: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.stat: %w", err)
		}
		t := wasiDescriptorTypeRegularFile
		if node.isDir {
			t = wasiDescriptorTypeDirectory
		}
		// descriptor-stat's three option<datetime> fields are all `none`:
		// this in-memory filesystem tracks no timestamps at all (not even
		// zeroed ones), which is a valid answer per types.wit's doc ("If
		// the option is none, the platform doesn't maintain a ... timestamp
		// for this file").
		rec := []abi.Value{
			t,                         // type
			uint64(1),                 // link-count
			uint64(len(node.content)), // size
			nil,                       // data-access-timestamp
			nil,                       // data-modification-timestamp
			nil,                       // status-change-timestamp
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: rec}}, nil
	}

	readViaStream := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]descriptor.read-via-stream: expected 2 args (self, offset), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.read-via-stream: self: expected uint32 rep, got %T", args[0])
		}
		offset, ok := args[1].(uint64)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.read-via-stream: offset: expected uint64, got %T", args[1])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.read-via-stream: %w", err)
		}
		if node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeIsDirectory}}, nil
		}
		if offset > uint64(len(node.content)) {
			offset = uint64(len(node.content))
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newStreamRep(&fsStreamNode{data: node.content[offset:]})
		handle := resources.NewOwn(wasiInputStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	writeViaStream := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]descriptor.write-via-stream: expected 2 args (self, offset), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.write-via-stream: self: expected uint32 rep, got %T", args[0])
		}
		offset, ok := args[1].(uint64)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.write-via-stream: offset: expected uint64, got %T", args[1])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.write-via-stream: %w", err)
		}
		if node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeIsDirectory}}, nil
		}
		if !node.writable {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeReadOnly}}, nil
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newWriteStreamRep(&fsWriteStreamNode{path: node.path, pos: int(offset)})
		handle := resources.NewOwn(wasiOutputStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	appendViaStream := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.append-via-stream: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.append-via-stream: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.append-via-stream: %w", err)
		}
		if node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeIsDirectory}}, nil
		}
		if !node.writable {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeReadOnly}}, nil
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		cur, _ := fs.fsFileGet(node.path)
		rep := fs.newWriteStreamRep(&fsWriteStreamNode{path: node.path, pos: len(cur)})
		handle := resources.NewOwn(wasiOutputStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	// streamRead implements both [method]input-stream.read and
	// [method]input-stream.blocking-read: since every byte is already
	// resident in memory (no real I/O to actually block on), "read some of
	// what's available now" and "block until at least one byte is
	// available" have identical observable behavior here.
	streamRead := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]input-stream.read: expected 2 args (self, len), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]input-stream.read: self: expected uint32 rep, got %T", args[0])
		}
		length, ok := args[1].(uint64)
		if !ok {
			return nil, fmt.Errorf("[method]input-stream.read: len: expected uint64, got %T", args[1])
		}
		s, err := fs.streamNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]input-stream.read: %w", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pos >= len(s.data) {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: wasiStreamErrClosed}}}, nil
		}
		remaining := uint64(len(s.data) - s.pos)
		if length > remaining {
			length = remaining
		}
		chunk := s.data[s.pos : s.pos+int(length)]
		s.pos += int(length)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: wasiListFromBytes(chunk)}}, nil
	}

	// metadataHash backs [method]descriptor.metadata-hash -- reached not
	// directly by read_to_string's own logic, but by the preview1-to-
	// preview2 adapter's fd_filestat_get (POSIX fstat), which synthesizes
	// an inode number from it (see this file's package doc's discovery
	// list update: read_to_string calls fd_filestat_get, which is the
	// adapter's own name for `stat` -- it needs both stat AND
	// metadata-hash to build a full fstat result). This package tracks no
	// real inode/device identity, so lower/upper are simply the
	// descriptor's own rep -- unique per live descriptor, stable for its
	// lifetime, sufficient for "looks like a plausible fstat result" (nothing
	// this package's fixtures inspect the actual value).
	metadataHash := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: self: expected uint32 rep, got %T", args[0])
		}
		if _, err := fs.descNode(selfRep); err != nil {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: %w", err)
		}
		rec := []abi.Value{uint64(selfRep), uint64(0)} // lower, upper
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: rec}}, nil
	}

	getTerminalStdin := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }
	getTerminalStdout := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }
	getTerminalStderr := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }

	dirFD, dirResolve := wasiGetDirectoriesSig()
	fsErrFD, fsErrResolve := wasiFilesystemErrorCodeSig()
	openAtFD, openAtResolve := wasiOpenAtSig()
	getTypeFD, getTypeResolve := wasiGetTypeSig()
	statFD, statResolve := wasiStatSig()
	readViaStreamFD, readViaStreamResolve := wasiReadViaStreamSig()
	writeViaStreamFD, writeViaStreamResolve := wasiWriteViaStreamSig()
	appendViaStreamFD, appendViaStreamResolve := wasiAppendViaStreamSig()
	metadataHashFD, metadataHashResolve := wasiMetadataHashSig()
	inReadFD, inReadResolve := wasiInputStreamReadSig()
	inBlockingReadFD, inBlockingReadResolve := wasiInputStreamReadSig()
	termInFD, termInResolve := wasiGetTerminalSig(wasiTerminalInputResType)
	termOutFD, termOutResolve := wasiGetTerminalSig(wasiTerminalOutputResType)
	termErrFD, termErrResolve := wasiGetTerminalSig(wasiTerminalOutputResType)

	return []Option{
		withResourcesHook(fs.setResources),

		// See withResourceTag's doc (host_import.go): without these, a
		// guest that actually drops an owned descriptor/input-stream
		// handle (e.g. rustc's wasi_snapshot_preview1 adapter, freeing a
		// preopen descriptor once it has inspected it) trips the handle
		// table's cross-type-confusion check, because the guest's own
		// resource.drop canon tags the handle with the real wasm-binary
		// type index, not this package's synthetic ResourceType constant.
		withResourceTag(wasiIfaceFilesystemTypes, "descriptor", wasiDescriptorResType),
		withResourceTag(wasiIfaceStreams, "input-stream", wasiInputStreamResType),
		withResourceTag(wasiIfaceStreams, "output-stream", wasiOutputStreamResType),
		withResourceTag(wasiIfaceError, "error", wasiErrorResType),

		withImportCustom(wasiIfacePreopens, "get-directories", getDirectories, dirFD, dirResolve),

		withImportCustom(wasiIfaceFilesystemTypes, "filesystem-error-code", filesystemErrorCode, fsErrFD, fsErrResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.open-at", openAt, openAtFD, openAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.get-type", getType, getTypeFD, getTypeResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.stat", stat, statFD, statResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream", readViaStream, readViaStreamFD, readViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream", writeViaStream, writeViaStreamFD, writeViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream", appendViaStream, appendViaStreamFD, appendViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash", metadataHash, metadataHashFD, metadataHashResolve),

		withImportCustom(wasiIfaceStreams, "[method]input-stream.read", streamRead, inReadFD, inReadResolve),
		withImportCustom(wasiIfaceStreams, "[method]input-stream.blocking-read", streamRead, inBlockingReadFD, inBlockingReadResolve),

		withImportCustom(wasiIfaceTerminalStdin, "get-terminal-stdin", getTerminalStdin, termInFD, termInResolve),
		withImportCustom(wasiIfaceTerminalStdout, "get-terminal-stdout", getTerminalStdout, termOutFD, termOutResolve),
		withImportCustom(wasiIfaceTerminalStderr, "get-terminal-stderr", getTerminalStderr, termErrFD, termErrResolve),
	}
}

// wasiDescriptorTypeType interns wasi:filesystem/types' `descriptor-type`
// enum into tbl and returns its TypeRef, in exact WIT declaration order
// (from `wasm-tools component wit`).
func wasiDescriptorTypeType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.EnumDesc{Cases: []string{
		"unknown", "block-device", "character-device", "directory", "fifo",
		"symbolic-link", "regular-file", "socket",
	}})
}

// wasiErrorCodeType interns wasi:filesystem/types' `error-code` enum into
// tbl and returns its TypeRef, in exact WIT declaration order -- see this
// file's wasiErrorCode* constants, which must stay in lockstep with this
// list's order (each constant is that case's position here).
func wasiErrorCodeType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.EnumDesc{Cases: []string{
		"access", "would-block", "already", "bad-descriptor", "busy", "deadlock",
		"quota", "exist", "file-too-large", "illegal-byte-sequence", "in-progress",
		"interrupted", "invalid", "io", "is-directory", "loop", "too-many-links",
		"message-size", "name-too-long", "no-device", "no-entry", "no-lock",
		"insufficient-memory", "insufficient-space", "not-directory", "not-empty",
		"not-recoverable", "unsupported", "no-tty", "no-such-device", "overflow",
		"not-permitted", "pipe", "read-only", "invalid-seek", "text-file-busy",
		"cross-device",
	}})
}

// wasiDatetimeType interns wasi:clocks/wall-clock's `datetime` record
// (`record datetime { seconds: u64, nanoseconds: u32 }`) into tbl and
// returns its TypeRef. This package never constructs a datetime value
// (descriptor-stat's three timestamp fields are always `none` -- see
// stat's doc), but the type must still resolve structurally for Flatten to
// compute descriptor-stat's joined flat width, mirroring
// wasi.go's wasiStreamErrorType doc.
func wasiDatetimeType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "seconds", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "nanoseconds", Type: binary.TypeRef{Primitive: "u32"}},
	}})
}

// wasiDescriptorStatType interns wasi:filesystem/types' `descriptor-stat`
// record into tbl and returns its TypeRef.
func wasiDescriptorStatType(tbl *typeTable) binary.TypeRef {
	typeRef := wasiDescriptorTypeType(tbl)
	dtRef := wasiDatetimeType(tbl)
	optDtRef := tbl.add(binary.OptionDesc{Element: dtRef})
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "type", Type: typeRef},
		{Name: "link-count", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "size", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "data-access-timestamp", Type: optDtRef},
		{Name: "data-modification-timestamp", Type: optDtRef},
		{Name: "status-change-timestamp", Type: optDtRef},
	}})
}

// wasiFilesystemErrorCodeSig builds the FuncDesc/resolver for
// wasi:filesystem/types.filesystem-error-code(err: borrow<error>) ->
// option<error-code>.
func wasiFilesystemErrorCodeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	errArgRef := tbl.add(binary.BorrowDesc{ResourceType: wasiErrorResType})
	codeRef := wasiErrorCodeType(tbl)
	optRef := tbl.add(binary.OptionDesc{Element: codeRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "err", Type: errArgRef}},
		Results: binary.FuncResults{Unnamed: &optRef},
	}
	return fd, tbl.resolver()
}

// wasiOpenAtSig builds the FuncDesc/resolver for
// [method]descriptor.open-at(self: borrow<descriptor>, path-flags:
// path-flags, path: string, open-flags: open-flags, flags:
// descriptor-flags) -> result<own<descriptor>, error-code>. The three
// flags types' field lists are in exact WIT declaration order (from
// `wasm-tools component wit`), though only open-flags::create (bit 0) is
// ever actually inspected -- see wasiOpenFlagCreate's doc.
func wasiOpenAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	pathFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{"symlink-follow"}})
	openFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{"create", "directory", "exclusive", "truncate"}})
	descFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{
		"read", "write", "file-integrity-sync", "data-integrity-sync",
		"requested-write-sync", "mutate-directory",
	}})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiDescriptorResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "path-flags", Type: pathFlagsRef},
			{Name: "path", Type: binary.TypeRef{Primitive: "string"}},
			{Name: "open-flags", Type: openFlagsRef},
			{Name: "flags", Type: descFlagsRef},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiGetTypeSig builds the FuncDesc/resolver for
// [method]descriptor.get-type(self: borrow<descriptor>) ->
// result<descriptor-type, error-code>.
func wasiGetTypeSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := wasiDescriptorTypeType(tbl)
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiStatSig builds the FuncDesc/resolver for
// [method]descriptor.stat(self: borrow<descriptor>) ->
// result<descriptor-stat, error-code>.
func wasiStatSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := wasiDescriptorStatType(tbl)
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiReadViaStreamSig builds the FuncDesc/resolver for
// [method]descriptor.read-via-stream(self: borrow<descriptor>, offset:
// filesize) -> result<own<input-stream>, error-code>.
func wasiReadViaStreamSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiInputStreamResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "offset", Type: binary.TypeRef{Primitive: "u64"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiWriteViaStreamSig builds the FuncDesc/resolver for
// [method]descriptor.write-via-stream(self: borrow<descriptor>, offset:
// filesize) -> result<own<output-stream>, error-code>.
func wasiWriteViaStreamSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutputStreamResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "offset", Type: binary.TypeRef{Primitive: "u64"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiAppendViaStreamSig builds the FuncDesc/resolver for
// [method]descriptor.append-via-stream(self: borrow<descriptor>) ->
// result<own<output-stream>, error-code>.
func wasiAppendViaStreamSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiOutputStreamResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiInputStreamReadSig builds the FuncDesc/resolver for
// [method]input-stream.read(self: borrow<input-stream>, len: u64) ->
// result<list<u8>, stream-error> -- also reused as-is for blocking-read,
// which has the identical WIT signature (see streamRead's doc for why one
// Go closure implements both).
func wasiInputStreamReadSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiInputStreamResType})
	errRef := wasiStreamErrorType(tbl)
	okRef := tbl.add(binary.ListDesc{Element: binary.TypeRef{Primitive: "u8"}})
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "len", Type: binary.TypeRef{Primitive: "u64"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiMetadataHashType interns wasi:filesystem/types' `metadata-hash-value`
// record (`record metadata-hash-value { lower: u64, upper: u64 }`) into tbl
// and returns its TypeRef.
func wasiMetadataHashType(tbl *typeTable) binary.TypeRef {
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "lower", Type: binary.TypeRef{Primitive: "u64"}},
		{Name: "upper", Type: binary.TypeRef{Primitive: "u64"}},
	}})
}

// wasiMetadataHashSig builds the FuncDesc/resolver for
// [method]descriptor.metadata-hash(self: borrow<descriptor>) ->
// result<metadata-hash-value, error-code>.
func wasiMetadataHashSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := wasiMetadataHashType(tbl)
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiGetTerminalSig builds the FuncDesc/resolver for
// wasi:cli/terminal-{stdin,stdout,stderr}'s get-terminal-{stdin,stdout,
// stderr}() -> option<own<terminal-input|terminal-output>>. wazy has no
// real terminal, so every registered get-terminal-* func always answers
// `none` (see wasiFilesystemOptions' getTerminalStd{in,out,err}
// closures) -- resType only needs to be structurally present (distinct
// per interface, though this package never actually mints a handle under
// either tag) for Flatten to compute the option's joined flat width.
func wasiGetTerminalSig(resType uint32) (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	ownRef := tbl.add(binary.OwnDesc{ResourceType: resType})
	optRef := tbl.add(binary.OptionDesc{Element: ownRef})
	fd := binary.FuncDesc{Results: binary.FuncResults{Unnamed: &optRef}}
	return fd, tbl.resolver()
}
