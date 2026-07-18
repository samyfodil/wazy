package sysfs

import (
	"encoding/binary"
	"io/fs"
	"path"
	"sync"
	"syscall"
	"unsafe"

	"github.com/samyfodil/wazy/sys"
)

// direntGetdentsSupported is true on Linux: osFile.Readdir uses getdents64
// directly (via readdirGetdents below) instead of os.File.Readdir (FileInfo
// mode).
//
// # Why not os.File.Readdir?
//
// os.File.Readdir(n) returns []fs.FileInfo, which internally calls
// lstat(2) on every single entry name, even though wazy's Dirent only needs
// Name, Ino and Type -- all three of which are already present in the raw
// getdents64 records that os.File parses and discards. For a directory
// with a non-trivial entry count, this turns an O(1)-syscall listing into
// O(n) syscalls (plus a path-concatenation allocation and an os.fileStat
// allocation per entry).
//
// Instead, we read the raw getdents64 buffer ourselves via
// syscall.ReadDirent and parse each record's d_ino, d_type and d_name
// directly. d_type covers the type of the vast majority of entries; the
// rare DT_UNKNOWN case (returned by some filesystems that don't populate
// d_type, e.g. XFS without ftype, some FUSE/network filesystems) falls
// back to lstat for that entry only.
const direntGetdentsSupported = true

// direntBufSize is the size of the reusable buffer used to read raw
// directory entries via getdents64. This mirrors the size Go's os package
// uses internally (os.blockSize), comfortably larger than any single
// directory record (19 header bytes + up to NAME_MAX+1 name bytes).
const direntBufSize = 8192

// direntBufPool pools direntBufSize buffers used to read raw getdents64
// output, the same way os.dirBufPool does for os.File's own internal
// dirInfo buffer. Every osFile representing a directory needs one of these
// while its Readdir is being drained, but since directories are typically
// opened, read and closed in short order, pooling avoids an 8KB
// allocation (and zeroing) per directory File.
var direntBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, direntBufSize)
		return &buf
	},
}

// acquireDirentBuf lazily borrows a buffer from direntBufPool for f, if it
// doesn't already have one.
func (f *osFile) acquireDirentBuf() {
	if f.direntBuf == nil {
		f.direntBuf = *(direntBufPool.Get().(*[]byte))
	}
}

// releaseDirentBuf returns f's borrowed buffer (if any) to direntBufPool.
// Called when f is closed.
func (f *osFile) releaseDirentBuf() {
	if buf := f.direntBuf; buf != nil {
		f.direntBuf = nil
		direntBufPool.Put(&buf)
	}
}

// Offsets of the fields wazy needs within a Linux getdents64 record, i.e.
// `struct linux_dirent64`:
//
//	d_ino    uint64 at offset 0
//	d_off    int64  at offset 8  (unused here)
//	d_reclen uint16 at offset 16
//	d_type   uint8  at offset 18
//	d_name   NUL-terminated string starting at offset 19
//
// These are computed from syscall.Dirent (generated from the kernel's
// linux_dirent64 layout for the target GOARCH) rather than hard-coded, so
// this remains correct on every Linux architecture (verified identical on
// amd64 and arm64).
const (
	direntInoOff    = unsafe.Offsetof(syscall.Dirent{}.Ino)
	direntReclenOff = unsafe.Offsetof(syscall.Dirent{}.Reclen)
	direntTypeOff   = unsafe.Offsetof(syscall.Dirent{}.Type)
	direntNameOff   = unsafe.Offsetof(syscall.Dirent{}.Name)
)

// readdirGetdents implements osFile.Readdir by reading raw directory
// entries via getdents64 (syscall.ReadDirent) directly on f.fd, instead of
// going through os.File.Readdir. See the docs on direntGetdentsSupported
// for rationale.
//
// # Buffering contract
//
// A single getdents64 call can return many more records than the caller
// asked for (as many as fit in direntBufSize). Records parsed but not
// returned by this call are kept on f.bufferedDirents and drained first by
// subsequent calls, so nothing is lost or read twice, matching the
// contract also honored by os.File.Readdir (which buffers unconsumed raw
// bytes internally the same way).
//
// # Errors
//
// A zero Errno is success. Notably, unlike os.File on Linux, this never
// returns io.EOF: like the rest of this package's Readdir implementations,
// running out of entries just yields a shorter (possibly empty) slice.
func (f *osFile) readdirGetdents(n int) (dirents []sys.Dirent, errno sys.Errno) {
	if f.closed {
		// os.File.Readdir (the non-Linux path) returns EBADF after Close via
		// os.File's own closed guard, never touching the raw descriptor. The
		// fast path uses the cached f.fd directly, which after Close is either
		// invalid or -- worse -- already reused by another open file, so guard
		// here to preserve that behavior and avoid reading an unrelated fd.
		// (f.direntsEOF may also be stale-true from a pre-Close drain, which
		// would otherwise short-circuit to a bogus empty success.)
		return nil, sys.EBADF
	}

	readAll := n <= 0

	for readAll || len(dirents) < n {
		if len(f.bufferedDirents) == 0 {
			if f.direntsEOF {
				break
			}
			if errno = f.fillBufferedDirents(); errno != 0 {
				return nil, errno
			}
			// Either we now have entries to drain (handled by the next
			// loop iteration), or the batch only contained "." / ".."
			// (filtered out) without yet reaching EOF: loop back around
			// to either drain or issue another getdents64 call.
			continue
		}

		if readAll || n-len(dirents) >= len(f.bufferedDirents) {
			dirents = append(dirents, f.bufferedDirents...)
			f.bufferedDirents = nil
		} else {
			need := n - len(dirents)
			dirents = append(dirents, f.bufferedDirents[:need:need]...)
			f.bufferedDirents = f.bufferedDirents[need:]
		}
	}
	return dirents, 0
}

