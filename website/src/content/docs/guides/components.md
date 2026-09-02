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
| `bool`, `u8`…`u64`, `s8`…`s64`, `f32`, `f64` | `bool`, `uint8`…`uint64`, `int8`…`int64`, `float32`, `float64` |
| `char` | `rune` |
| `string` | `string` |
| `list<T>` | `[]T` for a typed `T`; `component.ListOf[T]` to assert it |
| `record` | `map[string]any`, keyed by field name |
| `tuple<A, B>` | `[]any` |
| `option<T>` | `nil`, or the value |
| `result<T, E>` | the `T`, or a Go `error` carrying the `E` |
| `variant`, `enum` | a tagged value; `enum` cases by name |
| `flags` | a set of enabled names |
| `own<R>`, `borrow<R>` | a handle the runtime tracks |

A typed `list<T>` lifts to `[]T` directly, so `list<u32>` is a `[]uint32` and not a `[]any` of
boxed `uint32`s. `component.ListOf[T](v)` gets you the typed slice from a `component.Value` when
you need the assertion in one step.

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
