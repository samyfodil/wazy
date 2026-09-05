---
title: Coming from wazero
description: What changed, what didn't, and the one API break.
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

## The one break: host functions

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

This is the only compatibility break, and it is behind us — the surface has settled since.

## What is the same

`Runtime`, `CompileModule`, `InstantiateModule`, `ModuleConfig`, `FSConfig`, `api.Module`,
`api.Memory`, `sys.ExitError`, `CompilationCache`, `wasi_snapshot_preview1.MustInstantiate` — same
names, same shapes. A wazero program that does not register host functions with `WithFunc` usually
compiles after changing the import path.

## What is new

- **The Component Model, WASI 0.2 and the async ABI** — the `component` package and
  `imports/wasip2`. Not targets of upstream wazero.
- **Core Wasm 3.0** — `api.CoreFeaturesV3`, all eight folded-in proposals.
- **Performance work** — see [Performance](../performance/): 9.1x on instantiate, 12–13x on
  interruptible loops, geomean −17.8% cumulative.

## Release cadence

The API is stable; what keeps moving is everything under it. Performance work lands continuously,
guarded by the conformance, differential and fuzzing suites rather than by a release cadence. Those
same suites judge a contribution, not its author, machine-generated or human.
