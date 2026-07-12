// Package vswazero contains a benchmark suite comparing wazy
// (github.com/samyfodil/wazy) against upstream wazero
// (github.com/tetratelabs/wazero), pinned at the exact commit wazy forked
// from. All benchmarks and tests live in _test.go files; this file exists so
// the package has a non-test compilation unit (and a home for the package
// doc). See README.md for what it measures and how to run it.
//
// # Naming scheme (benchstat-friendly)
//
// Every benchmark encodes the runtime under test in a `/runtime=<name>`
// sub-benchmark segment, and any other dimensions as further `/key=value`
// segments. Both runtimes are exercised in a single run, so a single
// `go test -bench` invocation produces pairs such as:
//
//	BenchmarkHostCall/host=typed/op=Call/runtime=wazy
//	BenchmarkHostCall/host=typed/op=Call/runtime=wazero
//
// Feed the output to `benchstat -col /runtime out.txt` to get wazy and wazero
// side by side in adjacent columns for each row.
package vswazero
