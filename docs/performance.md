# Performance

This page is the long form of the README's [Benchmarks](../README.md#benchmarks)
section and the reader-facing companion to [OPTIMIZATIONS.md](../OPTIMIZATIONS.md),
which is an engineering log rather than a document. It collects what has actually
been measured, says which machine produced each figure, and separates three
comparisons that are easy to blur together:

1. **wazy against wazero**, measured in the same process, in the same run.
2. **wazy against its own past** — what the optimization work bought. Most of the
   tables in OPTIMIZATIONS.md are this, not a wazero comparison.
3. **wazy against wasmtime (Cranelift)**, where wazy is sometimes behind.

Every number below cites the run it came from. Where no run exists in this repo —
the 6.5 MB `anydoc.wasm` figures, which are a third party's — it is labelled and
the reason it cannot be reproduced here is stated.

## 1. How to reproduce

### The head-to-head module

`benchmarks/vs-wazero` is a separate Go module so that wazy itself stays
zero-dependency. It compiles the *same* guest bytes against wazy, upstream
wazero and (for the three-way arms) wasmtime, and runs both/all runtimes inside a
single `go test` invocation, so one output file already contains both sides.

```sh
cd benchmarks/vs-wazero
go test -run='^$' -bench=. -benchmem -count=10 . | tee out.txt
go install golang.org/x/perf/cmd/benchstat@latest
benchstat -col /runtime out.txt          # wazy and wazero in adjacent columns
```

Every benchmark encodes the runtime as a trailing `/runtime=<name>`
sub-benchmark segment (`.../runtime=wazy`, `.../runtime=wazero`,
`.../runtime=wasmtime`), which is what makes the `-col /runtime` pivot work.
Correctness is checked separately — `go test ./...` in that directory asserts the
runtimes return byte-identical results for every workload.

What each benchmark measures (from `benchmarks/vs-wazero/README.md` and the test
sources):

