package instance

import (
	"context"
	"fmt"
	"hash/fnv"
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
// A later fixture (testdata/conformance/f17_multifs.component.wasm, see
// conformance_test.go), whose main calls std::fs::metadata directly (not
// via read_to_string), surfaced one more func this same discovery process
// hadn't hit yet: [method]descriptor.stat-at. std::sys::fs::metadata on
// wasip2 goes through the preview1-to-preview2 adapter's
// path_filestat_get, which is stat-at (look a path up under a directory
// descriptor without opening it), not stat (fstat an already-open
// descriptor) -- read_to_string never calls metadata itself, so nothing in
// the original discovery list had exercised this path before.
//
// # Batch 4: directories, seek, and unlink
//
// f07/f08/f17's fixtures (and the read/write funcs above) only ever
// open-at + read/write-via-stream one flat file at a time -- nothing
// through f28_itertools ever asks this package to enumerate a directory's
// children, open a *directory* descriptor at all, or remove a path. Seven
// more conformance fixtures (f29_readdir through f35_remove --
// conformance_test.go) close that gap, discovered the same one-trap-at-a-
// time way as the original list above:
//
//   - std::fs::read_dir("/") (f29_readdir) opens "." *first*, not "/" --
//     its first host call is open-at(root, path=".", open-flags=directory),
//     not read-directory directly. Without wasiJoinFSPath treating rel=="."
//     as naming the directory itself, this resolves to the bogus path "/."
//     (found in no fs.files entry) and the guest panics on a spurious
//     error-code::no-entry before read-directory is ever reached.
//   - [method]descriptor.read-directory -> result<own<directory-entry-
//     stream>, error-code>, then repeated
//     [method]directory-entry-stream.read-directory-entry() ->
//     result<option<directory-entry{type, name}>, error-code> calls until
//     none, is std's actual iterator protocol over a directory (not one
//     batch list<T> call) -- mirrors read-via-stream's own
//     mint-a-resource-then-pull-from-it shape.
//   - [method]descriptor.unlink-file-at(path) (f35_remove) is
//     std::fs::remove_file's host call.
//   - Nothing new was needed for f31_seek (std::io::Seek is implemented
//     entirely in terms of repeated read-via-stream(offset) calls against
//     the same open descriptor -- no distinct "seek" WASI func exists) or
//     f34_append (append-via-stream/stat, both already implemented for
//     f08_filewrite/f17_multifs, are sufficient) -- both fixtures are
//     included anyway because nothing before batch 4 exercised that
//     *combination* (seek positions spanning start/current/end; a
//     stat-after-append/stat-after-truncate size sequence) even though no
//     new host func resulted.
//
// ## Directory modeling
//
// wasiFS.files stays exactly what it always was: a flat map<string,
// []byte> of *files* (WASIConfig.FS's own shape) -- no key is ever added
// for a directory's own existence. A directory (the preopened root, or any
// deeper path like "/a") is instead *synthetic*: fsIsDir reports a path is
// one if it's "/" or if any file lives strictly underneath it, and
// fsListDirEntries derives a directory's listing by scanning fs.files for
// keys under that prefix, folding every deeper path back down to its
// immediate next component (deduplicated) as a synthetic subdirectory
// entry. This means an explicitly-empty directory (one with zero files
// anywhere under it) cannot exist in this model -- there is no key to hang
// its existence off of -- which every batch-4 fixture avoids needing (each
// synthetic subdirectory always contains at least one file). Tracking
// directories as their own explicit set (rather than inferring them from
// file prefixes) was considered and rejected: it would require every
// directory-creating operation (there are none yet -- no fixture calls
// std::fs::create_dir) to remember to update a second data structure in
// lockstep with fs.files, for a case this package's fixtures never
// exercise; inferring from prefixes needs no such bookkeeping and cannot
// drift out of sync with the files that are its only source of truth.
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

// wasiDirEntryStreamResType tags wasi:filesystem/types' `directory-entry-
// stream` resource (see this file's "batch 4" doc addendum), minted by
// [method]descriptor.read-directory and consumed one entry at a time by
// [method]directory-entry-stream.read-directory-entry.
const wasiDirEntryStreamResType uint32 = 7

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
// (a descriptor is either the preopened root directory, a *synthetic*
// subdirectory minted the same way (see openAt's "batch 4" doc addendum and
// wasiFS.fsIsDir), or a regular file opened under one of those -- no other
// descriptor-type ever occurs).
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
// with error-code::no-entry, directory (bit 1) requests a directory
// descriptor rather than a file one -- see openAt's "batch 4" doc addendum,
// discovered by std::fs::read_dir("/") opening "." with this bit set before
// ever calling read-directory -- and truncate (bit 3) resets an existing
// (writable) entry's content to empty. exclusive is not inspected -- this
// in-memory filesystem has no concurrent opener to race against for it to
// matter.
const (
	wasiOpenFlagCreate    uint32 = 1 << 0
	wasiOpenFlagDirectory uint32 = 1 << 1
	wasiOpenFlagTruncate  uint32 = 1 << 3
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

// fsDirEntry is one child wasiFS.fsListDirEntries synthesizes for a
// directory: name is the child's own path component (never a full path),
// isDir says whether it is itself a (synthetic) subdirectory or a regular
// file -- see fsListDirEntries' doc for how these are derived from the
// flat fs.files map.
type fsDirEntry struct {
	name  string
	isDir bool
}

// fsDirStreamNode is one live wasi:filesystem/types `directory-entry-
// stream`, minted by read-directory: entries is the full listing captured
// at read-directory time (a real OS's readdir(3) offers no stronger
// consistency guarantee against concurrent mutation either, so a snapshot
// is a legitimate implementation choice, not a shortcut), pos is the next
// index [method]directory-entry-stream.read-directory-entry returns. mu
// guards pos, mirroring fsStreamNode's mu doc.
type fsDirStreamNode struct {
	mu      sync.Mutex
	entries []fsDirEntry
	pos     int
}

// wasiFS holds the mutable state wasi_fs.go's host funcs close over: the
// configured virtual filesystem (files -- no longer read-only once
// write-via-stream/append-via-stream are registered, see fsFileGet/
// fsFileSet), the live descriptor/input-stream/output-stream/directory-
// entry-stream rep tables, and a reference to the owning Instance's
// resource handle table (resources) -- set once via withResourcesHook, see
// this file's package doc's "Nested own<T> handles" section for why these
// closures cannot get it any other way.
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
	dirStreams   map[uint32]*fsDirStreamNode
	nextDirRep   uint32
}

// newWasiFS returns a wasiFS backed by files (WASIConfig.FS; a nil map
// behaves as an empty, unwritable-back filesystem -- fsFileSet lazily
// allocates its own internal map in that case so create/write still work
// within the run, but since that internal map is never the caller's own nil
// variable, a caller that wants to observe writes after run() must pass a
// non-nil (possibly empty) map, matching this package's doc comment on
// WASIConfig.FS). Rep numbering for descs, (read-)streams, writeStreams,
// and dirStreams each starts at 1, mirroring handleTable's own "0 is never
// allocated" convention (resource.go); the four counters are independent
// of each other, of wasiStdoutRep/wasiStderrRep (wasi.go), and of the
// handleTable's own handle numbering -- a rep is this package's private key
// into wasiFS's own maps, meaningful only together with which map it is
// looked up in. writeStreams' reps additionally never collide with
// wasiStdoutRep(1)/wasiStderrRep(2) because they share the same
// output-stream handle namespace (wasiOutputStreamResType) wasi.go's
// write/check-write/blocking-flush dispatch on: nextWriteRep starts at 3
// for exactly that reason.
func newWasiFS(files map[string][]byte) *wasiFS {
	return &wasiFS{
		files:        files,
		descs:        make(map[uint32]*fsDescNode),
		nextDesc:     1,
		streams:      make(map[uint32]*fsStreamNode),
		nextStream:   1,
		writeStreams: make(map[uint32]*fsWriteStreamNode),
		nextWriteRep: 3,
		dirStreams:   make(map[uint32]*fsDirStreamNode),
		nextDirRep:   1,
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

// fsFileDelete removes path from files, reporting whether it was present
// (unlink-file-at's only way to distinguish a real removal from a
// no-entry error -- see unlinkFileAt's doc).
func (w *wasiFS) fsFileDelete(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.files[path]; !ok {
		return false
	}
	delete(w.files, path)
	return true
}

// fsIsDir reports whether path names a directory in this package's
// synthetic directory model: the root "/" always is one (even with zero
// files, e.g. f33_createlist's fixture starts from an empty fs.files
// entirely), and any other path is one exactly when some file lives
// strictly underneath it (fs.files has no entry recording a directory's
// own existence the way it does a file's -- see this file's "batch 4" doc
// addendum's "Directory modeling" section for why). A path that is itself
// a live file (found in fs.files) is never also a directory -- this
// in-memory model has no path that is simultaneously both.
func (w *wasiFS) fsIsDir(path string) bool {
	if path == "/" {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, isFile := w.files[path]; isFile {
		return false
	}
	prefix := path + "/"
	for k := range w.files {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// fsListDirEntries returns dir's immediate children: every file directly
// under dir becomes a regular-file entry, and every distinct next path
// component of a more deeply nested file becomes one (deduplicated)
// synthetic directory entry -- e.g. with fs.files {"/a/b.txt", "/a/c.txt",
// "/d.txt"}, fsListDirEntries("/") yields [{"a", isDir:true}, {"d.txt",
// isDir:false}], and fsListDirEntries("/a") yields [{"b.txt", false},
// {"c.txt", false}]. Order is unspecified (Go map iteration), matching a
// real OS's own readdir(3) not guaranteeing order either -- every guest in
// this package's conformance fixtures sorts before printing for exactly
// this reason.
func (w *wasiFS) fsListDirEntries(dir string) []fsDirEntry {
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	seenDirs := make(map[string]bool)
	var out []fsDirEntry
	for k := range w.files {
		if !strings.HasPrefix(k, prefix) || k == prefix {
			continue
		}
		rest := k[len(prefix):]
		if idx := strings.IndexByte(rest, '/'); idx >= 0 {
			child := rest[:idx]
			if !seenDirs[child] {
				seenDirs[child] = true
				out = append(out, fsDirEntry{name: child, isDir: true})
			}
			continue
		}
		out = append(out, fsDirEntry{name: rest, isDir: false})
	}
	return out
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

// newDirStreamRep mints a fresh directory-entry-stream rep naming s and
// returns it.
func (w *wasiFS) newDirStreamRep(s *fsDirStreamNode) uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	rep := w.nextDirRep
	w.nextDirRep++
	w.dirStreams[rep] = s
	return rep
}

// dirStreamNode resolves rep to its fsDirStreamNode, failing loud if
// unknown -- mirrors streamNode's doc.
func (w *wasiFS) dirStreamNode(rep uint32) (*fsDirStreamNode, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.dirStreams[rep]
	if !ok {
		return nil, fmt.Errorf("wasi:filesystem/types: directory-entry-stream rep %d does not name a live stream", rep)
	}
	return s, nil
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
// rel of "." or "" names dir itself (discovered via std::fs::read_dir("/"),
// whose first host call is open-at(root, path=".", open-flags=directory) --
// std re-opens the preopened directory it already holds by its own POSIX
// "." convention rather than special-casing "no rename needed"; without
// this case, wasiJoinFSPath would produce "/." or "//", neither of which
// names anything in fs.files, and the guest would panic on a bogus
// error-code::no-entry).
func wasiJoinFSPath(dir, rel string) (path string, ok bool) {
	if rel == "." || rel == "" {
		return dir, true
	}
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
// instead of here. sockets is likewise constructed by WithWASI (always
// non-nil, even when WASIConfig.AllowTCP is false -- see its doc) and
// consulted as a fallback by streamRead (below), since input-stream is
// another resource/handle namespace spanning fs/stdin reads AND (when
// AllowTCP is set) socket reads -- mirrors the write-side dispatch's own
// three-way fallback in wasi.go's writeSink.
func wasiFilesystemOptions(fs *wasiFS, sockets *wasiSockets) []Option {
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
		// A directory open (open-flags::directory set -- discovered via
		// std::fs::read_dir("/"), whose first host call is exactly
		// open-at(root, ".", DIRECTORY) -- mints a synthetic directory
		// descriptor instead of falling into the file create/truncate/
		// read logic below: this in-memory model has no fs.files entry
		// recording a directory's own existence (see fsIsDir's doc), so a
		// directory open never touches fs.files at all, unlike a file
		// open's fsFileGet/fsFileSet.
		if openFlags&wasiOpenFlagDirectory != 0 {
			if !fs.fsIsDir(full) {
				return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
			}
			resources, err := fs.getResources()
			if err != nil {
				return nil, err
			}
			rep := fs.newDescRep(&fsDescNode{isDir: true, path: full})
			handle := resources.NewOwn(wasiDescriptorResType, rep)
			return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
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

	// statAt implements [method]descriptor.stat-at(self: borrow<descriptor>,
	// path-flags: path-flags, path: string) -> result<descriptor-stat,
	// error-code> -- discovered via f17_multifs.component.wasm
	// (testdata/conformance): std::fs::metadata resolves to
	// std::sys::fs::metadata on wasip2, which calls the preview1-to-preview2
	// adapter's path_filestat_get, NOT [method]descriptor.stat (that's
	// fd_filestat_get, for a descriptor already open) -- stat-at instead
	// looks a path up under a still-open directory descriptor without ever
	// minting a new descriptor for it, mirroring a real fstatat(2). Shares
	// its path resolution (wasiJoinFSPath) and not-found/not-a-directory
	// error handling with openAt, but never calls fs.fsFileSet: unlike
	// open-at, stat-at has no create/truncate flags to act on, so a missing
	// path is unconditionally error-code::no-entry. path-flags (args[1]) is
	// ignored for the same reason openAt ignores it (no symlinks in this
	// in-memory filesystem). Since batch 4 (see this file's doc addendum),
	// a resolved path may also name a synthetic subdirectory (fsIsDir) --
	// stat-at answers that case with descriptor-type::directory and a
	// zero size/link-count-1 record, the same directory-stat shape stat
	// itself returns for the preopened root.
	statAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("[method]descriptor.stat-at: expected 3 args (self, path-flags, path), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.stat-at: self: expected uint32 rep, got %T", args[0])
		}
		path, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.stat-at: path: expected string, got %T", args[2])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.stat-at: %w", err)
		}
		if !node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		content, found := fs.fsFileGet(full)
		if !found {
			if fs.fsIsDir(full) {
				rec := []abi.Value{
					wasiDescriptorTypeDirectory, // type
					uint64(1),                   // link-count
					uint64(0),                   // size
					nil, nil, nil,               // timestamps: always none, see stat's doc
				}
				return []abi.Value{abi.ResultValue{IsErr: false, Payload: rec}}, nil
			}
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNoEntry}}, nil
		}
		rec := []abi.Value{
			wasiDescriptorTypeRegularFile, // type
			uint64(1),                     // link-count
			uint64(len(content)),          // size
			nil,                           // data-access-timestamp
			nil,                           // data-modification-timestamp
			nil,                           // status-change-timestamp
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: rec}}, nil
	}

	// readDirectory implements [method]descriptor.read-directory(self:
	// borrow<descriptor>) -> result<own<directory-entry-stream>,
	// error-code> -- discovered via f29_readdir.component.wasm
	// (testdata/conformance): std::fs::read_dir("/") open-ats "." with
	// open-flags::directory (see openAt's "batch 4" doc addendum) to get a
	// directory descriptor, then calls this to get an iterator-shaped
	// stream over that directory's children. Snapshots fs.fsListDirEntries
	// once, at call time, into the minted fsDirStreamNode -- see that
	// type's doc for why a snapshot is a legitimate readdir(3) semantics
	// choice.
	readDirectory := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.read-directory: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.read-directory: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.read-directory: %w", err)
		}
		if !node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newDirStreamRep(&fsDirStreamNode{entries: fs.fsListDirEntries(node.path)})
		handle := resources.NewOwn(wasiDirEntryStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	// readDirectoryEntry implements
	// [method]directory-entry-stream.read-directory-entry(self:
	// borrow<directory-entry-stream>) -> result<option<directory-entry>,
	// error-code>: pulls the next entry off the stream's snapshot (see
	// fsDirStreamNode's doc), or option::none once exhausted -- mirroring
	// [method]input-stream.read's stream-error::closed-at-EOF shape, but
	// unlike that stream this one has no error case this package's
	// in-memory model can ever produce (a directory-entry-stream never
	// outlives the fs.files snapshot it was minted from), so the result is
	// always Ok.
	readDirectoryEntry := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]directory-entry-stream.read-directory-entry: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]directory-entry-stream.read-directory-entry: self: expected uint32 rep, got %T", args[0])
		}
		s, err := fs.dirStreamNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]directory-entry-stream.read-directory-entry: %w", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pos >= len(s.entries) {
			return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil // option::none
		}
		e := s.entries[s.pos]
		s.pos++
		t := wasiDescriptorTypeRegularFile
		if e.isDir {
			t = wasiDescriptorTypeDirectory
		}
		entry := []abi.Value{t, e.name} // directory-entry{type, name}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: entry}}, nil
	}

	// unlinkFileAt implements [method]descriptor.unlink-file-at(self:
	// borrow<descriptor>, path: string) -> result<_, error-code> --
	// discovered via f35_remove.component.wasm (testdata/conformance):
	// std::fs::remove_file resolves to this. Removes exactly one fs.files
	// entry; a path that resolves to a (synthetic) directory or to nothing
	// at all is rejected the same way a real unlink(2) rejects them
	// (is-directory / no-entry respectively) -- this in-memory model never
	// needs to worry about a directory becoming non-empty or empty as a
	// side effect, since directories are never separately represented (see
	// fsIsDir's doc).
	unlinkFileAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 2 {
			return nil, fmt.Errorf("[method]descriptor.unlink-file-at: expected 2 args (self, path), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.unlink-file-at: self: expected uint32 rep, got %T", args[0])
		}
		path, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.unlink-file-at: path: expected string, got %T", args[1])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.unlink-file-at: %w", err)
		}
		if !node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if fs.fsIsDir(full) {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeIsDirectory}}, nil
		}
		if !fs.fsFileDelete(full) {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNoEntry}}, nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
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
	// [method]input-stream.blocking-read. For an fs/stdin-backed stream,
	// every byte is already resident in memory (no real I/O to actually
	// block on), so "read some of what's available now" and "block until
	// at least one byte is available" have identical observable behavior.
	// For a socket-backed stream (rep not found in fs.streams, falling
	// through to sockets.inStreamNode -- see wasiFilesystemOptions' own doc
	// for why this func's dispatch spans both), the read is a genuine
	// blocking net.Conn.Read (sockInStream.read), so the two methods differ
	// there in name only, identically to how this package's fs path never
	// distinguished them either.
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
			if sock, found := sockets.inStreamNode(selfRep); found {
				return sock.read(length)
			}
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

	// metadataHashAt is metadata-hash's stat-at counterpart -- reached the
	// same way statAt is (the preview1-to-preview2 adapter's
	// path_filestat_get combines stat-at AND metadata-hash-at into a full
	// POSIX fstatat result, mirroring fd_filestat_get's stat+metadata-hash
	// pairing for an already-open descriptor). Unlike metadata-hash, there
	// is no live descriptor rep to reuse as an identity source here (stat-at
	// never mints one), so this hashes the resolved absolute path instead
	// (FNV-1a) -- still unique per distinct file and stable across repeated
	// calls against the same path within a run, which is all a "looks like
	// a plausible fstatat result" inode stand-in needs to be (nothing this
	// package's fixtures inspect the actual value).
	metadataHashAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 3 {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash-at: expected 3 args (self, path-flags, path), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash-at: self: expected uint32 rep, got %T", args[0])
		}
		path, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash-at: path: expected string, got %T", args[2])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash-at: %w", err)
		}
		if !node.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if _, found := fs.fsFileGet(full); !found {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNoEntry}}, nil
		}
		h := fnv.New64a()
		_, _ = h.Write([]byte(full))
		rec := []abi.Value{h.Sum64(), uint64(0)} // lower, upper
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
	statAtFD, statAtResolve := wasiStatAtSig()
	readDirectoryFD, readDirectoryResolve := wasiReadDirectorySig()
	readDirEntryFD, readDirEntryResolve := wasiReadDirectoryEntrySig()
	unlinkFileAtFD, unlinkFileAtResolve := wasiUnlinkFileAtSig()
	readViaStreamFD, readViaStreamResolve := wasiReadViaStreamSig()
	writeViaStreamFD, writeViaStreamResolve := wasiWriteViaStreamSig()
	appendViaStreamFD, appendViaStreamResolve := wasiAppendViaStreamSig()
	metadataHashFD, metadataHashResolve := wasiMetadataHashSig()
	metadataHashAtFD, metadataHashAtResolve := wasiMetadataHashAtSig()
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
		withResourceTag(wasiIfaceFilesystemTypes, "directory-entry-stream", wasiDirEntryStreamResType),
		withResourceTag(wasiIfaceStreams, "input-stream", wasiInputStreamResType),
		withResourceTag(wasiIfaceStreams, "output-stream", wasiOutputStreamResType),
		withResourceTag(wasiIfaceError, "error", wasiErrorResType),

		withImportCustom(wasiIfacePreopens, "get-directories", getDirectories, dirFD, dirResolve),

		withImportCustom(wasiIfaceFilesystemTypes, "filesystem-error-code", filesystemErrorCode, fsErrFD, fsErrResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.open-at", openAt, openAtFD, openAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.get-type", getType, getTypeFD, getTypeResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.stat", stat, statFD, statResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.stat-at", statAt, statAtFD, statAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.read-directory", readDirectory, readDirectoryFD, readDirectoryResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]directory-entry-stream.read-directory-entry", readDirectoryEntry, readDirEntryFD, readDirEntryResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.unlink-file-at", unlinkFileAt, unlinkFileAtFD, unlinkFileAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.read-via-stream", readViaStream, readViaStreamFD, readViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.write-via-stream", writeViaStream, writeViaStreamFD, writeViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.append-via-stream", appendViaStream, appendViaStreamFD, appendViaStreamResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash", metadataHash, metadataHashFD, metadataHashResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.metadata-hash-at", metadataHashAt, metadataHashAtFD, metadataHashAtResolve),

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

