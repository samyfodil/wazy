# wazy

[![Go Reference](https://pkg.go.dev/badge/github.com/samyfodil/wazy.svg)](https://pkg.go.dev/github.com/samyfodil/wazy) [![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

A fast WebAssembly runtime for Go: zero dependencies, no CGO, pure Go — with execution on par with [wasmtime][wasmtime], plus the Component Model, WASI 0.2, and the WASI 0.3 async ABI running today.

wazy embeds WebAssembly in your Go application. Run code compiled from Rust, C, C++, TinyGo, Zig, and anything else that targets Wasm. No external toolchain. No cgo. Nothing to install at runtime. It is built for speed and developed aggressively — pure-Go convenience without giving up native-runtime performance, and it targets the modern Wasm platform: core modules, [components][cm], WASI 0.2, and the WASI 0.3 async ABI ([below](#async--the-component-model-async-abi-wasi-03)).

```bash
go get github.com/samyfodil/wazy@latest
```

wazy is compliant with the WebAssembly Core Specification [1.0][1] and [2.0][2]. It runs on any Go target, with an optimizing native compiler on amd64 and arm64, and a pure-Go interpreter everywhere else.

## Fast

wazy is faster than [wazero][wazero], the runtime it descends from, on the paths that set real throughput and latency — and its native compiler runs compute code on par with [wasmtime][wasmtime]'s Cranelift, in pure Go with no CGO. Measured against upstream on the same hardware:

- **Host calls up to ~15x faster**, with zero allocations. Calling a Go function from Wasm is the hot path for WASI and for any host API you expose. wazy's typed host functions run at native-call speed.
- **Compiled execution 4–18% faster** on real TinyGo workloads (geomean ~6% vs `wazero@main`). Memory-heavy code leads — string manipulation −18%, array reversal −14%, base64 −12% — with recursive fibonacci −4%. The compiler also allocates less per module (up to −17% on real Rust/Zig/C output).
- **Interruptible loops stay cheap.** `WithCloseOnContextDone` (deadline and cancellation enforcement) amortizes its per-loop safety check instead of paying a Go round-trip on every iteration. On a realistic loop that calls a host function each iteration it adds **~5%** (vs upstream's ~75%); on the real fibonacci it runs **~12x faster** than upstream, and on a tight compute loop **~13x**. The overhead scales to the loop body, so only near-empty compute kernels pay a real tax — tunable per compile with `wazy.WithInterruptCheckInterval`. Opt in with `WithCloseOnContextDone(true)` when embedding untrusted or unbounded guests.
- **Cold start**: decode, validate, compile, instantiate, substantially faster, with far fewer allocations.
- **Interpreter ~30% faster**, with per-call heap allocation eliminated. A benchmark that allocated 1.35M times now allocates twice.
- **~87% less memory per call** for the common request-per-call pattern.

Methodology and per-optimization numbers are in [OPTIMIZATIONS.md](OPTIMIZATIONS.md). The head-to-head suite lives in [`benchmarks/vs-wazero`](benchmarks/vs-wazero) — `cd benchmarks/vs-wazero && go test -bench .` runs the same workloads (compile, execution, host calls) on wazy and upstream side by side. The same harness carries a **three-way comparison against wasmtime** (`BenchmarkExecute3`, `BenchmarkCompile3`): run it yourself and see wazy's native execution hold its own against Cranelift on the compute kernels — no numbers to take on faith.

The host-call speedup comes from dropping reflection. Instead of the usual `reflect`-per-call path, which is ~14x slower, typed generic helpers derive the Wasm signature from Go's types at compile time:

```go
wazy.HostFunc2(builder, func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
	return x + y
}).Export("add")
```

`HostFunc0`–`HostFunc8` and `HostProc0`–`HostProc8` cover most functions. `WithGoModuleFunction` handles the rest. All zero-allocation.

## The Component Model and WASI 0.2

wazy runs [WebAssembly Component Model][cm] components and [WASI 0.2][wasi] — genuine `wasm32-wasip2` binaries (rustc, wasm-tools), not just core modules and not hand-written `.wat`. Upstream [wazero][wazero] targets neither. This works today, exercised by real rustc components and the official component-model test suites:

- **The Canonical ABI** — lift and lower for every value type (primitives, `string`, `list`, `record`, `variant`, `enum`, `flags`, `option`, `result`, `tuple`) and `own`/`borrow` resource handles (drop/rep, cross-instance borrows), verified byte-for-byte against the `wasm-tools` value suites.
- **WASI 0.2 host interfaces** — `wasi:cli` (run, stdio, environment, exit), `wasi:clocks`, `wasi:filesystem`, `wasi:io` (streams, poll, error), `wasi:random`, `wasi:sockets` (TCP/UDP and DNS), and `wasi:http` (both the incoming-handler server and the outgoing-handler client).
- **Multi-module component graphs** — nested instances, canonical lowering of host imports, resource lifetimes, and the wasip2 adapter wiring, so a real rustc `wasi:cli/command` runs end to end and prints `hello world`.

Embed and run a component through the [`component`](component) package:

```go
r := wazy.NewRuntime(ctx)
defer r.Close(ctx)

inst, err := component.Instantiate(ctx, r, componentWasm,
	component.WithWASI(component.WASIConfig{Stdout: os.Stdout})...)
if err != nil {
	return err
}
defer inst.Close(ctx)

// A wasi:cli/command component: run its entry point.
_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
```

Call an interface export directly with `inst.CallExport("component:adder/calc", "add", uint32(2), uint32(3))`, or serve a `wasi:http/incoming-handler` component straight to `net/http` — `*component.Instance` satisfies `http.Handler`. The API is young and, like the rest of wazy, makes no stability promise.

## Async — the Component Model async ABI (WASI 0.3)

wazy runs the Component Model's async ABI: components that suspend, await, and resume — the model [WASI 0.3][wasi] is built on. Upstream [wazero][wazero] has none of it, and no other pure-Go runtime does either.

- **Callback and stackful lift** — both async lift shapes. A guest task that returns WAIT/YIELD is driven by a deterministic per-composition scheduler; a stackful task suspends on a goroutine with an unbuffered-channel baton, so exactly one runs at a time — race-free by construction, verified under `-race`.
- **Streams and futures** — `stream<T>`/`future<T>` with rendezvous copy and per-element `own<R>` resource transfer, synchronous and asynchronous read/write, and cancellation.
- **Task lifecycle** — subtasks, cancellation, backpressure, context-local storage, and borrow scopes that hold across async calls.
- **`thread.*`** — a cooperative fiber runtime (`thread.new-indirect`, `yield`, `suspend`, `yield-then-resume`) built on the same goroutine-plus-baton primitive.

It passes **all 31 official Component Model async `.wast` conformance suites** (one carries a fixture fix filed upstream as [component-model#679][pr679]), cross-checked by a differential trace-oracle that byte-compares wazy against the spec reference (`definitions.py`). Goroutines and channels back futures, streams, and threads naturally — the one place Go's substrate is an asset over the hand-written event loops other runtimes need.

## Moving fast

wazy targets the modern Wasm platform (Component Model, WASI 0.2, the async ABI today, above; the full WASI 0.3 host-interface surface next) and lands performance work continuously rather than waiting on a release cadence. [wazero][wazero], the runtime it descends from, is a mature, well-run project that prioritizes API stability and a scope centered on core modules and WASI 0.1 — the right call for its large user base. wazy makes different trade-offs.

Two things make that pace possible:

- **We ship fast.** wazy makes no API-stability promise. It has already broken compatibility with wazero, including host-function registration, and will do so again whenever that makes the runtime faster or moves it toward the Component Model. Correctness is guarded by the conformance and fuzzing suites, not by freezing the API.
- **We accept AI contributions.** Machine-generated changes are welcome on equal footing with human ones — the bar is the same for both: they pass the full spec-conformance, differential, and fuzzing suites, and they make the runtime measurably better.

## Two engines

`wazy.NewRuntime(ctx)` picks the optimizing compiler when the platform supports it, amd64 or arm64, and falls back to the interpreter otherwise. You can force either:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
```

- **Compiler** translates each module to machine code during `CompileModule`, so your functions run natively, typically an order of magnitude faster than interpretation, with no host-specific dependencies.
- **Interpreter** is pure Go with no architecture-specific code, so it runs anywhere Go runs, down to targets like `riscv64`.

## Example

The fastest way in is an [example](examples/README.md). The [basic one](examples/basic) extends a Go program with an addition function written in WebAssembly.

## Credit

wazy started from [wazero][wazero]'s code (Copyright 2020-2023 wazero authors) and still draws on its WebAssembly semantics, WASI implementation, and compliance and fuzzing test suites. We do not intend to keep wazero's API compatibility or its architecture. The goals are pure Go, performance, and conformance to the standard. See [RATIONALE.md](RATIONALE.md) for wazero's original design rationale and [LICENSE](LICENSE) for the Apache 2.0 license.

## License

Apache 2.0. See [LICENSE](LICENSE).

[1]: https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/
[2]: https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/
[wazero]: https://github.com/tetratelabs/wazero
[wasmtime]: https://github.com/bytecodealliance/wasmtime
[cm]: https://component-model.bytecodealliance.org/
[wasi]: https://wasi.dev/
[pr679]: https://github.com/WebAssembly/component-model/pull/679
