# Component Model **async** — implementation scoping

Successor to `component-model-wasip2-plan.md`. Adds the async canonical ABI (tasks,
waitables, streams, futures, error-context — the WASI 0.2 → 0.3 / "Preview 3" direction).

This is a synthesis of three independent scopings (a codebase analysis, a Fable pass, and a
Codex-xhigh pass) that converged almost entirely. Where they diverged (the async host API) the
decision is called out in §4. Full source scopings are archived in the PR that adds this file.

Behavior reference: `internal/component/abi/testdata/definitions.py` (2803 lines — the upstream
canonical-ABI reference; already models tasks, waitables, streams/futures, error-context,
backpressure, and the `thread.*` builtins). **Transliterate it; do not re-derive.**

---

## 1. Current state (verified in the tree)

- **Execution is strictly synchronous.** `Instance.Call`/`CallExport` → `invoke` → `lowerParams`
  → one `coreFn.CallWithStack` → `liftResult` → optional `post-return` (`instance/instance.go`).
- **The decoder tolerates async *options* but rejects async *builtins*.** `binary/decoder.go`
  decodes canon opts `0x06 async` / `0x07 callback`, but the canonical section only accepts lift,
  lower, and `resource.{new,drop,rep}` (kinds `0x00–0x04`); any real async canon (`task.return`,
  `stream.read`, `waitable-set.wait`, …) fails to decode today. `descriptor.go`'s `readDefValType`
  has no cases for `error-context`/`future`/`stream` — a `stream<u8>` in a func type fails to decode.
- **WIT already parses `async func`, `future<T>`, `stream<T>`** (`wit/parser.go`, `wit/ast.go`);
  `error-context` is not yet parsed.
- **The handle table is resources-only.** `instance/resource.go`'s `handleTable` holds only
  `resourceEntry` (1-based, free-list reuse, own/borrow, lends, dtors). The reference's table is
  **unified**: one index space (one free list, guest-observable reuse) over
  `ResourceHandle | Waitable | WaitableSet | ErrorContext` + stream/future ends.
