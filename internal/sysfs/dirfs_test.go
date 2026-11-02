package sysfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/fstest"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/sys"
)

func TestDirFS(t *testing.T) {
	testFS := DirFS(".")

	// Guest can look up /
	f, errno := testFS.OpenFile("/", sys.O_RDONLY, 0)
	require.EqualErrno(t, 0, errno)
	require.EqualErrno(t, 0, f.Close())

	t.Run("host path not found", func(t *testing.T) {
		testFS := DirFS("a")
		_, errno = testFS.OpenFile(".", sys.O_RDONLY, 0)
		require.EqualErrno(t, sys.ENOENT, errno)
	})
	t.Run("host path not a directory", func(t *testing.T) {
		arg0 := os.Args[0] // should be safe in scratch tests which don't have the source mounted.

		testFS := DirFS(arg0)
		_, errno := testFS.OpenFile(".", sys.O_RDONLY, 0)
		require.EqualErrno(t, sys.ENOTDIR, errno)
	})
}

func TestDirFS_join(t *testing.T) {
	testFS := DirFS("/").(*dirFS)
	require.Equal(t, "/", testFS.join(""))
	require.Equal(t, "/", testFS.join("."))
	require.Equal(t, "/", testFS.join("/"))
	require.Equal(t, "/tmp", testFS.join("tmp"))

	testFS = DirFS(".").(*dirFS)
	require.Equal(t, ".", testFS.join(""))
	require.Equal(t, ".", testFS.join("."))
	require.Equal(t, ".", testFS.join("/"))
	require.Equal(t, "."+string(os.PathSeparator)+"tmp", testFS.join("tmp"))
}

func TestDirFS_String(t *testing.T) {
	testFS := DirFS(".")

	// String has the name of the path entered
	require.Equal(t, ".", testFS.(fmt.Stringer).String())
}

func TestDirFS_Lstat(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)
	for _, path := range []string{"animals.txt", "sub", "sub-link"} {
		require.EqualErrno(t, 0, testFS.Symlink(path, path+"-link"))
	}

	testLstat(t, testFS)
}

func TestDirFS_MkDir(t *testing.T) {
	tmpDir := t.TempDir()
	testFS := DirFS(tmpDir)

	name := "mkdir"
	realPath := path.Join(tmpDir, name)

	t.Run("doesn't exist", func(t *testing.T) {
		require.EqualErrno(t, 0, testFS.Mkdir(name, fs.ModeDir))

		stat, err := os.Stat(realPath)
		require.NoError(t, err)

		require.Equal(t, name, stat.Name())
		require.True(t, stat.IsDir())
	})

	t.Run("dir exists", func(t *testing.T) {
		err := testFS.Mkdir(name, fs.ModeDir)
		require.EqualErrno(t, sys.EEXIST, err)
	})

	t.Run("file exists", func(t *testing.T) {
		require.NoError(t, os.Remove(realPath))
		require.NoError(t, os.Mkdir(realPath, 0o700))

		err := testFS.Mkdir(name, fs.ModeDir)
		require.EqualErrno(t, sys.EEXIST, err)
	})
	t.Run("try creating on file", func(t *testing.T) {
		filePath := path.Join("non-existing-dir", "foo.txt")
		err := testFS.Mkdir(filePath, fs.ModeDir)
		require.EqualErrno(t, sys.ENOENT, err)
	})

	// Remove the path so that we can test creating it with perms.
	require.NoError(t, os.Remove(realPath))

	// Setting mode only applies to files on windows
	if runtime.GOOS != "windows" {
		t.Run("dir", func(t *testing.T) {
			require.NoError(t, os.Mkdir(realPath, 0o444))
			defer os.RemoveAll(realPath)
			testChmod(t, testFS, name)
		})
	}

	t.Run("file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(realPath, nil, 0o444))
		defer os.RemoveAll(realPath)
		testChmod(t, testFS, name)
	})
}

func testChmod(t *testing.T, testFS sys.FS, path string) {
	// Test base case, using 0o444 not 0o400 for read-back on windows.
	requireMode(t, testFS, path, 0o444)

	// Test adding write, using 0o666 not 0o600 for read-back on windows.
	require.EqualErrno(t, 0, testFS.Chmod(path, 0o666))
	requireMode(t, testFS, path, 0o666)

	if runtime.GOOS != "windows" {
		// Test clearing group and world, setting owner read+execute.
		require.EqualErrno(t, 0, testFS.Chmod(path, 0o500))
		requireMode(t, testFS, path, 0o500)
	}
}

func requireMode(t *testing.T, testFS sys.FS, path string, mode fs.FileMode) {
	st, errno := testFS.Stat(path)
	require.EqualErrno(t, 0, errno)

	require.Equal(t, mode, st.Mode.Perm())
}

