# `CallAsync` — host-side non-blocking component calls

Status: **implemented (first slice).** A working `Instance.CallAsync` /
`PendingCall` ships alongside this document; an async export that awaits an
import completed from another goroutine runs end to end, verified under `-race`
with the existing async suite (all 31 conformance suites) still green. The
design below is what was built; the "Implemented" section at the end records the
exact surface and current limits.

## Motivation

`Instance.Call` / `CallExport` are synchronous: they drive the async scheduler
to completion and return the results. That is correct for every component whose
async work can make progress *within* the call (awaiting other guest tasks,
in-process host imports, streams/futures, threads).

It cannot express one thing: **an import that resolves externally** — a host
function that starts real I/O and completes later, from another goroutine or an
OS event, after the originating call would have returned. Today that path is a
hard stop, and the runtime already says so by name in two places:

- `sched.drive` → `errAsyncDeadlock`: *"the async task is suspended but the run
  queue is empty and no parked task is ready; an import resolved externally
  requires CallAsync (not yet implemented)."*
- `AsyncCall.Resolve` / `ResolveCancelled` panic when called off the scheduler:
  *"external completion requires CallAsync (not yet implemented)."*

So the feature is already scoped negatively by the code: **CallAsync = let an
`AsyncCall` be `Resolve`d from an arbitrary goroutine after the host call
returned, and let the host observe the eventual result.**

## What already exists (most of it)

The async substrate is built and conformance-tested; CallAsync reuses it whole:

| Piece | Where | Reused as-is |
|---|---|---|
| Lift/lower, task.return, cancel | `async_lift.go`, `host_import.go` | yes |
| Suspend/resume (callback loop **and** stackful goroutine) | `sched.go`, `guest_task.go`, `stackful_task.go` | yes |
| Park/run-queue scheduler (`park`, `enqueue`, `step`, `drive`, `drainReady`) | `sched.go` | yes |
| Async host import w/ deferred completion | `async_host_import.go` (`AsyncCall.Defer`, `Resolve`, `OnCancel`) | yes |
| Cancellation + reaping | `requestCancellation`, `reapParkedGoroutines` | yes |

A blocking `invoke` already *parks and would return a handle* at exactly the
right moment — it just currently treats "parked with an empty runq" as a
deadlock error instead of "hand control back to the host."

## Proposed API

```go
// CallAsync starts an export and returns as soon as the task first parks
// (or completes). The task makes further progress only when an outstanding
// AsyncHostFunc is Resolve()d, or when Await drives it.
func (in *Instance) CallAsync(ctx context.Context, export string, args ...abi.Value) (*PendingCall, error)

type PendingCall struct { /* ... */ }

func (p *PendingCall) Done() <-chan struct{}                          // closed on resolve/cancel/fail
func (p *PendingCall) Await(ctx context.Context) ([]abi.Value, error) // block until Done (or ctx)
func (p *PendingCall) Cancel(ctx context.Context) error               // request cancellation
```

An async host import gains a legal external completion:

```go
component.WithAsyncImport("wasi:io/poll", "pollable.block",
    func(ctx context.Context, args []abi.Value, ac *component.AsyncCall) {
        go func() { <-realIODone; ac.Resolve(nil) }() // legal under CallAsync
    }, params, results)
```

## The gap is one concurrency boundary — not the scheduler

The ABI is done. The cost is concentrated in a single place: **the instance is
deliberately single-runnable and lock-free** (a baton handoff over channels;
every state access is commented "only the driving goroutine touches this").
Everything internal — callback tasks, stackful tasks, `thread.*` — is
cooperatively scheduled, never concurrent. External completion is the first
goroutine that lives *outside* the baton.

Split the resume path into two hops:

```
[I/O goroutine completes] --hop A--> [driver goroutine] --hop B--> [parked fiber]
        (off the baton)     (must serialize)    (holds baton)   (cooperative switch)
```

