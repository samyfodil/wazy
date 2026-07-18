package instance

import "embed"

// wastAsyncFS bundles the vendored official async component-model conformance
// suites into the test binary. Mirrors wastFS (wast_conformance_embed_test.go)
// -- see its doc for why an embed rather than os.ReadFile is needed (CI runs
// the compiled test binary from the repo root, where relative testdata paths
// spelled as os.ReadFile("testdata/wast-async/...") wouldn't resolve).
//
//go:embed testdata/wast-async
var wastAsyncFS embed.FS
