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

	// The OS words these failures its own way.
	_, missing := os.Stat("missing.wit")
	missingWasm := filepath.Join(dir, "missing.wasm")
	_, missingWasmErr := os.ReadFile(missingWasm)
	notDir := os.WriteFile(filepath.Join(out, "x"), nil, 0o644)
	require.Error(t, missing)
	require.Error(t, notDir)

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-nope"}, 2, "flag provided but not defined"},
		{[]string{"-wit", "x.wit", "-package", "p"}, 1, "-world and -package are required"},
		{[]string{"-wit", "x.wit", "-world", "w"}, 1, "-world and -package are required"},
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
