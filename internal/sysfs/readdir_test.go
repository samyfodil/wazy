package sysfs

import (
	"io"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/sys"
)

// listedDir is a directory whose entries can fail to stat, like ones removed
// between the listing and the lstat that reads their inode.
type listedDir struct {
	entries []fs.DirEntry
	reads   []int
}

func (d *listedDir) Stat() (fs.FileInfo, error)         { return nil, syscall.ENOSYS }
func (d *listedDir) Read([]byte) (int, error)           { return 0, io.EOF }
func (d *listedDir) Close() error                       { return nil }
func (d *listedDir) Readdir(int) ([]fs.FileInfo, error) { return nil, syscall.ENOSYS }

func (d *listedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	d.reads = append(d.reads, n)
	if n <= 0 || n > len(d.entries) {
		n = len(d.entries)
	}
	ret := d.entries[:n]
	d.entries = d.entries[n:]
	return ret, nil
}

type listedEntry struct {
	name string
	ino  sys.Inode
	err  error
}

func (e listedEntry) Name() string               { return e.name }
func (e listedEntry) IsDir() bool                { return false }
func (e listedEntry) Type() fs.FileMode          { return 0 }
func (e listedEntry) Info() (fs.FileInfo, error) { return e, e.err }
func (e listedEntry) Size() int64                { return 0 }
func (e listedEntry) Mode() fs.FileMode          { return 0 }
func (e listedEntry) ModTime() time.Time         { return time.Time{} }
func (e listedEntry) Sys() any                   { return &sys.Stat_t{Ino: e.ino} }

func TestReaddir_skipsVanishedEntries(t *testing.T) {
	d := &listedDir{entries: []fs.DirEntry{
		listedEntry{name: "a", ino: 1},
		listedEntry{name: "removed", err: syscall.ENOENT},
		listedEntry{name: "closed fd", err: syscall.EBADF},
		listedEntry{name: "d", ino: 4},
		listedEntry{name: "e", ino: 5},
	}}

	dirents, errno := readdir(d, "", 3)
	require.EqualErrno(t, 0, errno)
	require.Equal(t, []sys.Dirent{{Name: "a", Ino: 1}, {Name: "d", Ino: 4}, {Name: "e", Ino: 5}}, dirents)
	require.Equal(t, []int{3, 2}, d.reads, "two entries were skipped, so it reads two more")

	dirents, errno = readdir(d, "", 3)
	require.EqualErrno(t, 0, errno)
	require.Equal(t, 0, len(dirents))
}

func TestReaddir_statError(t *testing.T) {
	d := &listedDir{entries: []fs.DirEntry{
		listedEntry{name: "a", ino: 1},
		listedEntry{name: "b", err: syscall.EACCES},
	}}

	_, errno := readdir(d, "", 0)
	require.EqualErrno(t, sys.EACCES, errno)
}
