# Cold-start baseline compilation tier — scope

**Status:** scoping only, no code. Companion to `relowering-exploration.md`, whose
verdict was: *don't tier up for speed (optimizing hot-swap buys ~0 on real code);
the one tiering with value is tiering **down** for cold-start.* This doc scopes
that down-tier concretely.

**One-line scope:** the tractable win is a **cheaper register allocator behind a
per-module compile flag** (Lever A) — it reuses the entire existing pipeline and
is additive. True **lazy per-function compile** (Lever B) is a separable,
research-grade project blocked by the rel32 direct-call design. Do A first, and
only after a gating measurement.

---

## 1. What actually costs cold-start compile (measured)

Compile-time breakdown on real producer modules (relowering-exploration.md §3):

| phase | share | notes |
|---|---|---|
| **RegAlloc** | **44%** | `backend/compiler.go:210` → `mach.RegAlloc()` → the generic `regalloc.Allocator[I,B,F].DoAllocation` |
| LowerToSSA | 13% | `frontend`, `compileLocalWasmFunction` `fe.LowerToSSA()` |
| SSA passes | 21% total | `ssa/pass.go` `RunPasses`; **opt passes only ~8%**, and they buy ~0 execution on producer code (C4) |
| lower + finalize + emit | ~22% | isa lowering, relocation, mmap |

**Correction to the naive plan.** "Skip the opt passes and use a cheap regalloc"
overweights the opt passes. Post-C4 (const-fold/CSE removed), `RunPasses` is
`passRedundantPhiEliminationOpt` + `passDeadCodeEliminationOpt` + dominator/layout
— and **most of that is load-bearing cleanup of wazy's *own* SSA construction, not
producer-redundancy optimization**. DCE resolves aliases later passes create;
phi-elim removes block params wazy emits. Skipping them risks *miscodegen*, not
just slower code, and reclaims only part of ~8%. So the SSA-pass lever is small
and risky. **The lever that matters is the 44% register allocator.**

## 2. Lever A — cheap tier-0 whole-module compile (TRACTABLE, do first)

A second, cheaper register allocator, selected per-module by a compile flag. The
current allocator (`regalloc.go:552`) is *already* a linear scan, but a good one:
it builds a loop tree (`loopTreeDFS`), tracks reloads, and places each spill at
the **lowest-common-ancestor** of its reload sites (`recordReload`,
`scheduleSpills`) — the sophistication that both improves code and costs the 44%.
A tier-0 allocator drops the loop analysis + LCA placement and spills at block
boundaries (or spills-most), trading code quality for compile speed.

**What it reuses (unchanged):** the whole-module compile path (`engine.go`
`compileModule`), `compileLocalWasmFunction`, the one-mmap/relocate/mprotect
finalize (`MmapCodeSegment` → `resolveRelocations` → `MprotectCodeSegment`,
engine.go:427-454), the EH tables, the PC→function mappers. **No new placement,
trampoline, or relocation machinery** — this is the key reason A is tractable: it
is the existing pipeline with one component swapped.

**The selection seam already exists.** `WithInterruptCheckInterval` folds a compile
parameter into `module.ID` (engine.go:275, the H6 seam), so a distinct value
yields a distinct cached `compiledModule` and the entire cache/ID/fileCache path
is unchanged. A `WithCompileTier(tier0)` flag folds in the same way.

