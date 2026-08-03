package instance

import (
	"context"
	"fmt"
	iofs "io/fs"
	"path"
	"strings"
	"sync"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
	internalsys "github.com/samyfodil/wazy/internal/sys"
	"github.com/samyfodil/wazy/sys"
)

// This file extends wasi.go's WASI 0.2 host surface with a genuine
// wasi:filesystem/types + wasi:io/streams input-stream implementation,
// backed by the mounts WASIConfig.FS (a wazy.FSConfig -- the same one the
// core wasi_snapshot_preview1 runtime takes) configures, plus the three
// wasi:cli/terminal-{stdin,stdout,stderr} funcs a real rustc guest's
// std::fs path also reaches (all three always answer "no terminal" -- see
// wasiGetTerminalSig's doc).
//
// # Mounts
//
// Every FSConfig mount becomes one preopened descriptor, reported under its
// guest path by preopens.get-directories; a descriptor records which mount's
// sys.FS it came from and its path within that mount, and every *-at method
// resolves against those two (see fsDescNode). Since a guest resolves an
// absolute path to the longest matching preopen itself -- the same logic it
// applies to preview1's fd preopens -- mounting "/", "/tmp", and
// "/site-packages" separately needs no routing on this side. The two
// operations that span descriptors, rename-at and link-at, are the only ones
// that must care, and they answer cross-device when the two descriptors turn
// out to belong to different mounts, exactly as rename(2) does.
//
// Nothing about a file lives in this package: reads, writes, listings, and
// metadata all go straight to the mounted sys.FS, so a WithDirMount guest
// write is on disk when the call returns, and a read-only mount
// (WithReadOnlyDirMount, or any WithFSMount -- io/fs.FS has no write
// surface) rejects a write with its own errno, translated by fsErrorCode.
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
//     as naming the directory itself, this resolves to a bogus path and the
//     guest panics on a spurious error-code::no-entry before read-directory
//     is ever reached.
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
// There is none: a directory is whatever the mounted sys.FS says is one.
// This replaced a flat map<string, []byte> of files in which directories
// were *synthetic* -- inferred from the path prefixes of the files under
// them, so an empty directory could not be represented at all, create_dir
// needed a second side table to fake one, and rename of a directory meant
// re-keying every entry beneath it by hand. Mkdir/Rmdir/Rename/Unlink/
// Readdir on the mount do all of that correctly and for free.
//
// # Batch 5: descriptor flags and sync
//
// A real CPython (componentize-py) guest -- the first guest here that is not
// a rustc binary -- reaches two descriptor methods no rustc fixture ever
// did, both through the preview1-to-preview2 adapter rather than from
// Python source directly:
//
//   - [method]descriptor.get-flags, the adapter's fd_fdstat_get: "was this
//     descriptor opened for reading, for writing, or both, and may its
//     contents be mutated?". The read/write answer comes from what open-at
//     recorded on the descriptor, never from the mount; mutate-directory is
//     advertised for every directory, mirroring the dirRightsBase posture
//     wazy's own preview1 fd_fdstat_get already takes -- see getFlags.
//   - [method]descriptor.sync, the adapter's fd_sync, which is os.fsync --
//     pip calls it on every file it writes while installing a wheel. Its
//     sibling [method]descriptor.sync-data (fd_datasync, os.fdatasync) is
//     registered alongside it, sharing one implementation; see fsSyncFunc
//     for both, and for what syncing means when a descriptor holds no
//     cached fd.
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

// wasi:filesystem/types' descriptor-type enum, in WIT declaration order.
// Directory and regular-file are what a mount reports for all but the most
// unusual paths; the rest exist because a mount is a real filesystem now and
// nothing stops a guest from stat-ing a device node or a socket in it (see
// fsDescriptorType).
const (
	wasiDescriptorTypeUnknown uint32 = iota
	wasiDescriptorTypeBlockDevice
	wasiDescriptorTypeCharacterDevice
	wasiDescriptorTypeDirectory
	wasiDescriptorTypeFIFO
	wasiDescriptorTypeSymbolicLink
	wasiDescriptorTypeRegularFile
	wasiDescriptorTypeSocket
)

// wasi:io/streams' stream-error variant case indices (see
// wasiStreamErrorType in wasi.go: case 0 is last-operation-failed(error),
// case 1 is closed). This package never constructs
// last-operation-failed -- a read never reports failure after the
// descriptor has already resolved -- so streamErrClosed is the only case
// ever produced.
const wasiStreamErrClosed uint32 = 1

// wasiMaxStreamRead caps the buffer a single file-backed
// [method]input-stream.read allocates. The `len` a guest passes is a u64 it
// chose; a short read is always a legal answer (the guest loops until
// stream-error::closed), so there is no reason to let one call ask the host
// for an arbitrary allocation. 1 MiB is far above any real std read buffer.
const wasiMaxStreamRead uint64 = 1 << 20

// wasi:filesystem/types' open-flags flag bits, per their WIT declaration
// order create/directory/exclusive/truncate. All four map onto the O_* flag
// openAt hands the mount: create (bit 0) -> O_CREAT, directory (bit 1) ->
// O_DIRECTORY (requesting a directory descriptor rather than a file one --
// see openAt's "batch 4" doc addendum, discovered by std::fs::read_dir("/")
// opening "." with this bit set before ever calling read-directory),
// exclusive (bit 2) -> O_EXCL, and truncate (bit 3) -> O_TRUNC.
const (
	wasiOpenFlagCreate    uint32 = 1 << 0
	wasiOpenFlagDirectory uint32 = 1 << 1
	wasiOpenFlagExclusive uint32 = 1 << 2
	wasiOpenFlagTruncate  uint32 = 1 << 3
)

// wasi:filesystem/types' descriptor-flags bits this package handles (bits 0,
// 1 and 5, per its WIT declaration order
// read/write/file-integrity-sync/data-integrity-sync/requested-write-sync/
// mutate-directory).
//
// A descriptor opened with the write bit set is the one
// [method]descriptor.write-via-stream/append-via-stream may be called
// against; every other descriptor (including the preopened root
// directories) is write-via-stream-ineligible, matching a real OS refusing
// to write through a read-only fd. Read and write are remembered on the
// descriptor node verbatim, since [method]descriptor.get-flags' whole job is
// to report back how a descriptor was opened (see getFlags).
//
// mutate-directory is not stored: it is a property of being a directory, and
// get-flags derives it from the node's isDir. It is reported for every
// directory descriptor, whatever mount it came from -- see getFlags for the
// advisory-capability reasoning and for its preview1 precedent.
//
// The three sync bits are never set: they are O_SYNC/O_DSYNC/O_RSYNC, which
// open-at does not request.
const (
	wasiDescFlagRead            uint32 = 1 << 0
	wasiDescFlagWrite           uint32 = 1 << 1
	wasiDescFlagMutateDirectory uint32 = 1 << 5
)

