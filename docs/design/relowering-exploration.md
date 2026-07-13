# Relowering (JIT-on-AOT tiering) — exploration & verdict

**Status:** exploration only, no code. Question: should wazy grow a runtime tier
that recompiles hot functions at a higher optimization level (or with
profile-guided opts like the `call_indirect` inline cache) and swaps them into a
running module?

**Verdict:** **A per-function optimizing hot-swap tier is not worth building for
wazy's real workloads, for two independent reasons** — (1) the AOT optimization
passes already buy ~0 on real producer code, so an "optimize hotter" tier has
almost nothing to add; (2) the hot-swap mechanism is a research project, not an
increment. The one direction with real value is the opposite: a **baseline /
lazy compilation tier for cold-start** — and even there the lever is a cheaper
register allocator, not skipping opt passes.

---

## 1. The decisive measurement: optimization doesn't move real code

The premise of an optimizing JIT tier is "spend more compile effort on hot code
to make it faster." We tested whether wazy's existing optimization even does
that. Env-gated `passConstFoldAndCSE` (the largest SSA opt pass, C4) off and A/B'd:

| metric | result |
|---|---|
| compile time saved (skipping it) | **−5.5% geomean** (Go/Rust/C/TinyGo modules) |
| compile allocation saved (Go) | −10% |
| **execution delta on caseWasm (fib/matmul/base64)** | **−1.1% / −1.9% — i.e. execution-NEUTRAL, not slower** |

Real code is produced by LLVM / rustc / the Go compiler, which already fold
constants and CSE. wazy's redundant passes find ~nothing in it (they deliver
−61% only on hand-written redundancy-heavy wasm). So **the optimization tier a
JIT would add improves real-code throughput by ~0.** The two profile-guided
compile wins we found (C12 −6% shipped, C16 ~3.4% queued) *also* leave codegen
byte-identical. The pattern is consistent: on producer output, wazy's headroom is
in *compile speed*, not *code quality*.

Corollary: the `call_indirect` inline cache and function inlining — the opts a
profiling tier would exist to enable — were already gated out (see
`OPTIMIZATIONS.md` C23): dispatch overhead is ~0% of real workloads and dilutes
to noise once callees do real work. A profiling tier would let us apply them
*only* where hot, solving the "which sites are hot" problem — but the ceiling is
still ~0 on the workloads we have, so it buys a mechanism to capture a
non-existent win.

## 2. Why hot-swap is a research project (the seam map)

Pipeline reality in `internal/engine/native/` (file:line):

- **Code layout — HARD.** `compiledModule.executables.executable` (`engine.go`)
  is **one immutable mmap'd, mprotected RX buffer per module**; a function is
  just `(executable, functionOffsets[i])`. There is no per-function code object
  to replace; a recompiled function needs a fresh mmap segment.
- **Swap target is fanned out — HARD.** `functionInstance.executable`
  (`module_engine.go`) is a raw pointer *into* that buffer, and it is **copied**
  into: the top-level `callEngine.executable` (captured once at `NewFunction`),
  every per-instance table `functionInstance` (`FunctionInstanceReference`), and
  imported-fn slots in each importer's opaque VMContext (`ResolveImportedFunction`).
  No single indirection cell backs it.
- **Direct calls have zero indirection — HARDEST.** Intra-module `call` lowers to
  a **PC-relative rel32 branch with the displacement baked in by relocation**
  (`amd64/machine.go` `ResolveRelocations`). Direct callers never load a pointer;
  redirecting `F` means patching every caller's machine code. Only
  `call_indirect` / `call_ref` re-load `executable` from `functionInstance` per
  call (`lower.go`) and are in principle swappable.
- **No OSR.** A frame running old code is walkable (EH stack walker in
  `nativeapi/eh_table.go` + per-ISA unwinders; PC→function via
  `functionIndexOf`/`compiledModuleOfAddr`) but not relocatable — there is no
  return-address patching or on-stack replacement. Old code stays mapped until a
  finalizer runs; you can't retire it while a frame is live.
