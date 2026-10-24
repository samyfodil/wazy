# wasm-tools fixed-length-lists

`fixed-length-lists.wast` is verbatim from wasm-tools' own test suite,
[bytecodealliance/wasm-tools@2e6b2d6d](https://github.com/bytecodealliance/wasm-tools/blob/2e6b2d6db499501a91a0b88a848435345bef1f66/tests/cli/component-model/fixed-length-lists.wast)
(`tests/cli/component-model/`): a component lifting a `list<u32, 4>` result
from memory and one lowering a `list<u32, 4>` param into four `i32`s, and the
components a validator must refuse. The `.json`, `.wasm` and `.wat` files are
what `wasm-tools json-from-wast` (1.262.0, in
`ghcr.io/samyfodil/wazy-build-c:34.0`) makes of it, the form
`fixedlist_wast_test.go` runs.

Licensed Apache-2.0 WITH LLVM-exception, or Apache-2.0, or MIT, per
wasm-tools; see `LICENSE-Apache-2.0_WITH_LLVM-exception` and `LICENSE-MIT`.
