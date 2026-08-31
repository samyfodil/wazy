//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris

// Note: This expression is not the same as compiler support, even if it looks
// similar. Platform functions here are used in interpreter mode as well.

package sysfs

import (
	"io/fs"
	"syscall"

	"github.com/samyfodil/wazy/sys"
)

// dirNlinkIncludesDot is true because even though os.File filters out dot
// entries, the underlying syscall.Stat includes them.
//
// Note: this is only used in tests
const dirNlinkIncludesDot = true

func lstat(path string) (sys.Stat_t, sys.Errno) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	}
	return statFromSyscall(&st), 0
}

func stat(path string) (sys.Stat_t, sys.Errno) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	}
	return statFromSyscall(&st), 0
}

func statFile(f fs.File) (sys.Stat_t, sys.Errno) {
	return defaultStatFile(f)
}

// statOSFile stats the already open descriptor instead of the os.File wrapping
// it, which is the same syscall without os.fileStat.
func statOSFile(f *osFile) (sys.Stat_t, sys.Errno) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.fd), &st); err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	}
	return statFromSyscall(&st), 0
}

// statFromSyscall converts a raw stat record, so that a stat costs no more than
// the syscall itself: os.Stat and friends fill a 208 byte os.fileStat on the
// heap that is thrown away right after this same conversion.
func statFromSyscall(d *syscall.Stat_t) sys.Stat_t {
	atim, mtim, ctim := statTimes(d)
	return sys.Stat_t{
		Dev:   uint64(d.Dev),
		Ino:   sys.Inode(d.Ino),
		Mode:  modeFromSyscall(uint32(d.Mode)),
		Nlink: uint64(d.Nlink),
		Size:  int64(d.Size),
		Atim:  atim,
		Mtim:  mtim,
		Ctim:  ctim,
	}
}

// modeFromSyscall converts a raw st_mode to the bits fs.FileInfo.Mode would
// have reported, mirroring os.fillFileStatFromSys.
func modeFromSyscall(mode uint32) fs.FileMode {
	m := fs.FileMode(mode & 0o777)
	switch mode & syscall.S_IFMT {
	case syscall.S_IFBLK:
		m |= fs.ModeDevice
	case syscall.S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case syscall.S_IFDIR:
		m |= fs.ModeDir
	case syscall.S_IFIFO:
		m |= fs.ModeNamedPipe
	case syscall.S_IFLNK:
		m |= fs.ModeSymlink
	case syscall.S_IFSOCK:
		m |= fs.ModeSocket
	}
	if mode&syscall.S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if mode&syscall.S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if mode&syscall.S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}
