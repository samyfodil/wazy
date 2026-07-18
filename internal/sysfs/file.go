package sysfs

import (
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/samyfodil/wazy/sys"
)

// fdPoller is implemented by files that can expose a raw descriptor for
// batched polling. osFile and the stdio/fs wrappers around it implement it.
// Defined here (untagged) so non-poll platforms still compile — only the batch
// poll implementation in poll_batch.go is platform-gated.
type fdPoller interface {
	pollFd() (fd uintptr, ok bool)
}

func NewStdioFile(stdin bool, f fs.File) (sys.File, error) {
	// Return constant stat, which has fake times, but keep the underlying
	// file mode. Fake times are needed to pass wasi-testsuite.
	// https://github.com/WebAssembly/wasi-testsuite/blob/af57727/tests/rust/src/bin/fd_filestat_get.rs#L1-L19
	var mode fs.FileMode
	if st, err := f.Stat(); err != nil {
		return nil, err
	} else {
		mode = st.Mode()
	}
	var flag sys.Oflag
	if stdin {
		flag = sys.O_RDONLY
	} else {
		flag = sys.O_WRONLY
	}
	var file sys.File
	if of, ok := f.(*os.File); ok {
		// This is ok because functions that need path aren't used by stdioFile
		file = newOsFile("", flag, 0, of)
	} else {
		file = &fsFile{file: f}
	}
	return &stdioFile{File: file, st: sys.Stat_t{Mode: mode, Nlink: 1}}, nil
}

func OpenFile(path string, flag sys.Oflag, perm fs.FileMode) (*os.File, sys.Errno) {
	return openFile(path, flag, perm)
}

func OpenOSFile(path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	f, errno := OpenFile(path, flag, perm)
	if errno != 0 {
		return nil, errno
	}
	return newOsFile(path, flag, perm, f), 0
}

func OpenFSFile(fs fs.FS, path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	if flag&sys.O_DIRECTORY != 0 && flag&(sys.O_WRONLY|sys.O_RDWR) != 0 {
		return nil, sys.EISDIR // invalid to open a directory writeable
	}
	f, err := fs.Open(path)
	if errno := sys.UnwrapOSError(err); errno != 0 {
		return nil, errno
	}
	// Don't return an os.File because the path is not absolute. osFile needs
	// the path to be real and certain FS.File impls are subrooted.
	return &fsFile{fs: fs, name: path, file: f}, 0
}

type stdioFile struct {
	sys.File
	st sys.Stat_t
}

// IsNonblock implements sys.PollableFile by forwarding to the
// underlying file if it supports it.
func (f *stdioFile) IsNonblock() bool {
	if pf, ok := f.File.(sys.PollableFile); ok {
		return pf.IsNonblock()
	}
	return false
}

// SetNonblock implements sys.PollableFile by forwarding to the
// underlying file if it supports it.
func (f *stdioFile) SetNonblock(enable bool) sys.Errno {
	if pf, ok := f.File.(sys.PollableFile); ok {
		return pf.SetNonblock(enable)
	}
	return sys.ENOSYS
}

// Poll implements sys.Pollable by forwarding to the underlying file
// if it supports polling.
func (f *stdioFile) Poll(flag sys.Pflag, timeoutMillis int32) (ready bool, errno sys.Errno) {
	if p, ok := f.File.(sys.Pollable); ok {
		return p.Poll(flag, timeoutMillis)
	}
	return false, sys.ENOSYS
}

// pollFd forwards fdPoller to the underlying file for batched polling (W3).
func (f *stdioFile) pollFd() (uintptr, bool) {
	if p, ok := f.File.(fdPoller); ok {
		return p.pollFd()
	}
	return 0, false
}

// SetAppend implements File.SetAppend
func (f *stdioFile) SetAppend(bool) sys.Errno {
	// Ignore for stdio.
	return 0
}

// IsAppend implements File.SetAppend
func (f *stdioFile) IsAppend() bool {
	return true
}

