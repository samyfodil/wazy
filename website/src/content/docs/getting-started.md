---
title: Getting started
description: Install wazy and run your first Wasm guest — a core module and a Component Model component.
sidebar:
  order: 2
---

```bash
go get github.com/samyfodil/wazy@latest
```

wazy needs Go 1.25 or newer and nothing else. `CGO_ENABLED=0` is fine — encouraged, even.

## Run a component

The shortest useful program: a genuine `wasm32-wasip2` component built by rustc, given stdout and
nothing else.

```go title="main.go"
package main

import (
	"context"
	_ "embed"
	"os"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/imports/wasip2"
)

// A genuine rustc component: cargo build --target wasm32-wasip2
//
//go:embed testdata/hello.wasm
var helloWasm []byte

func main() {
	ctx := context.Background()

	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx) // Closes everything this runtime created.

	// Stdout, and nothing else: no files mounted, no sockets registered.
	inst, err := component.Instantiate(ctx, r, helloWasm,
		wasip2.With(wasip2.Config{Stdout: os.Stdout})...)
	if err != nil {
		panic(err)
	}
	defer inst.Close(ctx)

	if _, err := inst.Call(ctx, "wasi:cli/run@0.2.3#run"); err != nil {
		panic(err)
	}
}
```

That is [`examples/component`](https://github.com/samyfodil/wazy/tree/main/examples/component),
minus the error handling.

Values cross typed. A component exporting `run: func() -> string` hands back a Go `string` rather
than a pointer and a length:

```go
out, err := inst.Call(ctx, "run")
fmt.Println(out[0].(string))
```

Call an interface export directly by name:

```go
res, err := inst.CallExport(ctx, "component:adder/calc", "add", uint32(2), uint32(3))
```

## Run a core module

Plain core modules and WASI 0.1 go through `r.Instantiate` and `mod.ExportedFunction`.

```go title="main.go"
r := wazy.NewRuntime(ctx)
defer r.Close(ctx)

// TinyGo's wasi target needs the WASI 0.1 host functions to implement panic.
wasi_snapshot_preview1.MustInstantiate(ctx, r)

mod, err := r.InstantiateWithConfig(ctx, addWasm,
	wazy.NewModuleConfig().WithStartFunctions("_initialize"))
if err != nil {
	return err
}

results, err := mod.ExportedFunction("add").Call(ctx, 1, 2)
```

That is [`examples/basic`](https://github.com/samyfodil/wazy/tree/main/examples/basic). More in
[Core modules and WASI 0.1](../guides/core-modules/).

## The three objects

| | |
| --- | --- |
| `wazy.Runtime` | Owns compiled code and every module instantiated into it. Closing it closes everything it created. |
| `api.Module` (core) | One instantiated core module: its exported functions, memory and globals. |
| `*component.Instance` | One instantiated component graph: typed exports, resource handles, an async scheduler. |

A runtime is safe for concurrent use and is meant to be long-lived — compile once, instantiate per
request. Instantiation is cheap on purpose: 1.7 µs and 3.3 KB of heap for a 37 KB module, so every
request can get its own fresh linear memory instead of a pooled one you scrub between calls.

## Where to go next

- [Core modules and WASI 0.1](../guides/core-modules/) — compile, instantiate, memory, exports.
- [Host functions](../guides/host-functions/) — the typed, allocation-free registration API.
- [Components](../guides/components/) — the Component Model, typed values, resources.
- [Sandboxing and limits](../guides/sandboxing/) — what the guest can reach, and how to cap it.
- [Examples](https://github.com/samyfodil/wazy/tree/main/examples) — runnable programs for each of
  the above.