func TestDirFS_Rename(t *testing.T) {
	t.Run("from doesn't exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		file1 := "file1"
		file1Path := path.Join(tmpDir, file1)
		err := os.WriteFile(file1Path, []byte{1}, 0o600)
		require.NoError(t, err)

		err = testFS.Rename("file2", file1)
		require.EqualErrno(t, sys.ENOENT, err)
	})
	t.Run("file to non-exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		file1 := "file1"
		file1Path := path.Join(tmpDir, file1)
		file1Contents := []byte{1}
		errno := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, errno)

		file2 := "file2"
		file2Path := path.Join(tmpDir, file2)
		errno = testFS.Rename(file1, file2)
		require.EqualErrno(t, 0, errno)

		// Show the prior path no longer exists
		_, errno = os.Stat(file1Path)
		require.EqualErrno(t, sys.ENOENT, sys.UnwrapOSError(errno))

		s, err := os.Stat(file2Path)
		require.NoError(t, err)
		require.False(t, s.IsDir())
	})
	t.Run("dir to non-exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		dir2 := "dir2"
		dir2Path := path.Join(tmpDir, dir2)
		errrno := testFS.Rename(dir1, dir2)
		require.EqualErrno(t, 0, errrno)

		// Show the prior path no longer exists
		_, err := os.Stat(dir1Path)
		require.EqualErrno(t, sys.ENOENT, sys.UnwrapOSError(err))

		s, err := os.Stat(dir2Path)
		require.NoError(t, err)
		require.True(t, s.IsDir())
	})
	t.Run("dir to file", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		dir2 := "dir2"
		dir2Path := path.Join(tmpDir, dir2)

		// write a file to that path
		f, err := os.OpenFile(dir2Path, os.O_RDWR|os.O_CREATE, 0o600)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		errno := testFS.Rename(dir1, dir2)
		require.EqualErrno(t, sys.ENOTDIR, errno)
	})
	t.Run("file to dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		file1 := "file1"
		file1Path := path.Join(tmpDir, file1)
		file1Contents := []byte{1}
		err := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, err)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		errno := testFS.Rename(file1, dir1)
		require.EqualErrno(t, sys.EISDIR, errno)
	})

	// Similar to https://github.com/ziglang/zig/blob/0.10.1/lib/std/fs/test.zig#L567-L582
	t.Run("dir to empty dir should be fine", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		// add a file to that directory
		file1 := "file1"
		file1Path := path.Join(dir1Path, file1)
		file1Contents := []byte{1}
		err := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, err)

		dir2 := "dir2"
		dir2Path := path.Join(tmpDir, dir2)
		require.NoError(t, os.Mkdir(dir2Path, 0o700))

		errno := testFS.Rename(dir1, dir2)
		require.EqualErrno(t, 0, errno)

		// Show the prior path no longer exists
		_, err = os.Stat(dir1Path)
		require.EqualErrno(t, sys.ENOENT, sys.UnwrapOSError(err))

		// Show the file inside that directory moved
		s, err := os.Stat(path.Join(dir2Path, file1))
		require.NoError(t, err)
		require.False(t, s.IsDir())
	})

	// Similar to https://github.com/ziglang/zig/blob/0.10.1/lib/std/fs/test.zig#L584-L604
	t.Run("dir to non empty dir should be EXIST", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		// add a file to that directory
		file1 := "file1"
		file1Path := path.Join(dir1Path, file1)
		file1Contents := []byte{1}
		err := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, err)

		dir2 := "dir2"
		dir2Path := path.Join(tmpDir, dir2)
		require.NoError(t, os.Mkdir(dir2Path, 0o700))

		// Make the destination non-empty.
		err = os.WriteFile(path.Join(dir2Path, "existing.txt"), []byte("any thing"), 0o600)
		require.NoError(t, err)

		errno := testFS.Rename(dir1, dir2)
		require.EqualErrno(t, sys.ENOTEMPTY, errno)
	})

	t.Run("file to file", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		file1 := "file1"
		file1Path := path.Join(tmpDir, file1)
		file1Contents := []byte{1}
		err := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, err)

		file2 := "file2"
		file2Path := path.Join(tmpDir, file2)
		file2Contents := []byte{2}
		err = os.WriteFile(file2Path, file2Contents, 0o600)
		require.NoError(t, err)

		errno := testFS.Rename(file1, file2)
		require.EqualErrno(t, 0, errno)

		// Show the prior path no longer exists
		_, err = os.Stat(file1Path)
		require.EqualErrno(t, sys.ENOENT, sys.UnwrapOSError(err))

		// Show the file1 overwrote file2
		b, err := os.ReadFile(file2Path)
		require.NoError(t, err)
		require.Equal(t, file1Contents, b)
	})
	t.Run("dir to itself", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir1 := "dir1"
		dir1Path := path.Join(tmpDir, dir1)
		require.NoError(t, os.Mkdir(dir1Path, 0o700))

		errno := testFS.Rename(dir1, dir1)
		require.EqualErrno(t, 0, errno)

		s, err := os.Stat(dir1Path)
		require.NoError(t, err)
		require.True(t, s.IsDir())
	})
	t.Run("file to itself", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		file1 := "file1"
		file1Path := path.Join(tmpDir, file1)
		file1Contents := []byte{1}
		err := os.WriteFile(file1Path, file1Contents, 0o600)
		require.NoError(t, err)

		errno := testFS.Rename(file1, file1)
		require.EqualErrno(t, 0, errno)

		b, err := os.ReadFile(file1Path)
		require.NoError(t, err)
		require.Equal(t, file1Contents, b)
	})
}

