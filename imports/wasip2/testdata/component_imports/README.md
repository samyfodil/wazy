# component imports, from a real composer

`composed.wasm` is what [wac](https://github.com/bytecodealliance/wac)
(wac-cli 0.10.1) makes of `compose.wac` when told to import its dependencies
rather than embed them:

```sh
wac compose compose.wac \
  --dep wazy:provider=provider.wasm --dep wazy:consumer=consumer.wasm \
  --import-dependencies -o composed.wasm
```

It imports each dependency as a component -- `(import
"unlocked-dep=<wazy:provider>" (component ...))` and the consumer's likewise,
each with the component type of the binary it was composed against -- and
instantiates them itself: the provider with the WASI instances the composition
imports, the consumer with the provider's `wazy:compose/greeter@0.1.0`. It
exports the consumer's `run`.

`provider.wasm` and `consumer.wasm` are the Rust provider and the C consumer
of `examples/compose` (built as their READMEs say), copied here so the test
can embed them. `component_imports_test.go` supplies them for the two imports
with `component.WithComponentImport` and calls `run`.