| Benchmark | Measures |
| --- | --- |
| `BenchmarkHostCall` | Go↔Wasm round trip. Sweeps `host=gomodule` (the raw `[]uint64` stack API both runtimes implement the same way — the shared baseline) vs `host=typed` (wazy's reflection-free `HostFunc1` against wazero's reflection-based `WithFunc`), and `op=Call` (allocates a result slice) vs `op=CallWithStack` (reuses the caller's). |
| `BenchmarkCompile` | `CompileModule` of `testdata/case.wasm`, a 37 KB TinyGo module, fresh runtime per iteration, no compilation cache. |
| `BenchmarkInstantiate` | `InstantiateModule` only: compiled once, then instantiated per iteration with `_start` skipped and an anonymous name. |
| `BenchmarkExecute` | Pure execution of `case.wasm`'s `fibonacci` export at `fib=20` and `fib=30`; no host calls. |
| `BenchmarkCaseWorkloads` | All five real TinyGo exports (`fibonacci`, `base64`, `string_manipulation`, `reverse_array`, `random_mat_mul`) on wazy and wazero from one instantiation each. |
| `BenchmarkCompileModulesExtensive` | Compiles five real producer outputs (TinyGo 370 KB, Rust 10 KB, Zig 5 KB, zig-cc 786 KB, cargo-wasi 104 KB) on a fresh runtime every iteration. |
| `BenchmarkConstAddrLoads`, `BenchmarkDynAddrLoads`, `BenchmarkURemAddrLoads`, `BenchmarkDominatedBounds` | Bounds-check-elision kernels (constant, masked, `urem`-bounded and dominated addresses). |
| `BenchmarkDispatch*` | A synthetic `call_indirect` dispatch kernel: `mono`, `poly`, `direct`, plus heavy-callee variants. |
| `BenchmarkCloseOnContextDone`, `BenchmarkHostCallLoopCloseOnContextDone`, `BenchmarkFibCloseOnContextDone`, `BenchmarkInterruptCheckInterval` | Interruptible-loop cost under `WithCloseOnContextDone`, and the `WithInterruptCheckInterval` sweep. |
| `BenchmarkCase3`, `BenchmarkCompile3`, `BenchmarkExecute3`, `BenchmarkExecute3Heavy`, `BenchmarkRelaxedSimd`, `BenchmarkSpectreCost` | The three-way arms that include wasmtime (see [§4](#4-against-wasmtime)). |

### The in-repo suites

```sh
go test -bench . ./imports/wasip2/... ./internal/integration_test/bench/...
```

`internal/integration_test/bench` carries `BenchmarkInvocation`,
`BenchmarkCompilation`, `BenchmarkInitialization`, `BenchmarkCodec`,
`BenchmarkExportedFunctionCall_FreshHandle`, `BenchmarkEHFastPath` /
`BenchmarkEHThrowPath`, `BenchmarkHostFunctionCall`, and the three
`BenchmarkMemoryGrow*` benchmarks. `imports/wasip2` carries `BenchmarkServeHTTP`,
one HTTP request end to end through `wasi:http/incoming-handler`; its own doc
comment says to read the allocs column first and treat its ns/op on a shared
machine as noise, and that roughly nine of the allocations belong to `httptest`
rather than to wazy and were deliberately left in.

### Cold start

`benchmarks/coldstart` builds a one-shot binary that decodes, compiles,
instantiates and runs a `wasi:cli/run` component, and `run.sh` times it against
`wasmtime run --disable-cache` on the identical component over N process
launches, reporting min/median/mean:

```sh
go build -o /tmp/wazy-coldstart ./benchmarks/coldstart
benchmarks/coldstart/run.sh /tmp/wazy-coldstart <component.wasm> 50
```

`--disable-cache` is what makes the arms comparable: wazy recompiles every
process because it has no on-disk cache in this path. **No cold-start numbers are
committed to this repository**, so none are quoted here; the harness is the
deliverable.

### The wasmtime arms need cgo

`benchmarks/vs-wazero` requires `github.com/bytecodealliance/wasmtime-go/v34`,
which is cgo — an irony worth stating on a runtime whose selling point is
`CGO_ENABLED=0`. Because all the benchmarks live in one package, `CGO_ENABLED=0`
does not merely skip the wasmtime arms, it fails to build the package at all:

```
$ CGO_ENABLED=0 go vet ./...
vet: ./case3_test.go:33:47: undefined: wasmtime.Store
```

So the whole head-to-head module, wazero arms included, needs a C toolchain.
wazy itself does not: the root module has exactly one requirement
(`golang.org/x/sys`) and no cgo anywhere, which is the whole point — the cgo
dependency lives in the benchmark module only.

### Hardware

| Label | Machine |
| --- | --- |
| amd64 | Intel Core i9-12900HK, core-pinned (`taskset -c 2`), Linux |
| arm64 | Apple M4 |

Each figure below names the machine its own source names. Note that the two
sweep-round tables in OPTIMIZATIONS.md state **Apple M4** for every row, while
the README summarizes the page with a blanket "core-pinned i9-12900HK unless
noted"; where those disagree, this page follows OPTIMIZATIONS.md, which is where
the runs were recorded.

### What the comparison is pinned against

`benchmarks/vs-wazero/go.mod` currently pins
`github.com/tetratelabs/wazero v1.12.1-0.20260829084255-f4779551afb4`. That
module's README describes the pin as the exact fork point
(`c0f3a4ec6411`), the intent being to isolate wazy's own changes from unrelated
upstream drift; the pin has since been moved forward and the prose has not caught
up. Check `go.mod` for what you are actually measuring against, and note that the
interruptible-loop figures in [§2](#2-head-to-head-with-wazero) were taken
against `wazero@main` at the time, not against the fork point.

## 2. Head-to-head with wazero

Only figures where wazy and wazero were measured **in the same run** appear here.

| Path | wazy | wazero | Ratio | Source |
| --- | --- | --- | --- | --- |
| `Instantiate` (37 KB TinyGo module) | 1.724 µs | 15.74 µs | **9.1x** | OPTIMIZATIONS.md sweep round 3, Apple M4, benchstat over interleaved A/B runs |
| `HostCall/gomodule/CallWithStack` | 47.82 ns | 48.26 ns | a tie | same run as above |
| `BenchmarkConstAddrLoads` (constant-address load kernel) | 1.62 µs | 5.10 µs | **~3.1x** | OPTIMIZATIONS.md C21, core-pinned i9-12900HK, interleaved benchstat n=10 |
| `BenchmarkCloseOnContextDone`, 100k-iteration tight loop, `WithCloseOnContextDone` | 160 µs | 2074 µs | **~12.9x** (−92.3%) | OPTIMIZATIONS.md H6, quiet i9-12900HK, benchstat n=10, vs `wazero@main` |
| `case.wasm` `fibonacci` under `WithCloseOnContextDone` | 6.56 ms | 76.0 ms | **~11.6x** (−91.4%) | same run as above |
| Loop calling a host function each iteration: cost of turning the feature on | +5.3% | +75.5% | wazy 2.0x faster close-on | same run as above |

Two things about the host-call row. First, `host=gomodule` is the arm both
runtimes implement identically, which is why it is the honest baseline — and
until recently wazy *lost* it: sweep round 1 recorded 56.13 ns against wazero's
48.68 ns before the fix and 47.61 ns against 48.87 ns after (Apple M4). The cause
was a `sync.Pool` round trip for the wasm stack on every call against an
already-obtained `api.Function`; a `callEngine` now keeps its stack from the
second call on. Second, wazy's typed path measured 48.52 ns in the round-3 run —
the same neighbourhood, so the typed helpers cost nothing over the raw stack API.
The win from deleting reflection is structural, not per-call, and its magnitude
belongs in [§3](#3-what-the-optimization-work-bought) because the path it beats
is wazy's own, now-deleted one.

The interruptible-loop advantage is not free everywhere. From the same H6 write-up:
near-empty compute kernels (a bare spin loop, the inner fib loop) pay a **1.7–2.4x
worst case** with `WithCloseOnContextDone` on, which is why the feature stays
opt-in and why the per-loop check interval is tunable with
`WithInterruptCheckInterval` (default 64, power of two, folded into the module ID
so distinct intervals are distinct cache entries).

### Compiled execution

The README's head-to-head table reports, for compiled execution against wazero:
`string_manipulation` **−18%**, `reverse_array` **−14%**, `base64` **−12%**, and
`fibonacci` **a wash** — the advantage tracking memory-access intensity rather
than arithmetic. Those are the `BenchmarkCaseWorkloads`/`BenchmarkCase3` exports,
which run both runtimes from one instantiation each. Unlike the rows in the table
above, no absolute per-arm numbers for that run are recorded in OPTIMIZATIONS.md,
so it is reproduced here as the README's summary rather than as a table you can
re-derive from the log. Run `BenchmarkCaseWorkloads` yourself for current values.

`fibonacci` being a wash is expected and has been chased to the instruction: after
the H7 follow-up fix, `Case3/fibonacci` measured wazy 2.849 ms against wazero
2.798 ms (min-of-8, interleaved, verified-quiet i9-12900HK) — 1.02x, with wazy's
emitted code for that function byte-identical to wazero's, leaving mmap placement
as the residual.

## 3. What the optimization work bought

**These are wazy before and after its own optimization work. They are not wazero
comparisons**, even where the wazero arm was present in the same run as a control
— its arms were flat throughout, which is what makes the wazy deltas trustworthy.
OPTIMIZATIONS.md's tables headed `| Benchmark | main | branch |` are all of this
kind.

### Cumulative, sweep round 3 (Apple M4, interleaved benchstat, wazero flat control)

`internal/integration_test/bench`:

| Benchmark | main | branch | |
| --- | --- | --- | --- |
| `EHFastPath/n=100000` | 5.836 ms | 3.459 ms | −40.7% |
| `EHFastPath/n=1000` | 59.61 µs | 35.59 µs | −40.3% |
| `ExportedFunctionCall_FreshHandle` | 189.5 ns | 138.3 ns | −27.0% (1.513 KiB → 1.011 KiB B/op) |
| `EHThrowPath/n=10` | 2.101 µs | 1.712 µs | −18.5% |
| `Invocation/compiler/fib_for_5` | 42.95 ns | 35.05 ns | −18.4% |
| `Compilation/without_extern_cache` | 5.002 ms | 4.109 ms | −17.9% |
| `Initialization/compiler` | 2.029 µs | 1.676 µs | −17.4% |
| `Codec/binary.DecodeModule` | 10.399 µs | 8.943 µs | −14.0% |
| `Invocation/compiler/base64_100` | 35.51 µs | 31.36 µs | −11.7% |
| **geomean** | 31.90 µs | 26.22 µs | **−17.8%** (B/op −22.9%) |

`benchmarks/vs-wazero`, and real producer modules:

| Benchmark | main | branch | |
| --- | --- | --- | --- |
| `CompileModulesExtensive/greet_zig_5k` | 753.3 µs | 556.4 µs | −26.1% |
| `CompileModulesExtensive/greet_rust_10k` | 1132.7 µs | 871.3 µs | −23.1% |
| `Compile` (`case.wasm`) | 4.972 ms | 4.132 ms | −16.9% |
| `Instantiate` | 2.054 µs | 1.724 µs | −16.1% |
| `HostCall/typed/CallWithStack` | 57.44 ns | 48.52 ns | −15.5% |
| `HostCall/gomodule/CallWithStack` | 55.80 ns | 47.82 ns | −14.3% |
| `Execute/fib=20` | 19.40 µs | 17.81 µs | −8.2% |
| `wasip2.ServeHTTP` | 6.560 µs | 3.370 µs | −48.6% (76 → 45 allocs/op) |

Sweep round 1, earlier and on the same machine, is where `wasip2.ServeHTTP` first
moved (6.589 µs → 3.569 µs, 76 → 65 allocs/op), along with `EHFastPath/n=1000`
59.66 µs → 35.55 µs and a **−12.5% geomean** over
`internal/integration_test/bench`.

One regression was taken deliberately: `Compilation/interpreter` **+1.7%**. The
interpreter's lowering does more work up front (labels resolved at compile time,
`local.get`/`local.set` split by V128-ness) to buy
`Invocation/interpreter/fib_for_20` −5.3% and `fib_for_10` −3.1%. A module is
lowered once and run many times.

### The older, larger structural wins

These predate the sweeps and are the reason the absolute numbers above look the
way they do.

| Path | Before → after | | Source |
| --- | --- | --- | --- |
| Host calls, reflection removed (amd64) | 1086 ns / 6 allocs → 74.9 ns / 0 allocs | **14.5x** | OPTIMIZATIONS.md §1, measured baseline on this checkout |
| `memory.grow`, 100 in-capacity `memory.grow 0` ops (amd64) | 2.441 µs → 109.0 ns, 0 B/op, 0 allocs/op | **22.4x** (−95.5%) | F3, core-pinned i9-12900HK, n=8 |
| `memory.grow` Go fallback exposing 16 already-reserved pages | 15.12 µs → 927 ns | **16.3x** (−93.9%) | same run |
| Interpreter, `fib_for_30` | ~184–211 ms → ~125–147 ms; allocs/op 1,346,273 → 2 | ~30% | I1+I2+I3 |
| Memory per call, `mod.ExportedFunction("f").Call(ctx)` | 11784 B / 3 allocs → 1551 B / 2 allocs | −87% | A1 (allocation counts, load-immune) |

The host-call row deserves a second reading. The 1086 ns figure is **wazy's own
inherited reflection path**, measured on this checkout before it was deleted — not
a measurement of wazero's current `WithFunc`. wazy removed reflection-based
registration rather than optimizing it, so the 14.5x is against a path that no
longer exists here; wazero still ships one, and the honest same-run comparison of
the two typed paths is the 48.52 ns row in [§2](#2-head-to-head-with-wazero).

`memory.grow`'s fast path is opt-in via `WithMemoryCapacityReservePages`;
out-of-capacity, shared, imported and custom-allocator memories keep the safe Go
path. The same F3 write-up records what happens when the reserve is *not* tuned:
on an optimized rustc fixture that retains 512 allocations of 64 KiB, reserves of
0, 64, 128, 512 and 513 extra pages took 7.89–8.05 ms, 14.81 ms, 11.36 ms,
8.20 ms and 29.45 µs/op respectively — a reserve one page short of the workload's
threshold is worse than none, which is why the default is zero.

## 4. Against wasmtime

wasmtime (Cranelift) is a native compiler with a C toolchain behind it. wazy is
pure Go. On several kernels wazy has caught up; on pure-compute recursion and on
a large real module it has not, and both are stated here with the runs that show
it.

### Where wazy is behind

| Workload | Result | Source |
| --- | --- | --- |
| `Case3/fibonacci` (pure recursion, ~2.7M direct calls) | wazy **~16% behind** wasmtime/Cranelift, described as a raw-codegen gap | OPTIMIZATIONS.md H7-followup, min-of-8 interleaved, verified-quiet i9-12900HK |
| 7.5 MB PDF through a 6.5 MB Rust module | native Rust 0.28 s, wasmtime (Cranelift) 0.37 s, **wazy 0.75 s**, wasmtime (Winch) 0.90 s, wazero 3.4 s — roughly **2x behind Cranelift** | Third-party: [go-anydoc][anydoc], reported in [#29][i29]. Recorded in this repository's README history (commit `35cb198a~1`); the current README keeps only the wazero columns |

### Where the gap closed

Two bounds-check-elision changes moved the memory-access kernels, measured on the
i9-12900HK, core-pinned, interleaved:

- **C24** (no stack-overflow check in leaf prologues, motivated by disassembling
  Cranelift's output) moved `dynaddr` from **0.60x to 0.93x** of wasmtime and
  `dispatch` from **0.75x to 0.92x**, for −16% geomean over the execute kernels.
- **C25** (masked dynamic addresses proven in-bounds) together with C24 shrank
  `dynaddr`'s hot leaf from **160 to 32 bytes** — wasmtime's is 26 — and **flipped
  `dynaddr` from wasmtime being 1.7x ahead to wazy being 1.18x ahead**, with
  `dispatch` at parity (0.94–0.97x).

### Reading the three-way arms honestly

The harness contains four traps it documents rather than hides:

- **The cgo boundary is in the wasmtime numbers.** `wasmtime-go` costs roughly
  2.6 µs per call. `BenchmarkExecute3` runs at ~1–150 µs/call, so it is partly a
  call-overhead comparison; `BenchmarkExecute3Heavy` exists for this reason — same
  kernels and same wasm, trip counts cranked so each call runs ~10 ms and the
  boundary falls below 0.1%. Use the heavy variant for codegen claims.
- **`Case3/base64` is not a codegen benchmark.** It calls the
  `env.get_random_string` host import 100 times, so it measures host-call crossing
  — wazy's native Go call against wasmtime's cgo boundary.
- **wasmtime is doing work wazy does not.** Cranelift clamps the index with a
  conditional move on every guarded heap and table access
  (`enable_heap_access_spectre_mitigation`, `enable_table_access_spectre_mitigation`,
  both on by default). wazy emits a plain conditional branch and clamps nothing,
  so any comparison against wasmtime's defaults charges it for a mitigation wazy
  omits. `BenchmarkSpectreCost` measures exactly that: the same kernels, wasmtime
  against itself, mitigations off. Whatever it recovers is the part of wazy's lead
  that is missing hardening rather than better code. No committed numbers.
- **The relaxed-SIMD arms are not all like-for-like.** Upstream wazero does not
  implement the proposal, so `BenchmarkRelaxedSimd` runs wazy, wasmtime (default,
  fastest-per-host lowerings) and wasmtime-det (`SetWasmRelaxedSIMDDeterministic`,
  which pins results the way wazy always does). Only the **wasmtime-det** arm is
  the comparable one — `TestRelaxedSimdParity` asserts wazy and wasmtime-det agree
  bit for bit on min/max and the dot product. They cannot agree on `madd`
  (wasmtime fuses even in deterministic mode; wazy rounds the multiply and the add
  separately, as the spec's deterministic profile requires), so that arm compares
  two different computations.

### The 6.5 MB module, at scale, measured by someone else

The first-party suites are kernels; their largest guest is 37 KB.
[go-anydoc][anydoc] measured two orders of magnitude up — a 6.5 MB
`wasm32-wasip1` command module (Rust 1.88, `opt-level = 3`, `wasm-opt -O3`) run as
a single instantiate, `CompileModule` excluded from every timing:

| Converting | wazero v1.12.0 | wazy | |
| --- | :---: | :---: | :---: |
| 1 KB docx, compiled | 0.4 ms | 0.62 ms | **0.6x — wazero wins** |
| 5 MB docx body, compiled | 0.86 s | 0.19 s | 4.5x |
| 7.5 MB PDF, compiled | 3.4 s | 0.75 s | 4.5x |
| 1 KB docx, interpreted | 3.5 ms | 2.7 ms | 1.3x |
| 5 MB docx body, interpreted | 11.1 s | 8.6 s | 1.3x |
| 7.5 MB PDF, interpreted | 41.4 s | 32.6 s | 1.3x |

Long compute is where the compiler wins; a document small enough that
instantiation dominates goes the other way, and the first row is the crossover.

**Third-party measurement, not ours, and not reproducible from this repository**
(the module is not vendored here). Apple M5 Pro (18-core), 48 GB, macOS 26.5,
Go 1.26.1, `CGO_ENABLED=0`, wazy `v0.0.0-20260807033006-cd2607360a17`,
`anydoc.wasm` 6,542,355 bytes, best of 3 (best of 20 for the small input).
Reported in [#29][i29]. The wasmtime and native-Rust columns for the 7.5 MB PDF
are in [§4](#where-wazy-is-behind).

## 5. Method

The rules the numbers above were produced under, taken from the repository's own
practice:

- **Interleaved A/B runs, not two files taken hours apart.** Both arms run inside
  a single `go test` invocation wherever the harness allows it, and `benchstat`
  pivots on `/runtime`. Sample-by-sample interleaving is what makes a delta
  survive a machine that is not perfectly still.
- **Core-pinned.** amd64 wall-clock work runs under `taskset` (`taskset -c 2` in
  the recorded runs). A pinned single-threaded run absorbs background load
  symmetrically, so ratios stay meaningful even when absolute nanoseconds do not.
- **n = 8 or more**, with `-count` 5–10 in the sweep rounds and n=10 or n=12 for
  the individual optimization write-ups; benchstat wants at least 6 samples for a
  confidence interval. p-values are recorded where a claim depends on one.
- **A control arm in the same run.** Upstream wazero is measured alongside wazy in
  the sweep rounds precisely because it is *not* changing: if the wazero line
  moves between the two halves of an A/B, the run is noise and the wazy delta is
  not trustworthy. In sweep rounds 1 and 3 the wazero arms were flat throughout.
- **Load discipline.** Wall-clock conclusions require a quiet machine; allocation
  counts do not. The repository's rule is stated in OPTIMIZATIONS.md in exactly
  those terms, and the cold-start driver (`benchmarks/coldstart/driver.sh`) waits
  in a loop until the 1-minute load average drops below a threshold and a known
  CPU-hog test process is gone before it times anything. Several write-ups say
  outright that the box was loaded and that only ratios or allocation counts
  should be read from them.
- **Allocation counts are the load-immune measure.** Where a number had to be
  taken on a busy machine, it is an alloc/B-per-op figure (`ServeHTTP`, `A1`'s
  11784 B → 1551 B, `I8`'s 719 KiB → 411 KiB op body).
- **Both ISAs, and arm64 for real.** amd64 changes to the backend are verified on
  arm64 too — on Apple M4 hardware for timing, under qemu for correctness where
  hardware is not to hand.

### Rejected optimizations are published, with their numbers

The point of the log is that changes which *lost* are recorded as fully as the
ones that won, so nobody rebuilds them:

- **C27 on arm64** (reserved prologue trap-context slot, ported from amd64) —
  measured loss: `string_manipulation_size_1000` +7.99%, `reverse_array_size_500`
  +7.70%, geomean +0.24% versus −3.90% with it reverted. Isolated by bisection.
  amd64's version is unaffected and stays.
- **A dense `[]functionInstance` for the `ref.func` memo** — Instantiate +63% B/op
  and +7.07%. Replaced by a prepend-only linked list.
- **A `call_indirect` inline cache (C23)** — validated, then gated out before
  implementation. On a purpose-built dispatch kernel, `mono` 3.34 ns/call vs
  `direct` 1.73 (dispatch overhead 1.61 ns), but with a realistic ~16-op callee
  `mono_heavy` 10.54 vs `direct_heavy` 10.92 — below the noise floor. Ceiling
  ~25–30% on the pathological kernel, ~0% on every real workload in the suite.
- **Reusing the per-call param/result buffer (C10)** — worked (`HostCall/gomodule/op=Call`
  99.7 → 91.5 ns, 2 → 1 allocs) and was still rejected: it makes a previous call's
  result slice alias the next one's, breaking a result-independence guarantee an
  inherited test enshrines. `CallWithStack` already exists for zero-alloc callers.
- **Collapsing the interpreter's call layers (I9)** — −0.95%, p=0.061, on the most
  call-bound workload there is. Not significant, reverted.
- **A lock-free GC handshake** — fixed rather than reverted, but the trap is on
  record: removing the mutex inlined the bodies of `EnterGo`/`LeaveGo` and pushed
  them from inlining cost 64 to 94, past Go's budget of 80, turning a nil-receiver
  fast path into two real calls. Measured **+9.2% / +6.9%** on
  `BenchmarkHostCallLoopCloseOnContextDone`. A microbenchmark written alongside the
  change had shown the lock-free version winning; it never reached the real
  host-call path.

Known gaps are published in the same place: the component decoder reads embedded
core modules with a full `DecodeModule` when it needs a few sections, `Store.mux`
still serializes instantiate against close, and the 2431-line validator body was
left alone deliberately because correctness of validation outranks its speed.

[anydoc]: https://github.com/xusenlin/go-anydoc
[i29]: https://github.com/samyfodil/wazy/issues/29