- **Hop B — driver ↔ fiber** already works (the channel baton / callback loop).
  It can be *optimized* (see coro, below) but needs no new capability.
- **Hop A — external goroutine → driver** does not exist. It is the whole
  feature: get an off-baton completion onto the one goroutine allowed to touch
  scheduler state, safely, even when nobody is currently driving.

Single-runnable itself must stay — it is a correctness requirement, not a style
choice: the CM async model is defined single-threaded (the differential
trace-oracle pins exact interleavings) and one instance shares one linear
memory / handle table that guest code was compiled to mutate single-threaded.
Parallelism across *instances* already exists (each has its own `sched` on its
own goroutine). CallAsync must add hop A **without** loosening single-runnable.

## Implementation shape: actor-loop driver (+ optional `coroswitch`)

The cleanest way to add hop A while preserving the baton is to make the
baton-holder a **long-lived per-instance driver goroutine** — an actor. Outside
goroutines never touch scheduler state; they post commands.

```go
// One per Instance, spawned lazily on the first CallAsync, stopped by Close.
func (in *Instance) driveLoop() {
    for cmd := range in.mailbox {          // hop A: the serialization boundary
        switch c := cmd.(type) {
        case cmdStart:          in.runUntilPark(c.task)          // hop B
        case cmdExternalResolve: c.ac.applyResolve(c.vals)       // mark ready...
                                 in.runUntilPark(c.ac.task)      // ...then hop B
        case cmdCancel:         in.requestCancellation(c.task)
        case cmdClose:          in.reapAll(); return
        }
    }
}

// AsyncCall.Resolve, from ANY goroutine, becomes a non-blocking post:
func (ac *AsyncCall) Resolve(vals []abi.Value) {
    if ac.inCall || ac.in.sched.pumping {   // already on the driver — run inline (today's path)
        ac.applyResolveInline(vals); return
    }
    ac.in.mailbox <- cmdExternalResolve{ac, vals} // external — hand to the driver
}
```

`runUntilPark` is today's `sched.drive`, stopping at the next park instead of
erroring on an empty runq. `PendingCall.Await` blocks on a `Done()` channel the
driver closes at resolution; `PendingCall.Cancel` posts `cmdCancel`.

**Hop B — the switch — can optionally use `runtime.coro`** (`newcoro` /
`coroswitch`, what `iter.Pull` is built on) instead of the channel baton: it
does a *direct* driver→fiber transfer, cheaper than a channel round-trip through
the Go scheduler. It slots in exactly at `runUntilPark`'s resume of a stackful
fiber. Caveats that keep it optional, not foundational:

- **Unexported.** `//go:linkname`-only, no compatibility promise — acceptable
  inside one file, a liability to bet the core loop on.
- **Still a goroutine.** coro is `g`-backed, so it saves switch *latency*, not
  per-fiber *memory*.
- **Single-consumer.** Only the owning driver may `coroswitch` a coro; an
  arbitrary I/O goroutine cannot — which is *why* it can only serve hop B, never
  hop A. It reinforces the actor loop rather than replacing it.

So coro composes with the actor loop (driver *is* the single consumer) but does
not enable CallAsync on its own. Ship the actor loop first with the existing
channel baton; swap hop B to `coroswitch` later purely for latency if measured.

## Lifetime, cancellation, races (H3)

- A `PendingCall` outstanding at `Close` must be cancelled and reaped
  (`requestCancellation` + `reapParkedGoroutines` exist; wire them to `cmdClose`).
- `ctx` cancellation on the original call → `cmdCancel`.
- Double-`Resolve`, resolve-after-cancel, resolve-after-close: a state-machine
  guard on `AsyncCall` (it already tracks `inCall`) turns the second into a
  no-op.
- The driver goroutine's lifetime is the mailbox: lazily spawned on first
  `CallAsync`, drained and stopped on `Close`.

## What is *not* hard

