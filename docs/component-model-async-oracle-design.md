# Component Model Async Trace-Oracle Design

Differential conformance harness proving wazy's async runtime state machine
(task/subtask/waitable/stream/future + canon builtins) matches the vendored
reference implementation (`internal/component/abi/testdata/definitions.py`),
the same way the sync oracle (`gen_oracle.py` / `oracle_golden.json` /
`oracle_test.go`) proves flatten/size/alignment parity.

Status: design. Companion docs: `component-model-async-runtime-design.md`,
`component-model-async-phase3-design.md`.

---

## 0. Architecture

Three artifacts, mirroring the sync oracle's layout exactly:

| Artifact | Role |
|---|---|
| `internal/component/abi/testdata/async_scenarios.json` | The shared battery: an ordered list of scenarios, each an abstract op sequence. Hand-written, checked in. |
| `internal/component/abi/testdata/gen_async_oracle.py` | Loads `definitions.py` (same `load_definitions()` loader as `gen_oracle.py`), interprets each scenario against the reference objects, writes `async_oracle_golden.json`. Run manually; output checked in. |
| `internal/component/instance/async_oracle_test.go` | Reads `../abi/testdata/async_scenarios.json` + `async_oracle_golden.json`, interprets the SAME scenarios against wazy's builtins (direct host-func calls, the `async_builtins_test.go` style — no wasm needed for the builtin surface, `memModule(t)` for real linear memory), and deep-diffs the produced trace against the golden trace. |

The test lives in `instance` (not `abi`) because the async state machine is
unexported there; it reaches the shared testdata by relative path, exactly as
`wast_conformance_test.go` reaches its suites.

The oracle's unit of comparison is a **trace**: one JSON entry per executed
op, capturing everything observable at the canon-builtin boundary (packed i32
returns, event tuples read back from memory, trap sites), plus a final
handle-table snapshot. Two runtimes that produce byte-identical traces for
every scenario have observably identical state machines *for the surface the
scenario language can express* (§5 bounds that surface honestly).

### 0.1 Execution model shared by both interpreters

A scenario is the program of **exactly one guest task** ("the root task") in
**exactly one component instance**, executed op-by-op, top to bottom:

- **Python** (`gen_async_oracle.py`): the op list is the body of a scripted
  core callee invoked through the real embedding API —
  `Store.invoke(store.lift(callee, ft, opts, inst), on_start, on_resolve)`
  with `ft.async_ = True`, `opts.async_ = True`, `opts.callback = None`
  (a stackful async lift: the reference's green thread genuinely blocks
  inside blocking builtins). Ops call `ref.canon_*` directly from inside the
  green thread, so `current_thread()/current_task()` are real. A driver loop
  (§3.4) pumps `store.tick()` and fires deferred host actions while the
  scenario thread is blocked.
- **Go** (`async_oracle_test.go`): ops call the builtin `hostFuncDef`s
  directly from the test goroutine on a hand-built
  `Instance{sched: &sched{}, mayLeave: true, resources: newHandleTable(),
  resolve: ...}` with `activeTask` set to a hand-built
  `task{inst: in, gt: &guestTask{}, onResolve: <trace hook>}` in state
  `taskStarted`. wazy has no stackful lift, but its blocking builtins
  (`waitable-set.wait`, sync `subtask.cancel`, sync stream/future copies)
  drive `in.sched` internally — sequential ops + internally-pumping blocking
  ops is semantically the same shape as the Python green thread. Deferred
  host actions are `sched.enqueue` thunk chains (§3.4).

Traps end a scenario on both sides: Python catches `ref.Trap`
(`definitions.py` line 23), Go `recover()`s the builtin panic. Everything
after the trapping op is unexecuted, and the trace records the trap site.

---

## 1. The scenario language

### 1.1 Scenario file schema

```json
{
  "scenarios": [
    {
      "name": "subtask-deferred-wait",
      "desc": "one deferred import, joined, waited, dropped",
      "task_result": "u32",
      "ops": [ { "op": "...", ... }, ... ],
      "go_trap_contains": "already resolved"
    }
  ]
}
```