// fillBufferedDirents issues a single getdents64 syscall on f.fd and parses
// the result into f.bufferedDirents, growing/reusing f.direntBuf as needed.
// It sets f.direntsEOF once the underlying directory is fully drained (a
// zero-length read).
func (f *osFile) fillBufferedDirents() sys.Errno {
	f.acquireDirentBuf()

	// Note: this uses f.fd directly, the same raw descriptor already cached
	// on osFile (see newOsFile) and used elsewhere in this package (e.g.
	// poll, setNonblock) instead of calling f.file.Fd(), which would
	// (redundantly) force the descriptor back into blocking mode.
	n, err := syscall.ReadDirent(int(f.fd), f.direntBuf)
	if err != nil {
		return sys.UnwrapOSError(err)
	}
	if n <= 0 {
		f.direntsEOF = true
		return 0
	}

	dirents, errno := parseDirents(f.direntBuf[:n], f.path, f.bufferedDirents[:0])
	if errno != 0 {
		return errno
	}
	f.bufferedDirents = dirents
	return 0
}

// parseDirents parses a buffer of raw getdents64 records (as returned by
// syscall.ReadDirent) into dst, skipping "." and ".." the same way
// os.File.Readdir does (those are synthesized separately by
// internal/sys.DirentCache). dirPath is the directory's real path, used
// only to lstat entries whose d_type is DT_UNKNOWN.
func parseDirents(buf []byte, dirPath string, dst []sys.Dirent) ([]sys.Dirent, sys.Errno) {
	for len(buf) > 0 {
		if uintptr(len(buf)) < direntNameOff {
			break // truncated record: shouldn't happen, but don't panic.
		}

		reclen := binary.NativeEndian.Uint16(buf[direntReclenOff:])
		if reclen == 0 || uintptr(reclen) > uintptr(len(buf)) || uintptr(reclen) < direntNameOff {
			break // truncated or corrupt record: be defensive and stop.
		}

		rec := buf[:reclen]
		buf = buf[reclen:]

		ino := binary.NativeEndian.Uint64(rec[direntInoOff:])
		dtype := rec[direntTypeOff]

		nameBytes := rec[direntNameOff:]
		if i := indexNUL(nameBytes); i >= 0 {
			nameBytes = nameBytes[:i]
		}

		// Check for useless names before allocating a string, matching
		// os.File.Readdir's behavior of never returning dot entries: the
		// wasi_snapshot_preview1 layer (internal/sys.DirentCache) is what
		// synthesizes "." and "..".
		if isDotOrDotDot(nameBytes) {
			continue
		}
		name := string(nameBytes)

		typ, ok := direntTypeToFileMode(dtype)
		if !ok {
			// DT_UNKNOWN (or any other value the kernel might use, e.g.
			// DT_WHT): some filesystems don't populate d_type at all (old
			// XFS without ftype, some FUSE/network filesystems). Fall back
			// to lstat for this entry only, mirroring what os.File.Readdir
			// does via its own direntType/newUnixDirent lazy-stat path.
			st, lerrno := lstat(path.Join(dirPath, name))
			if lerrno != 0 {
				if lerrno == sys.ENOENT {
					// The file disappeared between getdents64 and lstat;
					// skip it, the same way os.File.Readdir does.
					continue
				}
				return dst, lerrno
			}
			typ = st.Mode.Type()
		}

		dst = append(dst, sys.Dirent{Name: name, Ino: sys.Inode(ino), Type: typ})
	}
	return dst, 0
}

// isDotOrDotDot returns true if name is "." or "..".
func isDotOrDotDot(name []byte) bool {
	if len(name) == 0 || name[0] != '.' {
		return false
	}
	return len(name) == 1 || (len(name) == 2 && name[1] == '.')
}

// indexNUL returns the index of the first NUL byte in b, or -1 if absent.
func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// direntTypeToFileMode converts a getdents64 d_type value (the DT_*
// constants in the syscall package) to the fs.FileMode bits used by
// sys.Dirent.Type. This mirrors the mapping the Go runtime
// uses internally to implement os.DirEntry.Type() (see os.direntType in
// $GOROOT/src/os/dirent_linux.go), which is also what the pre-existing
// FileInfo-based path here ends up producing via info.Mode().Type().
//
// The second result is false for DT_UNKNOWN and any other value the switch
// doesn't recognize (e.g. DT_WHT), meaning the caller must fall back to
// lstat for this entry.
func direntTypeToFileMode(dtype byte) (fs.FileMode, bool) {
	switch dtype {
	case syscall.DT_BLK:
		return fs.ModeDevice, true
	case syscall.DT_CHR:
		return fs.ModeDevice | fs.ModeCharDevice, true
	case syscall.DT_DIR:
		return fs.ModeDir, true
	case syscall.DT_FIFO:
		return fs.ModeNamedPipe, true
	case syscall.DT_LNK:
		return fs.ModeSymlink, true
	case syscall.DT_REG:
		return 0, true
	case syscall.DT_SOCK:
		return fs.ModeSocket, true
	default:
		return 0, false
	}
}
