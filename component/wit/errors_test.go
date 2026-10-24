package wit_test

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/samyfodil/wazy/component/wit"
)

func TestParseFS_Errors(t *testing.T) {
	named := &fstest.MapFile{Data: []byte("package a:b@1.0.0; interface i {}")}
	for _, tc := range []struct {
		name string
		fs   fs.FS
		want string
	}{
		{"no dir", fstest.MapFS{}, "file does not exist"},
		{"no wit files", fstest.MapFS{"w/x.txt": {}}, "no `package` header was found in any WIT file for this package"},
		{"no package", fstest.MapFS{"w/a.wit": {Data: []byte("interface i {}")}}, "no `package` header was found in any WIT file for this package"},
		{
			"two packages",
			fstest.MapFS{"w/a.wit": named, "w/b.wit": {Data: []byte("package c:d;")}},
			"w/b.wit:1:9: package identifier `c:d` does not match previous package name of `a:b@1.0.0`",
		},
		{"bad root file", fstest.MapFS{"w/a.wit": {Data: []byte("interface {")}}, "w/a.wit"},
		{"bad dep", fstest.MapFS{"w/a.wit": named, "w/deps/x/x.wit": {Data: []byte("interface {")}}, "w/deps/x/x.wit"},
		{"bad single-file dep", fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": {Data: []byte("interface {")}}, "w/deps/x.wit"},
		{"unnamed dep", fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": {Data: []byte("interface j {}")}}, "no `package` header was found in any WIT file for this package"},
		{"deps is a file", fstest.MapFS{"w/a.wit": named, "w/deps": {}}, "w/deps is not a directory"},
		{"deps unstattable", failingDeps{fstest.MapFS{"w/a.wit": named}, true}, "permission denied"},
		{"deps unreadable", failingDeps{fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": named}, false}, "permission denied"},
		{
			"package twice",
			fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": named},
			"package `a:b@1.0.0` is defined in two different locations:\n  * w/deps/x.wit:1:9\n  * w/a.wit:1:9",
		},
		{
			// A version names one package exactly, as in wit-parser.
			"versionless use",
			fstest.MapFS{
				"w/a.wit":      {Data: []byte("package a:b; interface i { use c:d/j.{t}; }")},
				"w/deps/1.wit": {Data: []byte("package c:d@1.0.0; interface j { type t = u8; }")},
			},
			"package 'c:d' not found. known packages:\n    a:b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wit.ParseFS(tc.fs, "w")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// failingDeps fails a Stat, or else a ReadDir, of the deps directory.
type failingDeps struct {
	fstest.MapFS
	stat bool
}

func (f failingDeps) Stat(name string) (fs.FileInfo, error) {
	if f.stat && path.Base(name) == "deps" {
		return nil, fs.ErrPermission
	}
	return f.MapFS.Stat(name)
}

func (f failingDeps) ReadDir(name string) ([]fs.DirEntry, error) {
	if path.Base(name) == "deps" {
		return nil, fs.ErrPermission
	}
	return f.MapFS.ReadDir(name)
}

func TestParseDir_Errors(t *testing.T) {
	write := func(t *testing.T, path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("deps is a file", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a.wit"), "package a:b;")
		write(t, filepath.Join(dir, "deps"), "")
		if _, err := wit.ParseDir(dir); err == nil {
			t.Fatal("a deps file that is not a directory was accepted")
		}
	})
	t.Run("unreadable file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "a.wit")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := wit.ParseDir(dir); err == nil {
			t.Fatal("a dangling file was accepted")
		}
	})
	t.Run("unreadable dep", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "a.wit"), "package a:b;")
		if err := os.MkdirAll(filepath.Join(dir, "deps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "deps", "x.wit")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if _, err := wit.ParseDir(dir); err == nil {
			t.Fatal("a dangling dep file was accepted")
		}
	})
}