- **Borrow drops are rejected outright** today ("borrow handles cannot be dropped by the
  receiver") precisely because there's no `Task` to scope them to — async introduces the Task and
  retires that ceiling.
- **No re-entrancy state** (`may_enter`/`may_leave`) exists; the sync path never re-enters.
- **Host builtins wire through one switch**: `graph.go`'s `canonToDef`/`buildCanonHostModule`
  builds a private single-func Go host module per canon core func, memory/realloc resolved via
  `canonMemoryAndRealloc`. Every async builtin is ~one more `hostFuncDef` case with a fixed
  i32/i64 signature — no reflection, no `WithFunc`, identical for both engines.
- **Guest→host→guest re-entry already works** on both engines (resource dtors call the guest from
  inside `resource.drop`; `ServeHTTP` drives exports). What does **not** exist and cannot without
  engine work: **suspending a live core-wasm frame and resuming it later.** This one fact dictates
  the whole sequencing.

## 2. The pivotal decision: callback ABI only

The async ABI has two guest shapes:

- **`async` + `callback`** — the guest export returns a packed `(code, waitable-set)` i32
  *immediately*; the host re-invokes the `callback` core export with `(event_code, p1, p2)` until
  it returns `EXIT`. **The guest frame never blocks — every suspension is a normal core return.**
  Implementable purely host-side, **engine-agnostic, zero engine changes.** This is what
  wit-bindgen/rustc emit for async on runtimes without stack-switching.
- **`async` without `callback`** (stackful) — the guest blocks *inside* `waitable-set.wait` with a
  live frame beneath it. Requires fibers/stack-switching or parked green threads + the `thread.*`
  builtins.

**Decision: implement callback lift only.** A stackful (`async`, no `callback`) lift decodes fine
and **fails loud at bind time**. Stackful + `thread.*` is deferred indefinitely (§3, Phase 4) —
it's the highest-risk, engine-touching work and isn't needed for the first useful Preview-3
stream/future components.

## 3. Phases

| # | Deliverable | Size (LOC incl. tests) |
|---|---|---|
| **0** | Decode + `stream`/`future`/`error-context` types plumbed through the ABI (no execution) | ~2.5k |
| **1** | **MVP**: unified handle table, Task/Subtask/WaitableSet, callback lift, async host imports, core builtins | ~4–5k |
| **2** | Streams/futures/error-context runtime + host stream/future API | ~3–3.5k |
| **3** | Cancellation, guest↔guest async, full re-entrancy, borrow scopes | ~2.5k |
| **4** | *Deferred:* stackful lift + `thread.*` (needs engine continuations) | — |
| **5** | *Separate scoping:* WASI 0.3 / Preview-3 host (async wasi:io/http/fs/sockets, real readiness) | — |

### Phase 0 — decode + type plumbing (mechanical, no execution)
- `binary/decoder.go`: canon kinds `0x05–0x23` (task.cancel, subtask.cancel, resource.drop-async,
  backpressure.set, task.return, context.get/set, yield, subtask.drop, stream.\*, future.\*,
  error-context.\*, waitable-set.\*, waitable.join). **Verify every opcode against fresh
  `wasm-tools` output, not memory or the vendored definitions.py vintage.**
- `binary/component.go`: `Canon` gains `Async bool`, `MemIdx`, `Imm uint32`, a result-type field.
- `binary/descriptor.go`: defvaltype `0x64/0x65/0x66` → `ErrorContextDesc`, `FutureDesc{Element}`,
  `StreamDesc{Element}`.
- `abi/{flatten,flat,memory,plan}.go`: all three flatten/load/store as **i32 handles** (like
  own/borrow; lifted Go value is `uint32`). Add async flattening (`MAX_FLAT_ASYNC_PARAMS=4`; async
  lower → i32; async-lift-with-callback → i32).
- `wit/`: add `error-context` parsing (async/stream/future already parse).
- Tests: decoder round-trips on wasm-tools/wit-bindgen fixtures; oracle vectors for the three new
  type kinds; a per-kind "async canon %#x not yet executable" fail-loud bind test.

### Phase 1 — the MVP (callback async, end-to-end)
**MVP definition (the acceptance test):** a real rustc/wit-bindgen component with an
`export async func` (callback ABI) that awaits an async-lowered host import, run on wazy — host
starts the call, guest returns `WAIT(wset)`, host completes the import, scheduler delivers
`EventCode.SUBTASK`, callback resumes, guest calls `task.return`, callback returns `EXIT`. Plus the
degenerate first-light: an async export that computes synchronously and `task.return`s with no wait.

- **Unified handle table lands first, as an isolated no-behavior-change PR** gated on the existing
  `resources`/`multiple-resources` `.wast` suites. `handleTable` becomes `Table[tableEntry]` with
  kinds `resourceEntry` (existing) + `subtask`/`waitableSet`/`streamEnd`/`futureEnd`/`errorContext`;
  every existing resource method keeps its exact signature and error text (type-assert + fail-loud
  on kind mismatch = the spec's `trap_if(not isinstance(...))`).
- New files `instance/{task,waitable,sched,async_builtins}.go`:
  - `task` (mirrors reference `Task`): state machine, `onStart/onResolve/caller`, `numBorrows`,
    two context slots, `exclusive` flag.
  - `subtask` (mirrors `Subtask`): state machine, `onCancel`, `lenders []*resourceEntry` (finally
    using the dormant `Lend/Unlend`), pending-event closure, `flatResults`.
  - `waitable` (embedded) + `waitableSet`; event codes 0–6.
  - instance-level: `backpressure int`, `mayEnter/mayLeave`, `numWaitingToEnter`, `exclusiveTask`.
- Builtins (as `canonToDef` cases, fixed i32 sigs): `task.return`, `task.cancel`, `context.get/set`,
  `backpressure.set` (+`inc/dec` if pinned to newest reference), `waitable-set.new/wait/poll/drop`,
  `waitable.join`, `subtask.drop`, `yield`.
- Call path: `finalizeBoundExport` detects `async`/`callback` → routes to `invokeAsyncCallback`
  implementing `canon_lift`'s callback loop verbatim (pack/unpack `code | wset<<4`, exclusive-thread
  acquire/release around each callback call, backpressure gate on task start). Async-lowered
  imports: `buildHostWrapper` grows an async arm (subtask in table, packed `[state | subtaski<<4]`
  return); host imports get a completion API (`WithAsyncImport(..., resolve func([]abi.Value))`).

### Phase 2 — streams, futures, error-context
- `sharedStream`/`sharedFuture` mirroring the reference's **rendezvous** model (single pending
  buffer, no host-side elastic buffer — backpressure is inherent); `copyEnd` waitable with
  `{idle,copying,cancelling,done}`; element copies reuse the compiled `abi.Load/Store` plans, with a
  `none_or_number_type` fast path (`stream<u8>` → `copy()`).
- Builtins: `stream.{new,read,write,cancel-read,cancel-write,drop-readable,drop-writable}`, future's
  seven twins, `error-context.{new,debug-message,drop}`. Lift/lower of stream/future/error-context
  values as own-like handle transfer (readable end moves, writable stays — same rep-transfer the
  composition path already does for resources).
