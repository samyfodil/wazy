package binary

import "embed"

// fixtureFS bundles the component .wasm fixtures into the test binary. The
// scratch/BSD CI jobs run the compiled binary from the repo root, where a
// relative os.ReadFile("testdata/...") can't find them.
//
//go:embed testdata/*.wasm
var fixtureFS embed.FS
