# WASI 0.2 Component Model runtime — build plan

Status: planned
Branch: `feat/wasip2-component-model`
Reviewed via: `/plan-eng-review` (2026-07-14, CLEARED)
Scope decision: **Real p2 CM runtime** — load arbitrary off-the-shelf `.component.wasm` at runtime.

---

## Goal

Give wazy a real WebAssembly Component Model runtime so it can **load and run an
arbitrary, off-the-shelf `.component.wasm`** implementing WASI 0.2 — not a
pre-flattened core module, a real component.

First pass goal is **spec compliance**, keeping performance *in kind* (no gross
regressions, no premature optimization). Optimization is a second pass, after
compliance is proven.

Explicitly **out**: async / WASI 0.3, nested component composition, build-time
codegen bindings, WasmGC (orthogonal), 100% conformance-suite green.

---

## Why this is bounded, not a "someday second runtime"

The component runtime is a **layer above** wazy's core wasm engine. It parses the
component container and instantiates the inner core modules on the *existing*
engine. Blast radius on the native backend and all prior perf work (I5, C-series)
is near zero. We add a layer; we do not touch the hot path.

The Canonical ABI — the hard, subtle core — is published by the spec as
**executable Python** (`definitions.py`). That turns "get the ABI exactly right"
from hand-asserted guesswork into a **differential oracle**: lift-then-lower a
value through both our Go engine and the Python reference, assert byte-identical.
That single asset closes the edge-case tail that would otherwise leak silent
miscompiles into real guests.

---

## Architecture decisions (from eng review)

### D1 — Canonical ABI: data-driven engine
One lift/lower engine that walks WIT type descriptors at runtime (record / variant
/ enum / flags / option / result / list / tuple / string / handle), mirroring
`definitions.py`. DRY (one engine, every type), works for any component loaded at
runtime, and testable line-for-line against the executable spec.
Rejected: codegen bindings (needs build-time WIT, can't load unknown guests);
hand-written per-type Go (the wit-go trap, not DRY, endless edge tail).

### D2 — Interfaces: WIT files are the oracle
The interface *surface* (signatures, types, resource shapes) comes from the
**official WASI `.wit` files**, vendored. Syscall *bodies* may reference
`wazero-wasip2` as one input, but WIT is the source of truth. Same WIT-derived
type descriptors drive both the ABI engine and host-function registration.
Drop `wazero-wasip2`'s `wit-go` glue and per-type managers — superseded by D1 and
the generic handle tables.

### D3 — WIT-text parser: in
Build a WIT lexer/parser/resolver package. Not needed to *run* a compiled
component (types come from the component binary's type section), but kept for
load-time world validation / tooling. User decision, overriding the minimal path.

### Package layout
```
component/binary   — component container parser (runtime type source)
component/wit      — WIT-text lexer/parser/resolver (D3)
component/abi      — data-driven lift/lower engine (the spine; oracle = definitions.py)
component/resource — generic per-instance handle tables, own/borrow/drop
component/instance — instantiation + import/export linking (single-level)
component/wasi/*   — interface impls (WIT-defined surface, syscall bodies)
```

---

## Pipeline

```
.component.wasm
   ├─ component/binary : parse container, extract type descriptors + import/export graph
   ├─ component/instance : instantiate inner core module(s) on wazy's existing engine
   ├─ component/abi : wire imports/exports via lift/lower over linear memory + realloc
   ├─ component/resource : own/borrow handle tables per instance
   └─ run start / exports ; guest↔host calls flow through the ABI engine
```

---

## Test strategy (gate = oracle + real-guest E2E)

v1 gates on:
1. **Differential ABI oracle** — every WIT type lift/lower diffed against
   `definitions.py`. Property-based; closes variant-flattening, string re-encode,
   spill-to-memory tails automatically.
2. **Real-guest E2E** — a real Rust/TinyGo `wasi:cli/command` component runs;
   a `wasi:http` round-trip works.

Official `WebAssembly/component-model` + WASI 0.2 conformance suites are vendored
and run, but remaining failures are a **tracked burn-down**, not a v1 blocker.

### Failure modes — the two silent ones are covered
| Codepath | Failure | Guard |
|---|---|---|
| binary parser | adversarial/truncated → panic | fuzz + return-err (no panic) |
| **ABI lift/lower** | nested/variant miscompute → **silent wrong data** | **differential oracle** |
| **resource tables** | borrow across drop → **use-after-free** | lifecycle + leak tests |
| realloc/post-return | missed post-return → leak/UAF | leak assert |
| instance linking | missing import | clean err at instantiate |
| wasi:http | body dropped (inherited bug) | audit + round-trip test |

With the plan as written, no silent critical failure is left uncovered.

---

## Milestones (checkpoints, not one number)
1. **Parse** — load a real `.component.wasm`, dump its type section + import/export graph.
2. **Lift/lower** — differential oracle green on all WIT types.
3. **Hello-world** — a real `wasi:cli/command` component prints to stdout E2E. *(money milestone)*
4. **Resources + http** — a component doing a `wasi:http` round-trip with handles.
5. **Burn-down** — conformance suites vendored, failures tracked toward zero.

## Parallelization (worktree lanes)
```
Lane A: component/binary      ─┐
Lane B: component/wit          │  parallel worktrees
Lane C: component/abi (vs stub)┤  ← critical path, staff heaviest, start first
Lane D: component/resource     │
Lane E: component/wasi/* bodies┘
        ↓ (merge A,C,D)
Lane F: component/instance → wire interfaces → E2E hello-world  (integration)
```

## Effort
~23–25k LOC (base ~20–22k + WIT parser ~2–3k). CC-driven ~4–4.5 weeks to
milestones 1–4, conformance tail tracked after.

---

## Execution policy — model routing

First pass optimizes for **compliance**; keep perf in kind; optimize in a later pass.

| Situation | Model | Notes |
|---|---|---|
| Write code (fast first draft) | **Haiku** | bulk implementation from this plan + WIT/spec refs |
| Fix issues surfaced by running tests | **Sonnet** | iterate on test failures, wire-ups, mechanical bugs |
| Verify + fix hard issues | **Opus** | ABI edge cases, resource lifetimes, subtle miscompiles |
| User sign-off decision needed | **Opus → then Codex xhigh** | Opus proposes, run it by `codex exec -c model_reasoning_effort=xhigh`; if no good solution emerges, escalate to the user |
| Reference oracles | — | `definitions.py` (ABI), vendored WASI `.wit` (interface surface), `wazero-wasip2` (syscall bodies, treat as suspect) |

Rules:
- Compliance first, performance "in kind" — no premature optimization in pass 1.
- Silent-failure paths (ABI lift/lower, resource own/borrow) always get a test
  before they're considered done.
- The differential ABI oracle is non-negotiable; it is the safety net.

---

## Deferred (see TODOS.md)
- **Async / WASI 0.3 follow-on** — +8–10k LOC, after p2 solid and 0.3 spec settles.
- **Inherited-bug audit** — http body loss + unaudited fs/sockets from `wazero-wasip2`.

## References
- Component Model: https://github.com/WebAssembly/component-model (binary format, `definitions.py`)
- WASI 0.2: https://github.com/WebAssembly/WASI (vendored `.wit`)
- Prior art (interfaces, treat as suspect): https://github.com/OpenListTeam/wazero-wasip2