func TestDirFS_Rmdir(t *testing.T) {
	t.Run("doesn't exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"

		err := testFS.Rmdir(name)
		require.EqualErrno(t, sys.ENOENT, err)
	})

	t.Run("dir not empty", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"
		realPath := path.Join(tmpDir, name)

		require.NoError(t, os.Mkdir(realPath, 0o700))
		fileInDir := path.Join(realPath, "file")
		require.NoError(t, os.WriteFile(fileInDir, []byte{}, 0o600))

		err := testFS.Rmdir(name)
		require.EqualErrno(t, sys.ENOTEMPTY, err)

		require.NoError(t, os.Remove(fileInDir))
	})

	t.Run("dir previously not empty", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"
		realPath := path.Join(tmpDir, name)
		require.NoError(t, os.Mkdir(realPath, 0o700))

		// Create a file and then delete it.
		fileInDir := path.Join(realPath, "file")
		require.NoError(t, os.WriteFile(fileInDir, []byte{}, 0o600))
		require.NoError(t, os.Remove(fileInDir))

		// After deletion, try removing directory.
		errno := testFS.Rmdir(name)
		require.EqualErrno(t, 0, errno)
	})

	t.Run("dir empty", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"
		realPath := path.Join(tmpDir, name)
		require.NoError(t, os.Mkdir(realPath, 0o700))
		require.EqualErrno(t, 0, testFS.Rmdir(name))
		_, err := os.Stat(realPath)
		require.Error(t, err)
	})

	t.Run("dir empty while opening", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"
		realPath := path.Join(tmpDir, name)
		require.NoError(t, os.Mkdir(realPath, 0o700))

		f, errno := testFS.OpenFile(name, sys.O_DIRECTORY, 0o700)
		require.EqualErrno(t, 0, errno)
		defer f.Close()

		require.EqualErrno(t, 0, testFS.Rmdir(name))
		_, err := os.Stat(realPath)
		require.Error(t, err)
	})

	t.Run("not directory", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "rmdir"
		realPath := path.Join(tmpDir, name)

		require.NoError(t, os.WriteFile(realPath, []byte{}, 0o600))

		err := testFS.Rmdir(name)
		require.EqualErrno(t, sys.ENOTDIR, err)

		require.NoError(t, os.Remove(realPath))
	})
}

func TestDirFS_Unlink(t *testing.T) {
	t.Run("doesn't exist", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)
		name := "unlink"

		err := testFS.Unlink(name)
		require.EqualErrno(t, sys.ENOENT, err)
	})

	t.Run("target: dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		dir := "dir"
		realPath := path.Join(tmpDir, dir)

		require.NoError(t, os.Mkdir(realPath, 0o700))

		err := testFS.Unlink(dir)
		require.EqualErrno(t, sys.EISDIR, err)

		require.NoError(t, os.Remove(realPath))
	})

	t.Run("target: symlink to dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		// Create link target dir.
		subDirName := "subdir"
		subDirRealPath := path.Join(tmpDir, subDirName)
		require.NoError(t, os.Mkdir(subDirRealPath, 0o700))

		// Create a symlink to the subdirectory.
		const symlinkName = "symlink-to-dir"
		require.EqualErrno(t, 0, testFS.Symlink("subdir", symlinkName))

		// Unlinking the symlink should suceed.
		errno := testFS.Unlink(symlinkName)
		require.EqualErrno(t, 0, errno)
	})

	t.Run("file exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		testFS := DirFS(tmpDir)

		name := "unlink"
		realPath := path.Join(tmpDir, name)

		require.NoError(t, os.WriteFile(realPath, []byte{}, 0o600))

		require.EqualErrno(t, 0, testFS.Unlink(name))

		_, err := os.Stat(realPath)
		require.Error(t, err)
	})
}

