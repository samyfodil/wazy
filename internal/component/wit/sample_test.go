package wit

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/upstream"
)

// sample reads one of the sample files. random.wit and streams.wit are WASI's
// own, which `make build.wasi_testdata` fetches rather than the tree
// committing them.
func sample(t *testing.T, name string) []byte {
	t.Helper()
	return upstream.ReadFile(t, samples, "testdata/"+name, "build.wasi_testdata")
}
