package v2

import "embed"

// Testcases is exported for testing native in internal/engine/native.
//
//go:embed all:testdata
var Testcases embed.FS