func TestDirFS_Utimesns(t *testing.T) {
	tmpDir := t.TempDir()
	testFS := DirFS(tmpDir)

	file := "file"
	err := os.WriteFile(path.Join(tmpDir, file), []byte{}, 0o700)
	require.NoError(t, err)

	dir := "dir"
	err = os.Mkdir(path.Join(tmpDir, dir), 0o700)
	require.NoError(t, err)

	t.Run("doesn't exist", func(t *testing.T) {
		err := testFS.Utimens("nope", 0, 0)
		require.EqualErrno(t, sys.ENOENT, err)
	})

	// Note: This sets microsecond granularity because Windows doesn't support
	// nanosecond.
	//
	// Negative isn't tested as most platforms don't return consistent results.
	tests := []struct {
		name       string
		atim, mtim int64
	}{
		{
			name: "nil",
		},
		{
			name: "a=omit,m=omit",
			atim: sys.UTIME_OMIT,
			mtim: sys.UTIME_OMIT,
		},
		{
			name: "a=set,m=omit",
			atim: int64(123*time.Second + 4*time.Microsecond),
			mtim: sys.UTIME_OMIT,
		},
		{
			name: "a=omit,m=set",
			atim: sys.UTIME_OMIT,
			mtim: int64(123*time.Second + 4*time.Microsecond),
		},
		{
			name: "a=set,m=set",
			atim: int64(123*time.Second + 4*time.Microsecond),
			mtim: int64(223*time.Second + 5*time.Microsecond),
		},
	}

	for _, fileType := range []string{"dir", "file", "link"} {
		for _, tt := range tests {
			tc := tt
			fileType := fileType
			name := fileType + " " + tc.name

			t.Run(name, func(t *testing.T) {
				tmpDir := t.TempDir()
				testFS := DirFS(tmpDir)

				file := path.Join(tmpDir, "file")
				errno := os.WriteFile(file, []byte{}, 0o700)
				require.NoError(t, errno)

				link := file + "-link"
				require.NoError(t, os.Symlink("file", link))

				dir := path.Join(tmpDir, "dir")
				errno = os.Mkdir(dir, 0o700)
				require.NoError(t, errno)

				var path, statPath string
				switch fileType {
				case "dir":
					path = "dir"
					statPath = "dir"
				case "file":
					path = "file"
					statPath = "file"
				case "link":
					path = "file-link"
					statPath = "file"
				default:
					panic(tc)
				}

				oldSt, errno := testFS.Lstat(statPath)
				require.EqualErrno(t, 0, errno)

				errno = testFS.Utimens(path, tc.atim, tc.mtim)
				require.EqualErrno(t, 0, errno)

				newSt, errno := testFS.Lstat(statPath)
				require.EqualErrno(t, 0, errno)

				if platform.CompilerSupported() {
					if tc.atim == sys.UTIME_OMIT {
						require.Equal(t, oldSt.Atim, newSt.Atim)
					} else {
						require.Equal(t, tc.atim, newSt.Atim)
					}
				}

				// When compiler isn't supported, we can still check mtim.
				if tc.mtim == sys.UTIME_OMIT {
					require.Equal(t, oldSt.Mtim, newSt.Mtim)
				} else {
					require.Equal(t, tc.mtim, newSt.Mtim)
				}
			})
		}
	}
}

func TestDirFS_OpenFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a subdirectory, so we can test reads outside the sys.FS root.
	tmpDir = path.Join(tmpDir, t.Name())
	require.NoError(t, os.Mkdir(tmpDir, 0o700))
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)

	testOpen_Read(t, testFS, true, true)

	testOpen_O_RDWR(t, tmpDir, testFS)

	t.Run("path outside root", func(t *testing.T) {
		_, errno := testFS.OpenFile("../foo", sys.O_RDONLY, 0)
		require.EqualErrno(t, sys.EPERM, errno)
	})
}

func TestDirFS_Stat(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)
	testStat(t, testFS)

	// from os.TestDirFSPathsValid
	if runtime.GOOS != "windows" {
		t.Run("strange name", func(t *testing.T) {
			name := `e:xperi\ment.txt`
			require.NoError(t, os.WriteFile(path.Join(tmpDir, name), nil, 0o600))

			_, errno := testFS.Stat(name)
			require.EqualErrno(t, 0, errno)
		})
	}
}

func TestDirFS_Readdir(t *testing.T) {
	root := t.TempDir()
	testFS := DirFS(root)

	const readDirTarget = "dir"
	errno := testFS.Mkdir(readDirTarget, 0o700)
	require.EqualErrno(t, 0, errno)

	// Open the empty directory
	dirFile, errno := testFS.OpenFile(readDirTarget, sys.O_RDONLY, 0)
	require.EqualErrno(t, 0, errno)
	defer dirFile.Close()

	// Write files to the directory after it is open.
	require.NoError(t, os.WriteFile(path.Join(root, readDirTarget, "1"), nil, 0o444))
	require.NoError(t, os.WriteFile(path.Join(root, readDirTarget, "2"), nil, 0o444))

	// Test files are visible. This fails in windows unless the implementation
	// re-opens the underlying file.
	// https://github.com/ziglang/zig/blob/e3736baddb8ecff90f0594be9f604c7484ce9aa2/lib/std/fs/test.zig#L290-L317
	t.Run("Sees files written after open", func(t *testing.T) {
		dirents, errno := dirFile.Readdir(1)
		require.EqualErrno(t, 0, errno)

		require.Equal(t, 1, len(dirents))
		n := dirents[0].Name
		switch n {
		case "1", "2": // order is inconsistent on scratch images.
		default:
			require.Equal(t, "1", n)
		}
	})

	// Test there is no error reading the directory if it was deleted while
	// iterating. See docs on Readdir for why in general, but specifically Zig
	// tests enforce this. This test is Windows sensitive as well.
	//
	// https://github.com/ziglang/zig/blob/e3736baddb8ecff90f0594be9f604c7484ce9aa2/lib/std/fs/test.zig#L311C1-L311C1
	t.Run("sys.ENOENT or no error, deleted while reading", func(t *testing.T) {
		require.NoError(t, os.RemoveAll(path.Join(root, readDirTarget)))

		dirents, errno := dirFile.Readdir(-1)
		if errno != 0 {
			require.EqualErrno(t, sys.ENOENT, errno)
		}

		require.Equal(t, 0, len(dirents))
	})

	// Readdir after Close must report EBADF, not silently succeed on a stale
	// (possibly reused) descriptor. This guards the Linux getdents64 fast path,
	// which reads the cached f.fd directly instead of going through os.File.
	t.Run("sys.EBADF after close", func(t *testing.T) {
		require.EqualErrno(t, 0, testFS.Mkdir("closed", 0o700))
		require.NoError(t, os.WriteFile(path.Join(root, "closed", "a"), nil, 0o444))

		closedDir, errno := testFS.OpenFile("closed", sys.O_RDONLY, 0)
		require.EqualErrno(t, 0, errno)

		// Drain to EOF first so any EOF short-circuit can't hide the close.
		_, errno = closedDir.Readdir(-1)
		require.EqualErrno(t, 0, errno)

		require.EqualErrno(t, 0, closedDir.Close())

		_, errno = closedDir.Readdir(-1)
		require.EqualErrno(t, sys.EBADF, errno)
	})
}

