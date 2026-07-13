# wazy

[![Go Reference](https://pkg.go.dev/badge/github.com/samyfodil/wazy.svg)](https://pkg.go.dev/github.com/samyfodil/wazy) [![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

A fast WebAssembly runtime for Go: zero dependencies, no CGO, pure Go.

wazy embeds WebAssembly in your Go application. Run code compiled from Rust, C, C++, TinyGo, Zig, and anything else that targets Wasm. No external toolchain. No cgo. Nothing to install at runtime. It is built for speed and developed aggressively.

```bash
go get github.com/samyfodil/wazy@latest
```

wazy is compliant with the WebAssembly Core Specification [1.0][1] and [2.0][2]. It runs on any Go target, with an optimizing native compiler on amd64 and arm64, and a pure-Go interpreter everywhere else.

## Fast

wazy is measurably faster than [wazero][wazero], the runtime it descends from, on the paths that set real throughput and latency. Measured against upstream on the same hardware:

- **Host calls up to ~15x faster**, with zero allocations. Calling a Go function from Wasm is the hot path for WASI and for any host API you expose. wazy's typed host functions run at native-call speed.
- **Compiled execution 4–18% faster** on real TinyGo workloads (geomean ~6% vs `wazero@main`). Memory-heavy code leads — string manipulation −18%, array reversal −14%, base64 −12% — with recursive fibonacci −4%. The compiler also allocates less per module (up to −17% on real Rust/Zig/C output).
- **Interruptible loops stay cheap.** `WithCloseOnContextDone` (deadline and cancellation enforcement) amortizes its per-loop safety check instead of paying a Go round-trip on every iteration. On a realistic loop that calls a host function each iteration it adds **~5%** (vs upstream's ~75%); on the real fibonacci it runs **~12x faster** than upstream, and on a tight compute loop **~13x**. The overhead scales to the loop body, so only near-empty compute kernels pay a real tax — tunable per compile with `wazy.WithInterruptCheckInterval`. Opt in with `WithCloseOnContextDone(true)` when embedding untrusted or unbounded guests.
- **Cold start**: decode, validate, compile, instantiate, substantially faster, with far fewer allocations.
- **Interpreter ~30% faster**, with per-call heap allocation eliminated. A benchmark that allocated 1.35M times now allocates twice.
- **~87% less memory per call** for the common request-per-call pattern.

Methodology and per-optimization numbers are in [OPTIMIZATIONS.md](OPTIMIZATIONS.md). The head-to-head suite lives in [`benchmarks/vs-wazero`](benchmarks/vs-wazero) — `cd benchmarks/vs-wazero && go test -bench .` runs the same workloads (compile, execution, host calls) on wazy and upstream side by side.

The host-call speedup comes from dropping reflection. Instead of the usual `reflect`-per-call path, which is ~14x slower, typed generic helpers derive the Wasm signature from Go's types at compile time:

```go
wazy.HostFunc2(builder, func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
	return x + y
}).Export("add")
```

`HostFunc0`–`HostFunc8` and `HostProc0`–`HostProc8` cover most functions. `WithGoModuleFunction` handles the rest. All zero-allocation.

## Moving fast

wazy is an actively developed performance fork. It is built for where WebAssembly is going: WASI 0.3 and the Component Model, which upstream does not target.

That choice has a cost. wazy makes no API-stability promise. It has already broken compatibility with wazero, including host-function registration, and will do so again when that makes the runtime faster or moves it toward the Component Model.

If you want a mature, stability-guaranteed runtime with a large user base, use [wazero][wazero]. If you want a fast runtime moving toward the modern Wasm platform, use wazy.

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
