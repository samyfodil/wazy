# WASI 0.2 Component Model runtime — build plan

Status: **core goal achieved** — a real off-the-shelf rustc-built WASI 0.2 component runs on wazy and prints "hello world"
Branch: `feat/wasip2-component-model`
Reviewed via: `/plan-eng-review` (2026-07-14, CLEARED)
Scope decision: **Real p2 CM runtime** — load arbitrary off-the-shelf `.component.wasm` at runtime.

## Achieved (2026-07-15)
- **Parse** real `.component.wasm` (incl. the 62KB rustc `wasi:cli/command` guest, nested components, full type index space).
- **Canonical ABI** — layout + memory store/load + flat lift/lower, all diff-verified against `definitions.py` (the differential oracle).
- **Instantiate + call**, both directions — exports lifted, host imports lowered; resources (own/borrow handle tables), streams, post-return, spilled args/results.
- **General multi-module instantiation graph** — the CLI guest's 4 core modules + 17 core instances + preview1 adapter wire and execute.
- **WASI 0.2 host layer** (`WithWASI`) — cli/io-streams/environment/preopens/filesystem. Real rustc guests run and are verified genuine (values flow through guest memory, varied inputs, not Go literals):
  - `real_adder` — add + `greet(string)->string`
  - `real_hello` — the full `wasi:cli/command` prints "hello world"
  - `real_args` — echoes args + env (host->guest `list<string>`/`list<tuple>`)
  - `real_readfile` — reads a file from a host FS
  - `real_transform` — reads `/input.txt`, uppercases, writes `/output.txt`
- 4 packages, all >=90% coverage; ~18 real bugs caught by the oracle + independent verification (incl. enum >32 cases, nested own/borrow resolution, missing get-random-bytes / stat-at).
- **Differential conformance vs wasmtime: 19/19 byte-identical** — a suite of real rustc `wasi:cli/command` programs (arithmetic/float/int edge cases, serde_json roundtrip, 10k collections, deep recursion, unicode casing, args/env, file read/write/multi-file, panic, exit, iterators) each run under wasmtime for golden stdout, then asserted identical on wazy (verified non-circular). `internal/component/instance/conformance_test.go`.

## Remaining for a production WASI runtime (not started; deliberate)
- WASI interface **breadth left**: sockets, real clocks, actual stdin input — the common file/cli/env surface is done and conformance-verified.
- Grow the conformance suite further; run the upstream wasi-testsuite.
- The **performance** pass (compliance-first was the plan; optimization deferred).
- **Conformance**: run the official component-model + WASI 0.2 suites; burn down.
- Decoder gaps in TODOS.md (resourcetype rep mislabel, type-sort *export* index tracking).
- The **performance** pass (plan was compliance-first; optimization deferred).

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

### Coverage gate (hard requirement)
Every package must reach **>=90% statement coverage** (`go test -cover`) before its
work is committed, and every fail-loud error branch must have a test that triggers
it. Self-authored happy-path tests are not enough — they missed 3 real ABI bugs
(sizeList, alignmentList, joinCoreTypes) because the fixtures only used list<u32>.
Coverage is measured, not assumed: happy path + every error path + boundary cases
(discriminant/flags boundaries, spill-to-memory, nested/empty aggregates). The
differential oracle covers ABI correctness; table-driven tests + real wasm-tools
fixtures cover the parsers. A commit that drops a package below the bar is blocked.

---

## Deferred (see TODOS.md)
- **Async / WASI 0.3 follow-on** — +8–10k LOC, after p2 solid and 0.3 spec settles.
- **Inherited-bug audit** — http body loss + unaudited fs/sockets from `wazero-wasip2`.

## References
- Component Model: https://github.com/WebAssembly/component-model (binary format, `definitions.py`)
- WASI 0.2: https://github.com/WebAssembly/WASI (vendored `.wit`)
- Prior art (interfaces, treat as suspect): https://github.com/OpenListTeam/wazero-wasip2
