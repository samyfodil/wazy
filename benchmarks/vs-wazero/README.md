# wazy vs wazero benchmarks

A self-contained Go module that runs the **same workloads against both
[wazy](https://github.com/samyfodil/wazy) and upstream
[wazero](https://github.com/tetratelabs/wazero)** so the two can be measured
side by side.

It is its own module (`github.com/samyfodil/wazy/benchmarks/vs-wazero`) on
purpose: wazy itself stays zero-dependency, and pulling upstream wazero in as a
require here does not leak into the root module. A `replace` points wazy at the
parent checkout (`../..`), so you always benchmark the working tree.

Because it is a separate module, the root `go test ./...` does **not** pick it
up. Run it from this directory.

## What it measures

All benchmarks use the **compiler engine** (the default). The interpreter is
skipped.

1. **`BenchmarkHostCall` — host function call round trip (the headline).**
   A guest wasm function takes a `u32` offset, calls an imported host function
   `(ctx, mod, u32) -> f32` that reads 4 bytes of guest memory at that offset,
   and returns them as an `f32`. This mirrors
   `internal/integration_test/bench/hostfunc_bench_test.go`. It sweeps three
   dimensions:
   - `host=gomodule` — host function registered with `WithGoModuleFunction`
     (raw `[]uint64` stack) on **both** runtimes. This is the shared baseline.
   - `host=typed` — the typed registration path: **wazy** uses
     `wazy.HostFunc1` (compile-time-typed, reflection-free); **wazero** uses
     `WithFunc` (reflection-based). This is the pair the suite exists to
     compare.
   - `op=Call` (allocates a result slice) vs `op=CallWithStack` (reuses a
     caller-provided stack).
2. **`BenchmarkCompile` — `CompileModule`** of a real, nontrivial TinyGo module
   (`testdata/case.wasm`), fresh runtime per iteration, no compilation cache, so
   every iteration truly recompiles.
3. **`BenchmarkInstantiate` — `InstantiateModule`** post-compile per-instance
   cost. The module is compiled once; each iteration instantiates it with
   `_start` skipped (`WithStartFunctions()`) and an anonymous name.
4. **`BenchmarkExecute` — pure execution** of the compute-heavy `fibonacci`
   export of `case.wasm` (deterministic, no host calls), at `fib=20` and
   `fib=30`.

The guest bytes are **identical for both runtimes** (see Test data below), so
any difference is the runtime, not the module.

## Running

From this directory:

```sh
go test -run='^$' -bench=. -benchmem -count=1 ./...
```

For a statistically meaningful comparison, use more samples (benchstat wants
>= 6 for a confidence interval):

```sh
go test -run='^$' -bench=. -benchmem -count=10 ./... | tee out.txt
```

Run the correctness tests (they assert both runtimes return byte-identical
results for every workload):

```sh
go test ./...
```

## Comparing with benchstat

Every benchmark encodes the runtime in a trailing `/runtime=<name>`
sub-benchmark segment, and the other dimensions as `/key=value` segments, e.g.

```
BenchmarkHostCall/host=typed/op=Call/runtime=wazy
BenchmarkHostCall/host=typed/op=Call/runtime=wazero
```

Both runtimes run in a **single** `go test` invocation, so one output file
already contains both sides. Pivot on the `runtime` key to get them in adjacent
columns:

```sh
go install golang.org/x/perf/cmd/benchstat@latest
benchstat -col /runtime out.txt
```

This yields one row per workload with `wazy` and `wazero` columns and a
percentage delta — no need to juggle two separate files.

## Test data

- `testdata/hostcall.wat` / `testdata/hostcall.wasm` — the host-call guest
  module. `hostcall.wat` is the checked-in source; `hostcall.wasm` is compiled
  from it with `wat2wasm` and embedded via `go:embed`. It reproduces the module
  hand-encoded (via `binaryencoding.EncodeModule`) in
  `internal/integration_test/bench/hostfunc_bench_test.go`, but built through
  each runtime's **public** API only (internal packages are not importable
  across the module boundary). Regenerate with:
  `wat2wasm testdata/hostcall.wat -o testdata/hostcall.wasm`.
- `testdata/case.wasm` — copied verbatim from
  `internal/integration_test/bench/testdata/case.wasm` (a TinyGo build of
  `case.go`). Copied in rather than referenced across the module boundary.

## Why upstream wazero is pinned

`go.mod` pins wazero at

```
github.com/tetratelabs/wazero v1.12.1-0.20260630042819-c0f3a4ec6411
```

the pseudo-version for commit `c0f3a4ec6411fa065b6db7e112e351816a760e3c` — the
**exact commit wazy forked from**. Pinning to the fork point means the
comparison isolates *wazy's own changes* (such as the reflection-free typed
host-function helpers) rather than conflating them with unrelated upstream drift
that landed after the fork. Resolve/refresh it with:

```sh
go get github.com/tetratelabs/wazero@c0f3a4ec6411
```
