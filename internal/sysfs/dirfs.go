package sysfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/sys"
)

func DirFS(dir string) sys.FS {
	return &dirFS{
		dir:        dir,
		cleanedDir: ensureTrailingPathSeparator(dir),
	}
}

func ensureTrailingPathSeparator(dir string) string {
	if !os.IsPathSeparator(dir[len(dir)-1]) {
		return dir + string(os.PathSeparator)
	}
	return dir
}

// dirFS is not exported because the input fields must be maintained together.
// This is likely why os.DirFS doesn't, either!
//
// Every path resolves through an os.Root opened on dir, so neither ".." nor a
// symlink, not even one the guest made itself, leads outside dir.
type dirFS struct {
	sys.UnimplementedFS

	dir string
	// cleanedDir is for easier OS-specific concatenation, as it always has
	// a trailing path separator.
	cleanedDir string

	// bound is set on the copy BindFS makes for one module instance, which
	// opens dir on first use and keeps it open until Close, as a WASI runtime
	// opens its preopens when it instantiates. Unbound, every operation opens
	// dir afresh, which costs more than most operations do.
	bound  bool
	rootMu sync.Mutex
	cached atomic.Pointer[os.Root]
	// rootDir is the directory cached is rooted at, open for openat2 where
	// the platform has it.
	rootDir atomic.Pointer[os.File]
}

// BindFS returns fs as one module instance uses it, and what releases it when
// the instance is done, or nil when nothing needs to. A DirFS, bare or behind
// a ReadFS, gets its own handle on its directory.
func BindFS(fs sys.FS) (sys.FS, io.Closer) {
	switch fs := fs.(type) {
	case *dirFS:
		bound := &dirFS{dir: fs.dir, cleanedDir: fs.cleanedDir, bound: true}
		return bound, bound
	case *ReadFS:
		if inner, closer := BindFS(fs.FS); closer != nil {
			return &ReadFS{FS: inner}, closer
		}
	}
	return fs, nil
}

// Close releases the directory a bound dirFS holds open.
func (d *dirFS) Close() error {
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	if f := d.rootDir.Swap(nil); f != nil {
		f.Close()
	}
	if r := d.cached.Swap(nil); r != nil {
		return r.Close()
	}
	return nil
}

// String implements fmt.Stringer
func (d *dirFS) String() string {
	return d.dir
}

// root returns the root for one operation, which done releases.
func (d *dirFS) root() (*os.Root, sys.Errno) {
	if !d.bound {
		r, err := os.OpenRoot(d.dir)
		return r, rootErrno(err)
	}
	if r := d.cached.Load(); r != nil {
		return r, 0
	}
	d.rootMu.Lock()
	defer d.rootMu.Unlock()
	if r := d.cached.Load(); r != nil {
		return r, 0
	}
	r, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, rootErrno(err)
	}
	d.rootDir.Store(openRootDir(r))
	d.cached.Store(r)
	return r, 0
}

func (d *dirFS) done(r *os.Root) {
	if !d.bound {
		r.Close()
	}
}

// rootName is path as a name inside the root. The WASI layers clean paths
// before they get here, but may leave a leading or trailing slash.
func rootName(path string) string {
	if path = strings.TrimLeft(path, "/"); path == "" {
		return "."
	}
	return path
}

// open returns the root for one operation, which done releases, and path as
// a name in it.
func (d *dirFS) open(path string) (*os.Root, string, sys.Errno) {
	name := rootName(path)
	if errno := hostName(name); errno != 0 {
		return nil, "", errno
	}
	r, errno := d.root()
	return r, name, errno
}

// lastElem is the last element of name. A "." or ".." there names a
// directory the path already reached rather than an entry in one, which
// Windows acts on where POSIX refuses, so dirFS refuses first.
func lastElem(name string) string {
	return path.Base(name)
}

func isDotElem(name string) bool {
	last := lastElem(name)
	return last == "." || last == ".."
}

// rootErrno is the errno for an error from os.Root, which reports some
// conditions with errors of its own rather than an errno. A path that resolves
// outside the root, through ".." or a symlink, is refused as the WASI layers
// refuse one that does so lexically.
func rootErrno(err error) sys.Errno {
	errno := sys.UnwrapOSError(err)
	if errno != sys.EIO {
		return errno
	}
	if inner := errors.Unwrap(err); inner != nil {
		switch inner.Error() {
		case "path escapes from parent":
			return sys.EPERM
		case "not a directory":
			return sys.ENOTDIR
		}
	}
	return errno
}

// OpenFile implements the same method as documented on sys.FS
func (d *dirFS) OpenFile(path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return nil, errno
	}
	defer d.done(r)
	f, errno, ok := openFileBeneath(d.rootDir.Load(), name, flag, perm)
	if !ok {
		f, errno = openFileIn(r, name, flag, perm)
	}
	if errno != 0 {
		return nil, errno
	}
	file := newOsFile(d.join(path), flag, perm, f)
	file.(*osFile).underRoot = d.bound
	return file, 0
}

// Lstat implements the same method as documented on sys.FS
func (d *dirFS) Lstat(path string) (sys.Stat_t, sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return sys.Stat_t{}, errno
	}
	defer d.done(r)
	if st, errno, ok := statBeneath(d.rootDir.Load(), name, false); ok {
		return st, errno
	}
	return statIn(r, name, false)
}