// Stat implements File.Stat
func (f *stdioFile) Stat() (sys.Stat_t, sys.Errno) {
	return f.st, 0
}

// Close implements File.Close
func (f *stdioFile) Close() sys.Errno {
	return 0
}

// fsFile is used for wrapped fs.File, like os.Stdin or any fs.File
// implementation. Notably, this does not have access to the full file path.
// so certain operations can't be supported, such as inode lookups on Windows.
type fsFile struct {
	sys.UnimplementedFile

	// fs is the file-system that opened the file, or nil when wrapped for
	// pre-opens like stdio.
	fs fs.FS

	// name is what was used in fs for Open, so it may not be the actual path.
	name string

	// file is always set, possibly an os.File like os.Stdin.
	file fs.File

	// reopenDir is true if reopen should be called before Readdir. This flag
	// is deferred until Readdir to prevent redundant rewinds. This could
	// happen if Seek(0) was called twice, or if in Windows, Seek(0) was called
	// before Readdir.
	reopenDir bool

	// closed is true when closed was called. This ensures proper sys.EBADF
	closed bool

	// cachedStat includes fields that won't change while a file is open.
	cachedSt cachedStat
}

type cachedStat struct {
	// dev is the same as sys.Stat_t Dev.
	dev uint64

	// dev is the same as sys.Stat_t Ino.
	ino sys.Inode

	// isDir is sys.Stat_t Mode masked with fs.ModeDir
	isDir bool

	// valid is set once the cacheable fields above are populated. Stored by
	// value (not behind a pointer) so a Stat call doesn't allocate (W5).
	valid bool
}

// cachedStat returns the cacheable parts of sys.Stat_t or an error if they
// couldn't be retrieved.
func (f *fsFile) cachedStat() (dev uint64, ino sys.Inode, isDir bool, errno sys.Errno) {
	if !f.cachedSt.valid {
		if _, errno = f.Stat(); errno != 0 {
			return
		}
	}
	return f.cachedSt.dev, f.cachedSt.ino, f.cachedSt.isDir, 0
}

// Dev implements the same method as documented on sys.File
func (f *fsFile) Dev() (uint64, sys.Errno) {
	dev, _, _, errno := f.cachedStat()
	return dev, errno
}

// Ino implements the same method as documented on sys.File
func (f *fsFile) Ino() (sys.Inode, sys.Errno) {
	_, ino, _, errno := f.cachedStat()
	return ino, errno
}

// IsDir implements the same method as documented on sys.File
func (f *fsFile) IsDir() (bool, sys.Errno) {
	_, _, isDir, errno := f.cachedStat()
	return isDir, errno
}

// IsAppend implements the same method as documented on sys.File
func (f *fsFile) IsAppend() bool {
	return false
}

// SetAppend implements the same method as documented on sys.File
func (f *fsFile) SetAppend(bool) (errno sys.Errno) {
	return fileError(f, f.closed, sys.ENOSYS)
}

// Stat implements the same method as documented on sys.File
func (f *fsFile) Stat() (sys.Stat_t, sys.Errno) {
	if f.closed {
		return sys.Stat_t{}, sys.EBADF
	}

	st, errno := statFile(f.file)
	switch errno {
	case 0:
		f.cachedSt = cachedStat{dev: st.Dev, ino: st.Ino, isDir: st.Mode&fs.ModeDir == fs.ModeDir, valid: true}
	case sys.EIO:
		errno = sys.EBADF
	}
	return st, errno
}

// Read implements the same method as documented on sys.File
func (f *fsFile) Read(buf []byte) (n int, errno sys.Errno) {
	if n, errno = read(f.file, buf); errno != 0 {
		// Defer validation overhead until we've already had an error.
		errno = fileError(f, f.closed, errno)
	}
	return
}

