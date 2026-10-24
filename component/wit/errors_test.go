package wit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/samyfodil/wazy/component/wit"
)

func TestParse_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
	}{
		{"syntax", "interface {", "test.wit"},
		{"interface twice", "package a:b; interface i {} interface i {}", "interface i is defined more than once"},
		{"world twice", "package a:b; world w {} world w {}", "world w is defined more than once"},
		{"package-level use of types", "package a:b; interface i {} use i.{t};", "only an interface or world can"},
		{"package-level type", "package a:b; type t = u32;", "type t is outside any interface or world"},
		{"type twice", "package a:b; interface i { type t = u32; type t = u8; }", "t is defined more than once"},
		{"func twice", "package a:b; interface i { f: func(); f: func(); }", "f is defined more than once"},
		{"use clashes with a type", "package a:b; interface j { type t = u8; } interface i { type t = u8; use j.{t}; }", "t is defined more than once"},
		{"method twice", "package a:b; interface i { resource r { m: func(); m: func(); } }", "[method]r.m is defined more than once"},
		{"world type twice", "package a:b; world w { type t = u8; type t = u8; }", "world w: t is defined more than once"},
		{"world use twice", "package a:b; interface j { type t = u8; } world w { use j.{t}; use j.{t}; }", "world w: t is defined more than once"},
		{"world func twice", "package a:b; world w { import f: func(); import f: func(); }", "f is defined more than once"},
		{"world name twice", "package a:b; world w { import f: func(); import f: interface { g: func(); } }", "world w: f is declared more than once"},
		{"inline interface", "package a:b; world w { import x: interface { f: func(); f: func(); } }", "interface x: f is defined more than once"},
		{"unknown interface", "package a:b; world w { import nope; }", "world w: unknown interface nope"},
		{"unknown world", "package a:b; world w { include nope; }", "world w: unknown world nope"},
		{"malformed include", "package a:b; world w { include a:b; }", "malformed path a:b"},
		{"include missing package", "package a:b; world w { include x:y/w; }", "package x:y not found"},
		{"include missing world", "package a:b@1.0.0; world w { include a:b/v@1.0.0; }", "unknown world a:b/v@1.0.0"},
		{"include cycle", "package a:b; world w { include v; } world v { include w; }", "includes itself"},
		{"include renames interface", "package a:b; world w { include v with { x as y }; } world v { import x: interface { f: func(); } }", "renaming interface x is not supported"},
		{"include conflict", "package a:b; world w { import f: func(); include v; } world v { import f: func(x: u8); }", "world w: f is declared more than once"},
		{"use of unknown interface", "package a:b; interface i { use nope.{t}; }", "use nope: unknown interface nope"},
		{"use alias of unknown interface", "package a:b; use nope as n; interface i { use n.{t}; }", "unknown interface nope"},
		{"malformed use", "package a:b; interface i { use a:b.{t}; }", "malformed path a:b"},
		{"use of missing package", "package a:b; interface i { use x:y/z.{t}; }", "package x:y not found (is it in deps/?)"},
		{"use of missing interface", "package a:b@1.0.0; interface i { use a:b/z@1.0.0.{t}; }", "unknown interface a:b/z@1.0.0"},
		{"use of unknown type", "package a:b; interface j {} interface i { use j.{t}; }", "interface a:b/j: unknown type t"},
		{"circular use", "package a:b; interface i { use j.{t}; } interface j { use i.{t}; }", "use of t is circular"},
		{"unknown type", "package a:b; interface i { f: func(x: nope); }", "a:b/i#f: param x: interface a:b/i: unknown type nope"},
		{"unknown result type", "package a:b; interface i { f: func() -> nope; }", "result: interface a:b/i: unknown type nope"},
		{"unknown world type", "package a:b; world w { export f: func(x: nope); }", "f: param x: world w: unknown type nope"},
		{"alias cycle", "package a:b; interface i { type a = b; type b = a; }", "refers to itself"},
		{"recursive record", "package a:b; interface i { record r { next: option<r> } }", "type r refers to itself"},
		{"bad record field", "package a:b; interface i { record r { x: list<nope> } }", "type r: field x:"},
		{"bad variant case", "package a:b; interface i { variant v { x(tuple<u8, nope>) } }", "type v: case x:"},
		{"bad result error", "package a:b; interface i { type t = result<u8, nope>; }", "unknown type nope"},
		{"fixed-size list", "package a:b; interface i { type t = list<u8, 4>; }", "fixed-size list is not supported"},
		{"map key", "package a:b; interface i { type t = map<f32, u8>; }", "map key must be"},
		{"own of a non-resource", "package a:b; interface i { type t = u8; f: func(x: own<t>); }", "own<t>: t is not a resource"},
		{"handle of an alias cycle", "package a:b; interface i { type t = borrow<a>; type a = b; type b = a; }", "refers to itself"},
		{"interface imported twice", "package a:b; interface i {} world w { import i; import i; }", "a:b/i: interface cannot be imported more than once"},
		{"interface exported twice", "package a:b; interface i {} world w { export i; export i; }", "a:b/i: interface cannot be exported more than once"},
		// A world's interfaces bring in what they use, so a bad use fails
		// when the world is elaborated: from an import, a dependency of
		// one, the world's own use, a dependency of that, and an export.
		{"import uses unknown", "package a:b; interface i { use nope.{t}; } world w { import i; }", "interface a:b/i: use nope: unknown interface nope"},
		{"import dep uses unknown", "package a:b; interface j { use nope.{t}; } interface i { use j.{t}; } world w { import i; }", "interface a:b/j: use nope: unknown interface nope"},
		{"world use of unknown", "package a:b; world w { use nope.{t}; }", "world w: use nope: unknown interface nope"},
		{"world use dep uses unknown", "package a:b; interface j { use nope.{t}; } world w { use j.{t}; }", "interface a:b/j: use nope: unknown interface nope"},
		{"export uses unknown", "package a:b; interface i { use nope.{t}; } world w { export i; }", "interface a:b/i: use nope: unknown interface nope"},
		{"export dep uses unknown", "package a:b; interface j { use nope.{t}; } interface i { use j.{t}; } world w { export i; }", "interface a:b/j: use nope: unknown interface nope"},
		{"borrow of nothing", "package a:b; interface i { f: func(x: borrow<nope>); }", "unknown type nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wit.Parse("test.wit", tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseFS_Errors(t *testing.T) {
	named := &fstest.MapFile{Data: []byte("package a:b@1.0.0; interface i {}")}
	for _, tc := range []struct {
		name string
		fs   fstest.MapFS
		want string
	}{
		{"no dir", fstest.MapFS{}, "file does not exist"},
		{"no wit files", fstest.MapFS{"w/x.txt": {}}, "w: no .wit files"},
		{"no package", fstest.MapFS{"w/a.wit": {Data: []byte("interface i {}")}}, "w: no package declaration"},
		{
			"two packages",
			fstest.MapFS{"w/a.wit": named, "w/b.wit": {Data: []byte("package c:d;")}},
			"files declare both package a:b@1.0.0 and c:d",
		},
		{"bad root file", fstest.MapFS{"w/a.wit": {Data: []byte("interface {")}}, "w/a.wit"},
		{"bad dep", fstest.MapFS{"w/a.wit": named, "w/deps/x/x.wit": {Data: []byte("interface {")}}, "w/deps/x/x.wit"},
		{"bad single-file dep", fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": {Data: []byte("interface {")}}, "w/deps/x.wit"},
		{"unnamed dep", fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": {Data: []byte("interface j {}")}}, "w/deps/x.wit: no package declaration"},
		{
			"package twice",
			fstest.MapFS{"w/a.wit": named, "w/deps/x.wit": named},
			"package a:b@1.0.0 is defined more than once",
		},
		{
			"ambiguous version",
			fstest.MapFS{
				"w/a.wit":      {Data: []byte("package a:b; interface i { use c:d/j.{t}; }")},
				"w/deps/1.wit": {Data: []byte("package c:d@1.0.0; interface j { type t = u8; }")},
				"w/deps/2.wit": {Data: []byte("package c:d@2.0.0; interface j { type t = u8; }")},
			},
			"package c:d is ambiguous",
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
