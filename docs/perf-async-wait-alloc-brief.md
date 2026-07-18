# Design brief: eliminate the remaining async WAIT-path allocations

wazy = wazero-derived Go WebAssembly runtime, SINGLE-runnable async runtime (one goroutine holds
the baton per composition tree; multiple composition trees can run on different goroutines).
Branch perf/component-model-async. `BenchmarkCallAsyncAwaitImport` is at **7 allocs/op / 895 B**
(down from 11) after two safe tranches. Design how to eliminate the REMAINING per-WAIT allocations,
ranked by SAFETY, with an exact "definitely-done" reset/free point for each and a `-race`/leak/
use-after-free argument. Implementable by a Sonnet in stages. IMPORTANT: allocs/op is verifiable
now (load-independent); the *latency* payoff is NOT (the dev box is at load 36) — so favor changes
whose CORRECTNESS is provable now and note which need a quiet-machine wall-clock check before we
trust their value.

## The remaining per-WAIT alloc sites (profiled with -memprofilerate=1)
1. `async_host_import.go:288` — `st.applyResolve = func(vals) error {...}` — a per-call CLOSURE on
   `subtask.applyResolve`. Captures per-call `retPtr`,`memMod` + bind-time `iface`,`funcName`,
   `resultCount`,`resultUsesMem`,`resultType`,`outPtrIdx`,`resources`. Called at :83 and :374.
2. `async_host_import.go:445` — `ac := &AsyncCall{in, st, inCall:true}` — the host-import call handle
   (5 fields). ESCAPES: passed to `hi.asyncFn(ctx, args, ac)`; the host may retain it (Defer/Resolve).
3. `async_host_import.go:283` — `&subtask{state, lenders: []*resourceEntry{}}` (newSubtask). The
   `lenders` empty-slice literal may also be avoidable (start nil, grow on first lend).
4. `async_builtins.go:149` — `&waitableSet{}` (waitable-set.new; guest-created, guest-dropped).
5. The co-allocated `callbackFrame{task,guestTask}` from Tranche 1 (async_lift.go) — 1 alloc/call on
   EVERY async call (FirstLight's remaining 1).
6. (Also present) `subtask.onCancel func() error` closure — set from AsyncCall.OnCancel or the
   callee's requestCancellation.

## Read these (verify everything against the actual code — line numbers are approximate)
- `internal/component/instance/async_host_import.go`: buildAsyncHostWrapper (the whole fn + its two
  arms: plain-host-import and guest<->guest), AsyncCall (struct ~50, Resolve/Defer/OnCancel),
  applyResolve wiring, newSubtask.
- `internal/component/instance/waitable.go`: `subtask` struct (all fields + their lifecycle docs),
  `waitable`, `waitableSet`; how a subtask is parked (`resources.addEntry`), resolved
  (`resolve`/`deliverResolve`/`resolveDelivered`), and dropped (subtask.drop builtin).
- `internal/component/instance/async_builtins.go`: waitableSetNewHostFunc (:149), subtask.drop,
  waitable-set.drop — the guest-controlled free points.
- `internal/component/instance/async_lift.go`: invokeAsyncCallback / startAsyncExportTask (the
  callbackFrame alloc + the task/guestTask lifetime: created at entry, resolved at EXIT, but a
  PARKED task lives across WAIT rounds).
- `internal/component/instance/task.go`, `guest_task.go`, `stackful_task.go`, `sched.go`: task
  lifecycle, park/resume, reap on Close.
- Reference: `internal/component/abi/testdata/definitions.py` Subtask/Task/WaitableSet lifecycle.

## For EACH of the 6 sites, deliver
- **Technique** — pick the safest that works: (a) destructure a closure into typed fields +
  a bind-time-config pointer (no alloc, no pool — like the just-landed closure-free pendingSub);
  (b) sync.Pool with a precise Reset + a "definitely-done" return point; (c) eliminate/inline; or
  (d) leave it (say why pooling isn't safe). Prefer (a)/(c) over (b) where possible — a pool is only
  worth it when there's a single unambiguous return point AND the object can't still be referenced.
- **The definitely-done point** — the exact call site where the object is provably unreachable by
  any parked task, undelivered event, or host-held handle, so it can be reset/returned. For subtask:
  is it subtask.drop? after deliverResolve? Beware: a resolved-but-not-yet-delivered subtask still
  has a pending event; a host holding an AsyncCall via Defer resolves LATER.
- **The use-after-free / -race argument** — why the object is single-owner at the free point under
  the baton invariant; why sync.Pool's cross-P behavior is fine (or why not).
- **applyResolve/onCancel specifically** — which captured vars are bind-time (belong on a shared
  per-import config built once in buildAsyncHostWrapper) vs per-call (belong as subtask fields);
  give the exact field list and the method signature replacing the closure. Confirm applyResolve is
  set at exactly ONE site (so a single method works) — or handle the variants.
- **callbackFrame pooling** — the highest-frequency win (every async call). Where is the frame
  provably done (EXIT + result fully lifted by the caller)? What must Reset clear (all the guestTask/
  task fields, ctxStorage, resultBuf, park state) so a recycled frame can't leak stale state into
  the next call? This one MUST be -race + goleak stress-tested and wall-clocked when quiet.

## Also deliver
- A staged landing order (safest/most-verifiable-now first; pooling last), each stage a Sonnet-sized
  increment that keeps `TestAsyncWastConformance` 31/31 + `TestAsyncOracle` + `-race` green and
  reduces allocs/op measurably. Flag which stages' VALUE (not correctness) needs a quiet-machine
  wall-clock check. Note anything that would touch the sched/park core.
