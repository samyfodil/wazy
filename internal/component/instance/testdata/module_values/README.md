# core modules and components as values, by hand

Hand-written components for what the vendored wasmtime-nested and
wasm-tools-instantiate suites cannot show: an inline instance bundle passed
to a nested component that has code of its own (the suites only pass one to
a component with none). Each `.wat` says what it is; the `.wasm` beside it is
what `wasm-tools parse` (1.253.0) encodes.

- `bundle_arg` carries a core module and a sibling's func into a nested
  component, which instantiates the module with the func lowered as its
  import.
- `bundle_arg_unresolved` carries a func aliased out of a pure re-export
  shim, which wazy never instantiates, and is refused for it.
