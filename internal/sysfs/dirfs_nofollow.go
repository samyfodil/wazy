package sysfs

import (
	"io/fs"
	"os"

	"github.com/samyfodil/wazy/sys"
)

// noFollowIn refuses an O_NOFOLLOW open of a symlink as the host would:
// os.Root.OpenFile follows a final symlink that stays inside the root, whatever
// the flags say. An O_CREAT|O_EXCL open never follows one, so it needs no
// check.
func noFollowIn(r *os.Root, name string, oflag sys.Oflag) sys.Errno {
	if oflag&sys.O_NOFOLLOW == 0 || oflag&(sys.O_CREAT|sys.O_EXCL) == sys.O_CREAT|sys.O_EXCL {
		return 0
	}
	if info, err := r.Lstat(name); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return 0
	}
	if oflag&sys.O_DIRECTORY != 0 {
		return sys.ENOTDIR
	}
	return sys.ELOOP
}
