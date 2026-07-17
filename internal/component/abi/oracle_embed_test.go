package abi

import "embed"

// oracleTestdata bundles the differential-oracle golden JSON into the test
// binary. The scratch/BSD CI jobs run the compiled test binary from the repo
// root (not the package dir), so a relative os.ReadFile("testdata/...") can't
// find these; embedding makes them available regardless of CWD.
//
//go:embed testdata/oracle_types.json testdata/oracle_golden.json testdata/oracle_flat.json testdata/oracle_flat_golden.json testdata/oracle_values.json testdata/oracle_values_golden.json
var oracleTestdata embed.FS