func TestDirFS_Link(t *testing.T) {
	t.Parallel()

	// Set up the test files
	tmpDir := t.TempDir()
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)

	require.EqualErrno(t, testFS.Link("cat", ""), sys.ENOENT)
	require.EqualErrno(t, testFS.Link("sub/test.txt", "sub/test.txt"), sys.EEXIST)
	require.EqualErrno(t, testFS.Link("sub/test.txt", "."), sys.EEXIST)
	require.EqualErrno(t, testFS.Link("sub/test.txt", ""), sys.EEXIST)
	require.EqualErrno(t, testFS.Link("sub/test.txt", "/"), sys.EEXIST)
	require.EqualErrno(t, 0, testFS.Link("sub/test.txt", "foo"))
	// A directory cannot be hard linked, which Windows reports as access
	// denied.
	require.EqualErrno(t, sys.EPERM, testFS.Link("sub", "sub-link"))
	// A trailing slash names a directory, which a link never makes.
	require.EqualErrno(t, sys.ENOENT, testFS.Link("sub/test.txt", "bar/"))
	require.EqualErrno(t, sys.EEXIST, testFS.Link("sub/test.txt", "sub/"))
}

func TestDirFS_Symlink(t *testing.T) {
	t.Parallel()

	// Set up the test files
	tmpDir := t.TempDir()
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)

	require.EqualErrno(t, sys.EPERM, testFS.Symlink("/test.txt", "sub/test.txt"))
	require.EqualErrno(t, sys.EEXIST, testFS.Symlink("sub/test.txt", "sub/test.txt"))
	// Non-existing old name is allowed.
	require.EqualErrno(t, 0, testFS.Symlink("non-existing", "aa"))
	require.EqualErrno(t, 0, testFS.Symlink("sub/", "symlinked-subdir"))

	st, err := os.Lstat(path.Join(tmpDir, "aa"))
	require.NoError(t, err)
	require.Equal(t, "aa", st.Name())
	require.True(t, st.Mode()&fs.ModeSymlink > 0 && !st.IsDir())

	st, err = os.Lstat(path.Join(tmpDir, "symlinked-subdir"))
	require.NoError(t, err)
	require.Equal(t, "symlinked-subdir", st.Name())
	require.True(t, st.Mode()&fs.ModeSymlink > 0)
}

func TestDirFS_Readlink(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, fstest.WriteTestFiles(tmpDir))

	testFS := DirFS(tmpDir)
	testReadlink(t, testFS, testFS)
}

// forBinding runs test once with a DirFS and once with one BindFS bound for a
// module instance, which resolves deeper names another way on Linux.
func forBinding(t *testing.T, test func(t *testing.T, mount func(dir string) sys.FS)) {
	t.Run("unbound", func(t *testing.T) { test(t, DirFS) })
	t.Run("bound", func(t *testing.T) {
		test(t, func(dir string) sys.FS {
			fs, closer := BindFS(DirFS(dir))
			t.Cleanup(func() { require.NoError(t, closer.Close()) })
			return fs
		})
	})
}

// TestDirFS_confined checks that no path leads outside the mounted directory,
// through ".." in a symlink the guest made or in one already on the host.
func TestDirFS_confined(t *testing.T) {
	t.Parallel()
	forBinding(t, testConfined)
}

