package abi

import _ "embed"

// AsyncScenariosJSON / AsyncOracleGoldenJSON hand the async trace-oracle's
// shared testdata to package instance (internal/component/instance's
// async_oracle_test.go), which cannot embed these files itself: Go's
// //go:embed patterns may not cross a package directory boundary (no
// "../abi/testdata/..."), and duplicating the golden file would fork the
// single source of truth gen_async_oracle.py writes to. Exporting them from
// here -- the abi package instance already imports -- keeps exactly one copy
// on disk while still giving the instance package's tests the same CWD-
// independent embed guarantee oracle_embed_test.go documents (CI's
// scratch/BSD jobs run the compiled test binary from the repo root, where a
// relative os.ReadFile can't find package-relative testdata).
//
// See docs/component-model-async-oracle-design.md §0 for the three-artifact
// layout this file bridges.

//go:embed testdata/async_scenarios.json
var AsyncScenariosJSON []byte

//go:embed testdata/async_oracle_golden.json
var AsyncOracleGoldenJSON []byte
