package v1

import (
	"embed"
)

// Testcases is exported for cross-process file cache tests.
//
//go:embed all:testdata
var Testcases embed.FS