func testConfined(t *testing.T, mount func(dir string) sys.FS) {
	tmpDir := t.TempDir()
	outside := path.Join(tmpDir, "outside")
	require.NoError(t, os.Mkdir(outside, 0o700))
	secret := path.Join(outside, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0o600))
	mnt := path.Join(tmpDir, "mnt")
	require.NoError(t, os.Mkdir(mnt, 0o700))
	require.NoError(t, os.WriteFile(path.Join(mnt, "inside"), []byte("inside"), 0o600))
	require.NoError(t, os.Symlink(secret, path.Join(mnt, "host-link")))
	// An absolute symlink is refused even where it names a file inside, as
	// one the guest makes is.
	require.NoError(t, os.Symlink(path.Join(mnt, "inside"), path.Join(mnt, "absolute-link")))
	require.NoError(t, os.Mkdir(path.Join(mnt, "sub"), 0o700))

	testFS := mount(mnt)
	require.EqualErrno(t, 0, testFS.Symlink("../outside/secret", "file-link"))
	require.EqualErrno(t, 0, testFS.Symlink("../outside", "dir-link"))
	require.EqualErrno(t, 0, testFS.Symlink("inside", "inside-link"))
	require.EqualErrno(t, 0, testFS.Symlink("../../outside/secret", "sub/file-link"))
	require.EqualErrno(t, 0, testFS.Symlink("../inside", "sub/inside-link"))

	for _, name := range []string{"file-link", "dir-link/secret", "host-link", "absolute-link", "sub/file-link", "sub/../file-link"} {
		t.Run(name, func(t *testing.T) {
			_, errno := testFS.OpenFile(name, sys.O_RDONLY, 0)
			require.EqualErrno(t, sys.EPERM, errno)
			_, errno = testFS.OpenFile(name, sys.O_WRONLY|sys.O_TRUNC, 0)
			require.EqualErrno(t, sys.EPERM, errno)
			_, errno = testFS.Stat(name)
			require.EqualErrno(t, sys.EPERM, errno)
			require.EqualErrno(t, sys.EPERM, testFS.Utimens(name, 0, 0))
		})
	}

	_, errno := testFS.Lstat("dir-link/secret")
	require.EqualErrno(t, sys.EPERM, errno)
	_, errno = testFS.Readlink("dir-link/secret")
	require.EqualErrno(t, sys.EPERM, errno)
	require.EqualErrno(t, sys.EPERM, testFS.Unlink("dir-link/secret"))
	require.EqualErrno(t, sys.EPERM, testFS.Rename("dir-link/secret", "stolen"))
	require.EqualErrno(t, sys.EPERM, testFS.Rename("inside", "dir-link/planted"))
	require.EqualErrno(t, sys.EPERM, testFS.Link("dir-link/secret", "hard-link"))
	require.EqualErrno(t, sys.EPERM, testFS.Mkdir("dir-link/new", 0o700))
	require.EqualErrno(t, sys.EPERM, testFS.Symlink("x", "dir-link/new-link"))

	b, err := os.ReadFile(secret)
	require.NoError(t, err)
	require.Equal(t, "secret", string(b))
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Equal(t, 1, len(entries))

	// A symlink that stays inside the directory still resolves.
	for _, name := range []string{"inside-link", "sub/inside-link", "sub/../inside-link"} {
		f, errno := testFS.OpenFile(name, sys.O_RDONLY, 0)
		require.EqualErrno(t, 0, errno, name)
		require.EqualErrno(t, 0, f.Close())
		st, errno := testFS.Stat(name)
		require.EqualErrno(t, 0, errno, name)
		require.Equal(t, int64(len("inside")), st.Size)
		st, errno = testFS.Lstat(name)
		require.EqualErrno(t, 0, errno, name)
		require.Equal(t, fs.ModeSymlink, st.Mode.Type())
	}
	// The links themselves are inside, so they can be read and removed.
	link, errno := testFS.Readlink("file-link")
	require.EqualErrno(t, 0, errno)
	require.Equal(t, "../outside/secret", link)
	require.EqualErrno(t, 0, testFS.Unlink("file-link"))
}

func TestDirFS_OpenFile_noFollow(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(path.Join(tmpDir, "sub", "dir"), 0o700))
		require.NoError(t, os.WriteFile(path.Join(tmpDir, "sub", "file"), nil, 0o600))
		testFS := mount(tmpDir)
		require.EqualErrno(t, 0, testFS.Symlink("dir", "sub/dir-link"))
		require.EqualErrno(t, 0, testFS.Symlink("file", "sub/file-link"))

		// Through the root itself and through a directory in it, which takes
		// a walk.
		for _, prefix := range []string{"", "sub/../"} {
			if prefix == "" {
				require.EqualErrno(t, 0, testFS.Symlink("sub/dir", "dir-link"))
				require.EqualErrno(t, 0, testFS.Symlink("sub/file", "file-link"))
			} else {
				prefix = "sub/"
			}
			_, errno := testFS.OpenFile(prefix+"file-link", sys.O_RDONLY|sys.O_NOFOLLOW, 0)
			require.EqualErrno(t, sys.ELOOP, errno)
			_, errno = testFS.OpenFile(prefix+"dir-link", sys.O_RDONLY|sys.O_NOFOLLOW, 0)
			require.EqualErrno(t, sys.ELOOP, errno)
			_, errno = testFS.OpenFile(prefix+"dir-link", sys.O_RDONLY|sys.O_DIRECTORY|sys.O_NOFOLLOW, 0)
			require.EqualErrno(t, sys.ENOTDIR, errno)

			f, errno := testFS.OpenFile(prefix+"file-link", sys.O_RDONLY, 0)
			require.EqualErrno(t, 0, errno)
			require.EqualErrno(t, 0, f.Close())
		}
	})
}

