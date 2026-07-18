# Component Model & async

This example runs three things through the [`component`](../../component) package
that upstream wazero does not support:

1. **An interface export** — instantiate `adder.wasm` and call
   `component:adder/calc#add(2, 3)`, passing and receiving lifted Component Model
   values.
2. **A WASI 0.2 command** — a genuine rustc `wasm32-wasip2` `wasi:cli/command`
   (`hello.wasm`) with WASI stdio wired up, run to print `hello world`.
3. **An async export** — `async_first_light.wasm` exports `run-async`, a
   Component Model *async* (callback-ABI) export. From the embedder it is called
   exactly like a synchronous export; the async scheduler is driven transparently
   underneath, and the call returns once the task completes.

Run it:

```sh
go run .
```

```
component:adder/calc add(2, 3) = 5
wasi:cli hello: hello world
async run-async() = 42
```

The `.wasm` files under [`testdata`](testdata) are the same real component
binaries exercised by wazy's own conformance tests.