// fsMount is one preopened directory: the sys.FS an FSConfig mount supplied,
// and the absolute guest path wasi:filesystem/preopens.get-directories
// reports it under (always slash-prefixed, "/" for the root mount). One
// descriptor per mount is minted per get-directories call; the guest itself
// resolves an absolute path to the longest matching preopen, so this package
// never needs a prefix table of its own -- see getDirectories.
type fsMount struct {
	fs        sys.FS
	guestPath string
}

// fsDescNode is one live wasi:filesystem/types `descriptor` this package's
// handle table (wasiFS.descs, keyed by rep) tracks: fs is the mount it lives
// in and path is its location within that mount, relative to the mount root
// ("." names the mount root itself, matching io/fs and sys.FS convention).
// readable/writable are the access mode the descriptor was opened with
// (open-at's descriptor-flags, corrected where open-at overrode them -- see
// its oflag switch): writable gates write-via-stream/append-via-stream and
// picks the mode sync reopens with, and the pair is what
// [method]descriptor.get-flags reports back. They are recorded at open time
// rather than derived from the path's own mode on demand for the reason
// fcntl(fd, F_GETFL) exists: a descriptor's flags say how it was opened, not
// what its file happens to permit today.
//
// A descriptor holds no open sys.File: every operation opens the path, acts,
// and closes again. ponytail: that is one extra open+close per stream chunk;
// it buys a descriptor (and stream) lifetime with no host resource to leak
// when a guest drops a handle without telling us, or aborts mid-call. Cache
// the open file on the node -- with a withHostResourceDtor to close it -- if
// a profile ever shows the reopens mattering.
type fsDescNode struct {
	fs sys.FS
	// mount is the index in wasiFS.mounts that fs came from -- the identity
	// rename-at/link-at compare to detect a cross-mount operation. Comparing
	// the sys.FS values themselves would panic on a third-party mount whose
	// dynamic type is uncomparable (a struct holding a map, say), which is
	// exactly the sort of filesystem WithSysFSMount exists to accept.
	mount    int
	path     string
	isDir    bool
	readable bool
	writable bool
}

// fsWriteStreamNode is one live wasi:io/streams `output-stream` writing into
// a file: fs/path name it, and pos is the next write offset (mirrors a real
// file descriptor's write cursor: write-via-stream seeds it at a fixed
// offset, append-via-stream seeds it at the file's current length). mu
// guards pos and serializes the open-Pwrite-close each write performs --
// mirrors fsStreamNode's mu doc.
type fsWriteStreamNode struct {
	mu   sync.Mutex
	fs   sys.FS
	path string
	pos  int64
}

// fsStreamNode is one live wasi:io/streams `input-stream`, in one of two
// flavors:
//
//   - byte-backed (fs nil): reads out of data, the shape wasi.go's stdin and
//     wasi_http.go's response bodies mint (both are fully-resident byte
//     strings with no file behind them).
//   - file-backed (fs non-nil): reads out of the file at path via Pread,
//     starting at read-via-stream's offset -- data stays nil.
//
// pos is the next read offset in whichever of the two it is. mu guards pos,
// since nothing prevents a guest from racing two reads against the same
// stream handle (undefined which read gets which bytes, but neither may
// corrupt the other or the host).
type fsStreamNode struct {
	mu   sync.Mutex
	data []byte
	pos  int64

	fs   sys.FS
	path string
}

