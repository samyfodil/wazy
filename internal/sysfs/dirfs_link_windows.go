package sysfs

import (
	"os"
	"path"
	"syscall"
	"unsafe"
)

var (
	ntdll                          = syscall.NewLazyDLL("ntdll.dll")
	procNtCreateFile               = ntdll.NewProc("NtCreateFile")
	procRtlNtStatusToDosErrorNoTeb = ntdll.NewProc("RtlNtStatusToDosErrorNoTeb")
)

type unicodeString struct {
	length, maximumLength uint16
	buffer                *uint16
}

type objectAttributes struct {
	length                       uint32
	rootDirectory                syscall.Handle
	objectName                   *unicodeString
	attributes                   uint32
	securityDescriptor, qualitys uintptr
}

type ioStatusBlock struct {
	status, information uintptr
}

const (
	fileReadAttributes        = 0x80
	ntSynchronize             = 0x100000
	objCaseInsensitive        = 0x40
	fileOpen                  = 1
	fileSynchronousIoNonalert = 0x20
	fileOpenForBackupIntent   = 0x4000
	fileOpenReparsePoint      = 0x200000
)

// openLinkIn opens name with access, and when it is a symlink opens the link
// rather than what it points to. os.Root's OpenFile honors
// FILE_FLAG_OPEN_REPARSE_POINT only as of Go 1.26, so the last element is
// opened with NtCreateFile, relative to the directory the root resolves,
// where it is one name that nothing follows.
func openLinkIn(r *os.Root, name string, access uint32) (syscall.Handle, error) {
	dir, leaf := path.Split(name)
	if leaf == "" || leaf == "." || leaf == ".." {
		f, err := r.OpenFile(name, os.O_RDONLY, 0)
		if err != nil {
			return syscall.InvalidHandle, err
		}
		h, err := duplicate(syscall.Handle(f.Fd()))
		f.Close()
		return h, err
	}
	if dir == "" {
		dir = "."
	}
	parent, err := r.Open(dir)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	defer parent.Close()

	leaf16, err := syscall.UTF16FromString(leaf)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	size := uint16((len(leaf16) - 1) * 2)
	objectName := unicodeString{length: size, maximumLength: size, buffer: &leaf16[0]}
	attrs := objectAttributes{
		length:        uint32(unsafe.Sizeof(objectAttributes{})),
		rootDirectory: syscall.Handle(parent.Fd()),
		objectName:    &objectName,
		attributes:    objCaseInsensitive,
	}
	var h syscall.Handle
	var iosb ioStatusBlock
	status, _, _ := procNtCreateFile.Call(
		uintptr(unsafe.Pointer(&h)),
		uintptr(access|ntSynchronize),
		uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&iosb)),
		0, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		fileOpen,
		fileOpenReparsePoint|fileSynchronousIoNonalert|fileOpenForBackupIntent,
		0, 0)
	if status != 0 {
		errno, _, _ := procRtlNtStatusToDosErrorNoTeb.Call(status)
		return syscall.InvalidHandle, &os.PathError{Op: "open", Path: name, Err: syscall.Errno(errno)}
	}
	return h, nil
}

func duplicate(h syscall.Handle) (syscall.Handle, error) {
	p, err := syscall.GetCurrentProcess()
	if err != nil {
		return syscall.InvalidHandle, err
	}
	var dup syscall.Handle
	err = syscall.DuplicateHandle(p, h, p, &dup, 0, false, syscall.DUPLICATE_SAME_ACCESS)
	return dup, err
}
