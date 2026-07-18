package sysfs

import (
	"io/fs"
	"os"
	"path"

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
type dirFS struct {
	sys.UnimplementedFS

	dir string
	// cleanedDir is for easier OS-specific concatenation, as it always has
	// a trailing path separator.
	cleanedDir string
}

// String implements fmt.Stringer
func (d *dirFS) String() string {
	return d.dir
}

// OpenFile implements the same method as documented on sys.FS
func (d *dirFS) OpenFile(path string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	return OpenOSFile(d.join(path), flag, perm)
}

// Lstat implements the same method as documented on sys.FS
func (d *dirFS) Lstat(path string) (sys.Stat_t, sys.Errno) {
	return lstat(d.join(path))
}

// Stat implements the same method as documented on sys.FS
func (d *dirFS) Stat(path string) (sys.Stat_t, sys.Errno) {
	return stat(d.join(path))
}

// Mkdir implements the same method as documented on sys.FS
func (d *dirFS) Mkdir(path string, perm fs.FileMode) (errno sys.Errno) {
	err := os.Mkdir(d.join(path), perm)
	if errno = sys.UnwrapOSError(err); errno == sys.ENOTDIR {
		errno = sys.ENOENT
	}
	return
}

// Chmod implements the same method as documented on sys.FS
func (d *dirFS) Chmod(path string, perm fs.FileMode) sys.Errno {
	err := os.Chmod(d.join(path), perm)
	return sys.UnwrapOSError(err)
}

// Rename implements the same method as documented on sys.FS
func (d *dirFS) Rename(from, to string) sys.Errno {
	from, to = d.join(from), d.join(to)
	return rename(from, to)
}

// Rmdir implements the same method as documented on sys.FS
func (d *dirFS) Rmdir(path string) sys.Errno {
	return rmdir(d.join(path))
}

// Unlink implements the same method as documented on sys.FS
func (d *dirFS) Unlink(path string) (err sys.Errno) {
	return unlink(d.join(path))
}

// Link implements the same method as documented on sys.FS
func (d *dirFS) Link(oldName, newName string) sys.Errno {
	err := os.Link(d.join(oldName), d.join(newName))
	return sys.UnwrapOSError(err)
}

// Symlink implements the same method as documented on sys.FS
func (d *dirFS) Symlink(oldName, link string) sys.Errno {
	// Creating a symlink with an absolute path string fails with a "not permitted" error.
	// https://github.com/WebAssembly/wasi-filesystem/blob/v0.2.0/path-resolution.md#symlinks
	if path.IsAbs(oldName) {
		return sys.EPERM
	}
	// Note: do not resolve `oldName` relative to this dirFS. The link result is always resolved
	// when dereference the `link` on its usage (e.g. readlink, read, etc).
	// https://github.com/bytecodealliance/cap-std/blob/v1.0.4/cap-std/src/fs/dir.rs#L404-L409
	err := os.Symlink(oldName, d.join(link))
	return sys.UnwrapOSError(err)
}

// Readlink implements the same method as documented on sys.FS
func (d *dirFS) Readlink(path string) (string, sys.Errno) {
	// Note: do not use syscall.Readlink as that causes race on Windows.
	// In any case, syscall.Readlink does almost the same logic as os.Readlink.
	dst, err := os.Readlink(d.join(path))
	if err != nil {
		return "", sys.UnwrapOSError(err)
	}
	return platform.ToPosixPath(dst), 0
}

// Utimens implements the same method as documented on sys.FS
func (d *dirFS) Utimens(path string, atim, mtim int64) sys.Errno {
	return utimens(d.join(path), atim, mtim)
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
	// TODO: Enforce similar to safefilepath.FromFS(path), but be careful as
	// relative path inputs are allowed. e.g. dir or path == ../
	return d.cleanedDir + path
}
