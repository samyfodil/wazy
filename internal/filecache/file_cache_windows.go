package filecache

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openEntry opens an entry for read and execute access, so the compiler can
// map the native code in it as executable instead of copying it (see
// platform.MapCodeSegmentFromFile). It shares delete access, as os.Open
// does, so the entry can still be replaced or removed while it is open.
func openEntry(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_EXECUTE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// replaceEntry renames tmp over path. Windows refuses to replace a file with
// a mapped view, which an entry has while a module compiled from it is alive
// in some process. Entries are keyed by everything that changes their
// content, so the existing entry is as good as tmp: keep it and drop tmp.
func replaceEntry(tmp, path string) error {
	err := os.Rename(tmp, path)
	if err != nil && isMapped(err) {
		if _, statErr := os.Stat(path); statErr == nil {
			return os.Remove(tmp)
		}
	}
	return err
}

// removeEntry removes a stale entry. One that some process still has mapped
// can't be removed; it stays, and is reported stale again next time.
func removeEntry(path string) error {
	if err := os.Remove(path); err != nil && !isMapped(err) {
		return err
	}
	return nil
}

func isMapped(err error) bool {
	return errors.Is(err, windows.ERROR_USER_MAPPED_FILE) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
