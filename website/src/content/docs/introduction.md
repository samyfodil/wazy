---
title: Introduction
description: What wazy is, what it runs, and when to reach for something else.
sidebar:
  order: 1
---

wazy is a **WebAssembly runtime written in pure Go**. It embeds Wasm compiled from Rust, C/C++,
TinyGo, Zig, AssemblyScript and Go, and runs it with an optimizing amd64/arm64 compiler or a
portable interpreter. There is no cgo, nothing to install at runtime, and one dependency:
`golang.org/x/sys`.

```bash
go get github.com/samyfodil/wazy@latest
```

## What it's for

**Run code you didn't write.** Ship a plugin system, execute a customer's untrusted script, or use
a Rust or C library you would otherwise reach through cgo: compiled to Wasm it becomes a byte slice
you `go:embed` and a function you call.

The guest starts with nothing — no files beyond what you mount, no sockets unless you allow them,
a fake clock on the WASI 0.1 path. You cap its memory with `WithMemoryLimitPages`, and a `context`
deadline kills even a tight compiled loop with `WithCloseOnContextDone`.

## What it runs

| Standard | |
| --- | :---: |
| Core Wasm 1.0 / 2.0 | ✅ |
| Core Wasm 3.0 | ✅ |
| WASI 0.1 (`wasip1`) | ✅ |
| WASI 0.2 · Component Model (`wasip2`) | ✅ |
| WASI 0.3 async ABI (`stream`, `future`, tasks, threads) | ✅ |

Core Wasm 3.0 means all eight proposals it folded in: tail calls, extended const, exception
handling, typed function references, relaxed SIMD, multiple memories, memory64 and garbage
collection. Turn them on together with `api.CoreFeaturesV3`; the default stays
`api.CoreFeaturesV2`.

Beyond core Wasm, wazy runs the [Component Model](../guides/components/), the
[WASI 0.2 host interfaces](../guides/wasi/) — 114 host functions across `wasi:cli`,
`wasi:clocks`, `wasi:filesystem`, `wasi:io`, `wasi:random`, `wasi:sockets` and `wasi:http` — and
the [WASI 0.3 async ABI](../guides/async/). None of those are targets of upstream wazero.

## Where the alternative still wins

Being honest about the edges is cheaper than a support thread later.

- **cgo**, when the library must spawn OS threads of its own, or needs the host machine itself.
  wazy exposes shared memory and atomics (`experimental.CoreFeaturesThreads`) but not
  `wasi-threads` spawn.
- **A container or microVM**, when you need kernel-enforced CPU quotas per tenant. wazy caps
  memory and enforces a deadline, but there is no fuel metering.
- **Starlark or a scripting interpreter**, when the code is config-shaped and your authors will
  not install a Wasm toolchain.
- **Trust**, when the code is first-party and what you wanted was extensibility, not safety.

## Relationship to wazero

wazy started from [wazero](https://github.com/tetratelabs/wazero)'s code (Copyright 2020-2023
wazero authors) and still draws on its WebAssembly semantics, WASI implementation, and compliance
and fuzzing test suites. It keeps neither wazero's API compatibility nor its architecture: the
goals are pure Go, performance, and conformance to the standard.

The API broke with wazero exactly once, over host-function registration, and that break is behind
it — typed generics replaced the reflection path. See
[Coming from wazero](../reference/from-wazero/) for what changed.