// fsDirEntry is one child of a directory listing: name is the child's own
// path component (never a full path), isDir says whether it is itself a
// directory or a regular file.
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
// configured mounts (see fsMount), the live descriptor/input-stream/
// output-stream/directory-entry-stream rep tables, and a reference to the
// owning Instance's resource handle table (resources) -- set once via
// withResourcesHook, see this file's package doc's "Nested own<T> handles"
// section for why these closures cannot get it any other way.
//
// The filesystem contents themselves are NOT state here: every read, write,
// and metadata lookup goes straight through to the mounted sys.FS, so what
// a guest wrote is on the host filesystem (for a WithDirMount) the moment
// the call returns, with no copy for this package to keep coherent.
type wasiFS struct {
	mu     sync.Mutex
	mounts []fsMount

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

// newWasiFS returns a wasiFS serving mounts (fsMountsFromConfig's result; a
// nil/empty slice preopens nothing, so get-directories returns an empty list
// -- see WASIConfig.FS). Rep numbering for descs, (read-)streams, writeStreams,
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
func newWasiFS(mounts []fsMount) *wasiFS {
	return &wasiFS{
		mounts:       mounts,
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

// fsMountsFromConfig reads cfg's mounts back out as fsMounts, in the order
// they were configured. Guest paths are normalized to an absolute, slash-
// prefixed form ("" / "." / "tmp" / "/tmp/" all being ways to write "/" or
// "/tmp"), since that is what get-directories must report for a guest's own
// longest-prefix match against an absolute path to work.
//
// The mounts are read through a Preopens() type assertion rather than an
// FSConfig method: FSConfig is wazy's public configuration surface, and the
// index-correlated (sys.FS, guestPath) pair behind it is an implementation
// detail no embedder should be building against. A cfg that is not wazy's
// own implementation (there is none -- see FSConfig's doc) preopens nothing.
func fsMountsFromConfig(cfg wazy.FSConfig) []fsMount {
	preopener, ok := cfg.(interface {
		Preopens() ([]sys.FS, []string)
	})
	if !ok {
		return nil
	}
	fss, guestPaths := preopener.Preopens()
	mounts := make([]fsMount, 0, len(fss))
	for i, f := range fss {
		mounts = append(mounts, fsMount{fs: f, guestPath: "/" + internalsys.StripPrefixesAndTrailingSlash(guestPaths[i])})
	}
	return mounts
}

// fsErrorCode maps a sys.Errno from a mounted filesystem onto the
// wasi:filesystem/types error-code a guest expects for it. A zero errno must
// never reach here (callers check success first). An errno with no exact
// error-code counterpart answers `io`, the generic "the filesystem failed"
// case, rather than inventing a more specific claim.
func fsErrorCode(errno sys.Errno) uint32 {
	switch errno {
	case sys.EACCES:
		return wasiErrorCodeAccess
	case sys.EAGAIN:
		return wasiErrorCodeWouldBlock
	case sys.EBADF:
		return wasiErrorCodeBadDescriptor
	case sys.EEXIST:
		return wasiErrorCodeExist
	case sys.EFAULT, sys.EINVAL:
		return wasiErrorCodeInvalid
	case sys.EINTR:
		return wasiErrorCodeInterrupted
	case sys.EIO:
		return wasiErrorCodeIO
	case sys.EISDIR:
		return wasiErrorCodeIsDirectory
	case sys.ELOOP:
		return wasiErrorCodeLoop
	case sys.ENAMETOOLONG:
		return wasiErrorCodeNameTooLong
	case sys.ENOENT:
		return wasiErrorCodeNoEntry
	case sys.ENOSYS:
		return wasiErrorCodeUnsupported
	case sys.ENOTDIR:
		return wasiErrorCodeNotDirectory
	case sys.ENOTEMPTY:
		return wasiErrorCodeNotEmpty
	case sys.ENOTSOCK, sys.ENOTSUP:
		return wasiErrorCodeUnsupported
	case sys.EPERM:
		return wasiErrorCodeNotPermitted
	case sys.EROFS:
		return wasiErrorCodeReadOnly
	default:
		return wasiErrorCodeIO
	}
}

// fsErrResult wraps an errno as the ready-to-return `result<_, error-code>`
// error branch every wasi:filesystem/types method shares.
func fsErrResult(errno sys.Errno) []abi.Value {
	return []abi.Value{abi.ResultValue{IsErr: true, Payload: fsErrorCode(errno)}}
}

// fsOkResult wraps payload as the `result<T, error-code>` success branch.
func fsOkResult(payload abi.Value) []abi.Value {
	return []abi.Value{abi.ResultValue{IsErr: false, Payload: payload}}
}

// fsDescriptorType maps a mount's file mode onto the wasi:filesystem/types
// descriptor-type enum. Anything that is neither a directory nor one of the
// four special modes below is a regular file, matching how sys.Stat_t models
// modes it has no bit for.
func fsDescriptorType(mode iofs.FileMode) uint32 {
	switch {
	case mode.IsDir():
		return wasiDescriptorTypeDirectory
	case mode&iofs.ModeSymlink != 0:
		return wasiDescriptorTypeSymbolicLink
	case mode&iofs.ModeNamedPipe != 0:
		return wasiDescriptorTypeFIFO
	case mode&iofs.ModeSocket != 0:
		return wasiDescriptorTypeSocket
	case mode&iofs.ModeCharDevice != 0:
		return wasiDescriptorTypeCharacterDevice
	case mode&iofs.ModeDevice != 0:
		return wasiDescriptorTypeBlockDevice
	default:
		return wasiDescriptorTypeRegularFile
	}
}

// fsDatetime lowers an epoch-nanosecond timestamp into wasi:clocks' datetime
// record {seconds: u64, nanoseconds: u32}, as the `some` payload of one of
// descriptor-stat's option<datetime> fields. A zero (or negative) timestamp
// -- what a mount that keeps no times reports -- lowers to `none`, which
// types.wit explicitly allows ("If the option is none, the platform doesn't
// maintain a ... timestamp for this file") and which is what this package
// returned for every field before mounts made real times available.
func fsDatetime(nanos int64) abi.Value {
	if nanos <= 0 {
		return nil
	}
	return []abi.Value{uint64(nanos / 1e9), uint32(nanos % 1e9)}
}

// fsStatRecord lowers a mount's Stat_t into the descriptor-stat record
// [method]descriptor.stat and stat-at both return.
func fsStatRecord(st sys.Stat_t) []abi.Value {
	return []abi.Value{
		fsDescriptorType(st.Mode), // type
		st.Nlink,                  // link-count
		uint64(st.Size),           // size
		fsDatetime(st.Atim),       // data-access-timestamp
		fsDatetime(st.Mtim),       // data-modification-timestamp
		fsDatetime(st.Ctim),       // status-change-timestamp
	}
}

// fsListDirEntries returns dir's immediate children, read from the mount
// itself. Order is whatever the mount returns (a real readdir(3) guarantees
// none either); every guest in this package's conformance fixtures sorts
// before printing for exactly that reason. The "." and ".." entries a POSIX
// readdir includes are filtered out: wasi:filesystem's directory-entry-
// stream is specified to omit them, and a guest that saw them would recurse
// forever.
func fsListDirEntries(fsys sys.FS, dir string) ([]fsDirEntry, sys.Errno) {
	f, errno := fsys.OpenFile(dir, sys.O_RDONLY|sys.O_DIRECTORY, 0)
	if errno != 0 {
		return nil, errno
	}
	defer f.Close()
	dirents, errno := f.Readdir(-1)
	if errno != 0 {
		return nil, errno
	}
	out := make([]fsDirEntry, 0, len(dirents))
	for _, d := range dirents {
		if d.Name == "." || d.Name == ".." {
			continue
		}
		out = append(out, fsDirEntry{name: d.Name, isDir: d.IsDir()})
	}
	return out, 0
}

// fsSelfPathArgs parses the common (self: borrow<descriptor>, path: string)
// method args and resolves the descriptor, which must be a directory. On a
// non-directory it returns errVal (a ready-to-return not-directory result) and
// a nil node; on an arg/lookup error it returns a Go error. Shared by
// create-directory-at and remove-directory-at.
func fsSelfPathArgs(method string, fs *wasiFS, args []abi.Value) (node *fsDescNode, path string, errVal []abi.Value, err error) {
	if len(args) != 2 {
		return nil, "", nil, fmt.Errorf("[method]descriptor.%s: expected 2 args (self, path), got %d", method, len(args))
	}
	selfRep, ok := args[0].(uint32)
	if !ok {
		return nil, "", nil, fmt.Errorf("[method]descriptor.%s: self: expected uint32 rep, got %T", method, args[0])
	}
	path, ok = args[1].(string)
	if !ok {
		return nil, "", nil, fmt.Errorf("[method]descriptor.%s: path: expected string, got %T", method, args[1])
	}
	node, err = fs.descNode(selfRep)
	if err != nil {
		return nil, "", nil, fmt.Errorf("[method]descriptor.%s: %w", method, err)
	}
	if !node.isDir {
		return nil, "", []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
	}
	return node, path, nil, nil
}

// fsSyncFunc builds the HostFunc behind [method]descriptor.sync (syncFile =
// sys.File.Sync, POSIX fsync) and its sibling [method]descriptor.sync-data
// (sys.File.Datasync, POSIX fdatasync). The two differ in exactly that one
// call, so method (the WIT name, for error text) and syncFile are the whole
// difference between them.
//
// # Syncing without a cached fd
//
// A descriptor here holds no open sys.File (see fsDescNode), so this opens
// the path, syncs, and closes -- fsyncing a handle opened moments ago, which
// reads odd until you notice what fsync(2) actually names: the FILE, not the
// descriptor. It flushes the inode's dirty pages and metadata, state shared
// by every descriptor open on it, which is why fsync through a freshly
// opened handle is indistinguishable from fsync through one held since the
// guest's own open. Nothing is lost by not caching, either: this package
// buffers no writes of its own (writeStreamWrite Pwrites straight to the
// mount, see its doc), so a cached fd would have nothing extra to give the
// kernel. That leaves caching purely a cost -- a host file handle per live
// descriptor, leaked whenever a guest drops a handle without saying so or
// aborts mid-call, which is the exact failure fsDescNode's no-open-file rule
// exists to prevent. So: no cached fd, and no correctness debt from it.
//
// # What each kind of descriptor syncs
//
//   - A directory syncs for real, opened O_RDONLY|O_DIRECTORY. fsync on a
//     directory fd is what makes a create/unlink/rename durable, so no-oping
//     it would quietly break the one guest that does the right thing. (A
//     read-only mount answers bad-descriptor here, since wazy's read-only
//     layer has no sync surface at all -- the same answer wazy's own
//     preview1 fd_sync gives for that mount, so the two runtimes agree.)
//   - A file opened for writing syncs for real, in the access mode it was
//     opened with (mirroring openAt's own O_RDWR/O_WRONLY choice) -- the
//     case pip's os.fsync after writing a wheel's files actually hits.
//   - A file NOT opened for writing answers Ok without touching the mount:
//     types.wit says so outright ("This function succeeds with no effect if
//     the file descriptor is not opened for writing"), and it is the only
//     answer that is the same everywhere -- POSIX fsync(2) on a read-only fd
//     is a successful no-op, but Windows' FlushFileBuffers refuses a handle
//     without write access, so honoring the call literally would make one
//     guest's os.fsync succeed on Linux and fail on Windows for no reason
//     the guest could act on.
func fsSyncFunc(fs *wasiFS, method string, syncFile func(sys.File) sys.Errno) HostFunc {
	return func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.%s: expected 1 arg (self), got %d", method, len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.%s: self: expected uint32 rep, got %T", method, args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.%s: %w", method, err)
		}
		var oflag sys.Oflag
		switch {
		case node.isDir:
			oflag = sys.O_RDONLY | sys.O_DIRECTORY
		case !node.writable:
			return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil // nothing this descriptor could have dirtied
		case node.readable:
			oflag = sys.O_RDWR
		default:
			oflag = sys.O_WRONLY
		}
		f, errno := node.fs.OpenFile(node.path, oflag, 0)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		errno = syncFile(f)
		f.Close()
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}
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

// writeStreamWrite writes buf into the file the write-stream named by rep
// targets, at that stream's current write cursor, and advances the cursor by
// len(buf). A cursor past the file's current end leaves a hole, exactly as
// pwrite(2) does -- the mount, not this package, decides how that is stored.
// Every write goes straight to the mount, with no buffering to distinguish
// "written" from "written and flushed" (mirrors wasi.go's write/
// blocking-write-and-flush sharing one implementation for the same reason),
// so [method]output-stream.blocking-flush against one of these reps has
// nothing left to do beyond confirming the rep is live.
func (w *wasiFS) writeStreamWrite(rep uint32, buf []byte) error {
	s, ok := w.writeStreamNode(rep)
	if !ok {
		return fmt.Errorf("wasi:io/streams: output-stream rep %d does not name a live stream", rep)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	f, errno := s.fs.OpenFile(s.path, sys.O_WRONLY, 0)
	if errno != 0 {
		return fmt.Errorf("wasi:io/streams: output-stream rep %d: opening %q: %w", rep, s.path, errno)
	}
	defer f.Close()
	// Loop: pwrite(2) may write fewer bytes than asked (a full disk, an
	// interrupted call), and this func's caller reports success for the whole
	// buffer, so a short write left unfinished would silently lose the tail.
	for len(buf) > 0 {
		n, errno := f.Pwrite(buf, s.pos)
		if errno != 0 {
			return fmt.Errorf("wasi:io/streams: output-stream rep %d: writing %q at %d: %w", rep, s.path, s.pos, errno)
		}
		if n == 0 {
			return fmt.Errorf("wasi:io/streams: output-stream rep %d: writing %q at %d: wrote 0 of %d bytes", rep, s.path, s.pos, len(buf))
		}
		s.pos += int64(n)
		buf = buf[n:]
	}
	return nil
}

// wasiJoinFSPath joins a directory descriptor's mount-relative path (dir,
// "." for the mount root) with a guest-supplied relative path component
// (rel), the same way [method]descriptor.open-at resolves its `path`
// argument against `self`. The result is mount-relative too, ready to hand
// to a sys.FS method. Per wasi:filesystem/types' doc (see types.wit), a
// `rel` that itself starts with "/" is invalid -- it must be relative to
// dir, not another absolute path -- so that case returns ok=false rather
// than silently concatenating into a bogus path. rel of "." or "" names dir
// itself (discovered via std::fs::read_dir("/"), whose first host call is
// open-at(root, path=".", open-flags=directory) -- std re-opens the
// preopened directory it already holds by its own POSIX "." convention
// rather than special-casing "no rename needed").
//
// # Escaping
//
// rel may not escape the descriptor it is resolved against: it is cleaned
// first (so "a/../b", which ends up going nowhere, is fine) and then checked
// with io/fs.ValidPath, which rejects a rooted path and anything left
// starting with "..". This is the same two-line rule
// wasi_snapshot_preview1's atPath applies (imports/wasi_snapshot_preview1/
// fs.go), for the same reason: a descriptor is a capability, and a guest
// holding one for a subdirectory must not be able to walk up out of it --
// not to the preopen root, and certainly not off the mount and into the
// host filesystem. The WASI testsuite's interesting_paths cases are what
// pinned that behavior for preview1; the check here is deliberately
// identical so the two runtimes cannot disagree about what a guest may
// reach.
//
// This is unconditional: there is no mount option, config field, or build
// tag that turns it off, and adding one was considered and rejected. The
// component model never asks for it -- wasi:filesystem has no method or
// flag that means "leave this directory", and a descriptor obtained from
// preopens.get-directories is the only way a guest can name anything at
// all, so escape is not a capability being withheld, it is one that was
// never granted. Every guest this package runs (the conformance fixtures,
// all real rustc wasm32-wasip2 binaries) works without it. If a future
// guest genuinely needs to reach a second directory, the answer is a
// second mount, not a hole in this one.
//
// A trailing slash survives the clean (preview1 restores it the same way):
// it is what makes opening "file/" -- a regular file named as a directory --
// fail with ENOTDIR at the mount, rather than silently succeeding.
func wasiJoinFSPath(dir, rel string) (joined string, ok bool) {
	if rel == "." || rel == "" {
		return dir, true
	}
	hasTrailingSlash := strings.HasSuffix(rel, "/")
	rel = path.Clean(rel)
	if !iofs.ValidPath(rel) {
		return "", false
	}
	joined = path.Join(dir, rel)
	if hasTrailingSlash {
		joined += "/" // path.Join cleans it off again, so restore it last
	}
	return joined, true
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
	// getDirectories mints one descriptor per configured mount, reported
	// under that mount's guest path. A guest resolves an absolute path to
	// the longest matching preopen itself (its own POSIX path logic, the
	// same one preview1's fd preopens rely on), so mounting "/" and "/tmp"
	// and "/site-packages" needs no prefix routing on this side: whichever
	// preopen the guest picked is the descriptor -- and therefore the
	// sys.FS -- every subsequent *-at call resolves against.
	getDirectories := func(context.Context, []abi.Value) ([]abi.Value, error) {
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		dirs := make([]abi.Value, 0, len(fs.mounts))
		for i, m := range fs.mounts {
			// A preopen is readable and not writable. `read` is honest: the
			// guest may list it, stat it, and open paths under it, which is
			// every use a directory descriptor has here. `write` would not
			// be: in descriptor-flags it means "this descriptor may be
			// written through", i.e. write-via-stream/append-via-stream, and
			// both answer is-directory against any directory descriptor --
			// the same thing a real open(2) does when asked for O_RDWR on a
			// directory. Mutating what is *inside* the directory
			// (create-directory-at, unlink-file-at, rename-at) is the
			// separate mutate-directory flag, which get-flags reports for
			// every directory descriptor including this one -- see getFlags.
			rep := fs.newDescRep(&fsDescNode{fs: m.fs, mount: i, path: ".", isDir: true, readable: true})
			handle := resources.NewOwn(wasiDescriptorResType, rep)
			dirs = append(dirs, []abi.Value{handle, m.guestPath})
		}
		return []abi.Value{dirs}, nil
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
		// path-flags (args[1]) is ignored: symlink-follow is the mount's
		// business, and sys.FS's OpenFile follows links like open(2).

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
		readable := descFlags&wasiDescFlagRead != 0
		// The open is real: it is what applies create/truncate/exclusive and
		// what reports a missing path, a read-only mount, or a permission
		// problem as the errno the guest gets back. The handle itself is
		// closed again immediately -- a descriptor node holds only the mount
		// and path (see fsDescNode's doc) -- so this open is the access
		// check and the create/truncate side effect, nothing more.
		// The read/write descriptor-flags pick the access mode, so a guest
		// that asked for write-only (what std::fs::write does) gets O_WRONLY
		// rather than an O_RDWR the mount might refuse on a file it is only
		// allowed to write.
		//
		// readable/writable are then corrected to the mode the open
		// actually used, which is what the descriptor records and what
		// [method]descriptor.get-flags reports back (see getFlags): the two
		// differ from the request only where this switch overrides it.
		oflag := sys.O_RDONLY
		switch {
		case writable && readable:
			oflag = sys.O_RDWR
		case writable:
			oflag = sys.O_WRONLY
		default:
			// Neither bit, or read alone: there is no mode below O_RDONLY to
			// fall back to, so a descriptor opened with empty
			// descriptor-flags is a readable one -- read-via-stream works
			// through it, and saying otherwise would be a lie a guest could
			// act on.
			readable = true
		}
		if openFlags&wasiOpenFlagDirectory != 0 {
			// A directory open (discovered via std::fs::read_dir("/"), whose
			// first host call is exactly open-at(root, ".", DIRECTORY)) must
			// stay read-only: a real open(2) refuses O_RDWR on a directory.
			// The descriptor is therefore not a writable one whatever the
			// guest asked for -- and a directory opened WITHOUT this flag but
			// with the write bit never gets a descriptor at all, since the
			// mount refuses a write-mode open of a directory outright
			// (EISDIR on POSIX, whatever the platform says elsewhere), so no
			// directory descriptor can ever report `write`.
			oflag = sys.O_RDONLY | sys.O_DIRECTORY
			readable, writable = true, false
		} else {
			if openFlags&wasiOpenFlagCreate != 0 {
				oflag |= sys.O_CREAT
			}
			if openFlags&wasiOpenFlagExclusive != 0 {
				oflag |= sys.O_EXCL
			}
			// A truncate request against a descriptor that wasn't even
			// opened for writing is not honored, matching a real OS's
			// O_TRUNC|O_RDONLY combination doing nothing useful.
			if openFlags&wasiOpenFlagTruncate != 0 && writable {
				oflag |= sys.O_TRUNC
			}
		}
		f, errno := node.fs.OpenFile(full, oflag, 0o644)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		isDir, errno := f.IsDir()
		f.Close()
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		if openFlags&wasiOpenFlagDirectory != 0 && !isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newDescRep(&fsDescNode{fs: node.fs, mount: node.mount, path: full, isDir: isDir, readable: readable, writable: writable})
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
		st, errno := node.fs.Stat(node.path)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return fsOkResult(fsDescriptorType(st.Mode)), nil
	}

	// getFlags implements [method]descriptor.get-flags(self:
	// borrow<descriptor>) -> result<descriptor-flags, error-code>, which a
	// real CPython (componentize-py) guest reaches through the
	// preview1-to-preview2 adapter's fd_fdstat_get -- the host half of
	// Python's os.fstat/io mode checks, and of every std that asks "was
	// this opened for reading, for writing, or both?".
	//
	// The answer is read straight off the descriptor node, with no call to
	// the mount: descriptor-flags describes how the descriptor was OPENED
	// (the access mode open-at's `flags` argument resolved to), not what the
	// underlying path would permit -- the same distinction fcntl(fd,
	// F_GETFL) draws, and the reason a descriptor opened read-only on a
	// writable file must still report only `read`. That also means this func
	// has no error branch of its own: an unknown rep is the shared fail-loud
	// descNode error, and everything else is Ok.
	//
	// mutate-directory is reported for every directory descriptor, and for
	// no other kind -- the flag is directory-scoped by definition (it
	// governs create-directory-at/unlink-file-at/rename-at/link-at, all of
	// which take a directory as self), so a regular-file descriptor
	// carrying it would be meaningless.
	//
	// Crucially it is reported whatever mount the directory came from,
	// including one that will refuse every mutation. A capability flag is
	// advisory, not a guarantee of outcome: `write` on a file descriptor
	// does not promise the next write beats ENOSPC either, and nobody reads
	// it that way. wazy's own preview1 says exactly this already --
	// fd_fdstat_get advertises dirRightsBase (RIGHT_PATH_CREATE_FILE,
	// RIGHT_PATH_UNLINK_FILE, RIGHT_PATH_RENAME_SOURCE/TARGET, ...) for
	// every directory descriptor unconditionally, WithReadOnlyDirMount or
	// not, and lets the real operation return the real errno
	// (imports/wasi_snapshot_preview1/fs.go's dirRightsBase). Since
	// mutate-directory is WASI 0.2's successor to those rights, withholding
	// it here would make wazy's two runtimes disagree about the same mount.
	//
	// The failure modes are not symmetric either. Reported on a read-only
	// mount, a guest tries and gets error-code::read-only or ::unsupported
	// from the operation itself: an errno it can print. Withheld, a guest
	// that gates on the flag never calls at all, and the mount looks
	// read-only with no errno anywhere to explain why -- and a guest gating
	// on it is the concrete case here, since the preview1-to-preview2
	// adapter synthesizes preview1 rights from this func's result, so a
	// missing mutate-directory can become a missing RIGHT_PATH_CREATE_FILE
	// and an ENOTCAPABLE raised inside the guest, before any host call.
	//
	// The three sync bits (file-integrity-sync, data-integrity-sync,
	// requested-write-sync) are O_SYNC/O_DSYNC/O_RSYNC, which open-at never
	// requests, so they are never reported.
	getFlags := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.get-flags: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.get-flags: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.get-flags: %w", err)
		}
		var flags uint32
		if node.readable {
			flags |= wasiDescFlagRead
		}
		if node.writable {
			flags |= wasiDescFlagWrite
		}
		if node.isDir {
			flags |= wasiDescFlagMutateDirectory
		}
		return fsOkResult(flags), nil
	}

	// syncFn implements [method]descriptor.sync(self: borrow<descriptor>) ->
	// result<_, error-code> -- Python's os.fsync, which a real CPython
	// (componentize-py) guest reaches during a pip wheel install. (Named
	// syncFn rather than sync only because this file imports the sync
	// package.)
	syncFn := fsSyncFunc(fs, "sync", sys.File.Sync)

	// syncDataFn implements [method]descriptor.sync-data -- sync's sibling
	// (POSIX fdatasync, Python's os.fdatasync), registered for the same
	// reason append-via-stream is (see this file's package doc): no fixture
	// calls it yet, but it is one Datasync call away from a func that is
	// already here, and a guest that does call it would otherwise hit the
	// graph engine's trap stub. sys.File's Datasync is a real fdatasync(2)
	// on Linux and dispatches to a full fsync everywhere else
	// (internal/sysfs) -- syncing more than asked is always a legal answer
	// to "flush this file's data".
	syncDataFn := fsSyncFunc(fs, "sync-data", sys.File.Datasync)

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
		st, errno := node.fs.Stat(node.path)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return fsOkResult(fsStatRecord(st)), nil
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
	// ignored for the same reason openAt ignores it (symlink following is
	// the mount's business).
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
		st, errno := node.fs.Stat(full)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return fsOkResult(fsStatRecord(st)), nil
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
		entries, errno := fsListDirEntries(node.fs, node.path)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newDirStreamRep(&fsDirStreamNode{entries: entries})
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
	// package can produce once the stream exists (it was minted from a
	// listing already read off the mount), so the result is always Ok.
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
	// std::fs::remove_file resolves to this. The mount's own Unlink decides
	// every rejection (a directory, a missing path, a read-only mount), so
	// this func only resolves the path.
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
		if errno := node.fs.Unlink(full); errno != 0 {
			return fsErrResult(errno), nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// createDirectoryAt implements [method]descriptor.create-directory-at(self,
	// path) -> result<_, error-code> (std::fs::create_dir).
	createDirectoryAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		node, path, errVal, err := fsSelfPathArgs("create-directory-at", fs, args)
		if err != nil || errVal != nil {
			return errVal, err
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if errno := node.fs.Mkdir(full, 0o755); errno != 0 {
			return fsErrResult(errno), nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// removeDirectoryAt implements [method]descriptor.remove-directory-at(self,
	// path) -> result<_, error-code> (std::fs::remove_dir): removes an empty
	// directory.
	removeDirectoryAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		node, path, errVal, err := fsSelfPathArgs("remove-directory-at", fs, args)
		if err != nil || errVal != nil {
			return errVal, err
		}
		full, ok := wasiJoinFSPath(node.path, path)
		if !ok {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if errno := node.fs.Rmdir(full); errno != 0 {
			return fsErrResult(errno), nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// renameAt implements [method]descriptor.rename-at(self, old-path,
	// new-descriptor: borrow<descriptor>, new-path) -> result<_, error-code>
	// (std::fs::rename): moves a file or directory subtree.
	renameAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 4 {
			return nil, fmt.Errorf("[method]descriptor.rename-at: expected 4 args (self, old-path, new-descriptor, new-path), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.rename-at: self: expected uint32 rep, got %T", args[0])
		}
		oldPath, ok := args[1].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.rename-at: old-path: expected string, got %T", args[1])
		}
		newDescRep, ok := args[2].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.rename-at: new-descriptor: expected uint32 rep, got %T", args[2])
		}
		newPath, ok := args[3].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.rename-at: new-path: expected string, got %T", args[3])
		}
		selfNode, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.rename-at: %w", err)
		}
		newNode, err := fs.descNode(newDescRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.rename-at: new-descriptor: %w", err)
		}
		if !selfNode.isDir || !newNode.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		if selfNode.mount != newNode.mount {
			// Both descriptors must live in the same mount: a rename across
			// two filesystems is exactly what rename(2) answers EXDEV to,
			// and std turns error-code::cross-device back into that errno,
			// which is the signal for its copy-then-delete fallback.
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeCrossDevice}}, nil
		}
		oldFull, ok1 := wasiJoinFSPath(selfNode.path, oldPath)
		newFull, ok2 := wasiJoinFSPath(newNode.path, newPath)
		if !ok1 || !ok2 {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if errno := selfNode.fs.Rename(oldFull, newFull); errno != 0 {
			return fsErrResult(errno), nil
		}
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: nil}}, nil
	}

	// linkAt implements [method]descriptor.link-at(self, old-path-flags,
	// old-path, new-descriptor, new-path) -> result<_, error-code>
	// (std::fs::hard_link) -- a real hard link, made by the mount's own Link,
	// with the shared inode a real one has (a mount that cannot make them,
	// such as any WithFSMount, answers unsupported).
	linkAt := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 5 {
			return nil, fmt.Errorf("[method]descriptor.link-at: expected 5 args (self, old-path-flags, old-path, new-descriptor, new-path), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.link-at: self: expected uint32 rep, got %T", args[0])
		}
		// args[1] old-path-flags (symlink-follow flags) is not inspected: this
		// model has no symlinks, so there is nothing to follow or not.
		oldPath, ok := args[2].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.link-at: old-path: expected string, got %T", args[2])
		}
		newDescRep, ok := args[3].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.link-at: new-descriptor: expected uint32 rep, got %T", args[3])
		}
		newPath, ok := args[4].(string)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.link-at: new-path: expected string, got %T", args[4])
		}
		selfNode, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.link-at: %w", err)
		}
		newNode, err := fs.descNode(newDescRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.link-at: new-descriptor: %w", err)
		}
		if !selfNode.isDir || !newNode.isDir {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotDirectory}}, nil
		}
		if selfNode.mount != newNode.mount {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeCrossDevice}}, nil // see renameAt
		}
		oldFull, ok1 := wasiJoinFSPath(selfNode.path, oldPath)
		newFull, ok2 := wasiJoinFSPath(newNode.path, newPath)
		if !ok1 || !ok2 {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: wasiErrorCodeNotPermitted}}, nil
		}
		if errno := selfNode.fs.Link(oldFull, newFull); errno != 0 {
			return fsErrResult(errno), nil
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
		resources, err := fs.getResources()
		if err != nil {
			return nil, err
		}
		rep := fs.newStreamRep(&fsStreamNode{fs: node.fs, path: node.path, pos: int64(offset)})
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
		rep := fs.newWriteStreamRep(&fsWriteStreamNode{fs: node.fs, path: node.path, pos: int64(offset)})
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
		st, errno := node.fs.Stat(node.path)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		rep := fs.newWriteStreamRep(&fsWriteStreamNode{fs: node.fs, path: node.path, pos: st.Size})
		handle := resources.NewOwn(wasiOutputStreamResType, rep)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: handle}}, nil
	}

	// streamRead implements both [method]input-stream.read and
	// [method]input-stream.blocking-read. For a stdin-backed stream every
	// byte is already resident in memory, and a file-backed one reads from a
	// mount that answers immediately, so "read some of what's available now"
	// and "block until at least one byte is available" have identical
	// observable behavior for both.
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

		if s.fs != nil {
			// File-backed (read-via-stream): pull the next chunk straight out
			// of the mount at the stream's cursor. A short read is fine (the
			// guest loops); a zero-length one is EOF, reported as
			// stream-error::closed exactly like the byte-backed path below.
			f, errno := s.fs.OpenFile(s.path, sys.O_RDONLY, 0)
			if errno != 0 {
				return nil, fmt.Errorf("[method]input-stream.read: opening %q: %w", s.path, errno)
			}
			defer f.Close()
			if length > wasiMaxStreamRead {
				length = wasiMaxStreamRead
			}
			buf := make([]byte, length)
			n, errno := f.Pread(buf, s.pos)
			if errno != 0 {
				// A real read failure, NOT end of file: sys.FS reports EOF as
				// a zero-length read with a zero errno (UnwrapOSError maps
				// io.EOF to 0), so anything else here is genuine I/O trouble
				// and must fail loud rather than masquerade as EOF and hand
				// the guest a silently truncated file.
				return nil, fmt.Errorf("[method]input-stream.read: reading %q at %d: %w", s.path, s.pos, errno)
			}
			if n == 0 {
				return []abi.Value{abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: wasiStreamErrClosed}}}, nil
			}
			s.pos += int64(n)
			return []abi.Value{abi.ResultValue{IsErr: false, Payload: wasiListFromBytes(buf[:n])}}, nil
		}

		if s.pos >= int64(len(s.data)) {
			return []abi.Value{abi.ResultValue{IsErr: true, Payload: abi.VariantValue{Disc: wasiStreamErrClosed}}}, nil
		}
		remaining := uint64(int64(len(s.data)) - s.pos)
		if length > remaining {
			length = remaining
		}
		chunk := s.data[s.pos : s.pos+int64(length)]
		s.pos += int64(length)
		return []abi.Value{abi.ResultValue{IsErr: false, Payload: wasiListFromBytes(chunk)}}, nil
	}

	// metadataHash backs [method]descriptor.metadata-hash -- reached not
	// directly by read_to_string's own logic, but by the preview1-to-
	// preview2 adapter's fd_filestat_get (POSIX fstat), which synthesizes
	// an inode number from it (see this file's package doc's discovery
	// list update: read_to_string calls fd_filestat_get, which is the
	// adapter's own name for `stat` -- it needs both stat AND
	// metadata-hash to build a full fstat result). The mount supplies real
	// device/inode identity, so that is what lower/upper carry: two paths
	// hash equal exactly when they are the same file, which is the property
	// types.wit actually asks of this func (and what makes a hard link
	// detectable as one).
	metadataHash := func(_ context.Context, args []abi.Value) ([]abi.Value, error) {
		if len(args) != 1 {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: expected 1 arg (self), got %d", len(args))
		}
		selfRep, ok := args[0].(uint32)
		if !ok {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: self: expected uint32 rep, got %T", args[0])
		}
		node, err := fs.descNode(selfRep)
		if err != nil {
			return nil, fmt.Errorf("[method]descriptor.metadata-hash: %w", err)
		}
		st, errno := node.fs.Stat(node.path)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return fsOkResult([]abi.Value{uint64(st.Ino), st.Dev}), nil // lower, upper
	}

	// metadataHashAt is metadata-hash's stat-at counterpart -- reached the
	// same way statAt is (the preview1-to-preview2 adapter's
	// path_filestat_get combines stat-at AND metadata-hash-at into a full
	// POSIX fstatat result, mirroring fd_filestat_get's stat+metadata-hash
	// pairing for an already-open descriptor).
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
		st, errno := node.fs.Stat(full)
		if errno != 0 {
			return fsErrResult(errno), nil
		}
		return fsOkResult([]abi.Value{uint64(st.Ino), st.Dev}), nil // lower, upper
	}

	getTerminalStdin := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }
	getTerminalStdout := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }
	getTerminalStderr := func(context.Context, []abi.Value) ([]abi.Value, error) { return []abi.Value{nil}, nil }

	dirFD, dirResolve := wasiGetDirectoriesSig()
	fsErrFD, fsErrResolve := wasiFilesystemErrorCodeSig()
	openAtFD, openAtResolve := wasiOpenAtSig()
	getTypeFD, getTypeResolve := wasiGetTypeSig()
	getFlagsFD, getFlagsResolve := wasiGetFlagsSig()
	syncFD, syncResolve := wasiSyncSig()
	syncDataFD, syncDataResolve := wasiSyncSig()
	statFD, statResolve := wasiStatSig()
	statAtFD, statAtResolve := wasiStatAtSig()
	readDirectoryFD, readDirectoryResolve := wasiReadDirectorySig()
	readDirEntryFD, readDirEntryResolve := wasiReadDirectoryEntrySig()
	unlinkFileAtFD, unlinkFileAtResolve := wasiUnlinkFileAtSig()
	createDirAtFD, createDirAtResolve := wasiUnlinkFileAtSig()
	removeDirAtFD, removeDirAtResolve := wasiUnlinkFileAtSig()
	renameAtFD, renameAtResolve := wasiRenameAtSig()
	linkAtFD, linkAtResolve := wasiLinkAtSig()
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
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.get-flags", getFlags, getFlagsFD, getFlagsResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.sync", syncFn, syncFD, syncResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.sync-data", syncDataFn, syncDataFD, syncDataResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.stat", stat, statFD, statResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.stat-at", statAt, statAtFD, statAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.read-directory", readDirectory, readDirectoryFD, readDirectoryResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]directory-entry-stream.read-directory-entry", readDirectoryEntry, readDirEntryFD, readDirEntryResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.unlink-file-at", unlinkFileAt, unlinkFileAtFD, unlinkFileAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.create-directory-at", createDirectoryAt, createDirAtFD, createDirAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.remove-directory-at", removeDirectoryAt, removeDirAtFD, removeDirAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.rename-at", renameAt, renameAtFD, renameAtResolve),
		withImportCustom(wasiIfaceFilesystemTypes, "[method]descriptor.link-at", linkAt, linkAtFD, linkAtResolve),
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

