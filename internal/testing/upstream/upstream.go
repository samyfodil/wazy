// Package upstream reads test data that a make target fetches from where it lives rather than the tree
// committing it.
package upstream

import (
	"io/fs"
	"os"
	"testing"
)

// RequireEnv is set in CI, where the data is always fetched first. There a test skipping for want of
// it would mean it silently never ran, so it fails instead.
const RequireEnv = "WAZY_SPECTEST_REQUIRE_CASES"

// Require skips t, or fails it under RequireEnv, when nothing in fsys matches pattern, naming the make
// target that fetches it.
func Require(t testing.TB, fsys fs.FS, pattern, target string) {
	t.Helper()
	if m, err := fs.Glob(fsys, pattern); err == nil && len(m) > 0 {
		return
	}
	if os.Getenv(RequireEnv) != "" {
		t.Fatalf("no %s and %s is set: `make %s` must run first", pattern, RequireEnv, target)
	}
	t.Skipf("no %s: run `make %s` to fetch it", pattern, target)
}

// ReadFile is Require for a single file, which it returns.
func ReadFile(t testing.TB, fsys fs.FS, name, target string) []byte {
	t.Helper()
	Require(t, fsys, name, target)
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
