---
title: Async — the WASI 0.3 ABI
description: Components that suspend, await and resume — streams, futures, tasks and cooperative threads, on goroutines and channels.
---

wazy runs the Component Model **async ABI**, the machinery WASI 0.3 is built on: components that
suspend, await, and resume. It passes all **31 official async conformance suites**, zero skipped.

## Calling an async export

You call it exactly like a synchronous one. The scheduler runs underneath and `Call` returns when
the task completes:

```go
out, err := inst.Call(ctx, "run-async")
```

That is the whole API for the common case. Nothing about your call site changes because the guest
decided to await something.

## When the host is the slow half

`CallAsync` returns a `*component.PendingCall` — a live invocation suspended on an import your Go
code has not answered yet. Another goroutine resolves it:

```go
pending, err := inst.CallAsync(ctx, "run-async")
if err != nil {
	return err
}

go func() {
	value := fetchFromSomewhereSlow()
	pending.Complete(ctx, "example:store/kv", "fetch", []component.Value{value})
}()

out, err := pending.Wait(ctx)
```

Register the import itself with `component.WithAsyncImport` rather than `WithImport`, so the
canonical lowering knows the call may not return immediately.

## What the ABI covers

- **Callback and stackful lift** — both shapes. A task returning WAIT or YIELD is driven by a
  deterministic per-composition scheduler. A stackful task suspends on a goroutine with an
  unbuffered-channel baton, so exactly one runs at a time — race-free by construction, verified
  under `-race`.
- **Streams and futures** — `stream<T>` and `future<T>` with rendezvous copy and per-element
  `own<R>` transfer, sync and async read and write.
- **Task lifecycle** — subtasks, cancellation, backpressure, context-local storage, and borrow
  scopes that hold across async calls.
- **`thread.*`** — a cooperative fiber runtime (`thread.new-indirect`, `yield`, `suspend`,
  `yield-then-resume`) on that same primitive.

## The concurrency model

Goroutines and channels back futures, streams and threads, so a parked guest costs one goroutine
and nothing else. But *cooperative* is literal: exactly one task in a composition runs at a time,
handed the baton over an unbuffered channel. The async ABI buys you suspension and composition, not
parallelism inside one component.

[`examples/component`](https://github.com/samyfodil/wazy/tree/main/examples/component) runs an
async export, a thread-spawning component, and a five-thread map-reduce over one shared array.

## Scope

What runs is the Component Model **async ABI**. The reworked WASI 0.3 host-interface worlds —
`wasi:io`, `wasi:sockets` and `wasi:http` at 0.3 — land once that specification settles, and no
`wasm32-wasip3` target exists yet, here or anywhere. Guests today target `wasm32-wasip2` and use
the async ABI through it.

## Conformance

The 31 official async `.wast` suites run in CI on both engines, and a differential trace-oracle
byte-compares wazy's async runtime against the specification's reference implementation
(`definitions.py`).

One official fixture — the sync-streams case — is itself broken upstream, missing a store. The
31/31 above runs against a corrected copy, and the fix is filed as
[component-model#679](https://github.com/WebAssembly/component-model/pull/679).