// wasiGetFlagsSig builds the FuncDesc/resolver for
// [method]descriptor.get-flags(self: borrow<descriptor>) ->
// result<descriptor-flags, error-code>. descriptor-flags' field list is in
// exact WIT declaration order, and must stay identical to open-at's own
// (wasiOpenAtSig) -- it is the same WIT type, and wasiDescFlagRead/
// wasiDescFlagWrite are positions in this list.
func wasiGetFlagsSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	okRef := tbl.add(binary.FlagsDesc{Names: []string{
		"read", "write", "file-integrity-sync", "data-integrity-sync",
		"requested-write-sync", "mutate-directory",
	}})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Ok: &okRef, Err: &errRef})
	fd := binary.FuncDesc{
		Params:  []binary.FuncParam{{Name: "self", Type: selfRef}},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiSyncSig builds the FuncDesc/resolver for
// [method]descriptor.sync(self: borrow<descriptor>) -> result<_,
// error-code> -- reused as-is for [method]descriptor.sync-data, which has
// the identical WIT signature (see fsSyncFunc for why one Go builder
// implements both).
func wasiSyncSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
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

// wasiRenameAtSig builds the FuncDesc/resolver for
// [method]descriptor.rename-at(self: borrow<descriptor>, old-path: string,
// new-descriptor: borrow<descriptor>, new-path: string) -> result<_,
// error-code>.
func wasiRenameAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	newDescRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "old-path", Type: binary.TypeRef{Primitive: "string"}},
			{Name: "new-descriptor", Type: newDescRef},
			{Name: "new-path", Type: binary.TypeRef{Primitive: "string"}},
		},
		Results: binary.FuncResults{Unnamed: &resultRef},
	}
	return fd, tbl.resolver()
}

// wasiLinkAtSig builds the FuncDesc/resolver for [method]descriptor.link-at(
// self: borrow<descriptor>, old-path-flags: path-flags, old-path: string,
// new-descriptor: borrow<descriptor>, new-path: string) -> result<_,
// error-code>.
func wasiLinkAtSig() (binary.FuncDesc, abi.Resolver) {
	tbl := &typeTable{}
	selfRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	pathFlagsRef := tbl.add(binary.FlagsDesc{Names: []string{"symlink-follow"}})
	newDescRef := tbl.add(binary.BorrowDesc{ResourceType: wasiDescriptorResType})
	errRef := wasiErrorCodeType(tbl)
	resultRef := tbl.add(binary.ResultDesc{Err: &errRef})
	fd := binary.FuncDesc{
		Params: []binary.FuncParam{
			{Name: "self", Type: selfRef},
			{Name: "old-path-flags", Type: pathFlagsRef},
			{Name: "old-path", Type: binary.TypeRef{Primitive: "string"}},
			{Name: "new-descriptor", Type: newDescRef},
			{Name: "new-path", Type: binary.TypeRef{Primitive: "string"}},
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
