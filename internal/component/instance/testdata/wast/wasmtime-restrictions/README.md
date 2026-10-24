# wasmtime restrictions

`restrictions.wast` is verbatim from Wasmtime's own test suite,
[bytecodealliance/wasmtime@efcc95eb](https://github.com/bytecodealliance/wasmtime/blob/efcc95eb2e9864a924ce4385b6932ae99e37e8dd/tests/misc_testsuite/component-model/restrictions.wast)
(`tests/misc_testsuite/component-model/`): what Wasmtime refuses to run --
among it, any component-typed import of the root component, which wazy runs
when the embedder supplies a component for it (`WithComponentImport`). The
`.json` and `.wasm` files are what `wasm-tools json-from-wast` (1.262.0, in
`ghcr.io/samyfodil/wazy-build-c:34.0`) makes of it, the form
`wast_validation_test.go` runs.

Licensed Apache-2.0 WITH LLVM-exception, per Wasmtime; see `LICENSE`.
