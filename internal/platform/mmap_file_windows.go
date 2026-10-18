package platform

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// memMapped is MEM_MAPPED, the MemoryBasicInformation.Type of a file view.
const memMapped = 0x40000

// mapCodeSegmentFromFile maps a view of f. A PAGE_EXECUTE_READ section needs
// a handle opened with GENERIC_EXECUTE, which os.Open does not request; the
// file cache opens its entries with it (see filecache.openEntry), and any
// other file fails here so that the caller copies the code instead.
func mapCodeSegmentFromFile(f *os.File, offset int64, size int) ([]byte, error) {
	end := uint64(offset) + uint64(size)
	section, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_EXECUTE_READ,
		uint32(end>>32), uint32(end), nil)
	if err != nil {
		return nil, err
	}
	// The view keeps the section alive, so the handle is not needed past
	// MapViewOfFile.
	defer func() { _ = windows.CloseHandle(section) }()
	addr, err := windows.MapViewOfFile(section, windows.FILE_MAP_READ|windows.FILE_MAP_EXECUTE,
		uint32(uint64(offset)>>32), uint32(offset), uintptr(size))
	if err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

// isFileView reports whether code starts a view from mapCodeSegmentFromFile,
// which is released with UnmapViewOfFile rather than VirtualFree.
func isFileView(code []byte) bool {
	var info windows.MemoryBasicInformation
	if err := windows.VirtualQuery(uintptr(unsafe.Pointer(&code[0])), &info, unsafe.Sizeof(info)); err != nil {
		return false
	}
	return info.Type == memMapped
}
