package main

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// The WIT the CLI reads, written out for each run: a scratch or BSD builder
// runs the test binary without the source tree.
const (
	dirWIT  = `package a:b; interface api { add: func(a: u32, b: u32) -> u32; } world w { export api; }`
	fileWIT = `package c:d; interface kv { get: func(k: string) -> option<string>; } interface other { f: func(); } world app { import kv; import other; }`
)

// testdata/app.wasm is fileWIT's world app as a component: wasm-tools
// component embed --dummy, then component new.
//
//go:embed testdata/app.wasm
var appWasm []byte

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o644))
		return p
	}
	witDir := filepath.Dir(write("wit/a.wit", dirWIT))
	witFile := write("store.wit", fileWIT)
	bad := write("bad.wit", "not wit")
	wasm := write("app.wasm", string(appWasm))
	out := filepath.Join(dir, "bindings.go")
	var stdout, stderr bytes.Buffer

	// A directory, as wit.ParseDir reads one, written to a file.
	code := cli([]string{"-wit", witDir, "-world", "w", "-package", "p", "-o", out}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	src, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(src), "package p")

	// A single file, to standard output, with -skip's list trimmed.
	code = cli([]string{"-wit", witFile, "-world", "app", "-package", "store", "-skip", " wasi:, c:d/other "}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Contains(t, stdout.String(), "type Kv interface")
	require.False(t, strings.Contains(stdout.String(), "type Other interface"), "c:d/other is skipped")

	// A compiled component, its world "root" unless -world says otherwise.
	stdout.Reset()
	code = cli([]string{"-wasm", wasm, "-package", "app"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Contains(t, stdout.String(), "type Root struct{ inst *component.Instance }")
	require.Contains(t, stdout.String(), "type Kv interface")

	// A tree of packages, its import path read from the go.mod above it, or
	// given; and a world using the tree's package for an interface.
	mod := filepath.Join(dir, "mod")
	write("mod/go.mod", "module example.com/m\n")
	code = cli([]string{"-wit", witFile, "-world", "app", "-package", "app", "-out", filepath.Join(mod, "b")}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	kvSrc, err := os.ReadFile(filepath.Join(mod, "b", "c", "d", "kv", "bindings.go"))
	require.NoError(t, err)
	require.Contains(t, string(kvSrc), "package kv")
	appSrc, err := os.ReadFile(filepath.Join(mod, "b", "app", "bindings.go"))
	require.NoError(t, err)
	require.Contains(t, string(appSrc), `kv "example.com/m/b/c/d/kv"`)
	code = cli([]string{"-wit", witFile, "-world", "app", "-package", "app", "-out", filepath.Join(dir, "t"), "-import", "x.com/t"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	appSrc, err = os.ReadFile(filepath.Join(dir, "t", "app", "bindings.go"))
	require.NoError(t, err)
	require.Contains(t, string(appSrc), `kv "x.com/t/c/d/kv"`)
	stdout.Reset()
	code = cli([]string{"-wit", witFile, "-world", "app", "-package", "app", "-with", "c:d/kv=x.com/t/c/d/kv"}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Contains(t, stdout.String(), "o = append(o, kv.Options(imports.Kv)...)")
	require.False(t, strings.Contains(stdout.String(), "type Kv interface"), "-with leaves the interface to its package")

	// The OS words these failures its own way.
	_, missing := os.Stat("missing.wit")
	missingWasm := filepath.Join(dir, "missing.wasm")
	_, missingWasmErr := os.ReadFile(missingWasm)
	notDir := os.WriteFile(filepath.Join(out, "x"), nil, 0o644)
	notDirTree := os.MkdirAll(filepath.Join(out, "x", "y"), 0o755)
	require.Error(t, missing)
	require.Error(t, notDir)
	require.Error(t, notDirTree)

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-nope"}, 2, "flag provided but not defined"},
		{[]string{"-wit", "x.wit", "-package", "p"}, 1, "-world is required"},
		{[]string{"-wit", witFile, "-world", "app"}, 1, "-package is required"},
		{[]string{"-wit", witFile, "-world", "app", "-package", "p", "-o", "x", "-out", dir}, 1, "-o and -out are exclusive"},
		{[]string{"-with", "nope"}, 2, `want interface=import/path, got "nope"`},
		{[]string{"-wit", witFile, "-world", "app", "-package", "p", "-with", "c:d/none=x"}, 1, "world app imports no interface c:d/none"},
		{[]string{"-wit", witFile, "-world", "app", "-package", "p", "-out", filepath.Join(dir, "nomod")}, 1, "no go.mod at or above"},
		{[]string{"-wit", witFile, "-world", "app", "-package", "p", "-out", filepath.Join(out, "x"), "-import", "x.com/t"}, 1, notDirTree.Error()},
		{[]string{"-wit", "missing.wit", "-world", "w", "-package", "p"}, 1, missing.Error()},
		{[]string{"-wit", bad, "-world", "w", "-package", "p"}, 1, "wit:"},
		{[]string{"-wasm", wasm, "-wit", witFile, "-package", "p"}, 1, "-wit and -wasm are exclusive"},
		{[]string{"-wasm", wasm}, 1, "-package is required"},
		{[]string{"-wasm", wasm, "-world", "nope", "-package", "p"}, 1, `no world "nope"`},
		{[]string{"-wasm", missingWasm, "-package", "p"}, 1, missingWasmErr.Error()},
		{[]string{"-wasm", bad, "-package", "p"}, 1, "wit: decode component: "},
		{[]string{"-wit", witFile, "-world", "nope", "-package", "p"}, 1, `no world "nope"`},
		{[]string{"-wit", witFile, "-world", "app", "-package", "p", "-o", filepath.Join(out, "x")}, 1, notDir.Error()},
	} {
		stderr.Reset()
		require.Equal(t, c.code, cli(c.args, &stdout, &stderr), strings.Join(c.args, " "))
		require.Contains(t, stderr.String(), c.want)
	}
}