- **No lazy-compile precedent.** Compilation is fully eager
  (`engine.compileModule`); instantiation hard-requires a pre-compiled module. No
  first-call trampolines, no per-function placement/relocation/cache path — the
  cache/ID seam (`AssignModuleID`, `fileCacheKey`) is **module-granular**.

Building safe per-function hot-swap = add an indirection cell every call site
loads from (defeating the rel32-direct-call optimization, a throughput
regression on the common path) + drain/OSR in-flight frames + per-function mmap
& relocation. Large surface, and §1 says it would guard a ~0 win.

## 3. What *does* exist, and the tractable directions

- **Module-granular variant selection — already works.** `WithInterruptCheckInterval`
  folds a compile parameter into `module.ID` (`module.go`), so recompiling the
  whole module under a different context yields a distinct cached
  `compiledModule` (H6). This is the "seam" — but it is whole-module recompile
  under a *flag*, not hotness tiering, and it produces fresh code only for the
  *next* instantiation. Useful for e.g. dialing interrupt-check density per
  deployment; not a path to swapping a running instance.
- **Reusable pieces for a future baseline tier:** `compileLocalWasmFunction`
  (`engine.go`) is an isolated per-function compile; the PC→function mappers and
  the native stack walker exist (read side of OSR). The write side does not.

**The only direction with real value is a baseline / lazy tier for cold-start**
(tier *down* for fast start, not *up* for speed):

- Compile is dominated by **RegAlloc (44%)**, then LowerToSSA (13%), then the SSA
  passes (21% total, of which opt passes ~8% and buy ~0 execution per §1). A
  genuine fast-start tier would pair a **cheap register allocator** (linear-scan
  or spill-most) with skipping the opt passes — plausibly ~halving compile — at a
  *real* execution cost (bad regalloc = many spills = slow code). That is the
  classic tier-0(fast compile/slow code) ↔ tier-1(slow compile/fast code)
  tradeoff, and it is only worth it for **cold-start-sensitive, then long-running**
  workloads that also need lazy per-function compile (compile on first call) —
  which needs the first-call-trampoline + per-function placement machinery that
  doesn't exist yet.

## 4. Recommendation

1. **Do not build a per-function optimizing hot-swap JIT.** §1 (opt buys ~0 on
   real code) + §2 (research-grade mechanism) make it negative-value.
2. **Bank the profile-guided compile wins instead** — they're where wazy's real
   headroom is: **C12 shipped (−6% compile, done), C16 next (~3.4%)**, then the
   RegAlloc structure (44%). These help every workload with zero risk.
3. **If cold-start becomes a product goal**, scope a *baseline compilation tier*
   (cheap regalloc + skip opt passes + lazy first-call compile) as its own
   project — that's the tiering shape that pays for wazy, and it reuses
   `compileLocalWasmFunction` + the existing PC→function mappers. Note the
   module-granular seam (H6) does **not** get you there; per-function lazy compile
   needs new first-call-trampoline + placement machinery.

The one-line summary: **wazy's opportunity is faster compilation, not faster
compiled code — so tier *down* for cold-start, don't tier *up* for speed.**

---

## 5. Prototype: runtime-adjustable interrupt interval (the one relowering-shaped win)

The single profile-dependent knob worth retuning at runtime is the **loop-yield
(interrupt-check) interval** (H6): a hot loop tolerates a larger interval (fewer
GC/cancellation round-trips) but the value is unknowable at compile time. Today
the mask (`interval-1`) is a **baked constant** in the loop header
(`frontend/lower.go`, `AsIconst64(mask)`), so changing it needs a whole-module
recompile — which is the module-granular seam, and cannot retune a *live* loop.

