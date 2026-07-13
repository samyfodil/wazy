package v2

import "embed"

// Testcases is exported for testing native in internal/engine/native.
//
//go:embed testdata/*.wasm
//go:embed testdata/*.json
var Testcases embed.FS