**The one real new component:** a stripped `Allocator` implementation behind the
existing generic interface (`backend.Machine.RegAlloc` → `Allocator.DoAllocation`).
Estimated a few hundred LOC. It must produce *correct* code (the ABI/spill/reload
contract is identical; only allocation quality differs), so the gate is the full
spec suite (v1+v2, both engines' equivalent) + fuzzing, exactly as for the current
allocator.

**Effort: MEDIUM. Risk: MEDIUM** (a new allocator is correctness-critical, but it
is additive — gated behind a flag, the default tier-1 path is untouched).

**The gating measurement (do BEFORE wiring a product flag):** build the cheap
allocator, then A/B compile-time-saved vs execution-slowed on real modules
(interleaved, core-pinned, per benchmark-load-discipline). Target: the cheap
allocator ~halves the 44% regalloc → ~−20% whole-module compile, at some execution
cost X% (more spills). Lever A only pays when, over the invocation's lifetime, the
compile saving exceeds X% × runtime. That crossover is the whole decision:

- one-shot / very-short-lived (compile dominates run) → tier-0 wins;
- long-running (execution dominates) → tier-1 wins;
- **CompileCache hit (A3) → neither: the 2nd instantiation skips compile entirely,**
  so tier-0 only ever helps the cold cache *miss*.

## 3. Lever B — lazy per-function compile (HARD, research-grade, separable)

The win Lever A *cannot* give: **skip compiling functions that are never called.**
Large modules make this huge — a 3.8 MB Go module has 2313 functions; a CLI
one-shot or a single component export touches a fraction, yet today compilation is
fully eager (`compileModule` compiles all locals up front).

**The blocker (relowering-exploration.md §2, verified).** Intra-module `call`
lowers to a **PC-relative rel32 branch with the displacement baked in by
relocation** (`amd64/machine.go` `ResolveRelocations`) — a direct caller holds no
pointer, so it cannot target an un-compiled function. Only `call_indirect`/
`call_ref` re-load `executable` from the `functionInstance` per call and are
swappable. Two ways out, both with a cost:

- **(a) First-call trampolines + caller patching.** Each function starts as a stub
  that compiles-then-jumps; when compiled, rewrite every caller's baked rel32.
  Needs a reverse caller map + re-mprotect-to-RW/patch/RX-again of caller pages,
  and per-function mmap placement (today one contiguous module buffer). Complex,
  but keeps the hot path's zero-indirection direct calls.
- **(b) Route direct calls through an indirection cell.** Simple to make lazy, but
  **defeats the rel32-direct-call optimization** — a throughput regression on the
  common path (every intra-module call gains a load). Net-negative for
  long-running code.

**Reuses:** `compileLocalWasmFunction` (already an isolated per-function compile),
the PC→function mappers (`functionIndexOf`/`compiledModuleOfAddr` — the read side).
**Missing entirely:** first-call-trampoline emission, per-function placement/
relocation, and the caller-patching-or-indirection strategy.

**Effort: HIGH** (the direct-call patching is the research part). **Risk: HIGH.**
Scope option (a) vs (b) as its own spike before committing.

## 4. Phasing & recommendation

1. **Phase 1 — Lever A, gated on its own measurement.** Build the cheap allocator,
   A/B the compile-speedup vs execution-slowdown curve on real modules, and only
   wire the `WithCompileTier` flag if the crossover favors the target workload.
   Additive, reuses the whole pipeline, standalone PR-able.
2. **Phase 2 — Lever B, only if** Phase 1's whole-module tier-0 compile is *still*
   too slow for the cold-start goal **and** never-called-function skip (large
   modules) is the actual bottleneck. Spike the direct-call strategy first.

## 5. Where it pays / doesn't

- **Pays:** serverless/FaaS cold start, CLI one-shots, per-request instantiation of
  large components — cold-start-sensitive *and* short-lived; and (Lever B) large
  modules where most functions never run.
- **Doesn't:** long-running hot loops (you want tier-1 code); small modules
  (compile is already sub-millisecond — greet_rust's 90 funcs compile in µs); the
  CompileCache-hit path (A3 already skips compile on re-instantiation).

## 6. The honest bottom line

The whole tier is **conditional on a measurement that hasn't been taken**: does a
cheap allocator's ~−20% compile win justify its execution slowdown for the target
workload's lifetime? Lever A is the tractable, additive way to *get* that
measurement (build the allocator, A/B it) without any of Lever B's research risk.
Until a cold-start product goal exists, this stays scoped-not-built — consistent
with the relowering verdict that wazy's headroom is compile *speed*, and the
biggest single compile cost is the register allocator.
