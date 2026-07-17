package instance

import "embed"

// wastFS bundles the vendored component-model conformance suites into the test
// binary. The scratch/BSD CI jobs run the compiled binary from the repo root,
// where a relative os.ReadFile("testdata/wast/...") can't find these.
//
//go:embed testdata/wast
var wastFS embed.FS
