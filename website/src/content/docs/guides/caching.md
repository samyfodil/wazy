---
title: Compilation caching
description: Amortize compilation across processes and across component instantiations.
---

Compiling a module to machine code is the expensive half. Two caches make it a one-time cost.

## Core modules

`CompilationCache` is shared by every module compiled into a runtime configured with it.

```go
cache, err := wazy.NewCompilationCacheWithDir("/var/cache/wazy")
if err != nil {
	return err
}
defer cache.Close(ctx)

r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
	WithCompilationCache(cache))
```

With a directory, the cache survives process restarts: the second boot reads machine code off disk
instead of compiling. `wazy.NewCompilationCache()` is the in-memory form, useful when several
runtimes in one process compile the same modules.

The cache key covers the module bytes, the engine, the enabled features and the wazy version, so a
stale entry cannot be served after an upgrade. Pre-warm it ahead of time with
[`wazy compile`](../../reference/cli/).

## Components

A component is a graph of core modules plus a decoded type section. `component.CompileCache`
amortizes both across repeated instantiations of the same component bytes:

```go
cc := component.NewCompileCache()
defer cc.Close(ctx)

for range requests {
	inst, err := component.Instantiate(ctx, r, guestWasm,
		append(wasip2.With(cfg), component.WithCompileCache(cc))...)
	if err != nil {
		return err
	}
	// ... serve ...
	inst.Close(ctx)
}
```

With the cache warm, re-instantiating a full rustc `wasi:cli/command` graph costs **350.5 µs**.
Without it you pay decode and compilation every time.

One cache per `Runtime`; it is safe for concurrent use, and you close it when you are done.

## What to cache where

| | |
| --- | --- |
| One long-lived process, many instances of one module | Hoist `CompileModule` out of the request path; you may not need a cache at all. |
| Many short-lived processes (a CLI, a serverless worker) | `NewCompilationCacheWithDir` — the win is on the *next* boot. |
| A component instantiated per request | `component.NewCompileCache`, plus a `CompilationCache` if the process is short-lived. |

Instantiation itself is already cheap — 1.7 µs and 3.3 KB for a 37 KB module — which is what makes
a fresh instance per request practical instead of pooling and scrubbing.
