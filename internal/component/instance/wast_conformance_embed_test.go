package instance

import (
	"embed"
	"io/fs"
	"os"
	"testing"
)

// wastFS bundles the vendored component-model conformance suites into the test
// binary. The scratch/BSD CI jobs run the compiled binary from the repo root,
// where a relative os.ReadFile("testdata/wast/...") can't find these. It also
// bundles whichever suites `make build.component_testdata` has fetched.
//
//go:embed testdata/wast
var wastFS embed.FS

// fetched skips t when dir, which `make build.component_testdata` makes, is
// not there.
func fetched(t *testing.T, dir string) {
	t.Helper()
	if _, err := fs.Stat(wastFS, dir); err != nil {
		// Skipping is right for a working tree that has not fetched it; in CI
		// it would mean the suite silently never ran, so the jobs set this.
		if os.Getenv("WAZY_SPECTEST_REQUIRE_CASES") != "" {
			t.Fatalf("no %s and WAZY_SPECTEST_REQUIRE_CASES is set: `make build.component_testdata` must run first (%v)", dir, err)
		}
		t.Skipf("no %s: run `make build.component_testdata` to fetch it (%v)", dir, err)
	}
}
