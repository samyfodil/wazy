# `greet` in Go

Standard Go (not TinyGo). Go has no component target, so this is a two-step
build: a **wasip1 reactor** core module, then `wasm-tools` wraps it in a
component against `greet.wit`.

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
GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o core.wasm .

wasm-tools component embed greet.wit core.wasm -o embedded.wasm

wasm-tools component new embedded.wasm \
    --adapt wasi_snapshot_preview1=wasi_snapshot_preview1.reactor.wasm \
    -o greet.wasm
```

`-buildmode=c-shared` is what makes it a *reactor*: the module exports
`_initialize` instead of `_start`, so it can sit still and be called. A plain
`go build` produces a command, which has nothing to call.

## What the host sees

`greet` is exported by the world, so there is no interface prefix:

```go
inst.Call(ctx, "greet", "wazy")
```

The component **does** import WASI 0.2 — not because the greeting needs it, but
because a standard-Go binary carries the Go runtime, which reads the clock and
the environment on the way up. Instantiate it with `wasip2.With(wasip2.Config{})`.

That runtime is also why `greet.wasm` is ~1.8 MB.

## Notes

`main.go` writes the Canonical ABI out by hand — standard Go has no WIT
bindings generator. Two details are load-bearing:

- `cabi_realloc` allocates from a plain array, never the Go heap. The adapter
  calls it from inside a WASI call the Go runtime is already making, and
  allocating there re-enters the runtime on its own system stack. The failure
  is a bare `out of bounds memory access` during `_initialize`, with a stack
  full of `runtime.badsystemstack`.
- That array must clear 128 KiB, which is what the adapter's own state takes.
