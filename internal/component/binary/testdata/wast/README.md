# Vendored validation suites

Verbatim `.wast` files, with what `wasm-tools json-from-wast` (1.262.0, in
`ghcr.io/samyfodil/wazy-build-c:34.0`) makes of them -- a `.json` manifest and
one `.wasm` per component -- for `valuesize_test.go`:

- `component-model-max-value-size/` -- `test/validation/max-value-size.wast`
  from [WebAssembly/component-model@d095e2f6](https://github.com/WebAssembly/component-model/blob/d095e2f60ddae1f9a1e9b1e2fb547ec1114da677/test/validation/max-value-size.wast);
  Apache-2.0, see its `LICENSE` and `LICENSE-APACHE`.
- `wasm-tools-max-value-size/` -- `tests/cli/component-model/max-value-size.wast`
  from [bytecodealliance/wasm-tools@2e6b2d6d](https://github.com/bytecodealliance/wasm-tools/blob/2e6b2d6db499501a91a0b88a848435345bef1f66/tests/cli/component-model/max-value-size.wast);
  Apache-2.0 WITH LLVM-exception, or Apache-2.0, or MIT, see its `LICENSE-*`.

Both check Binary.md's rule that every value type's Canonical ABI size, with
64-bit pointers, is under 2^28 bytes -- the bound a fixed-length list makes
reachable.