We prototyped the cheaper alternative: make the mask a **runtime value loaded
from the execution context** (`executionContext.interruptCheckMask`, offset 1256;
seeded per callEngine from the compiled module's interval), so retuning is a
field write — no recompile, no hot-swap.

**Measured** (i9-12900HK, core-pinned, benchstat n=10, `close=on` = interrupt
checks active), baked constant → runtime mask:

| loop | baked | runtime (naive, reload/iter) | runtime (hoisted to preheader) |
|---|---|---|---|
| spin (near-empty body, worst case) | baseline | **+10.6%** | **+5.3%** |
| host-call loop (realistic body) | baseline | **−1.2% (free)** | free |

Two findings:

1. **Free on realistic loops, ~+5% only on pathological empty kernels.** The mask
   is loop-invariant, so hoisting the load to the preheader (it dominates the
   header) halves the worst-case cost. The residual +5% is fundamental: a baked
   *immediate* (`and $imm,reg`) beats a *register-resident* mask that must stay
   live across the loop — runtime-tunability isn't free on tight loops, even
   perfectly hoisted.
2. **The knob is benignly racy — no locking needed for live retune.** The mask
   affects only *yield frequency*, never correctness: any value (power-of-two-1,
   0 = every iteration, or garbage) still yields *sometimes* and the loop stays
   correct (H6: every interval is correct, only latency varies). So another
   goroutine can retune a running loop with a plain aligned `uint64` store; worst
   case the loop reads a stale mask for one iteration, which is harmless.

**Validated on real producer output** (Go/Rust/clang-C run under
close-on-context-done — checks emitted, never tripped; baked → runtime mask):

| producer | workload | Δ |
|---|---|---|
| Go (`_start`, runtime + main) | 3.8 ms | ~0% (n.s.) |
| Rust (`_start`, std init) | 136 µs | +4.4% (one-time ~6 µs) |
| clang-C (nested-loop kernel) | 91 µs | −2.2% (free) |
| geomean | | +0.26% |

So on **real control flow the runtime mask is free-to-neutral** — the synthetic
spin loop's +5% is the empty-body pathology, which producers don't emit. (Zig
proper wasn't available; clang-C stands in as the LLVM-family third producer, and
Go/Rust are exact.)

Correctness verified both ISAs (amd64 + arm64/qemu): spec v1/v2, and the
infinite-loop terminate-on-cancel/timeout/close suite all pass with the runtime
mask + hoist.

**Status: shipped (stable API).** The real-producer data retired the cost
objection (free-to-neutral on Go/Rust/clang-C; the +5% is a synthetic-empty-loop
artifact), so the runtime-mask path plus a setter were landed:

- `wazy.SetInterruptCheckInterval(fn api.Function, interval uint64) error` (the
  runtime counterpart of the compile-time `WithInterruptCheckInterval`, in the
  root package) — bridges (interface assertion) to the native `callEngine`, which
  validates the interval (0 or power of two) and writes `execCtx.interruptCheckMask`
  with an atomic store. Per-function (per-callEngine) granularity: set it on the
  `api.Function` handle you will call. Its doc carries the awareness notes
  (raising the interval defers cancellation/GC yield for up to `interval`
  iterations — only for loops known to be bounded).
- **Gated to modules compiled with `WithCloseOnContextDone` + a non-zero
  `WithInterruptCheckInterval`** — that is the *only* configuration under which any
  interrupt-check code (hence the runtime-mask path) is emitted at loop headers;
  otherwise the setter errors loudly rather than silently no-op'ing. The
  interpreter engine also errors (no support).
- **Benignly racy:** the mask affects only yield frequency, never correctness, so
  another goroutine may retune a running function; because the mask is hoisted to
  each loop's preheader, a change takes effect on the *next* loop entry (the
  set-then-call use), not mid-spin.

Verified both ISAs (amd64 + arm64/qemu): spec v1/v2, the terminate-on-cancel/
timeout/close suite, and dedicated setter tests (retune + still-cancellable,
invalid-interval / not-compiled-for-it / unsupported-engine error paths).

This is the one relowering-shaped capability worth having, and notably it is the
thing a recompile-JIT could *not* do (a whole-module recompile only affects the
next instantiation; this retunes a live callEngine). It does not change the
broader verdict: no per-function optimizing hot-swap tier (§1–§2).
