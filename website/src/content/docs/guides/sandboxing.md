---
title: Sandboxing and limits
description: What a wazy guest can reach, what it cannot, and how to cap memory and wall time.
---

A wazy guest starts with **nothing** of its own: no files, no network, no stdio. Every capability
below is something you hand it. The two ABIs are configured through different surfaces — WASI 0.1
through `ModuleConfig` methods on a core module, WASI 0.2 through the `wasip2.Config` struct you
pass to `wasip2.With` — and the defaults are not the same, so every row names its ABI.

| Capability | ABI | Default | Granted by |
| --- | --- | --- | --- |
| Filesystem | 0.1 | <span class="deny">nothing visible</span> | `WithFSConfig` mounts |
| Filesystem | 0.2 | <span class="deny">nothing visible</span> | `Config.FS` mounts |
| stdout, stderr, stdin | 0.1 | <span class="deny">discarded / empty</span> | `WithStdout`, `WithStderr`, `WithStdin` |
| stdout, stderr, stdin | 0.2 | <span class="deny">discarded / empty</span> | `Config.Stdout`, `Config.Stderr`, `Config.Stdin` |
| Environment | 0.1 | <span class="deny">empty</span> | `WithEnv` |
| Environment | 0.2 | <span class="deny">empty</span> | `Config.Env` |
| Arguments | 0.1 | <span class="deny">empty, no argv[0]</span> | `WithArgs` |
| Arguments | 0.2 | <span class="grant">argv[0] only</span> | `Config.Args` appends after it |
| Wall clock | 0.1 | <span class="deny">a **fake** clock, frozen</span> | `WithSysWalltime` |
| Wall clock | 0.2 | <span class="grant">the real `time.Now`</span> | already on; `Config.WallClock` replaces it |
| Randomness | 0.1 | <span class="deny">deterministic</span> | `WithRandSource` |
| Randomness | 0.2 | <span class="grant">real `crypto/rand`</span> | already on; not configurable |
| TCP / UDP | 0.2 | <span class="deny">unregistered</span> | `Config.AllowTCP` / `Config.AllowUDP` |
| Outgoing HTTP | 0.2 | <span class="deny">unregistered</span> | `Config.EnableHTTP` |

Cyan marks a capability that is already on before you ask; grey marks one that is not there until
you hand it over.

Arguments are the row that differs most between the two. WASI 0.1 defaults to no arguments at all,
argv[0] included — `WithArgs` sets the whole vector, and the runtime synthesizes nothing. WASI 0.2
always prepends a synthetic argv[0] of `"wazy"`, because `wasi:cli/environment.get-arguments`
returns the full argv and guests following the Unix convention skip the first element;
`Config.Args` holds only what comes after it.

The frozen clock is the WASI 0.1 default, and it is the one that surprises people. It is
deliberate: a guest that needs the real time has to be given it, and a guest that reads the clock
for fingerprinting gets nothing useful.

The component path does not inherit that default, or the deterministic random source. `wasip2.With`
registers `wasi:clocks` and `wasi:random` unconditionally, backed by `time.Now` and `crypto/rand`:
`WallClock` is the only clock you can replace, and `wasip2.Config` has no randomness field at all.
The way to deny a component those interfaces is not to register WASI on it — every WASI interface a
component imports that you did not register is an unregistered trap stub that fails loud, naming
the interface and function, the first time the guest calls it.

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

There is no `..` escape. Both the core and the component path resolve a guest path with the same
`path.Clean` + `fs.ValidPath` rule and reject anything that leaves the descriptor it is resolved
against — `../../..` and rooted paths alike — before the path reaches the filesystem. There is no
way to turn that off: to give a guest a second directory, add a second mount.

:::caution
That check is **lexical**. A symlink *inside* a mounted directory that points outside it is
resolved by the host OS well below this layer, and is followed. A directory whose symlinks you do
not control is not safe to mount.
:::

## Memory

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().
	WithMemoryLimitPages(256))          // 256 * 64 KiB = 16 MiB, hard cap
```

A module whose declared *minimum* exceeds the limit is rejected at compile time. A declared
*maximum* over it is clamped to the limit instead, so the module still loads and simply cannot grow
as far as it asked; a `memory.grow` past the limit fails at run time, returning -1, which is a value
the guest must handle.

This bounds a memory declared with an i64 index type as well — otherwise a module could reach past
your only general memory ceiling by doing nothing more than declaring `i64`, which is what
`clang --target=wasm64` and Rust's memory64 target emit. `WithMemory64LimitPages` sets that ceiling
independently, above or below this one, and wins whichever order the two are configured in; it is
the only way to allow a 64-bit memory past four gibibytes, since `WithMemoryLimitPages` itself tops
out at 65536 pages.

`WithMemoryCapacityReservePages` trades address space for speed: reserving capacity up front makes
in-capacity `memory.grow` a 109 ns operation instead of a reallocation. Out-of-capacity, shared and
imported memories keep the safe path.

:::note
On a 32-bit host, linear memory tops out just under 2 GiB rather than the 4 GiB a 32-bit Wasm
memory may declare — bound by what a Go slice holds. A module asking for more is rejected, which
the specification allows.
:::

## Wall-clock limits

A `context` deadline ends a call, even in a tight compiled loop:

```go
ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
defer cancel()

_, err := fn.Call(ctx)   // returns once the deadline passes, even mid-loop
// errors.Is(err, context.DeadlineExceeded) is true, and the module is still usable
```

There is nothing to turn on. Compiled code checks Go's own preemption request — the word every Go
function prologue compares against — at loop headers and at the entry of the functions that can
recurse, and returns to Go when it is set. A done context sets it, and so does the garbage
collector when it needs a spinning guest to stop.

The call ends the way a trap does. To close the module instead, say to release its memory at once,
close it when the context is done:

```go
stop := context.AfterFunc(ctx, func() { mod.Close(context.Background()) })
defer stop()
```

A call that is inside a host function when the context is done ends once it is back in the guest,
so a host function that can block should watch `ctx` itself.

Being interruptible costs little, and cannot be turned off: compiled code checks at loop headers
and at the entries of functions that can recur, which costs recursive code like `fibonacci` up
to about 4% against a build with no check at all, while most loops pay next to nothing, because
a loop with a counter tests a few of its bits instead, and one that ends soon enough is not
checked. The worst case measured is allocation-heavy TinyGo code on an Apple M4, at about 8%:
its allocator's loops each call a small helper, which leaves them a full check every
iteration. The interpreter looks at the context every 1024th loop iteration or recursive call.

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
