# wasm-tools instantiate

`instantiate.wast` is verbatim from wasm-tools' own test suite,
[bytecodealliance/wasm-tools@2e6b2d6d](https://github.com/bytecodealliance/wasm-tools/blob/2e6b2d6db499501a91a0b88a848435345bef1f66/tests/cli/component-model/instantiate.wast)
(`tests/cli/component-model/`): components importing components and
instantiating them with arguments of every sort, against the imported
component's declared type, plus the instantiations a validator must refuse.
The `.json` and `.wasm` files are what `wasm-tools json-from-wast` (1.262.0,
in `ghcr.io/samyfodil/wazy-build-c:34.0`) makes of it, the form
`wast_validation_test.go` runs.

Licensed Apache-2.0 WITH LLVM-exception, or Apache-2.0, or MIT, per
wasm-tools; see `LICENSE-Apache-2.0_WITH_LLVM-exception` and `LICENSE-MIT`.
