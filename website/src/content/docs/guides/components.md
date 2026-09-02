---
title: Components and the Component Model
description: Run a WebAssembly component — typed values, resources, multi-module graphs — without hand-decoding pointers.
---

Embedding a core module means `fn.Call(ctx, ptr, len)`, decoding a pair of i32s out of the guest's
memory, and both halves hand-agreeing on layout and on who frees what.

A **component** is a `.wasm` that carries its own typed interface. wazy runs them as components —
genuine `wasm32-wasip2` binaries from rustc and `wasm-tools`, not flattened to core modules — so
`string`, records, lists, `option` and `result` cross as Go values.

## Instantiating

```go
import "github.com/samyfodil/wazy/component"

inst, err := component.Instantiate(ctx, r, componentWasm)
if err != nil {
	return err
}
defer inst.Close(ctx)
```

`component.Instantiate` takes the same `wazy.Runtime` a core module uses, plus a variadic list of
`component.Option`. With no options the component gets nothing: no WASI, no host imports, no
filesystem. Add capabilities explicitly — [WASI 0.2](../wasi/) is one line, custom host imports are
[a WIT file away](../custom-wit/).

## Calling exports

A world-level export is called by name:

```go
out, err := inst.Call(ctx, "run")
fmt.Println(out[0].(string))   // run: func() -> string
```

An interface export needs the interface name too:

```go
res, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
```

A `wasi:cli/command` component's entry point is a versioned interface export:

```go
_, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run")
```

## How values map

| WIT | Go |
| --- | --- |
| `bool` | `bool` |
| `u8`, `u16`, `u32` | `uint32` — the narrow widths widen |
| `s8`, `s16`, `s32` | `int32` — the narrow widths widen |
| `u64`, `s64` | `uint64`, `int64` |
| `f32`, `f64` | `float32`, `float64` |
| `char` | `rune` |
| `string` | `string` |
| `list<T>` | `[]T` for a fixed-width primitive `T`, `[]component.Value` otherwise |
| `record` | `[]component.Value` in declared field order — **not** a map |
| `tuple<A, B>` | `[]component.Value` |
| `option<T>` | `nil` for `none`, or the inner value for `some` |
| `result<T, E>` | `component.ResultValue{IsErr, Payload}` — **not** a Go `error` |
| `variant` | `component.VariantValue{Disc, Payload}`, `Disc` the 0-based case index |
| `enum` | `uint32`, the case index |
| `flags` | `uint32` bitset, LSB = the first declared label |
| `own<R>`, `borrow<R>` | `uint32`, the handle's host representation |

Three of those rows are where hand-written host code usually goes wrong.

**Narrow integers arrive widened.** A `u8` is a `uint32` and an `s16` is an `int32`; there is no
`uint8` anywhere in the value vocabulary. Handing back a `uint8(5)` for a `u8` fails at store time
with `unsupported type uint8`, not at compile time.

**A record is a slice, not a map.** Its Go shape is `[]component.Value` in declared field order,
with one entry per field — the ABI transmits order and types, never names, so there is nothing for
a key to match on. A `map[string]any` is rejected outright: `storeRecord: expected []Value`.

**A `result` is a value, not a Go `error`.** `component.ResultValue{IsErr: true, Payload: …}` is an
ordinary value the guest receives and handles. Returning a Go `error` from a host function is a
different thing entirely — it *traps* the guest call. Use the error return for "this call cannot
proceed", and a `ResultValue` for a failure the WIT declared.

A typed `list<T>` lifts to `[]T` directly, so `list<u32>` is a `[]uint32` and not a `[]any` of
boxed `uint32`s. Lists of anything wider — strings, records, nested lists — lift as
`[]component.Value`. `component.ListOf[T](v)` returns `([]T, error)` from either shape, so a host
function can ask for the slice it wants in one step:

```go
names, err := component.ListOf[string](args[0])   // list<string>
sizes, err := component.ListOf[uint32](args[1])   // list<u32>
```

:::caution
A `list<u32>` that lifts correctly proves less than it looks like: it is the type most likely to
survive a layout bug by accident. Test the record and variant paths explicitly.
:::

## Resources

A resource is a handle to an object one side owns. `own<R>` transfers ownership; `borrow<R>` lends
it for the duration of a call. wazy tracks the lifetime and calls the destructor when the last
owner drops it.

Host-owned resources are declared with a tag and a destructor:

```go
const bucketTag = 1

opts := []component.Option{
	component.WithResourceTag("example:store/kv", "bucket", bucketTag),
	component.WithHostResourceDtor(bucketTag, func(ctx context.Context, rep uint32) error {
		return closeBucket(rep)
	}),
}
```

:::danger
A `WithResourceTag` **without** a matching `WithHostResourceDtor` leaks: nothing runs when the
guest drops the handle. If several interfaces share a tag — the WASI `input-stream` /
`output-stream` tags do — their destructors must compose into one function, because the last
registration wins.
:::

## Multi-module component graphs

A real `wasm32-wasip2` binary is not one module. It is a graph: nested instances, a core module per
compilation unit, the `wasi_snapshot_preview1` adapter, canonical lowering for every host import.
wazy instantiates the whole graph — which is why a rustc `wasi:cli/command` runs end to end and
prints `hello world` with no `wasm-tools` preprocessing on your side.

With a [compile cache](../caching/), re-instantiating that entire graph costs 350.5 µs.

## Async exports

An async export is called exactly like a synchronous one — the scheduler runs underneath and
`Call` returns when the task completes:

```go
out, err := inst.Call(ctx, "run-async")
```

`CallAsync` returns a pending call another goroutine resolves. See [Async](../async/).

## Serving a component over HTTP

A component exporting `wasi:http/incoming-handler` becomes an `http.Handler`:

```go
mux.Handle("/plugin/", http.StripPrefix("/plugin", wasip2.Handler(inst)))
```

Somebody else's handler, as a route on your mux. A warm one answers in 3.37 µs and 45 allocations.