Lifting/lowering, task suspension for both lift shapes, cancel, deferred
completion, and the "park with empty runq" detection are all done. The
`errAsyncDeadlock` site becomes a branch: if a `PendingCall` is driving, return
the handle instead of erroring.

## Pay-nothing guarantee

A component that never calls `CallAsync` must keep the current
fully-synchronous, lock-free, single-goroutine-of-control path **unchanged**: no
driver goroutine spawned, no mailbox, no lock in the hot suspend/resume path.
The actor loop exists only once an async call is actually outstanding. This is a
hard requirement and a test.

## Difficulty verdict

**Days, not hours; concentrated, not sprawling.** ~90% is reuse. The real work
is **hop A**: a lazily-spawned per-instance actor loop + mailbox, a
`PendingCall`/`Await` surface, the `AsyncCall.Resolve` on/off-driver split, and
`Close`/ctx lifecycle wiring — landing mostly in `sched.go`, a new
`callasync.go`, and the `AsyncCall` guard. The risk is entirely in the
concurrency boundary, so it lives or dies on `-race` tests: external resolve vs
`Close`, vs `ctx` cancel, vs a second `CallAsync`, plus the pay-nothing check.

`coroswitch` for hop B is a **separate, optional** latency optimization, not on
the critical path and easy to defer.

Not a research problem — the design is already implied by the existing
`AsyncCall` / `sched` seams. It's a focused systems change with a sharp,
testable blast radius.

## Implemented

The first slice landed (`internal/component/instance/callasync.go`, plus small
edits to `async_host_import.go`, `sched.go`, `instance.go`, and the public
`component` alias). It followed the plan above with one refinement.

Surface:

```go
func (in *Instance) CallAsync(ctx, export string, args ...abi.Value) (*PendingCall, error)
func (p *PendingCall) Done() <-chan struct{}
func (p *PendingCall) Await(ctx context.Context) ([]abi.Value, error)
func (p *PendingCall) Cancel(ctx context.Context) error
```

How hop A actually works: an `Instance` gains an `asyncActive` atomic, an `amu`
mutex, a `sync.Cond`, and a `mailbox []asyncCompletion`. While a `CallAsync` is
outstanding, `AsyncCall.Resolve` from an off-driver goroutine appends to the
mailbox under `amu` (a pure queue op — it touches no scheduler state) and
signals the cond; the sole driver (`CallAsync`'s initial pump, or `Await`)
drains and applies completions on its own goroutine via `sched.driveToQuiescence`
(a non-erroring `drive` that stops at a park instead of raising
`errAsyncDeadlock`). Single-runnable is preserved exactly: one goroutine ever
drives; external `Resolve` only enqueues.

The refinement over the sketch: the driver-vs-external discriminator is the
**per-`AsyncCall` `inCall` flag** (made `atomic.Bool`), not `sched.pumping`.
Because `inCall` is per-call, a truly-external `Resolve` always reads it false —
its host invocation has already returned — so a synchronous in-call `Resolve`
still takes the inline RETURNED fast path while an external one queues, with no
goroutine-identity check and no race (proven by the immediate + external tests
both passing under `-race`). This sidesteps the "external reads `pumping` true
mid-drive" hazard the sketch left open.

The **pay-nothing guarantee** holds: with `asyncActive` false, `Resolve` takes
its exact previous branch and the synchronous `Call` path is byte-for-byte
unchanged — no driver goroutine, no lock, no cond. The mailbox/actor machinery
exists only while a `CallAsync` is live.

Current limits (each a clean follow-up, none load-bearing):

- One outstanding `CallAsync` per `Instance` at a time.
- The export must be an async (callback) lift; stackful-lift `CallAsync` is not
  wired yet.
- `Cancel` is a hard abort (reap + resolve the handle with an error), not a
  graceful Component Model `task.cancel` delivered to the guest.
- `runtime.coro` for hop B is **not** used — the channel/cond baton is fine;
  it remains the optional latency-only swap described above.

