---
title: Performance
description: The benchmarks behind every figure on this site, how to run them, and what they mean.
---

Every first-party figure on this site is a benchmark in the repository. Run them yourself:

```bash
(cd benchmarks/vs-wazero && go test -bench .)  # Instantiate, HostCall, Compile, Execute
go test -bench . ./imports/wasip2/... ./internal/integration_test/bench/... ./internal/component/...
```

`./internal/component/...` is where the component figures live — `BenchmarkInstantiateHelloCached`
in `internal/component/instance` is the 350.5 µs cached component instantiate. `benchmarks/vs-wazero`
is its own Go module, hence the subshell.

The head-to-head sweeps below — instantiate **9.1x**, the cumulative geomean and **B/op −22.9%**,
the host-call pair and the compile **−23% to −26%** — were measured on an **Apple M4** (arm64),
`benchstat` over interleaved A/B runs with upstream wazero as a control in the same runs. The
per-optimization figures that predate them — the interruptible-loop numbers, `memory.grow`, and the
host-call reflection baseline — are a core-pinned **i9-12900HK** (amd64). Method, the machine behind
each result, and the optimizations **measured and rejected** (including one that made arm64 8%
slower) are in
[OPTIMIZATIONS.md](https://github.com/samyfodil/wazy/blob/main/OPTIMIZATIONS.md).

## Head to head with wazero

Measured against wazero in the same runs, on the same workloads.

| Path | wazy vs wazero | What & why |
| --- | :--: | --- |
| **Instantiate** | **9.1x** | 1.724 µs vs 15.74 µs, on a 37 KB TinyGo module. |
| **Interruptible loops** (`WithCloseOnContextDone`) | **12–13x**; +5% vs +75% overhead | On a loop calling a host function each iteration. The check is amortized, not a Go round-trip per iteration; a near-empty compute kernel is the worst case at 1.7–2.4x, tunable with `WithInterruptCheckInterval`. Against `wazero@main`. |
| **Compiled execution** | memory-heavy code leads | `string_manipulation` −18%, `reverse_array` −14%, `base64` −12%, `fibonacci` a wash — the advantage tracks memory-access intensity, not arithmetic. |
| **Host calls** (Go ↔ Wasm) | a tie | 47.8 ns here, 48.3 ns there, on `HostCall/gomodule/CallWithStack` — the shared baseline both runtimes implement the same way. The win is structural, not per-call. |
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

## At scale: a 6.8 MB Rust module, measured by someone else

The suites above are kernels. [go-anydoc](https://github.com/xusenlin/go-anydoc) measured two
orders of magnitude up: a 6.8 MB `wasm32-wasip1` command module (Rust 1.88, `opt-level = 3`,
`wasm-opt -O3`) run as a single instantiate — stdin in, stdout out, a long stretch of compute
between. Same `.wasm`, same input, same machine, identical output, `CompileModule` excluded from
every timing.

| Converting | wazero v1.12.0 | wazy v0.3.0 | |
| --- | :---: | :---: | :---: |
| 1 KB docx, compiled | 0.64 ms | 0.13 ms | 5.0x |
| 5 MB docx body, compiled | 0.98 s | 0.19 s | 5.3x |
| 7.6 MB PDF, compiled | 1.63 s | 0.33 s | 5.0x |
| 1 KB docx, interpreted | 2.58 ms | 1.38 ms | 1.9x |
| 5 MB docx body, interpreted | 11.9 s | 6.8 s | 1.8x |
| 7.6 MB PDF, interpreted | 20.6 s | 12.1 s | 1.7x |

**The compiled rows carry `WithCloseOnContextDone(true)`, and most of that 5x is what the option
costs wazero rather than what its code generator produces.** go-anydoc sets it unconditionally —
cancelling a context has to interrupt a conversion already running inside the guest — so every run
of its benchmark carries it, while the suites above never set it. Toggling only that call on the
5 MB body:

| Compiled, 5 MB docx body | wazero v1.12.0 | wazy v0.3.0 | |
| --- | :---: | :---: | :---: |
| close-on-context-done on | 0.94 s | 0.19 s | 5.0x |
| off | 0.14 s | 0.11 s | 1.3x |
| what the option costs | 6.5x | 1.7x | |

With it off the two compilers are ~30% apart. Against an interpreter the option is free on both, so
the 1.8x in the interpreted rows is the engines and nothing else.
[go-pdfium](https://github.com/klippa-app/go-pdfium/blob/main/experimental/BENCHMARKS.md#the-cost-of-close-on-context-done)
measured the same effect across 5,000 PDFs, at ~4.5x for wazero and ~1.4x for wazy. wazero's
[#2533](https://github.com/wazero/wazero/pull/2533) cuts its cost to about 1.04x, better than our
1.7x here; that is a gap to close, not one to report around.

The 1 KB compiled row is the one that moved most. The first version of this report had wazy
*losing* it at 0.6x, because instantiation dominates a document that small and two things made
instantiation expensive: a funcref memo that scanned a list, which made taking a reference for each
of the module's 593 element-segment entries quadratic, and a linear memory allocated at exactly its
initial page count, which made every instantiation of a guest whose allocator grows the heap
reallocate and copy the whole memory. Both are fixed as of v0.3.0; see
[OPTIMIZATIONS.md](https://github.com/samyfodil/wazy/blob/main/OPTIMIZATIONS.md).

Third-party measurement, not ours. Apple M5 Pro (18-core), 48 GB, macOS 26.5, Go 1.26.1,
`CGO_ENABLED=0`, `anydoc.wasm` 6,781,177 bytes (anydoc 0.2.3), min of 3 at `-benchtime 3x`. The
docx rows reproduce from a checkout of go-anydoc — `go test -run '^$' -bench 'ConvertDOCX|CloseOnContextDone' -benchtime 3x -count 3`,
since its harness generates its own inputs; the PDF rows need a file of your own, and the wazero
column is the same benchmarks with the three imports in its `anydoc.go` pointed at wazero.
Originally reported in [#29](https://github.com/samyfodil/wazy/issues/29).

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
- **Tune `WithInterruptCheckInterval`** if you use `WithCloseOnContextDone`. The +5% is a loop that
  calls a host function each iteration; a near-empty compute kernel pays 1.7–2.4x, and the interval
  sweep moves that from 26.9x of floor at 0 to 1.74x at 4096.
- **Do not pool instances.** Instantiation is 1.7 µs and 3.3 KB; a fresh instance per request is
  both faster to reason about and safer than scrubbing a reused one.