// Pread implements the same method as documented on sys.File
func (f *fsFile) Pread(buf []byte, off int64) (n int, errno sys.Errno) {
	if ra, ok := f.file.(io.ReaderAt); ok {
		if n, errno = pread(ra, buf, off); errno != 0 {
			// Defer validation overhead until we've already had an error.
			errno = fileError(f, f.closed, errno)
		}
		return
	}

	// See /RATIONALE.md "fd_pread: io.Seeker fallback when io.ReaderAt is not supported"
	if rs, ok := f.file.(io.ReadSeeker); ok {
		// Determine the current position in the file, as we need to revert it.
		currentOffset, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, fileError(f, f.closed, sys.UnwrapOSError(err))
		}

		// Put the read position back when complete.
		defer func() { _, _ = rs.Seek(currentOffset, io.SeekStart) }()

		// If the current offset isn't in sync with this reader, move it.
		if off != currentOffset {
			if _, err = rs.Seek(off, io.SeekStart); err != nil {
				return 0, fileError(f, f.closed, sys.UnwrapOSError(err))
			}
		}

		n, err = rs.Read(buf)
		if errno = sys.UnwrapOSError(err); errno != 0 {
			// Defer validation overhead until we've already had an error.
			errno = fileError(f, f.closed, errno)
		}
	} else {
		errno = sys.ENOSYS // unsupported
	}
	return
}

// Seek implements the same method as documented on sys.File
func (f *fsFile) Seek(offset int64, whence int) (newOffset int64, errno sys.Errno) {
	// If this is a directory, and we're attempting to seek to position zero,
	// we have to re-open the file to ensure the directory state is reset.
	var isDir bool
	if offset == 0 && whence == io.SeekStart {
		if isDir, errno = f.IsDir(); errno == 0 && isDir {
			f.reopenDir = true
			return
		}
	}

	if s, ok := f.file.(io.Seeker); ok {
		if newOffset, errno = seek(s, offset, whence); errno != 0 {
			// Defer validation overhead until we've already had an error.
			errno = fileError(f, f.closed, errno)
		}
	} else {
		errno = sys.ENOSYS // unsupported
	}
	return
}

// Readdir implements the same method as documented on sys.File
//
// Notably, this uses readdirFile or fs.ReadDirFile if available. This does not
// return inodes on windows.
func (f *fsFile) Readdir(n int) (dirents []sys.Dirent, errno sys.Errno) {
	// Windows lets you Readdir after close, FS.File also may not implement
	// close in a meaningful way. read our closed field to return consistent
	// results.
	if f.closed {
		errno = sys.EBADF
		return
	}

	if f.reopenDir { // re-open the directory if needed.
		f.reopenDir = false
		if errno = adjustReaddirErr(f, f.closed, f.rewindDir()); errno != 0 {
			return
		}
	}

	if of, ok := f.file.(readdirFile); ok {
		// We can't use f.name here because it is the path up to the sys.FS,
		// not necessarily the real path. For this reason, Windows may not be
		// able to populate inodes. However, Darwin and Linux will.
		if dirents, errno = readdir(of, "", n); errno != 0 {
			errno = adjustReaddirErr(f, f.closed, errno)
		}
		return
	}

	// Try with FS.ReadDirFile which is available on api.FS implementations
	// like embed:FS.
	if rdf, ok := f.file.(fs.ReadDirFile); ok {
		entries, e := rdf.ReadDir(n)
		if errno = adjustReaddirErr(f, f.closed, e); errno != 0 {
			return
		}
		dirents = make([]sys.Dirent, 0, len(entries))
		for _, e := range entries {
			// By default, we don't attempt to read inode data
			dirents = append(dirents, sys.Dirent{Name: e.Name(), Type: e.Type()})
		}
	} else {
		errno = sys.EBADF // not a directory
	}
	return
}

// Write implements the same method as documented on sys.File.
func (f *fsFile) Write(buf []byte) (n int, errno sys.Errno) {
	if w, ok := f.file.(io.Writer); ok {
		if n, errno = write(w, buf); errno != 0 {
			// Defer validation overhead until we've already had an error.
			errno = fileError(f, f.closed, errno)
		}
	} else {
		errno = sys.ENOSYS // unsupported
	}
	return
}

