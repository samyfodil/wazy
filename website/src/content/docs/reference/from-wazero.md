---
title: Coming from wazero
description: What changed, what didn't, and the two API breaks.
---

wazy started from [wazero](https://github.com/tetratelabs/wazero)'s code (Copyright 2020-2023
wazero authors) and still draws on its WebAssembly semantics, WASI implementation, and compliance
and fuzzing test suites. It keeps neither wazero's API compatibility nor its architecture: the
goals are pure Go, performance, and conformance to the standard.

## The import path

```diff
- import "github.com/tetratelabs/wazero"
+ import "github.com/samyfodil/wazy"
```

The sub-packages line up: `api`, `experimental`, `sys`, `imports/wasi_snapshot_preview1`.

## Break one: host functions

wazero's `WithFunc` takes an `interface{}` and reflects over it on every call. wazy deleted that
path rather than optimizing it.

```diff
- b.NewFunctionBuilder().
- 	WithFunc(func(ctx context.Context, x, y uint32) uint32 { return x + y }).
- 	Export("add")

+ wazy.HostFunc2(b.NewFunctionBuilder(),
+ 	func(ctx context.Context, mod api.Module, x, y uint32) uint32 { return x + y },
+ ).Export("add")
```

Two differences to note: the typed helpers always take `(ctx, mod, …)`, and the arity is in the
name. `HostFunc0`–`HostFunc16` return one result; `HostProc0`–`HostProc16` return none.

`WithGoModuleFunction` is unchanged and handles everything the typed helpers do not — multiple
results, dynamic signatures.

The payoff for the same workload: **1086 ns and 6 allocations → 74.9 ns and 0**.

## Break two: `WithCloseOnContextDone` is gone (v0.6.0)

wazero only interrupts a running guest when `RuntimeConfig.WithCloseOnContextDone(true)` is set.
wazy always does, so **v0.6.0 removes the option**. Compiled code checks Go's own preemption
request, so a done `context` ends a call however the guest runs — a loop, recursion, a tree of
calls — and a guest in a tight loop no longer holds up the garbage collector.

```diff
- r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCloseOnContextDone(true))
+ r := wazy.NewRuntime(ctx)
```

What a done context does is not quite what the option did:

| | wazero, with the option | wazy |
| --- | --- | --- |
| The error | `*sys.ExitError`, exit code `sys.ExitCodeContextCanceled` or `ExitCodeDeadlineExceeded` | wraps the context's: `errors.Is(err, context.DeadlineExceeded)` |
| The module | closed | still usable, as after a trap |
| A call waiting in a host function | the module closes at once | the call ends once it is back in the guest, so a host function that can block should watch `ctx` itself |

Code that checked for the exit codes checks the context's error instead. To close the module
when its context is done, as the option did, close it yourself:

```go
stop := context.AfterFunc(ctx, func() { mod.Close(context.Background()) })
defer stop()
```

## What is the same

`Runtime`, `CompileModule`, `InstantiateModule`, `ModuleConfig`, `FSConfig`, `api.Module`,
`api.Memory`, `sys.ExitError`, `CompilationCache`, `wasi_snapshot_preview1.MustInstantiate` — same
names, same shapes. A wazero program that does not register host functions with `WithFunc` usually
compiles after changing the import path.

## What is new

- **The Component Model, WASI 0.2 and the async ABI** — the `component` package and
  `imports/wasip2`. Not targets of upstream wazero.
- **Core Wasm 3.0** — `api.CoreFeaturesV3`, all eight folded-in proposals.
- **Performance work** — see [Performance](../performance/): 9.1x on instantiate, geomean −17.8%
  cumulative.

## Release cadence

The API is stable; what keeps moving is everything under it. Performance work lands continuously,
guarded by the conformance, differential and fuzzing suites rather than by a release cadence. Those
same suites judge a contribution, not its author, machine-generated or human.
