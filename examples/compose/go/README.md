# `wazy:compose/greeter` in Go — both halves

Standard Go (not TinyGo), on both sides of the interface. Go has no component
target and no WIT bindings generator, so each half is a two-step build — a
**wasip1 reactor** core module, then `wasm-tools` wraps it in a component —
and the Canonical ABI between them is written out by hand in [`abi/`](abi/abi.go).

| | world | file |
|---|---|---|
| provider | `export greeter` | [`provider/main.go`](provider/main.go) → `provider.wasm` |
| consumer | `import greeter` + `export run` | [`consumer/main.go`](consumer/main.go) → `consumer.wasm` |

## Install

- **Go 1.24+** — `//go:wasmexport` and `-buildmode=c-shared` for `wasip1`
  landed in 1.24. Built here with **go1.26.0**.
- **`wasm-tools`** — `cargo install wasm-tools`. Built here with **1.253.0**.
- **The preview1 reactor adapter**, which maps the `wasi_snapshot_preview1`
  imports the Go runtime emits onto WASI 0.2:

```sh
curl -LO https://github.com/bytecodealliance/wasmtime/releases/download/v48.0.1/wasi_snapshot_preview1.reactor.wasm
```

## Build

```sh
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o provider-core.wasm ./provider
wasm-tools component embed greeter.wit --world provider provider-core.wasm -o provider-embedded.wasm
wasm-tools component new provider-embedded.wasm \
    --adapt wasi_snapshot_preview1=wasi_snapshot_preview1.reactor.wasm \
    -o provider.wasm

GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o consumer-core.wasm ./consumer
wasm-tools component embed greeter.wit --world consumer consumer-core.wasm -o consumer-embedded.wasm
wasm-tools component new consumer-embedded.wasm \
    --adapt wasi_snapshot_preview1=wasi_snapshot_preview1.reactor.wasm \
    -o consumer.wasm
```

`-buildmode=c-shared` is what makes each one a *reactor*: the module exports
`_initialize` instead of `_start`, so it can sit still and be called. A plain
`go build` produces a command, which has nothing to call.

Both components import WASI 0.2 — not because greeting needs it, but because a
standard-Go binary carries the Go runtime, which reads the clock and the
environment on the way up. Instantiate either with `wasip2.With(wasip2.Config{})`.
That runtime is also why each `.wasm` is ~1.9 MB.

## Writing the ABI by hand

There is no bindings generator, so the core-level names and signatures have to
be exactly the ones `wasm-tools component new` expects. They are, verbatim:

```
provider   wazy:compose/greeter@0.1.0#greet            (ptr, len, id) -> i32
           wazy:compose/greeter@0.1.0#greet-all        (ptr, count)   -> i32
           cabi_post_wazy:compose/greeter@0.1.0#greet          (i32)
           cabi_post_wazy:compose/greeter@0.1.0#greet-all      (i32)
           cabi_realloc                                (i32 x4)       -> i32

consumer   import "wazy:compose/greeter@0.1.0" "greet"      (ptr, len, id, retptr)
           import "wazy:compose/greeter@0.1.0" "greet-all"  (ptr, count, retptr)
           run                                         ()             -> i32
           cabi_post_run                               (i32)
           cabi_realloc                                (i32 x4)       -> i32
```

Three things make this tractable, and one makes it awkward:

- **`//go:wasmexport` and `//go:wasmimport` take the names verbatim.** Go does
  not object to the colon, slash, `@` or `#` in
  `wazy:compose/greeter@0.1.0#greet`. That is the whole reason a bindings
  generator is not strictly needed.
- **The record never reaches memory.** `visitor` flattens to three values — the
  two of its `string` field, then its `u32` — which is under the Canonical
  ABI's 16-value limit, so it arrives as three bare `i32` parameters. Only
  `string` and `list<string>` are ever laid out, and both are the same
  two-word shape.
- **A result of more than one flat value goes through a pointer**, in opposite
  directions on the two sides: an *export* returns the address of the pair,
  an *import* is handed the address to write the pair into. That is the only
  real asymmetry between `provider/main.go` and `consumer/main.go`.
- **The allocator cannot be the Go heap.** `cabi_realloc` is called
  re-entrantly — by the preview1 adapter from inside a WASI call the Go
  runtime is already making, and, in the consumer, by the host lowering the
  greeter's answers back in *while `run` is still on the stack*. Allocating
  there re-enters the runtime on its own system stack, which surfaces as a
  bare `out of bounds memory access` during `_initialize`. `abi` bump-allocates
  through a plain 256 KiB array instead, which touches no runtime machinery
  at all and is re-entrant by construction. The array must clear 128 KiB,
  which is what the adapter's own state takes; the post-return hands the rest
  back.

The empty-list path is not special-cased anywhere. An empty `list<string>` gets
a real, 4-aligned address with a count of zero, and reading a list of count zero
never dereferences the pointer — which is what the Canonical ABI requires, since
that pointer is allowed to be any aligned address.

## Status on wazy

Both halves run correctly on wazy **individually**:

```
provider.wasm, called directly by the host:
  greet({wazy,42})   -> "Hello, wazy #42! (from Go)"
  greet-all([a,b])   -> ["a (via Go)" "b (via Go)"]
  greet-all([])      -> [] (len=0)
consumer.wasm, its greeter import served by the host:
  run()[0] = "Hello, wazy #42! (from Host)"
  run()[1] = "a (via Host)"
  run()[2] = "empty-len=0"
```

The **composed** component does not instantiate yet. `wasm-tools compose
consumer.wasm -d provider.wasm` produces a valid component that wazy rejects:

```
component/instance: component instance 13 arg "wasi:cli/environment@0.2.12"
references instance 0, which is not a prior nested instantiation
```

This is a wazy gap, not a defect in these two components. A composed
component links its nested components with
instantiate-args that name **imported instances** and **aliased instance
exports**; wazy's graph builder resolves only args that name a prior *nested
instantiation*. Both other shapes are what every `wasm-tools compose` output
is made of.