// Pwrite implements the same method as documented on sys.File.
func (f *fsFile) Pwrite(buf []byte, off int64) (n int, errno sys.Errno) {
	if wa, ok := f.file.(io.WriterAt); ok {
		if n, errno = pwrite(wa, buf, off); errno != 0 {
			// Defer validation overhead until we've already had an error.
			errno = fileError(f, f.closed, errno)
		}
	} else {
		errno = sys.ENOSYS // unsupported
	}
	return
}

// Close implements the same method as documented on sys.File.
func (f *fsFile) Close() sys.Errno {
	if f.closed {
		return 0
	}
	f.closed = true
	return f.close()
}

func (f *fsFile) close() sys.Errno {
	return sys.UnwrapOSError(f.file.Close())
}

// nonblocker is a subset of PollableFile for checking non-blocking mode on
// an fs.File. fs.File cannot implement PollableFile due to conflicting Close
// signatures (error vs Errno), so we use this narrower interface.
type nonblocker interface {
	IsNonblock() bool
	SetNonblock(enable bool) sys.Errno
}

// IsNonblock implements sys.PollableFile by forwarding to the
// underlying fs.File if it supports it.
func (f *fsFile) IsNonblock() bool {
	if nb, ok := f.file.(nonblocker); ok {
		return nb.IsNonblock()
	}
	return false
}

// SetNonblock implements sys.PollableFile by forwarding to the
// underlying fs.File if it supports it.
func (f *fsFile) SetNonblock(enable bool) sys.Errno {
	if nb, ok := f.file.(nonblocker); ok {
		return nb.SetNonblock(enable)
	}
	if !enable {
		return 0 // disabling nonblock on a file that doesn't support it is a no-op
	}
	return sys.ENOSYS
}

// Poll implements sys.Pollable by forwarding to the underlying
// fs.File if it supports polling.
//
// Note: fsFile cannot implement PollableFile because fs.File and
// sys.File have conflicting Close signatures (error vs Errno),
// so no type can satisfy both. Pollable has no such conflict.
func (f *fsFile) Poll(flag sys.Pflag, timeoutMillis int32) (ready bool, errno sys.Errno) {
	if p, ok := f.file.(sys.Pollable); ok {
		return p.Poll(flag, timeoutMillis)
	}
	return false, sys.ENOSYS
}

// pollFd forwards fdPoller to the underlying file for batched polling (W3).
func (f *fsFile) pollFd() (uintptr, bool) {
	if p, ok := f.file.(fdPoller); ok {
		return p.pollFd()
	}
	return 0, false
}

// dirError is used for commands that work against a directory, but not a file.
func dirError(f sys.File, isClosed bool, errno sys.Errno) sys.Errno {
	if vErrno := validate(f, isClosed, false, true); vErrno != 0 {
		return vErrno
	}
	return errno
}

// fileError is used for commands that work against a file, but not a directory.
func fileError(f sys.File, isClosed bool, errno sys.Errno) sys.Errno {
	if vErrno := validate(f, isClosed, true, false); vErrno != 0 {
		return vErrno
	}
	return errno
}

// validate is used to making syscalls which will fail.
func validate(f sys.File, isClosed, wantFile, wantDir bool) sys.Errno {
	if isClosed {
		return sys.EBADF
	}

	isDir, errno := f.IsDir()
	if errno != 0 {
		return errno
	}

	if wantFile && isDir {
		return sys.EISDIR
	} else if wantDir && !isDir {
		return sys.ENOTDIR
	}
	return 0
}

func read(r io.Reader, buf []byte) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // less overhead on zero-length reads.
	}

	n, err := r.Read(buf)
	return n, sys.UnwrapOSError(err)
}

