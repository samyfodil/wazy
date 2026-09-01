<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/wazy-logo-dark.svg">
    <img alt="wazy" src=".github/assets/wazy-logo-light.svg" width="360">
  </picture>
</p>

<div align="center">

[![Go Reference](https://pkg.go.dev/badge/github.com/samyfodil/wazy.svg)](https://pkg.go.dev/github.com/samyfodil/wazy) [![Test](https://github.com/samyfodil/wazy/actions/workflows/commit.yaml/badge.svg)](https://github.com/samyfodil/wazy/actions/workflows/commit.yaml) [![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

[**Docs**](https://pkg.go.dev/github.com/samyfodil/wazy) · [**Examples**](examples) · [**Components**](examples/component) · [**Benchmarks**](#benchmarks) · [**Optimizations**](OPTIMIZATIONS.md)

</div>

**A WebAssembly runtime written in pure Go** — guest code from other languages, sandboxed, inside your process, with nothing to install at runtime.

**Run code you didn't write.** Ship a plugin system, execute a customer's untrusted script, or use a Rust or C library you would otherwise reach through cgo: compiled to Wasm it becomes a byte slice you `go:embed` and a function you call. The guest starts with nothing — no files beyond what you mount, no sockets unless you allow them, a fake clock on the WASI 0.1 path — you cap its memory with `WithMemoryLimitPages`, and a `context` deadline kills even a tight compiled loop with `WithCloseOnContextDone`. CI builds and runs guests from Rust, C, C++, Zig, TinyGo, AssemblyScript and Go, `CGO_ENABLED=0`, with no dependency beyond `golang.org/x/sys`.

**Plugin contracts with types in them.** Embedding Wasm normally means `fn.Call(ctx, ptr, len)`, decoding a pair of i32s out of the guest's memory, and both halves hand-agreeing on layout and on who frees what. wazy runs WebAssembly *components* — a `.wasm` that carries its own typed interface — so `string`, records, lists, `option` and `result` cross as Go values, and a handle to a host object is a lifetime the runtime tracks and drops for you the moment you hand it a destructor.

Declare the contract in a small `.wit` interface file and implement your half in ordinary Go; the other half is anything that compiles to Wasm. A binary a stranger produced with `cargo build --target wasm32-wasip2` runs unmodified, on the **114 WASI 0.2 host functions** wazy implements: stdio, args and env, clocks, a mounted filesystem, randomness, TCP/UDP and DNS, HTTP in both directions. Sockets and HTTP stay unregistered until you switch them on; the filesystem hands over nothing until you mount something.

**Guests that suspend and resume.** wazy runs the **WASI 0.3 async ABI**: `stream<T>` and `future<T>`, subtasks, cancellation, backpressure and cooperative threads, with goroutines and channels as the substrate. It passes all 31 official async conformance suites. You call an async export exactly like a synchronous one — the scheduler runs underneath and `Call` returns when the task completes.

**Cheap.** Instantiating a compiled 37 KB module takes 1.7 µs and 3.3 KB of heap, so every request gets its own fresh linear memory instead of a pooled one you scrub between calls. A host call costs 48 ns and allocates nothing on the stack-reusing path, so your host API stays fine-grained instead of batching to hide the boundary. wazy serves a warm `wasi:http` component — a guest that answers HTTP requests — through `net/http` in 3.37 µs and 45 allocations; two orders of magnitude up, a 6.5 MB Rust document converter turns a 7.5 MB PDF in 0.75 s inside a pure-Go host ([go-anydoc][anydoc]'s measurement, [#29][i29]). Every first-party figure is a benchmark in this repo: commands under [Benchmarks](#benchmarks), method and the optimizations **measured and rejected** in [OPTIMIZATIONS.md](OPTIMIZATIONS.md).

## Run it

```bash
go get github.com/samyfodil/wazy@latest
```

```go
package main

import (
	"context"
	_ "embed"
	"os"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/imports/wasip2"
)

// A genuine rustc component: cargo build --target wasm32-wasip2
//
//go:embed testdata/hello.wasm
var helloWasm []byte

func main() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx) // Closes everything this runtime created.

	// Stdout, and nothing else: no files mounted, no sockets registered.
	inst, err := component.Instantiate(ctx, r, helloWasm,
		wasip2.With(wasip2.Config{Stdout: os.Stdout})...)
	if err != nil {
		panic(err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		panic(err)
	}
}
```

That is [`examples/component`](examples/component), minus the error handling. Values cross typed, so a component exporting `run: func() -> string` hands back a Go `string` rather than a pointer and a length:

```go
out, err := inst.Call(ctx, "run")
fmt.Println(out[0].(string))
```

Declare that contract yourself in a `.wit` file and implement the host half in Go: [`examples/custom-wit`](examples/custom-wit). Plain core modules and WASI 0.1 work the same way, through `r.Instantiate` and `mod.ExportedFunction` — [`examples/basic`](examples/basic). More in [`examples/`](examples/README.md).

## Why run Wasm inside your Go process?

The library you need exists only in Rust or C, or the code you must run is your customers'. Today that means cgo, a subprocess, a container, an embedded interpreter, or trust.

| Instead of | It costs | wazy |
| --- | --- | --- |
| **cgo**, to reach a Rust or C library | `CGO_ENABLED=0` and cross-compilation; the race detector and pprof stop at the library boundary; a bad pointer is your process's | The same library as one `.wasm`. [go-anydoc][anydoc] converts a 7.5 MB PDF through a Rust crate and still cross-compiles to `s390x` |
| **A subprocess, an RPC plugin, or a container per request** | a second artifact per OS/arch, a process or image to supervise, a serialized round trip per call | One file to version and sign; a fresh sandbox per request, 48 ns per host call, no daemon |
| **An embedded interpreter** (goja, yaegi, Starlark) | one language, forever | Any of the languages above — [one example](imports/wasi_snapshot_preview1/example) runs the same guest built by four toolchains |
| **Trusting the code** | a dependency's `init()` runs with your files, your credentials, your network | Deny by default: nothing mounted, no sockets registered, and a fake WASI 0.1 clock until you hand over a real one |

**Where the alternative still wins.** cgo, when the library must spawn OS threads of its own: wazy exposes shared memory and atomics (`experimental.CoreFeaturesThreads`) but not `wasi-threads` spawn — or the host machine itself. A container or microVM, when you need kernel-enforced CPU quotas per tenant: wazy caps memory and enforces a deadline, but there is no fuel metering. Starlark, when the code is config-shaped and your authors will not install a Wasm toolchain. Trust, when the code is first-party and what you wanted was extensibility, not safety.

## The Component Model and WASI 0.2

| Standard | |
| --- | :---: |
| Core Wasm 1.0 / 2.0 | ✅ |
| Core Wasm 3.0 | ✅ |
| WASI 0.1 &nbsp;(`wasip1`) | ✅ |
| WASI 0.2 · Component Model &nbsp;(`wasip2`) | ✅ |
| WASI 0.3 async ABI &nbsp;(`stream`, `future`, tasks, threads) | ✅ |

Core Wasm 3.0 means all eight proposals it folded in: tail calls, extended const, exception handling, typed function references, relaxed SIMD, multiple memories, memory64 and garbage collection. Turn them on together with `api.CoreFeaturesV3`; the default stays `api.CoreFeaturesV2`.

Components stay components: genuine `wasm32-wasip2` binaries from rustc and wasm-tools, not flattened to core modules or hand-written `.wat`. What the [Component Model][cm] and [WASI 0.2][wasi] layer covers:

- **The Canonical ABI** — the value conversions, both directions, for every type (primitives, `string`, `list`, `record`, `variant`, `enum`, `flags`, `option`, `result`, `tuple`) and `own`/`borrow` resource handles, including drop/rep and cross-instance borrows.
- **WASI 0.2 host interfaces** — `wasi:cli`, `wasi:clocks`, `wasi:filesystem`, `wasi:io`, `wasi:random`, `wasi:sockets`, `wasi:http`; capabilities opt in per instantiation (filesystem preopens, `AllowTCP`, `AllowUDP`, a custom `Dialer`, `EnableHTTP`).
- **Multi-module component graphs** — nested instances, canonical lowering of host imports, resource lifetimes and the wasip2 adapter wiring, so a real rustc `wasi:cli/command` runs end to end and prints `hello world`; with a `CompileCache`, re-instantiating that whole graph costs 350.5 µs.

```go
inst, err := component.Instantiate(ctx, r, helloWasm,
	wasip2.With(wasip2.Config{Stdout: os.Stdout})...)
if err != nil {
	return err
}
defer inst.Close(ctx)

// A wasi:cli/command component: run its entry point.
_, err = inst.Call(ctx, "wasi:cli/run@0.2.3#run")
```

Call an interface export directly with `inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))`, or hand a `wasi:http/incoming-handler` component to `wasip2.Handler(inst)`, which returns an `http.Handler` — somebody else's handler, as a route on your mux. [`examples/custom-wit`](examples/custom-wit) goes the other way: a WIT plugin API, resources and `result<list<string>, variant>` included, implemented host-side in Go.

## Async — the WASI 0.3 async ABI

wazy runs the [WASI 0.3][wasi] async ABI: components that suspend, await, and resume.

- **Callback and stackful lift** — both shapes. A task returning WAIT/YIELD is driven by a deterministic per-composition scheduler; a stackful task suspends on a goroutine with an unbuffered-channel baton, so exactly one runs at a time — race-free by construction, verified under `-race`.
- **Streams, futures, task lifecycle** — `stream<T>`/`future<T>` with rendezvous copy and per-element `own<R>` transfer, sync and async read/write; subtasks, cancellation, backpressure, context-local storage, borrow scopes that hold across async calls.
- **`thread.*`** — a cooperative fiber runtime (`thread.new-indirect`, `yield`, `suspend`, `yield-then-resume`) on that same primitive.

Goroutines and channels back futures, streams and threads, so a parked guest costs one goroutine. You call an async export exactly like a synchronous one — the scheduler runs underneath and `Call` returns when the task completes — while `CallAsync` returns a pending call another goroutine resolves. [`examples/component`](examples/component) runs an async export, a thread-spawning component and a five-thread map-reduce over one shared array. <sub>What runs is the Component Model **async ABI**, the machinery WASI 0.3 is built on; the reworked 0.3 host-interface worlds (`wasi:io`/`wasi:sockets`/`wasi:http` 0.3) land once that spec settles, and no `wasm32-wasip3` target exists yet, here or anywhere.</sub>

## Host functions, typed and allocation-free

Instead of a `reflect`-per-call path, typed generic helpers derive the Wasm signature from Go's types at compile time:

```go
b := r.NewHostModuleBuilder("env")

wazy.HostFunc2(b.NewFunctionBuilder(), func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
	return x + y
}).Export("add")

env, err := b.Instantiate(ctx)
```

`HostFunc0`–`HostFunc16` and `HostProc0`–`HostProc16` cover 16 parameters, one result or none; `WithGoModuleFunction` handles the rest. Dispatch allocates nothing. `Call` allocates the slice it returns results in, so a hot loop wants `CallWithStack` — the 48 ns path.

## CLI and engines

A standalone runner ships in the box — the same binary the wasi-testsuite runs against. It takes core modules and WASI 0.1; components go through the library.

```bash
go install github.com/samyfodil/wazy/cmd/wazy@latest
wazy run -mount=.:/:ro -env-inherit app.wasm arg1 arg2
```

`run` also takes `-listen host:port`, `-timeout`, `-interpreter`, `-cachedir` and `-hostlogging` (`clock,filesystem,memory,proc,poll,random,sock`); `wazy compile` precompiles into the cache ahead of time. Release binaries for Linux, macOS and Windows are built `CGO_ENABLED=0`. `wazy.NewRuntime(ctx)` picks the optimizing compiler on amd64 and arm64 and falls back to the interpreter elsewhere; force either with `wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())`. The compiler translates each module to machine code during `CompileModule` — 4x to 45x faster than the interpreter across the [go-anydoc][anydoc] runs below; the interpreter has no architecture-specific code, so it runs anywhere Go does.

### Platform support

| | |
| --- | --- |
| **Full suite + spec corpora, every commit** | `linux/amd64`, `windows/amd64`, `darwin/arm64`, on two Go versions, plus a `-race -short` run and four fuzz targets — three differential against the interpreter, one for validation. |
| **Test binaries run, every commit** | `linux/arm64` and `linux/amd64` in a `scratch` container (one variant with `PROT_EXEC` denied); FreeBSD, OpenBSD, NetBSD, DragonFly and illumos on amd64. The scratch runs exclude the spectest packages; the BSD and illumos runs also exclude `imports/**`, `sysfs` and the generated 3.0 corpus. |
| **Cross-compiled every commit** | `plan9`, `js/wasm`, `wasip1/wasm`, `aix/ppc64`, `linux/{s390x,ppc64le,arm,386}`, `freebsd/amd64` — they build and run the interpreter; CI does not execute their suites. `riscv64` runs locally under `qemu-riscv64-static`, because the hosted runner never completes. |

On a 32-bit host, linear memory tops out just under two gibibytes rather than the four a 32-bit Wasm memory may declare, bound by what a Go slice holds; a module asking for more is rejected, which the specification allows. If you depend on a target in the lower rows, open an issue and ask for CI coverage.

## Conformance

Correctness here is a pass count against somebody else's suite, not a claim.

| Suite | Source | Result |
| --- | --- | --- |
| Core spec tests | [WebAssembly/testsuite][testsuite] + the proposal repos | 13 corpora, **711 generated case files, no skip list**; the 3.0 corpus runs on **both** engines. |
| Component Model async | the official async `.wast` suites | **31 suites, 0 skipped, 0 failed.** |
| Canonical ABI values | the official `test/values` suites | 7 suites, through `wasm-tools json-from-wast`. Four modules across three of them fail to instantiate — see below. |
| wasi-testsuite | [WebAssembly/wasi-testsuite][wasitest] | AssemblyScript, C and Rust, **no skipped tests**, on Linux, macOS **and** Windows, through the `wazy` CLI. |
| Cross-proposal interaction | wasmtime's `tests/misc_testsuite` | 9 `.wast` vendored from wasmtime, beside 5 written here. |

```bash
go test ./internal/integration_test/spectest/...  # core spec corpora
go test ./internal/component/...                  # component ABI + async .wast
```

**The four Canonical ABI failures are named in the code, not hidden.** `wastKnownSkips` in `internal/component/instance/wast_conformance_test.go` pins `types.11`, `fused.22`, `fused.23` and `resources.14` with the composition gap each represents, and the harness fails if that set changes in *either* direction — a new entry is a regression, a stale one means a gap closed and the list must shrink. It exists because a silent skip once hid a bug ([#25](https://github.com/samyfodil/wazy/issues/25)). A separate workflow runs the Zig standard-library test binary, the Go `wasip1` standard-library tests on two Go versions and libsodium's suite on every push; a differential trace-oracle byte-compares the async runtime against the spec's reference implementation (`definitions.py`). One official async fixture is broken; the 31/31 above runs against a corrected copy, and the fix is filed upstream as [component-model#679][pr679].

## Benchmarks

Run them yourself — the numbers at the top come from these suites, the first a separate module that compiles the same workloads against a pinned wazero:

```bash
cd benchmarks/vs-wazero && go test -bench .    # Instantiate, HostCall, Compile, Execute
go test -bench . ./imports/wasip2/... ./internal/integration_test/bench/...
```

Figures on this page were measured on a core-pinned i9-12900HK (amd64) unless noted; arm64 work is measured on an Apple M4. Method, per-optimization numbers and the optimizations **measured and rejected** (including one that made arm64 8% slower) are in [OPTIMIZATIONS.md](OPTIMIZATIONS.md).

### Head to head with wazero

Measured against wazero in the same runs, on the same workloads:

| Path | wazy vs wazero | What & why |
| --- | :--: | --- |
| **Instantiate** | **9.1x** | 1.724 µs vs 15.74 µs, on a 37 KB TinyGo module. |
| **Interruptible loops** (`WithCloseOnContextDone`) | **12–13x**; +5% vs +75% overhead | The per-loop check is amortized, not a Go round-trip per iteration; tune with `WithInterruptCheckInterval`. Against `wazero@main`. |
| **Compiled execution** | memory-heavy code leads | `string_manipulation` −18%, `reverse_array` −14%, `base64` −12%, `fibonacci` a wash — the advantage tracks memory-access intensity, not arithmetic. |
| **Host calls** (Go ↔ Wasm) | a tie | 48.5 ns here, 48.3 ns there, on the raw stack API both runtimes expose. The win is structural, not per-call: see below. |
| **Cumulative** | geomean **−17.8%**, B/op **−22.9%** | Across `internal/integration_test/bench`, versus the wazero fork point, with upstream wazero as a control in the same runs — its arms stayed flat. |

### What the rewrite bought

These are wazy before and after its own optimization work — not a wazero comparison. They are why the absolute numbers at the top of this page look the way they do:

| Path | Before → after | |
| --- | --- | :--: |
| **Host calls**, reflection removed | 1086 ns / 6 allocs → 74.9 ns / 0 allocs | **14.5x** |
| **`memory.grow`** (in-capacity, amd64) | 2.441 µs → 109.0 ns, 0 allocs/op | **22x** |
| **Interpreter** | a benchmark that allocated 1.35M times now allocates twice | **~30%** |
| **Memory per call** | 11784 B / 3 allocs → 1551 B / 2 allocs | **−87%** |
| **Compile**, real modules | 5 KB Zig 753.3 µs → 556.4 µs; 10 KB Rust 1132.7 µs → 871.3 µs | **−23% to −26%** |

The host-call row is the one worth reading twice. wazy deleted reflection-based registration rather than optimizing it, so the 14.5x is against the path that no longer exists — wazero still ships one. `memory.grow` is opt-in via `WithMemoryCapacityReservePages`; out-of-capacity, shared and imported memories keep the safe Go path.

The head-to-head module also carries a three-way comparison against wasmtime (`BenchmarkExecute3`, `BenchmarkCompile3`, `BenchmarkExecute3Heavy`, `BenchmarkRelaxedSimd`); those arms link `wasmtime-go`, so they need cgo — an irony worth stating on a page selling `CGO_ENABLED=0`.

### At scale: a 6.5 MB Rust module, measured by someone else

The suites above are kernels. [go-anydoc][anydoc] measured two orders of magnitude up: a 6.5 MB `wasm32-wasip1` command module (Rust 1.88, `opt-level = 3`, `wasm-opt -O3`) run as a single instantiate — stdin in, stdout out, a long stretch of compute between. Same `.wasm`, same input, same machine, identical output, `CompileModule` excluded from every timing.

| Converting | wazero v1.12.0 | wazy | |
| --- | :---: | :---: | :---: |
| 1 KB docx, compiled | 0.4 ms | 0.62 ms | **0.6x — wazero wins** |
| 5 MB docx body, compiled | 0.86 s | 0.19 s | 4.5x |
| 7.5 MB PDF, compiled | 3.4 s | 0.75 s | 4.5x |
| 1 KB docx, interpreted | 3.5 ms | 2.7 ms | 1.3x |
| 5 MB docx body, interpreted | 11.1 s | 8.6 s | 1.3x |
| 7.5 MB PDF, interpreted | 41.4 s | 32.6 s | 1.3x |

Long compute is where the compiler wins; a document small enough that instantiation dominates goes the other way, and that first row is the crossover. One scale down, the same report has `fibonacci` a wash, `reverse_array` +4%, `random_mat_mul` +15%, `base64` +24%, `string_manipulation` +27%, and this module +350%.

<sub>Third-party measurement, not ours, and not reproducible from this repo. Apple M5 Pro (18-core), 48 GB, macOS 26.5, Go 1.26.1, `CGO_ENABLED=0`, wazy `v0.0.0-20260807033006-cd2607360a17`, `anydoc.wasm` 6,542,355 bytes, best of 3 (best of 20 for the small input). Reported in [#29][i29].</sub>

## Moving fast

The API is stable. wazy broke compatibility with wazero once, over host-function registration, and that break is behind it — typed generics replaced the reflection path and the surface has settled. What keeps moving is everything under it: performance work lands continuously, guarded by the conformance, differential and fuzzing suites above rather than by a release cadence. Those same suites judge a contribution, not its author, machine-generated or human.

## Users

- [taubyte/tau](https://github.com/taubyte/tau) — the open-source platform behind Taubyte clouds runs WebAssembly workloads on wazy.
- [go-anydoc][anydoc] — Go bindings for a Rust document converter; the 6.5 MB measurements above are theirs.

Using wazy in production? [Open a PR](https://github.com/samyfodil/wazy/pulls) to add yourself.

## Credit

wazy started from [wazero][wazero]'s code (Copyright 2020-2023 wazero authors) and still draws on its WebAssembly semantics, WASI implementation, and compliance and fuzzing test suites. wazy keeps neither wazero's API compatibility nor its architecture: the goals are pure Go, performance, and conformance to the standard. See [RATIONALE.md](RATIONALE.md) for wazero's original design rationale and [LICENSE](LICENSE) for the Apache 2.0 license.

The specification conformance suites are built from [WebAssembly/testsuite][testsuite] and the individual proposal repositories (Apache 2.0); the Canonical ABI suites are the official [WebAssembly/component-model][cmrepo] `test/values` cases. The cross-proposal cases in [`internal/integration_test/spectest/v3-interaction`](internal/integration_test/spectest/v3-interaction) named `wasmtime-*.wast` are vendored from [wasmtime][wasmtime]'s `tests/misc_testsuite` (Apache 2.0 WITH LLVM-exception); the `wazy-*.wast` beside them are this repository's. [`imports/http_handler`](imports/http_handler) and its `nethttp` subpackage are a port of [http-wasm-host-go](https://github.com/http-wasm/http-wasm-host-go) (Apache 2.0), whose Technology Compatibility Kit is vendored to test them; [`imports/wasi_http`](imports/wasi_http) reimplements the pre-standard WASI-HTTP ABI from [wasi-go](https://github.com/stealthrocket/wasi-go) (Apache 2.0).

## License

Apache 2.0. See [LICENSE](LICENSE).

[wazero]: https://github.com/tetratelabs/wazero
[wasmtime]: https://github.com/bytecodealliance/wasmtime
[cm]: https://component-model.bytecodealliance.org/
[cmrepo]: https://github.com/WebAssembly/component-model
[wasi]: https://wasi.dev/
[testsuite]: https://github.com/WebAssembly/testsuite
[wasitest]: https://github.com/WebAssembly/wasi-testsuite
[pr679]: https://github.com/WebAssembly/component-model/pull/679
[anydoc]: https://github.com/xusenlin/go-anydoc
[i29]: https://github.com/samyfodil/wazy/issues/29