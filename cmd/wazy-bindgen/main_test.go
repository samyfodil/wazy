package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

func TestCLI(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bindings.go")
	var stdout, stderr bytes.Buffer

	// A directory, as wit.ParseDir reads one, written to a file.
	code := cli([]string{"-wit", "../../component/bindgen/internal/kitchen/guest/wit", "-world", "kitchen", "-package", "kitchen", "-o", out}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	src, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(src), "package kitchen")

	// A single file, to standard output, with -skip's list trimmed.
	code = cli([]string{"-wit", "../../examples/custom-wit/testdata/store.wit", "-world", "app", "-package", "store", "-skip", " wasi:, other: "}, &stdout, &stderr)
	require.Equal(t, 0, code, stderr.String())
	require.Contains(t, stdout.String(), "type Kv interface")

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-nope"}, 2, "flag provided but not defined"},
		{[]string{"-wit", "x.wit", "-package", "p"}, 1, "-world and -package are required"},
		{[]string{"-wit", "x.wit", "-world", "w"}, 1, "-world and -package are required"},
		{[]string{"-wit", "missing.wit", "-world", "w", "-package", "p"}, 1, "no such file"},
		{[]string{"-wit", "main.go", "-world", "w", "-package", "p"}, 1, "wit:"},
		{[]string{"-wit", "../../examples/custom-wit/testdata/store.wit", "-world", "nope", "-package", "p"}, 1, `no world "nope"`},
		{[]string{"-wit", "../../examples/custom-wit/testdata/store.wit", "-world", "app", "-package", "p", "-o", filepath.Join(out, "x")}, 1, "not a directory"},
	} {
		stderr.Reset()
		require.Equal(t, c.code, cli(c.args, &stdout, &stderr), strings.Join(c.args, " "))
		require.Contains(t, stderr.String(), c.want)
	}
}