func pread(ra io.ReaderAt, buf []byte, off int64) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // less overhead on zero-length reads.
	}

	n, err := ra.ReadAt(buf, off)
	return n, sys.UnwrapOSError(err)
}

func seek(s io.Seeker, offset int64, whence int) (int64, sys.Errno) {
	if uint(whence) > io.SeekEnd {
		return 0, sys.EINVAL // negative or exceeds the largest valid whence
	}

	newOffset, err := s.Seek(offset, whence)
	return newOffset, sys.UnwrapOSError(err)
}

func (f *fsFile) rewindDir() sys.Errno {
	// Reopen the directory to rewind it.
	file, err := f.fs.Open(f.name)
	if err != nil {
		return sys.UnwrapOSError(err)
	}
	fi, err := file.Stat()
	if err != nil {
		return sys.UnwrapOSError(err)
	}
	// Can't check if it's still the same file,
	// but is it still a directory, at least?
	if !fi.IsDir() {
		return sys.ENOTDIR
	}
	// Only update f on success.
	_ = f.file.Close()
	f.file = file
	return 0
}

// readdirFile allows masking the `Readdir` function on os.File.
type readdirFile interface {
	Readdir(n int) ([]fs.FileInfo, error)
}

// readdir uses readdirFile.Readdir, special casing windows when path !="".
func readdir(f readdirFile, path string, n int) (dirents []sys.Dirent, errno sys.Errno) {
	fis, e := f.Readdir(n)
	if errno = sys.UnwrapOSError(e); errno != 0 {
		return
	}

	dirents = make([]sys.Dirent, 0, len(fis))

	// linux/darwin won't have to fan out to lstat, but windows will.
	var ino sys.Inode
	for fi := range fis {
		t := fis[fi]
		// inoFromFileInfo is more efficient than sys.NewStat_t, as it gets the
		// inode without allocating an instance and filling other fields.
		if ino, errno = inoFromFileInfo(path, t); errno != 0 {
			return
		}
		dirents = append(dirents, sys.Dirent{Name: t.Name(), Ino: ino, Type: t.Mode().Type()})
	}
	return
}

func write(w io.Writer, buf []byte) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // less overhead on zero-length writes.
	}

	n, err := w.Write(buf)
	return n, sys.UnwrapOSError(err)
}

func pwrite(w io.WriterAt, buf []byte, off int64) (n int, errno sys.Errno) {
	if len(buf) == 0 {
		return 0, 0 // less overhead on zero-length writes.
	}

	n, err := w.WriteAt(buf, off)
	return n, sys.UnwrapOSError(err)
}

func chtimes(path string, atim, mtim int64) (errno sys.Errno) { //nolint:unused
	// When both inputs are omitted, there is nothing to change.
	if atim == sys.UTIME_OMIT && mtim == sys.UTIME_OMIT {
		return
	}

	// UTIME_OMIT is expensive until progress is made in Go, as it requires a
	// stat to read-back the value to re-apply.
	// - https://github.com/golang/go/issues/32558.
	// - https://go-review.googlesource.com/c/go/+/219638 (unmerged)
	var st sys.Stat_t
	if atim == sys.UTIME_OMIT || mtim == sys.UTIME_OMIT {
		if st, errno = stat(path); errno != 0 {
			return
		}
	}

	var atime, mtime time.Time
	if atim == sys.UTIME_OMIT {
		atime = epochNanosToTime(st.Atim)
		mtime = epochNanosToTime(mtim)
	} else if mtim == sys.UTIME_OMIT {
		atime = epochNanosToTime(atim)
		mtime = epochNanosToTime(st.Mtim)
	} else {
		atime = epochNanosToTime(atim)
		mtime = epochNanosToTime(mtim)
	}
	return sys.UnwrapOSError(os.Chtimes(path, atime, mtime))
}

func epochNanosToTime(epochNanos int64) time.Time { //nolint:unused
	seconds := epochNanos / 1e9
	nanos := epochNanos % 1e9
	return time.Unix(seconds, nanos)
}