// wasiStatAtSig builds the FuncDesc/resolver for
// [method]descriptor.stat-at(self: borrow<descriptor>, path-flags:
// path-flags, path: string) -> result<descriptor-stat, error-code>.
// path-flags shares open-at's single-field "symlink-follow" shape (per its
// WIT declaration).
func wasiStatAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	pathFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{"symlink-follow"}})
	okRef := wasiDescriptorStatType(tbl)
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "path-flags", Type: pathFlagsRef},
			{Name: "path", Type: binary.TypeRef{Primitive: "string"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiReadDirectorySig builds the FuncDesc/resolver for
// [method]descriptor.read-directory(self: borrow<descriptor>) ->
// result<own<directory-entry-stream>, error-code>.
func wasiReadDirectorySig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := tbl.add(binary.OwnDesc{ResourceType: wasiDirEntryStreamResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiDirectoryEntryType interns wasi:filesystem/types' `directory-entry`
// record (`record directory-entry { type: descriptor-type, name: string
// }`) into tbl and returns its TypeRef, in exact WIT declaration order
// (from `wasm-tools component wit`).
func wasiDirectoryEntryType(tbl *typeTable) binary.TypeRef {
	typeRef := wasiDescriptorTypeType(tbl)
	return tbl.add(binary.RecordDesc{Fields: []binary.RecordField{
		{Name: "type", Type: typeRef},
		{Name: "name", Type: binary.TypeRef{Primitive: "string"}},
	}})
}

// wasiReadDirectoryEntrySig builds the FuncDesc/resolver for
// [method]directory-entry-stream.read-directory-entry(self:
// borrow<directory-entry-stream>) -> result<option<directory-entry>,
// error-code>.
func wasiReadDirectoryEntrySig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDirEntryStreamResType})
	entryRef := wasiDirectoryEntryType(tbl)
	okRef := tbl.add(binary.OptionDesc{Element: entryRef})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiUnlinkFileAtSig builds the FuncDesc/resolver for
// [method]descriptor.unlink-file-at(self: borrow<descriptor>, path:
// string) -> result<_, error-code>.
func wasiUnlinkFileAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "path", Type: binary.TypeRef{Primitive: "string"}},
		},
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

// wasiMetadataHashAtSig builds the FuncDesc/resolver for
// [method]descriptor.metadata-hash-at(self: borrow<descriptor>, path-flags:
// path-flags, path: string) -> result<metadata-hash-value, error-code>.
func wasiMetadataHashAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	pathFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{"symlink-follow"}})
	okRef := wasiMetadataHashType(tbl)
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "path-flags", Type: pathFlagsRef},
			{Name: "path", Type: binary.TypeRef{Primitive: "string"}},
		},
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
