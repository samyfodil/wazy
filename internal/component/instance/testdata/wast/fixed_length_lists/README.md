# fixed_length_lists

`fixed_length_lists.wast` is verbatim from Wasmtime's own test suite,
[bytecodealliance/wasmtime@ea4d2506](https://github.com/bytecodealliance/wasmtime/blob/ea4d25060538735999f24400760d3eec949a408e/tests/misc_testsuite/component-model/fixed_length_lists.wast)
(`tests/misc_testsuite/component-model/`): two components built with C++ and
wit-bindgen, one calling the other's `nested-roundtrip` with
`list<list<u32, 2>, 2>` and `list<list<s32, 2>, 2>` across a fused adapter,
returning how many elements came back wrong.

`fixed_length_lists.json` and `fixed_length_lists.0.wasm` are what
`wasm-tools json-from-wast` (1.262.0, in `ghcr.io/samyfodil/wazy-build-c:34.0`)
makes of it, the form `wast_conformance_test.go` runs.

Licensed Apache-2.0 WITH LLVM-exception, per Wasmtime; see `LICENSE`.
