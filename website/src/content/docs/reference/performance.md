---
title: Performance
description: The benchmarks behind every figure on this site, how to run them, and what they mean.
---

Every first-party figure on this site is a benchmark in the repository. Run them yourself:

```bash
cd benchmarks/vs-wazero && go test -bench .    # Instantiate, HostCall, Compile, Execute
go test -bench . ./imports/wasip2/... ./internal/integration_test/bench/...
```

Figures were measured on a core-pinned i9-12900HK (amd64) unless noted; arm64 work is measured on
an Apple M4. Method, per-optimization numbers and the optimizations **measured and rejected**
(including one that made arm64 8% slower) are in
[OPTIMIZATIONS.md](https://github.com/samyfodil/wazy/blob/main/OPTIMIZATIONS.md).

## Head to head with wazero

Measured against wazero in the same runs, on the same workloads.

| Path | wazy vs wazero | What & why |
| --- | :--: | --- |
| **Instantiate** | **9.1x** | 1.724 µs vs 15.74 µs, on a 37 KB TinyGo module. |
| **Interruptible loops** (`WithCloseOnContextDone`) | **12–13x**; +5% vs +75% overhead | The per-loop check is amortized, not a Go round-trip per iteration; tune with `WithInterruptCheckInterval`. Against `wazero@main`. |
| **Compiled execution** | memory-heavy code leads | `string_manipulation` −18%, `reverse_array` −14%, `base64` −12%, `fibonacci` a wash — the advantage tracks memory-access intensity, not arithmetic. |
| **Host calls** (Go ↔ Wasm) | a tie | 48.5 ns here, 48.3 ns there, on the raw stack API both runtimes expose. The win is structural, not per-call. |
| **Cumulative** | geomean **−17.8%**, B/op **−22.9%** | Across `internal/integration_test/bench`, versus the wazero fork point, with upstream wazero as a control in the same runs — its arms stayed flat. |

## What the rewrite bought

These are wazy **before and after its own optimization work** — not a wazero comparison. They are
why the absolute numbers on the front page look the way they do.

| Path | Before → after | |
| --- | --- | :--: |
| **Host calls**, reflection removed | 1086 ns / 6 allocs → 74.9 ns / 0 allocs | **14.5x** |
| **`memory.grow`** (in-capacity, amd64) | 2.441 µs → 109.0 ns, 0 allocs/op | **22x** |
| **Interpreter** | a benchmark that allocated 1.35M times now allocates twice | **~30%** |
| **Memory per call** | 11784 B / 3 allocs → 1551 B / 2 allocs | **−87%** |
| **Compile**, real modules | 5 KB Zig 753.3 µs → 556.4 µs; 10 KB Rust 1132.7 µs → 871.3 µs | **−23% to −26%** |

The host-call row is the one worth reading twice. wazy deleted reflection-based registration rather
than optimizing it, so the 14.5x is against a path that no longer exists here — wazero still ships
one. `memory.grow` is opt-in via `WithMemoryCapacityReservePages`; out-of-capacity, shared and
imported memories keep the safe Go path.

## At scale: a 6.5 MB Rust module, measured by someone else

The suites above are kernels. [go-anydoc](https://github.com/xusenlin/go-anydoc) measured two
orders of magnitude up: a 6.5 MB `wasm32-wasip1` command module (Rust 1.88, `opt-level = 3`,
`wasm-opt -O3`) run as a single instantiate — stdin in, stdout out, a long stretch of compute
between. Same `.wasm`, same input, same machine, identical output, `CompileModule` excluded from
every timing.

| Converting | wazero v1.12.0 | wazy | |
| --- | :---: | :---: | :---: |
| 1 KB docx, compiled | 0.4 ms | 0.62 ms | **0.6x — wazero wins** |
| 5 MB docx body, compiled | 0.86 s | 0.19 s | 4.5x |
| 7.5 MB PDF, compiled | 3.4 s | 0.75 s | 4.5x |
| 1 KB docx, interpreted | 3.5 ms | 2.7 ms | 1.3x |
| 5 MB docx body, interpreted | 11.1 s | 8.6 s | 1.3x |
| 7.5 MB PDF, interpreted | 41.4 s | 32.6 s | 1.3x |

Long compute is where the compiler wins; a document small enough that instantiation dominates goes
the other way, and that first row is the crossover.

Third-party measurement, not ours, and not reproducible from this repo. Apple M5 Pro (18-core),
48 GB, macOS 26.5, Go 1.26.1, `CGO_ENABLED=0`, wazy `v0.0.0-20260807033006-cd2607360a17`,
`anydoc.wasm` 6,542,355 bytes, best of 3 (best of 20 for the small input). Reported in
[#29](https://github.com/samyfodil/wazy/issues/29).

## Against wasmtime

The head-to-head module also carries a three-way comparison against wasmtime — `BenchmarkExecute3`,
`BenchmarkCompile3`, `BenchmarkExecute3Heavy`, `BenchmarkRelaxedSimd`. Those arms link
`wasmtime-go`, so they need cgo: an irony worth stating on a page selling `CGO_ENABLED=0`.

## Getting the most out of it

- **Hoist `CompileModule`** out of the request path, and add a
  [compilation cache](../../guides/caching/) if the process is short-lived.
- **Use `CallWithStack`** in hot loops; `Call` allocates the result slice.
- **Reserve memory capacity** with `WithMemoryCapacityReservePages` if the guest grows its memory
  repeatedly.
- **Tune `WithInterruptCheckInterval`** if you use `WithCloseOnContextDone` and the +5% matters.
- **Do not pool instances.** Instantiation is 1.7 µs and 3.3 KB; a fresh instance per request is
  both faster to reason about and safer than scrubbing a reused one.