func TestBindFS(t *testing.T) {
	tmpDir := t.TempDir()
	dir := path.Join(tmpDir, "dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(path.Join(dir, "f"), nil, 0o600))

	t.Run("DirFS", func(t *testing.T) {
		bound, closer := BindFS(DirFS(dir))
		require.NotNil(t, closer)
		_, errno := bound.Stat("f")
		require.EqualErrno(t, 0, errno)

		// The directory it opened is the one it keeps using. Windows refuses
		// to rename a directory while it is open.
		if runtime.GOOS != "windows" {
			require.NoError(t, os.Rename(dir, dir+"-moved"))
			defer func() { require.NoError(t, os.Rename(dir+"-moved", dir)) }()
			_, errno = bound.Stat("f")
			require.EqualErrno(t, 0, errno)
		}
		require.NoError(t, closer.Close())
	})

	t.Run("ReadFS", func(t *testing.T) {
		bound, closer := BindFS(&ReadFS{FS: DirFS(dir)})
		require.NotNil(t, closer)
		_, ok := bound.(*ReadFS)
		require.True(t, ok)
		require.EqualErrno(t, sys.EROFS, bound.Mkdir("new", 0o700))
		require.NoError(t, closer.Close())
	})

	t.Run("other", func(t *testing.T) {
		adapted := &AdaptFS{FS: os.DirFS(dir)}
		bound, closer := BindFS(adapted)
		require.Nil(t, closer)
		require.Equal(t, sys.FS(adapted), bound)
	})
}

// TestDirFS_dotElems checks that a path ending in "." or ".." names the
// directory it reached, which unlink, rmdir and rename refuse to act on as
// POSIX does. Windows acts on that directory.
func TestDirFS_dotElems(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(path.Join(tmpDir, "sub", "empty"), 0o700))
		require.NoError(t, os.WriteFile(path.Join(tmpDir, "f"), nil, 0o600))
		testFS := mount(tmpDir)

		require.EqualErrno(t, sys.EINVAL, testFS.Rmdir("."))
		require.EqualErrno(t, sys.EINVAL, testFS.Rmdir("sub/empty/."))
		require.EqualErrno(t, sys.ENOTEMPTY, testFS.Rmdir("sub/empty/.."))
		require.EqualErrno(t, sys.EISDIR, testFS.Unlink("sub/empty/."))
		require.EqualErrno(t, sys.EISDIR, testFS.Unlink("sub/.."))
		require.EqualErrno(t, sys.EINVAL, testFS.Rename("sub/empty/.", "moved"))
		require.EqualErrno(t, sys.EINVAL, testFS.Rename("f", "sub/empty/."))
		require.EqualErrno(t, sys.EINVAL, testFS.Rename("sub/empty/..", "moved"))

		for _, dir := range []string{tmpDir, path.Join(tmpDir, "sub"), path.Join(tmpDir, "sub", "empty")} {
			info, err := os.Stat(dir)
			require.NoError(t, err)
			require.True(t, info.IsDir())
		}
	})
}

// TestDirFS_notDir checks that a path through a file reports ENOTDIR, and one
// through a missing directory ENOENT. Windows reports both the same way.
func TestDirFS_notDir(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.WriteFile(path.Join(tmpDir, "f"), nil, 0o600))
		testFS := mount(tmpDir)

		for _, name := range []string{"f/x", "f/x/y"} {
			_, errno := testFS.OpenFile(name, sys.O_RDONLY, 0)
			require.EqualErrno(t, sys.ENOTDIR, errno, name)
			_, errno = testFS.OpenFile(name, sys.O_WRONLY|sys.O_CREAT, 0o600)
			require.EqualErrno(t, sys.ENOTDIR, errno, name)
			_, errno = testFS.Stat(name)
			require.EqualErrno(t, sys.ENOTDIR, errno, name)
		}
		for _, name := range []string{"missing/x", "missing/x/y"} {
			_, errno := testFS.OpenFile(name, sys.O_RDONLY, 0)
			require.EqualErrno(t, sys.ENOENT, errno, name)
			_, errno = testFS.Stat(name)
			require.EqualErrno(t, sys.ENOENT, errno, name)
		}
	})
}

// TestDirFS_Rename_caseOnly checks that renaming a directory to a name that
// differs only in case renames it, where on a case-insensitive filesystem
// the new name already reaches the directory itself.
func TestDirFS_Rename_caseOnly(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.Mkdir(path.Join(tmpDir, "renameFROM"), 0o700))
		testFS := mount(tmpDir)

		require.EqualErrno(t, 0, testFS.Rename("renameFROM", "RENAMEfrom"))
		entries, err := os.ReadDir(tmpDir)
		require.NoError(t, err)
		require.Equal(t, 1, len(entries))
		require.Equal(t, "RENAMEfrom", entries[0].Name())
	})
}

// TestDirFS_Rename_overSymlink checks that renaming onto a symlink replaces
// the symlink, even one to a directory, as rename(2) does.
func TestDirFS_Rename_overSymlink(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.Mkdir(path.Join(tmpDir, "dir"), 0o700))
		require.NoError(t, os.WriteFile(path.Join(tmpDir, "f"), []byte("f"), 0o600))
		testFS := mount(tmpDir)
		require.EqualErrno(t, 0, testFS.Symlink("dir", "dir-link"))

		require.EqualErrno(t, 0, testFS.Rename("f", "dir-link"))
		st, errno := testFS.Lstat("dir-link")
		require.EqualErrno(t, 0, errno)
		require.True(t, st.Mode.IsRegular())
		info, err := os.Stat(path.Join(tmpDir, "dir"))
		require.NoError(t, err)
		require.True(t, info.IsDir())
	})
}

// TestDirFS_backslash checks that a backslash is part of a name, not the
// separator Windows takes it for.
func TestDirFS_backslash(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.Mkdir(path.Join(tmpDir, "dir"), 0o700))
	require.NoError(t, os.WriteFile(path.Join(tmpDir, "dir", "x"), nil, 0o600))
	testFS := DirFS(tmpDir)

	_, errno := testFS.OpenFile(`dir\x`, sys.O_RDONLY, 0)
	require.EqualErrno(t, sys.ENOENT, errno)
	_, errno = testFS.Stat(`dir\x`)
	require.EqualErrno(t, sys.ENOENT, errno)
	require.EqualErrno(t, sys.ENOENT, testFS.Unlink(`dir\x`))
}

