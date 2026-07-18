# Component Model & async

This example runs five things through the [`component`](../../component) package
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
4. **A thread** — `thread.wasm` (source: [`testdata/thread.wat`](testdata/thread.wat))
   spawns a Component Model thread with `thread.new-indirect` and hands control to
   it with `thread.yield-then-resume`; the worker thread resolves the task. It is
   still a single blocking `Call` from Go.
5. **CallAsync** — `await_import.wasm` awaits an async import `get() -> u32`. The
   host registers it with `component.WithAsyncImport` and completes it **from
   another goroutine** (real I/O in a real host). `inst.CallAsync` returns a
   `PendingCall` the moment the guest parks; `PendingCall.Await` resumes it once
   the external `Resolve` lands — the flow a blocking `Call` cannot express.

Run it:

```sh
go run .
```

```
component:adder/calc add(2, 3) = 5
wasi:cli hello: hello world
async run-async() = 42
thread (spawn + resume) = 99
callasync run-async() = 42 (was parked until the external goroutine resolved)
```

The `.wasm` files under [`testdata`](testdata) are the same real component
binaries exercised by wazy's own conformance tests.
