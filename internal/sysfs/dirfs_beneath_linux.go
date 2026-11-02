//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package sysfs

import (
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/samyfodil/wazy/sys"
)

// os.Root resolves a path one directory at a time, opening and closing each
// directory on the way. openat2 with RESOLVE_BENEATH has the kernel do the
// same resolution, refusing any step outside the root, in a single call, and
// honor O_NOFOLLOW itself. A name in the root itself needs no walk, and is
// stat'ed with one fstatat that follows nothing.
const (
	sysOpenat2 = 437
	oPath      = 0x200000

	resolveNoMagiclinks = 0x02
	resolveBeneath      = 0x08
)

type openHow struct {
	flags, mode, resolve uint64
}

var openat2Supported = sync.OnceValue(func() bool {
	// Linux before 5.6 has no openat2, and a seccomp filter may refuse it.
	fd, errno := openat2(-100 /* AT_FDCWD */, ".", oPath|syscall.O_CLOEXEC, 0)
	if errno != 0 {
		return false
	}
	syscall.Close(fd)
	return true
})

func openat2(dirfd int, name string, flags int, mode uint32) (int, syscall.Errno) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return -1, syscall.EINVAL
	}
	how := openHow{flags: uint64(flags), mode: uint64(mode), resolve: resolveBeneath | resolveNoMagiclinks}
	for range 8 {
		fd, _, errno := syscall.Syscall6(sysOpenat2, uintptr(dirfd), uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		// EAGAIN means a rename raced a ".." in the path.
		if errno != syscall.EINTR && errno != syscall.EAGAIN {
			return int(fd), errno
		}
	}
	return -1, syscall.EAGAIN
}

// beneathErrno is the errno for an error from openat2. EXDEV is a path that
// resolves outside the root, refused as rootErrno refuses one.
func beneathErrno(errno syscall.Errno) sys.Errno {
	if errno == syscall.EXDEV {
		return sys.EPERM
	}
	return sys.UnwrapOSError(errno)
}

// openRootDir opens the directory r is rooted at, for openat2 to resolve
// under, or returns nil where openat2 is unavailable.
func openRootDir(r *os.Root) *os.File {
	if !openat2Supported() {
		return nil
	}
	f, err := r.Open(".")
	if err != nil {
		return nil
	}
	return f
}

// openFileBeneath opens name under dir, reporting false when the caller
// should open it through os.Root instead.
func openFileBeneath(dir *os.File, name string, oflag sys.Oflag, perm fs.FileMode) (*os.File, sys.Errno, bool) {
	if dir == nil || (!strings.Contains(name, "/") && oflag&sys.O_NOFOLLOW == 0) {
		return nil, 0, false
	}
	// Unlike openat, openat2 refuses a mode for an open that creates nothing.
	var mode uint32
	if oflag&sys.O_CREAT != 0 {
		mode = uint32(perm & fs.ModePerm)
	}
	fd, errno := openat2(int(dir.Fd()), name, toOsOpenFlag(oflag)|syscall.O_CLOEXEC, mode)
	switch errno {
	case 0:
		return os.NewFile(uintptr(fd), dir.Name()+"/"+name), 0, true
	case syscall.EAGAIN:
		return nil, 0, false
	}
	return nil, beneathErrno(errno), true
}

// statBeneath stats name under dir, reporting false when the caller should
// stat it through os.Root instead.
func statBeneath(dir *os.File, name string, follow bool) (sys.Stat_t, sys.Errno, bool) {
	if dir == nil {
		return sys.Stat_t{}, 0, false
	}
	if !strings.Contains(name, "/") && name != ".." {
		if sysFstatat == 0 {
			return sys.Stat_t{}, 0, false
		}
		var st syscall.Stat_t
		p, err := syscall.BytePtrFromString(name)
		if err != nil {
			return sys.Stat_t{}, sys.EINVAL, true
		}
		_, _, errno := syscall.Syscall6(sysFstatat, dir.Fd(), uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&st)), 0x100 /* AT_SYMLINK_NOFOLLOW */, 0, 0)
		if errno != 0 {
			return sys.Stat_t{}, sys.UnwrapOSError(errno), true
		}
		if !follow || st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			return statFromSyscall(&st), 0, true
		}
		// A symlink to follow, which openat2 does without leaving the root.
	}
	flags := oPath | syscall.O_CLOEXEC
	if !follow {
		flags |= syscall.O_NOFOLLOW
	}
	fd, errno := openat2(int(dir.Fd()), name, flags, 0)
	switch errno {
	case 0:
	case syscall.EAGAIN:
		return sys.Stat_t{}, 0, false
	default:
		return sys.Stat_t{}, beneathErrno(errno), true
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err), true
	}
	return statFromSyscall(&st), 0, true
}