- Host API: `component.StreamWriter`/`StreamReader`/future equivalents driving `sched` — this is
  what makes streams *usable* before Preview-3 WASI exists.

### Phase 3 — cancellation, guest↔guest, re-entrancy, borrow scopes
- `subtask.cancel` (+`BLOCKED`), `task.cancel`/`TASK_CANCELLED`, `CallAsync`'s cancel func;
  async-lowered calls to *guest* exports (composition A→B); `resource.drop async`; borrow scopes
  (`resourceEntry.borrowScope *task` + `task.numBorrows`) retiring resource.go's borrow-drop ceiling.
- Enforce `may_enter`/`may_leave` reentrance traps and the `trap_if(not candidates)` deadlock trap.

### Phase 4 (deferred) / Phase 5 (separate scoping)
Phase 4: stackful lift (engine fibers or handoff-goroutines) + `thread.*`. Everything in 0–3 is
forward-compatible (implicit thread ≡ task for callback tasks). Phase 5: `wasi:*@0.3`, real
netpoller/timer readiness feeding the scheduler, an `Instance.Serve`-style public pump.

## 4. Decision — async host API (the one divergence)

The two passes split on how the embedder drives async:

- **Keep `Call` synchronous but let it drive callback-async to completion** (blocking), + a
  `CallAsync(ctx, name, args, onResolve) → cancel` for genuine concurrency. Matches wazy's
  "settle synchronously" host philosophy; the first-light async export works through the existing
  `Call`.
- **Keep `Call` sync-only, reject async exports, make `CallAsync` (task handle: Poll/Wait/Cancel/
  Result) the sole async entry.** More conservative; avoids hiding re-entrancy bugs behind `Call`.

**Recommendation (blend):** ship `CallAsync` as the real async entry in the MVP (both agree on it),
**and** make `Call` transparently drive a callback-async export that resolves without external
concurrency — failing loud if the guest blocks on a host import only the embedder can complete. This
gets the best demo (an async export through the unchanged `Call` API) while `CallAsync` owns real
concurrency. Sync-lower-to-unresolved-callee = a bounded nested `sched` loop inside the hostcall
(legal: sync lowers may block; depth bounded by graph depth; no-progress traps as deadlock).

## 5. Forced changes to shipped code (honest list)
- `instance/resource.go`: unified table (Phase 1) + borrow-scope fields (Phase 3). **Highest
  regression risk** — isolate as a no-behavior-change PR, gate on existing `.wast` suites.
- `binary.Canon` + decoder canon/defvaltype switches (Phase 0). Mechanical.
- `instance.go` `invoke()`: **bind-time routing only.** Async-free components (everything shipping
  today) pay **zero**; sync exports of async-*using* components pay two flag writes + one pooled
  Task. Gate with `benchmarks/vs-wazero` before/after.
- `graph.go` `canonToDef`: ~30 additive cases. `abi` flatten/load/store: three additive i32-handle
  kinds.

## 6. Conformance strategy
1. **Trace oracle (the workhorse)** — `gen_async_oracle.py` drives definitions.py's embedding API
   through scripted scenarios with **monkeypatched determinism** (`random.choice→first`,
   `shuffle→no-op`, deterministic profile) matching wazy's FIFO scheduler; emits golden JSON of
   ordered event tuples, packed i32 results, **table indices (reuse is observable)**, and trap
   sites; Go replays and diffs. Same offline-golden shape as the existing `gen_oracle.py`.
2. **`.wast` suites** — keep the existing seven green as the table-rework regression gate; vendor
   upstream async value/builtin suites as they stabilize via the existing `json-from-wast` harness.
3. **Real components** — wit-bindgen/rustc async fixtures per phase, on interpreter + native
   (+ arm64 under qemu per existing practice).
4. **Discipline** — ≥90% coverage, a named test for every `trap_if` branch (the async ABI is mostly
   trap edges; per project history this gate catches what the happy path hides).

## 7. Top risks (ranked)
1. **Exclusive-thread / backpressure / cancellation semantics** — the subtlest, most recently
   churned area of the spec; where wasmtime historically found bugs. Mitigate: transliterate
   definitions.py branch-for-branch; trace-oracle every branch.
2. **Unified-table rework** under shipped resource semantics. Mitigate: isolated no-behavior-change
   PR gated on `resources`/`multiple-resources` `.wast`.
3. **Spec / toolchain drift** — the vendored definitions.py vintage vs `Binary.md` vs what
   `wasm-tools`/`wit-bindgen` emit *today* (canon opcode numbering, `backpressure.inc/dec` vs `set`,
   thread builtins). Mitigate: pin versions in testdata, validate Phase-0 decode against freshly
   built `wasm-tools`, record the pinned upstream commit in the package doc.
