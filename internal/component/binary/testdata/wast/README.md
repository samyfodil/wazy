# Fetched validation suites

`make build.component_testdata` fetches these `.wast` files at the commits the
Makefile pins, and turns each into what `valuesize_test.go` reads with
`wasm-tools json-from-wast` (1.262.0): a `.json` manifest and one `.wasm` per
component. None of it is committed.

- `component-model-max-value-size/` -- `test/validation/max-value-size.wast`
  from [WebAssembly/component-model](https://github.com/WebAssembly/component-model).
- `wasm-tools-max-value-size/` -- `tests/cli/component-model/max-value-size.wast`
  from [bytecodealliance/wasm-tools](https://github.com/bytecodealliance/wasm-tools).

Both check Binary.md's rule that every value type's Canonical ABI size, with
64-bit pointers, is under 2^28 bytes -- the bound a fixed-length list makes
reachable.