// TestOsFile_DirFS checks that a directory opened through a bound DirFS
// resolves paths through itself, so they reach it after a rename.
func TestOsFile_DirFS(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.Mkdir(path.Join(tmpDir, "a"), 0o700))
	testFS, closer := BindFS(DirFS(tmpDir))
	defer closer.Close()

	d, errno := testFS.OpenFile("a", sys.O_RDONLY|sys.O_DIRECTORY, 0)
	require.EqualErrno(t, 0, errno)
	defer d.Close()

	require.EqualErrno(t, 0, testFS.Rename("a", "b"))
	name := "b"
	require.NoError(t, os.WriteFile(path.Join(tmpDir, name, "f"), []byte("hello"), 0o600))

	dirFS, errno := d.(DirHandle).DirFS()
	require.EqualErrno(t, 0, errno)
	f, errno := dirFS.OpenFile("f", sys.O_RDONLY, 0)
	require.EqualErrno(t, 0, errno)
	defer f.Close()
	buf := make([]byte, 5)
	n, errno := f.Read(buf)
	require.EqualErrno(t, 0, errno)
	require.Equal(t, "hello", string(buf[:n]))
	// It stays inside the directory.
	_, errno = dirFS.Stat("../" + name)
	require.EqualErrno(t, sys.EPERM, errno)

	t.Run("unbound", func(t *testing.T) {
		d, errno := DirFS(tmpDir).OpenFile(name, sys.O_RDONLY|sys.O_DIRECTORY, 0)
		require.EqualErrno(t, 0, errno)
		defer d.Close()
		_, errno = d.(DirHandle).DirFS()
		require.EqualErrno(t, sys.ENOSYS, errno)
	})
	t.Run("file", func(t *testing.T) {
		f, errno := testFS.OpenFile(name+"/f", sys.O_RDONLY, 0)
		require.EqualErrno(t, 0, errno)
		defer f.Close()
		_, errno = f.(DirHandle).DirFS()
		require.EqualErrno(t, sys.ENOTDIR, errno)
	})
}

// TestDirFS_Lutimens checks the times of a symlink itself are set, and those
// of what it points to are left alone.
func TestDirFS_Lutimens(t *testing.T) {
	t.Parallel()
	forBinding(t, func(t *testing.T, mount func(dir string) sys.FS) {
		tmpDir := t.TempDir()
		require.NoError(t, os.MkdirAll(path.Join(tmpDir, "sub"), 0o700))
		require.NoError(t, os.WriteFile(path.Join(tmpDir, "sub", "file"), nil, 0o600))
		testFS := mount(tmpDir)
		require.EqualErrno(t, 0, testFS.Symlink("file", "sub/link"))

		fileSt, errno := testFS.Stat("sub/file")
		require.EqualErrno(t, 0, errno)
		linkSt, errno := testFS.Lstat("sub/link")
		require.EqualErrno(t, 0, errno)

		mtim := linkSt.Mtim - int64(200*time.Second)
		require.EqualErrno(t, 0, testFS.(LutimensFS).Lutimens("sub/link", sys.UTIME_OMIT, mtim))

		linkSt, errno = testFS.Lstat("sub/link")
		require.EqualErrno(t, 0, errno)
		require.Equal(t, mtim, linkSt.Mtim)
		after, errno := testFS.Stat("sub/file")
		require.EqualErrno(t, 0, errno)
		require.Equal(t, fileSt.Mtim, after.Mtim)

		// A file that is not a symlink has its own times set as well.
		require.EqualErrno(t, 0, testFS.(LutimensFS).Lutimens("sub/file", sys.UTIME_OMIT, mtim))
		after, errno = testFS.Stat("sub/file")
		require.EqualErrno(t, 0, errno)
		require.Equal(t, mtim, after.Mtim)
	})
}

// TestDirFS_OpenFile_appendTruncate checks a file opened to append and
// truncate appends, which on Windows takes reopening it after truncating.
func TestDirFS_OpenFile_appendTruncate(t *testing.T) {
	tmpDir := t.TempDir()
	require.NoError(t, os.WriteFile(path.Join(tmpDir, "f"), []byte("old content"), 0o600))
	testFS := DirFS(tmpDir)

	f, errno := testFS.OpenFile("f", sys.O_WRONLY|sys.O_CREAT|sys.O_TRUNC|sys.O_APPEND, 0o600)
	require.EqualErrno(t, 0, errno)
	defer f.Close()
	_, errno = f.Write([]byte("ab"))
	require.EqualErrno(t, 0, errno)
	_, errno = f.Seek(0, io.SeekStart)
	require.EqualErrno(t, 0, errno)
	_, errno = f.Write([]byte("cd"))
	require.EqualErrno(t, 0, errno)

	b, err := os.ReadFile(path.Join(tmpDir, "f"))
	require.NoError(t, err)
	require.Equal(t, "abcd", string(b))
}
