# core type check fixtures

Core modules the core half of the component type check (`ctcore.go`,
`ctenv_core.go`, `checkResourceDtors`) is tested against where the vendored
suites do not reach. Each `.wat` says what it is; the `.wasm` beside it is
what `wasm-tools parse` (1.262.0, in `ghcr.io/samyfodil/wazy-build-c:34.0`)
encodes.

- `kinds` imports and exports one item of every core extern sort, and
  exports items that are not there.
- `dtors` holds destructor candidates of a right and a wrong type.
