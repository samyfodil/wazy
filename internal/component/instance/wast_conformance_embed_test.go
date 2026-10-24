package instance

import (
	"embed"
	"io/fs"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/upstream"
)

// wastFS bundles the component-model conformance suites into the test binary.
// The scratch/BSD CI jobs run the compiled binary from the repo root, where a
// relative os.ReadFile("testdata/wast/...") can't find these. The suites are
// fetched by `make build.component_testdata` rather than committed.
//
//go:embed all:testdata/wast
var wastFS embed.FS

// fetched skips t when dir, which `make build.component_testdata` makes, is
// not there.
func fetched(t *testing.T, dir string) {
	t.Helper()
	requireFetched(t, wastFS, dir)
}

func requireFetched(t *testing.T, fsys fs.FS, dir string) {
	t.Helper()
	upstream.Require(t, fsys, dir, "build.component_testdata")
}