// Stat implements the same method as documented on sys.FS
func (d *dirFS) Stat(path string) (sys.Stat_t, sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return sys.Stat_t{}, errno
	}
	defer d.done(r)
	if st, errno, ok := statBeneath(d.rootDir.Load(), name, true); ok {
		return st, errno
	}
	return statIn(r, name, true)
}

// Mkdir implements the same method as documented on sys.FS
func (d *dirFS) Mkdir(path string, perm fs.FileMode) (errno sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	if errno = rootErrno(r.Mkdir(name, perm&fs.ModePerm)); errno == sys.ENOTDIR {
		errno = sys.ENOENT
	}
	return
}

// Chmod implements the same method as documented on sys.FS
func (d *dirFS) Chmod(path string, perm fs.FileMode) sys.Errno {
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	return rootErrno(r.Chmod(name, perm))
}

// Rename implements the same method as documented on sys.FS
func (d *dirFS) Rename(from, to string) sys.Errno {
	to = rootName(to)
	if errno := hostName(to); errno != 0 {
		return errno
	}
	r, from, errno := d.open(from)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	if isDotElem(from) || isDotElem(to) {
		return sys.EINVAL // as POSIX has it, where Linux reports EBUSY
	}
	return renameIn(r, from, to)
}

// Rmdir implements the same method as documented on sys.FS
func (d *dirFS) Rmdir(path string) sys.Errno {
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	switch lastElem(name) {
	case ".":
		return sys.EINVAL
	case "..":
		return sys.ENOTEMPTY
	}
	return rmdirIn(r, name)
}

// Unlink implements the same method as documented on sys.FS
func (d *dirFS) Unlink(path string) (err sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	if isDotElem(name) {
		return sys.EISDIR
	}
	return unlinkIn(r, name)
}

// Link implements the same method as documented on sys.FS
func (d *dirFS) Link(oldName, newName string) sys.Errno {
	newName = rootName(newName)
	if errno := hostName(newName); errno != 0 {
		return errno
	}
	r, oldName, errno := d.open(oldName)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	// A directory cannot be hard linked, which Windows reports as access
	// denied.
	if info, err := r.Lstat(oldName); err == nil && info.IsDir() {
		return sys.EPERM
	}
	// A new name ending in a slash names a directory, which a link never
	// makes, and Windows drops the slash.
	if strings.HasSuffix(newName, "/") {
		if _, err := r.Lstat(newName); err != nil {
			return rootErrno(err)
		}
		return sys.EEXIST
	}
	errno = rootErrno(r.Link(oldName, newName))
	if errno == sys.EINVAL && newName == "." {
		errno = sys.EEXIST // the directory itself, which Windows reports as EINVAL
	}
	return errno
}

// Symlink implements the same method as documented on sys.FS
func (d *dirFS) Symlink(oldName, link string) sys.Errno {
	// Creating a symlink with an absolute path string fails with a "not permitted" error.
	// https://github.com/WebAssembly/wasi-filesystem/blob/v0.2.0/path-resolution.md#symlinks
	if path.IsAbs(oldName) {
		return sys.EPERM
	}
	r, link, errno := d.open(link)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	// Note: do not resolve `oldName` relative to this dirFS. The link result is always resolved
	// when dereference the `link` on its usage (e.g. readlink, read, etc).
	// https://github.com/bytecodealliance/cap-std/blob/v1.0.4/cap-std/src/fs/dir.rs#L404-L409
	// Windows reports an existing directory written with a trailing slash as
	// EISDIR, which symlink never is on POSIX.
	if errno = rootErrno(r.Symlink(oldName, link)); errno == sys.EISDIR {
		errno = sys.EEXIST
	}
	return errno
}

// Readlink implements the same method as documented on sys.FS
func (d *dirFS) Readlink(path string) (string, sys.Errno) {
	r, name, errno := d.open(path)
	if errno != 0 {
		return "", errno
	}
	defer d.done(r)
	dst, err := r.Readlink(name)
	if err != nil {
		return "", rootErrno(err)
	}
	return platform.ToPosixPath(dst), 0
}

// Utimens implements the same method as documented on sys.FS
func (d *dirFS) Utimens(path string, atim, mtim int64) sys.Errno {
	if atim == sys.UTIME_OMIT && mtim == sys.UTIME_OMIT {
		return 0
	}
	r, name, errno := d.open(path)
	if errno != 0 {
		return errno
	}
	defer d.done(r)
	return rootErrno(r.Chtimes(name, omittableTime(atim), omittableTime(mtim)))
}

// omittableTime is the time os.Root.Chtimes takes for nanos: the zero time,
// which leaves a timestamp as it is, for UTIME_OMIT.
func omittableTime(nanos int64) time.Time {
	if nanos == sys.UTIME_OMIT {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

func (d *dirFS) join(path string) string {
	switch path {
	case "", ".", "/":
		if d.cleanedDir == "/" {
			return "/"
		}
		// cleanedDir includes an unnecessary delimiter for the root path.
		return d.cleanedDir[:len(d.cleanedDir)-1]
	}
	return d.cleanedDir + path
}
