# Reference WASI 0.2 WIT

Verbatim copies of the WASI Preview 2 interface definitions, taken from the
`wasip2` crate (`wasip2-1.0.4+wasi-0.2.12/wit/deps/`), which vendors them
from [WebAssembly/WASI](https://github.com/WebAssembly/WASI). They are the
*reference* — nothing here is generated from, or derived by, wazy.

`wasi_conformance_test.go` parses these and checks every WASI host import
wazy registers against them, so a typo'd or drifted function name cannot
sit unnoticed behind the graph engine's trap-stub fallback.

Do not hand-edit. To update, re-copy from a newer `wasip2` crate and re-run
the conformance test; a diff in the reported gaps is the point.

Licensed Apache-2.0 WITH LLVM-exception / MIT, per the upstream WASI
repository (see `LICENSE-Apache-2.0_WITH_LLVM-exception` and `LICENSE-MIT`
in the `wasip2` crate).