- `name` — unique key into the golden file.
- `task_result` — `null` or `"u32"`: the root task's declared result type
  (what `task.return` lifts). u32-only by design: the async oracle tests the
  *state machine*; value-shape richness is the sync oracle's job (§5).
- `go_trap_contains` — optional, wazy-only: a substring asserted against the
  Go panic text when the golden says this scenario traps. NOT diffed against
  Python (reference `trap()` carries no message — §2.3).
- `ops` — the program. Op indices (0-based position in this array) are the
  trace's `op` field and the trap-site coordinate.

### 1.2 Handle references

Table indices are themselves oracle output (index reuse is observable), so
ops never hard-code indices for handles they created. An op that returns
handles may bind them to names; later ops reference names:

- `{"as": "s"}` on a creating op binds its result handle (for `stream.new` /
  `future.new`: `{"as_read": "r", "as_write": "w"}` binds both halves).
- A handle-typed argument is either `{"$": "name"}` (previously bound) or a
  bare integer (deliberately raw — for stale/wrong-index trap scenarios;
  `0` for `waitable.join`'s "leave set" arm).

Both interpreters resolve `{"$": name}` from their own environment; the
*golden trace* then proves the two environments assigned identical indices.

### 1.3 Op vocabulary

Grouped: **B** = pure builtin op (one canon builtin call, 1:1 with a
`ref.canon_*` function and a wazy `hostFuncDef`); **H** = harness pseudo-op
(scripted environment, defined identically on both sides).

#### Waitables and sets (B)

| op | args | reference | wazy | trace |
|---|---|---|---|---|
| `waitable-set.new` | `as` | `canon_waitable_set_new` | `waitableSetNewHostFunc` | `ret [i]` |
| `waitable-set.wait` | `set`, `cancellable` (bool, default false) | `canon_waitable_set_wait` | `waitableSetWaitHostFunc` | `event {code,p1,p2}` |
| `waitable-set.poll` | `set`, `cancellable` | `canon_waitable_set_poll` | `waitableSetPollHostFunc` | `event {code,p1,p2}` |
| `waitable-set.drop` | `set` | `canon_waitable_set_drop` | `waitableSetDropHostFunc` | `ret []` |
| `waitable.join` | `w`, `set` (or 0) | `canon_waitable_join` | `waitableJoinHostFunc` | `ret []` |

`wait`/`poll` pass a fixed scratch `ptr` (harness constant, e.g. 0x100); the
interpreter reads back `(p1,p2)` from memory at `ptr`/`ptr+4` and folds them
into the `event` trace entry with the returned code — so the memory write
path of `unpack_event`/`storeEvent` is covered without a separate `mem.check`.

#### Subtasks / async host imports (B + H)

| op | args | notes |
|---|---|---|
| `import.call` (H+B) | `as`, `resolve_after` (int rounds; 0 = synchronous), `result` (u32 or null), `behavior` | The scripted callee. Python: a `FuncInst` closure passed to `canon_lower(callee, ft, opts, flat_args)` with `opts.async_ = True` — the real reference lower path, producing the real packed return. Go: `buildAsyncHostWrapper` over an `AsyncHostFunc` implementing the same behavior via `AsyncCall` (`Resolve` / `Defer` chains / `OnCancel`). Trace: `ret [packed]` — either `RETURNED` (2, no table entry) or `state | subtaski<<4` (index observable!). Result value delivery goes through the real retptr (trailing out-pointer param, fixed harness address per call); a follow-up `mem.check` observes it. |
| `subtask.cancel` (B) | `sub`, `async` (bool) | `canon_subtask_cancel` / `subtaskCancelHostFuncGraph`. Trace: `ret [state]` or `ret [4294967295]` (BLOCKED). |
| `subtask.drop` (B) | `sub` | `canon_subtask_drop` / `subtaskDropHostFunc`. |

`behavior` (default `"resolve"`) selects the scripted callee's shape,
implemented identically on both sides:

- `"resolve"` — start immediately (reference `on_start()` at call time; wazy
  lifts args eagerly, matching), then resolve with `result` after
  `resolve_after` rounds (0 = inside the call: reference calls `on_resolve`
  before returning; wazy calls `ac.Resolve` before the AsyncHostFunc
  returns).
- `"never"` — never resolves (deadlock scenarios).
- `"cancel-resolves-cancelled"` — on cancellation request, resolve as
  cancelled synchronously inside `on_cancel` (Python: `on_resolve(None)`;
  Go: `ac.OnCancel(func(){ ac.ResolveCancelled() })`).
- `"cancel-completes"` — on cancellation request, resolve *normally* with
  `result` synchronously inside `on_cancel` (completion beats cancel).
- `"ignore-cancel"` — register a no-op `on_cancel`; still resolve at the
  scheduled round (tests cancel racing completion / async-cancel BLOCKED).

Import signature is fixed: `(func async (result u32))` when `result` is
non-null, `(func async)` otherwise — no params (params exercise the sync
oracle's lifting, not the state machine).

#### Task lifecycle (B + H)

| op | args | notes |
|---|---|---|
| `task.return` (B) | `vals` ([] or [u32]) | `canon_task_return` / `taskReturnHostFuncGraph`. Must match `task_result`. Trace: `ret []` plus a `task-resolve {cancelled:false, vals}` entry from the harness's `on_resolve` hook. |
| `task.cancel` (B) | — | `canon_task_cancel` / `taskCancelHostFuncGraph`. Trace: `ret []` plus `task-resolve {cancelled:true}`. |
| `host.cancel-root` (H) | `after` (rounds) | The embedder requests cancellation of the root task, delivered while it is blocked in a cancellable `waitable-set.wait`. Python: fires the `on_cancel` returned by `Store.invoke` (i.e. `task.request_cancellation`) from the driver. Go: `sched.enqueue`-chained thunk calling `t.requestCancellation()`. Registered at its op position, fires later (§3.4). Trace: `ret []` (registration only). |
| `context.get` (B) | `slot` | trace `ret [v]` |
| `context.set` (B) | `slot`, `val` | trace `ret []` |
| `backpressure.inc` / `backpressure.dec` (B) | — | trap coverage only (underflow); entry-blocking is guest-to-guest, out of scope (§5) |

#### Streams and futures (B)

All within the single root task: the rendezvous partner is the *other end
held by the same task* — an async op on one end parks it (BLOCKED), the
sync op on the other end completes the copy. This is the reference's own
copy engine end-to-end (buffers, `on_copy`/`on_copy_done`, pending-event
thunks, packed `result | progress<<4`).

| op | args |
|---|---|
| `stream.new` | `elem` (`"u8"`/`"u32"`), `as_read`, `as_write` — trace `ret [ri, wi]` (i64 unpacked into two u32s for JSON safety) |
| `stream.read` / `stream.write` | `end`, `ptr`, `n`, `async` (bool) — trace `ret [packed]` (BLOCKED = 4294967295) |
| `stream.cancel-read` / `stream.cancel-write` | `end`, `async` |
| `stream.drop-readable` / `stream.drop-writable` | `end` |
| `future.new` | `elem`, `as_read`, `as_write` |
| `future.read` / `future.write` | `end`, `ptr`, `async` |
| `future.cancel-read` / `future.cancel-write` | `end`, `async` |
| `future.drop-readable` / `future.drop-writable` | `end` |

A blocked (async, BLOCKED-returning) copy is later observed by joining the
end to a set and waiting: the `STREAM_READ`/`STREAM_WRITE`/`FUTURE_*` event
tuple (code, end-index, packed result) lands in the trace via the `event`
entry. Element types restricted to u8/u32 (numeric memmove path + the
generic path via u32; list/string payload lifting is sync-oracle turf).

#### Error contexts (B)

| op | args |
|---|---|
| `error-context.new` | `msg`, `as` — harness writes `msg` bytes at a scratch ptr first; trace `ret [i]` |
| `error-context.debug-message` | `ec`, then `mem.check` of the [ptr,len] out-struct + string bytes |
| `error-context.drop` | `ec` |

Interleaved with other `new` ops these pin the *shared* handle-table
allocation order across kinds.

#### Memory (H)

| op | args |
|---|---|
| `mem.write` | `ptr`, `bytes` (hex) — precondition linear memory (stream write sources, task.return spill inputs) |
| `mem.check` | `ptr`, `len` — capture `len` bytes at `ptr` into the trace: `mem {bytes: "hex"}`. This is how rendezvous copy contents, import retptr results, and event out-params beyond the standard scratch slots are diffed. |

Both sides use one flat 64 KiB memory with a bump realloc (Python: a
`MemInst(bytearray(65536), 'i32')` + trivial realloc; Go: `memModule(t)`'s
real memory + `cabi_realloc`). Scenario scratch layout is by convention:
event ptr at 0x100, import retptrs at 0x200 + 16*k (k = import call
ordinal), stream buffers at 0x1000+, error-context strings at 0x2000+.
The generator validates no overlap.

---

## 2. The golden-output schema

`async_oracle_golden.json`:

```json
{
  "scenarios": {
    "subtask-deferred-wait": {
      "trace": [
        {"op": 0, "kind": "ret",   "vals": [1]},
        {"op": 1, "kind": "ret",   "vals": [33]},
        {"op": 2, "kind": "ret",   "vals": []},
        {"op": 3, "kind": "event", "code": 1, "p1": 2, "p2": 2},
        {"op": 4, "kind": "mem",   "bytes": "2a000000"},
        {"op": 5, "kind": "ret",   "vals": []},
        {"op": 6, "kind": "task-resolve", "cancelled": false, "vals": [7]}
      ],
      "table": [
        {"index": 1, "kind": "waitable-set"}
      ]
    },
    "double-task-return": {
      "trace": [ "...", {"op": 3, "kind": "trap"} ],
      "table": null
    }
  }
}
```

Entry kinds:

- `ret` — the op's core return values, as the guest would see them:
  unsigned decimal u32s (packed subtask states, packed copy results, table
  indices, context values). `stream.new`/`future.new`'s i64 is split
  `[ri, wi]`.
- `event` — for `wait`/`poll`: returned code + `(p1,p2)` read back from the
  event ptr. Covers EventCode NONE/SUBTASK/STREAM_READ/STREAM_WRITE/
  FUTURE_READ/FUTURE_WRITE/TASK_CANCELLED with their exact payloads —
  including table indices in `p1` and packed `result|progress<<4` in `p2`.
- `task-resolve` — emitted by the root task's `on_resolve` hook the moment
  it fires (ordering relative to op returns is part of the trace):
  `{cancelled, vals}` with u32 vals.
- `mem` — `mem.check`'s captured bytes, lowercase hex.
- `trap` — terminal. Coordinates only: the op index. No text (§2.3). The
  scenario's remaining ops are absent from the trace, and `table` is `null`
  (post-trap table state is not part of the reference's observable contract;
  wazy's trap tears down the instance).

Scenario footer:

- `table` — final handle-table snapshot, sorted by index: every live entry's
  `{index, kind}` with kind in `waitable-set | subtask | stream-read |
  stream-write | future-read | future-write | error-context`, plus
  state where the reference exposes it observably: subtasks carry
  `"state": <Subtask.State int>`, stream/future ends carry
  `"copy": "idle" | "copying" | "cancelling" | "done"`. Python reads
  `inst.handles.array`; Go iterates `in.resources` (an
  `entriesSnapshot()` test helper: sorted `(index, kind, state)`).

Diffing is a single `reflect.DeepEqual` (or per-entry loop with a rich
failure message: scenario, op index, field) of the Go-produced trace against
the golden — byte-comparable by construction given §3's pins.

### 2.3 Trap classification: site, not text

The reference's `trap()` raises a bare `Trap` — there is no message to
compare. The differential fact the oracle proves is **where** the trap fires
(which op) and that *everything before it* matched. Go-side panic text is
still asserted (against `go_trap_contains` in the scenario file) so wazy's
diagnostics stay honest, but that assertion is wazy-only metadata, not a
cross-runtime diff. A scenario that traps on one side and not the other, or
at different ops, is a state-machine divergence — exactly the bug class this
oracle exists to catch.

One asymmetric trap is normalized: **deadlock**. wazy detects it in the
blocking builtin (`errAsyncDeadlock` from `sched.drive` — a real runtime
guarantee); the reference just blocks forever (its green thread waits on a
`ready_func` nothing will satisfy). The Python driver detects quiescence
(scenario thread blocked + no ready threads in `store.waiting` + no pending
deferred actions) and emits `{"op": k, "kind": "trap", "deadlock": true}`;
Go records the same entry when the recovered panic wraps
`errAsyncDeadlock`. The `deadlock` flag keeps it distinguishable from a
reference-side `Trap` at the same op.

---

## 3. Determinism pins

Every source of scheduler freedom in the reference, and how it is pinned to
the SAME choice wazy's FIFO scheduler makes:

1. **`DETERMINISTIC_PROFILE = True`** — set by the generator after module
   load (`ref.DETERMINISTIC_PROFILE = True`). Kills `wait_until`'s
   `random.randint(0,1)` early-return coin flip (definitions.py ~406): a
   thread whose predicate is already true still enqueues + blocks, exactly
   one behavior.
2. **`random.shuffle` → no-op** (patch `ref.random.shuffle = lambda x: None`
   post-load). `WaitableSet.get_pending_event` (~812) then scans `elems` in
   **join order** — which is precisely wazy's `waitableSet.getPendingEvent`
   (first joined member with a pending event; `waitable.go` documents this
   pin explicitly).
3. **Singleton-thread invariant** instead of patching `random.choice`. The
   remaining `random.choice` sites (`Store.tick` ~612,
   `Task.request_cancellation` ~540, `canon_lift`'s sync loop ~2206) all
   choose among *ready threads* — and every oracle scenario has exactly ONE
   guest thread (the root task's; scripted imports are host closures, not
   threads), so every candidate set is a singleton or empty. The generator
   also patches `ref.random.choice = lambda l: l[0]` defensively and
   `assert len(l) == 1` — a failure means the scenario broke the invariant,
   not that ordering was silently random (note: `list(set)` iteration order
   is genuinely nondeterministic across CPython runs, so this assert is
   load-bearing).
4. **Handle-table allocation** — not pinned, *verified*: both tables start
   at index 1 and reuse freed indices LIFO (reference `Table.free.pop()`;
   wazy `handleTable.addEntry` pops `t.free[n-1]`). Every `ret`/`event`
   carrying an index diffs this for real, including reuse after drops.
5. **Deferred host actions** (`resolve_after` / `after`) — the one place the
   two implementations' *internal step granularity* genuinely differs
   (flagged honestly): Python fires actions from a driver-loop round
   counter; Go realizes each delay unit as one FIFO `sched` run-queue hop
   (`resolve_after: N` = an N-deep `AsyncCall.Defer` chain; `host.cancel-root
   after: N` likewise). Internal step counts diverge, but trace entries are
   only recorded at **op boundaries**, and at op granularity both schedules
   deliver completions in `(after, registration-order)` order. To keep even
   the tie case out of play, the generator **rejects scenarios where two
   deferred actions share an `after` value**. Actions only fire while the
   scenario is blocked inside a blocking builtin (Go: that's the only place
   `sched.drive` pumps; Python driver: only ticks while the scenario thread
   is in `store.waiting`) — so a `poll` never observes a completion that a
   `wait` hasn't paid a blocking round for, on either side.
6. **Blocking-wait cancellation point** — `waitable-set.wait`'s
   cancellable-wake is checked at the drive predicate on both sides
   (reference `wait_until`'s `deliver_pending_cancel` prologue + resume-
   with-Cancelled; wazy's `pred := hasPendingEvent || cancelDeliverable`
   then `deliverPendingCancel` before reading the set). `host.cancel-root`
   against a task blocked in a *cancellable* wait yields
   `(TASK_CANCELLED,0,0)`; against a *non-cancellable* wait it parks as
   PENDING_CANCEL — both arcs are scenario-expressible and diffed.
7. **NaN/values** — not applicable: payloads are u32-only by schema (§1.1),
   the sync oracle owns value-shape parity.

Regeneration discipline (same as the sync oracle): `async_oracle_golden.json`
is committed; CI runs only the Go side. Changing `async_scenarios.json`
requires rerunning `gen_async_oracle.py` (a staleness hash of the scenarios
file embedded in the golden, checked by the Go test, makes forgetting this
loud).

---

## 4. Starter battery (23 scenarios)

Happy paths first, then trap edges. Names are the golden keys.

| # | name | what it pins |
|---|---|---|
| 1 | `ws-new-drop-reuse` | set new (index 1), new (2), drop (1), new -> reuses 1 (LIFO free list) |
| 2 | `poll-empty-none` | poll on empty set -> `(NONE,0,0)` |
| 3 | `join-rejoin-leave` | subtask join set A, rejoin set B (implicit leave), join 0; final table |
| 4 | `import-immediate` | `resolve_after:0` -> `ret [2]` (RETURNED, no table entry), retptr `mem.check` |
| 5 | `import-deferred-wait` | `resolve_after:1` -> packed `1\|i<<4` (STARTED), join, wait -> `(SUBTASK,i,2)`, retptr bytes, drop; empty final table |
| 6 | `import-two-fifo` | A `after:1`, B `after:2`, both joined one set -> wait yields A then B (completion order + join-order event scan) |
| 7 | `import-two-sets-poll` | A,B in different sets; wait set2 (pays rounds), poll set1 -> pinned cross-set visibility |
| 8 | `double-task-return` | task.return, task.return -> trap at op 2 (`go_trap_contains: "already resolved"`) |
| 9 | `task-cancel-undelivered` | fresh task.cancel -> trap (no cancellation delivered) |
| 10 | `host-cancel-blocking-wait` | cancellable wait + `host.cancel-root after:1` -> `(TASK_CANCELLED,0,0)`, then task.cancel -> `task-resolve {cancelled:true}` |
| 11 | `host-cancel-uncancellable-then-poll` | non-cancellable wait on import `after:1` (completes normally), `host.cancel-root after:2`, then cancellable poll -> TASK_CANCELLED via deliver_pending_cancel prologue |
| 12 | `wait-empty-deadlock` | wait on empty set, nothing pending -> normalized deadlock trap (§2.3) |
| 13 | `ws-drop-nonempty` | pending subtask joined, waitable-set.drop -> trap |
| 14 | `wrong-handle-kinds` | wait(subtask-handle) -> trap; sibling scenario: join(w=set-handle), drop(stale index after remove) |
| 15 | `subtask-drop-unresolved` | `behavior:"never"`, subtask.drop -> trap (resolve not delivered) |
| 16 | `subtask-cancel-async-blocked` | `behavior:"ignore-cancel"` `after:2`, async cancel -> BLOCKED, join+wait -> `(SUBTASK,i,RETURNED)` (completion wins), then subtask.drop |
| 17 | `subtask-cancel-sync-cancelled` | `behavior:"cancel-resolves-cancelled"`, sync cancel -> `ret [4]` (CANCELLED_BEFORE_RETURNED), drop |
| 18 | `subtask-cancel-racing-completion` | import `after:1`, pay a round (wait on a second import `after:2`), then cancel the now-resolved-undelivered A -> immediate `ret [2]` fast path, no BLOCKED |
| 19 | `subtask-cancel-twice` | cancel (BLOCKED), cancel again -> trap (`cancellation_requested`) |
| 20 | `stream-rendezvous` | stream.new u8 -> `[1,2]`; mem.write source; write async (BLOCKED); read sync -> `COMPLETED\|n<<4`; join writer end + wait -> `(STREAM_WRITE,wi,packed)`; mem.check copied bytes; drop both ends |
| 21 | `stream-zero-len-then-drop-writable` | zero-length read/write handshake packed results; then drop-writable, read -> `DROPPED`; drop-readable-while-copying sibling -> trap |
| 22 | `stream-cancel-read` | read async (BLOCKED), cancel-read sync -> `CANCELLED\|0<<4`; state back to idle in final table |
| 23 | `future-once` | future.new u32, write async (BLOCKED), read sync -> value + `COMPLETED`; writer event via wait; second write -> trap (end DONE, not idle); plus `error-context` + `context.get/set` + `backpressure.dec` underflow folded into two small housekeeping scenarios |

(#23's tail housekeeping ops may split into `misc-context-ec` and
`backpressure-underflow` during implementation; the table above is the
coverage contract, not a hard 23-file count.)

Coverage check against the builtin surface: waitable-set new/wait/poll/drop,
waitable.join, subtask new(+packed return)/resolve/cancel(sync+async+racing+
double)/drop(+unresolved), task.return(double)/task.cancel(fresh+delivered),
host-side request_cancellation (delivered at cancellable wait AND parked
PENDING_CANCEL), context.get/set, backpressure inc/dec underflow,
stream/future new/read/write/cancel/drop + rendezvous + zero-length +
dropped-peer + not-idle traps, error-context new/debug-message/drop, index
reuse, deadlock. Not covered -> §5.

---

## 5. Honest scope

**Oracle-able with this design** (single root task, scripted host imports,
direct builtin calls):

- The whole waitable/waitable-set/subtask state machine and its trap edges.
- The canon_lower async path's packed returns and pending-event delivery
  (via the real `buildAsyncHostWrapper` on the Go side and real
  `canon_lower` on the Python side).
- Root-task lifecycle: return/cancel arcs, PENDING_CANCEL vs
  CANCEL_DELIVERED, embedder-requested cancellation at blocking waits.
- Intra-task stream/future rendezvous, cancellation, drop semantics, packed
  copy results, zero-length handshakes.
- Handle-table index allocation/reuse across all entry kinds.

**NOT oracle-able here — needs the .wast suites / e2e component tests
instead** (and it is fine that it does):

1. **The callback-lift loop itself** (`CallbackCode` EXIT/YIELD/WAIT,
   `unpack_callback_result`, `guest_task` parking/`pumpSnapshot` yield
   rounds, exclusive-thread bookkeeping around callback re-entry). The Go
   side cannot run `invokeAsyncCallback` without a real wasm callback;
   scripting one via a fake `api.Module` would test the harness, not the
   runtime. Covered today by `async_first_light_test.go`,
   `guest_guest_async_test.go`, and the wast conformance suites.
2. **Guest-to-guest composition**: `may_enter`/`enter_from` gating,
   backpressure *entry parking* (`num_waiting_to_enter` FIFO fairness),
   cross-instance subtasks, borrow lending across subtasks
   (`num_borrows`/`add_lender` — needs real resources flowing through real
   calls). The builtin-level trap (`task.return` with borrows outstanding)
   is reachable only through composition.
3. **Sync-lift-over-async / post-return**, multi-task instances, and
   `thread.*` builtins (wazy doesn't implement stackful threads).
4. **Value-shape parity** of async payloads beyond u32/u8 — deliberately
   excluded; the sync oracle + wast value suites own lift/lower.
5. **Trap message text** — site-only cross-runtime (§2.3).

**Genuinely hard to make byte-comparable (flagged):**

- **Scheduler step granularity** (§3.5): Python driver rounds vs Go run-queue
  hops are different clocks. Pinned by (a) recording only at op boundaries,
  (b) distinct `after` values per scenario, (c) actions fire only during
  blocking ops. A future op that exposes intra-block progress (e.g. a
  step-count probe) would break comparability — don't add one.
- **Deadlock** is a wazy runtime guarantee with no reference analog;
  normalized via the driver's quiescence detector (§2.3). The *shape* of
  quiescence ("nothing can ever run") is the same predicate on both sides,
  but it is a harness convention, not a reference-vs-wazy diff.
- **`task.return` result-type validation**: wazy validates against the bound
  export's declared type (`t.be`); the harness root task has no `boundExport`,
  so the Go check is skipped when `t.be == nil` while the reference compares
  against `task.ft.result`. The oracle keeps `task_result` and the
  `task.return` canon's type equal by construction, so the check is inert on
  both sides; a mismatch scenario would exercise reference-only validation
  and is excluded (belongs to wast).

---

## 6. Implementation order (for the implementing agent)

1. `async_scenarios.json` with battery #1-#8 (no imports needed beyond
   `resolve_after 0/1`), the two harness skeletons, golden generation,
   green diff. This forces the trace/golden plumbing + determinism pins
   before the hairy ops exist.
2. Subtask cancellation battery (#15-#19): implement `behavior` variants on
   both sides.
3. Root-task cancellation (#9-#11): `host.cancel-root`, Go `gt` stub,
   Python `on_cancel` capture from `Store.invoke`.
4. Streams/futures (#20-#23) + memory ops.
5. Staleness hash + `go_trap_contains` assertions + failure-message polish
   (scenario/op/field in every diff error).

Python harness skeleton (gen_async_oracle.py):

```python
ref = load_definitions()            # reuse gen_oracle.py's loader verbatim
ref.DETERMINISTIC_PROFILE = True
ref.random.shuffle = lambda l: None
_real_choice = ref.random.choice
ref.random.choice = lambda l: (l[0] if len(l) == 1 else
                               (_ for _ in ()).throw(AssertionError(
                                   "oracle invariant: non-singleton choice")))

def run_scenario(sc):
    trace, env = [], {}
    store = ref.Store()
    inst  = ref.ComponentInstance(store)              # ctor wires inst.store
    mem   = ref.MemInst(bytearray(65536), 'i32')
    opts  = ref.CanonicalOptions(memory=mem, realloc=bump_realloc(mem),
                                 async_=True, callback=None)
    ft    = make_ft(sc["task_result"])                # async_=True

    deferred = []                                     # [(after, fire_fn)] distinct afters
    def callee(flat_args):                            # the scenario program
        for k, op in enumerate(sc["ops"]):
            try:
                step(k, op, trace, env, deferred, mem, inst)   # dispatch table
            except ref.Trap:
                trace.append({"op": k, "kind": "trap"}); return []
        return []

    fi = store.lift(callee, ft, opts, inst)
    on_cancel = store.invoke(fi, on_start=lambda: [],
                             on_resolve=lambda r: trace.append(resolve_entry(r)))
    round_no = 0
    while scenario_thread_alive(inst):                # driver loop, §3.4
        round_no += 1
        fire_due(deferred, round_no, on_cancel)       # (after, registration) order
        if not tick_or_quiesce(store):                # no ready thread, nothing due
            trace.append({"op": current_op, "kind": "trap", "deadlock": True}); break
    return {"trace": trace, "table": snapshot(inst.handles)}
```

Go harness skeleton (async_oracle_test.go): per scenario, fresh
`memModule(t)` + `Instance` + root `task` (state `taskStarted`,
`gt: &guestTask{}`, `onResolve` appending `task-resolve`), a `map[string]uint32`
env, an op dispatch switch building/calling the same `hostFuncDef`s the
direct builtin tests use (`waitableSetWaitHostFunc(in, binary.Canon{Cancellable: c})`,
`buildAsyncHostWrapper(...)` for `import.call`, etc.), each op wrapped in a
`func() (entry traceEntry)` with `defer recover()` translating panics into
the `trap` entry (matching `errAsyncDeadlock` -> `deadlock: true`, asserting
`go_trap_contains`), then `diffTraces(t, sc.Name, got, golden)`.
