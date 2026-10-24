package instance

import "embed"

// wastAsyncFS bundles the official async component-model conformance suites
// into the test binary. Mirrors wastFS (wast_conformance_embed_test.go) -- see
// its doc for why an embed rather than os.ReadFile is needed, and for where the
// suites come from.
//
//go:embed all:testdata/wast-async
var wastAsyncFS embed.FS
