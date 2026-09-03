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

Sometimes the guest awaits something *your Go code* has not finished: a database round trip, an
HTTP response. Register that import with `component.WithAsyncImport` rather than `WithImport`, so
the canonical lowering knows the call may not return immediately. The `AsyncHostFunc` it takes is
handed an `*component.AsyncCall` and may return without resolving it — the result is delivered
later, from any goroutine, with `call.Resolve`:

```go
release := make(chan struct{})

getImport := component.WithAsyncImport("get", "", // a bare top-level func import "get"
	func(_ context.Context, _ []component.Value, call *component.AsyncCall) error {
		go func() {
			<-release // stand-in for I/O finishing on some other goroutine
			call.Resolve([]component.Value{uint32(42)})
		}()
		return nil // the import call returns unresolved
	},
	nil, // no params
	[]component.TypeDesc{component.PrimitiveDesc{Prim: "u32"}}, // -> u32
)

inst, err := component.Instantiate(ctx, r, componentBytes, getImport)
```

Start the export with `CallAsync` instead of `Call`. It returns a `*component.PendingCall` the
moment the guest parks on that import, so the host goroutine is free while the guest is suspended.
`Await` then drives the scheduler until the task completes:

```go
p, err := inst.CallAsync(ctx, "run-async")
if err != nil {
	return err
}

close(release) // let the other goroutine complete the import

out, err := p.Await(ctx)
```

`PendingCall` has exactly three methods. `Done()` returns a channel closed when the call resolves,
fails, is cancelled, or the instance closes — select on it to see whether the guest actually
parked. `Await(ctx)` blocks until then and returns the lifted results. `Cancel(ctx)` abandons the
call and reaps the parked guest state.

:::note
Two limits on this path today: one `CallAsync` may be outstanding per instance, and the export must
be an async (callback) lift — `CallAsync` on a synchronous export returns an error rather than a
handle. `Await` is the sole driver once `CallAsync` returns, so call it from a single goroutine;
`Resolve` may be called from any.
:::

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

[Components, threads and async](../../examples/components/) runs an
async export, a thread-spawning component, a five-thread map-reduce over one shared array, and the
`CallAsync` flow above end to end.

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
