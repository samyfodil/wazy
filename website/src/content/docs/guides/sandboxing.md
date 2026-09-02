---
title: Sandboxing and limits
description: What a wazy guest can reach, what it cannot, and how to cap memory and wall time.
---

A wazy guest starts with **nothing**. Every capability below is something you hand it.

| Capability | Default | Granted by |
| --- | --- | --- |
| Filesystem | nothing visible | `WithFSConfig` / `Config.FS` mounts |
| stdout, stderr, stdin | discarded / empty | `WithStdout`, `WithStderr`, `WithStdin` |
| Arguments, environment | empty | `WithArgs`, `WithEnv` |
| Wall clock (WASI 0.1) | a **fake** clock, frozen | `WithSysWalltime` |
| Randomness | deterministic | `WithRandSource` |
| TCP / UDP | unregistered | `Config.AllowTCP` / `AllowUDP` |
| Outgoing HTTP | unregistered | `Config.EnableHTTP` |

The frozen clock is the one that surprises people. It is deliberate: a guest that needs the real
time has to be given it, and a guest that reads the clock for fingerprinting gets nothing useful.

## Filesystem

Mounts are explicit, and read-only is a first-class option:

```go
fs := wazy.NewFSConfig().
	WithReadOnlyDirMount("./assets", "/assets").   // guest sees /assets, cannot write
	WithDirMount("./scratch", "/tmp").             // read-write
	WithFSMount(embeddedFS, "/lib")                // an fs.FS, e.g. go:embed
```

`WithFSMount` takes any `fs.FS`, so an `embed.FS` works and nothing touches the real filesystem.
`WithSysFSMount` is the escape hatch for a custom `sys.FS` implementation.

There is no `..` escape and no way to reach a path you did not mount.

## Memory

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
	WithMemoryLimitPages(256))          // 256 * 64 KiB = 16 MiB, hard cap
```

A module whose declared maximum exceeds the limit is rejected at compile time; a `memory.grow` past
it fails at run time, which is a value the guest must handle. `WithMemory64LimitPages` is the
memory64 equivalent.

`WithMemoryCapacityReservePages` trades address space for speed: reserving capacity up front makes
in-capacity `memory.grow` a 109 ns operation instead of a reallocation. Out-of-capacity, shared and
imported memories keep the safe path.

:::note
On a 32-bit host, linear memory tops out just under 2 GiB rather than the 4 GiB a 32-bit Wasm
memory may declare — bound by what a Go slice holds. A module asking for more is rejected, which
the specification allows.
:::

## Wall-clock limits

A `context` deadline kills even a tight compiled loop, if you ask for it:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
	WithCloseOnContextDone(true))

ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()

_, err := fn.Call(ctx)   // returns once the deadline passes, even mid-loop
```

The compiler inserts an amortized check rather than a Go round-trip per iteration, which is why the
overhead is about +5% rather than the +75% the naive shape costs. Tune the trade with
`WithInterruptCheckInterval(ctx, n)` — a larger interval is cheaper and coarser.

## What wazy does not do

- **No fuel metering.** There is no instruction budget. Wall time is the lever; if you need
  kernel-enforced CPU quotas per tenant, you want a container or a microVM.
- **No `wasi-threads` spawn.** Shared memory and atomics are available via
  `experimental.CoreFeaturesThreads`, but a guest cannot spawn OS threads.
- **No side-channel hardening.** Sandboxing here is memory-safety and capability isolation, not
  protection against a co-resident guest measuring timing.

## Trapping and errors

A guest trap — out-of-bounds access, an unreachable, a failed `memory.grow` the guest did not
handle — surfaces as an error from `Call`, with the guest stack in the message. It does not take
your process down. A host function that panics becomes a trap the same way.

Closing the runtime closes everything it created:

```go
r := wazy.NewRuntime(ctx)
defer r.Close(ctx)
```
