---
title: Host functions
description: Expose Go functions to the guest with typed, reflection-free, allocation-free helpers.
---

A host function is a Go function the guest can import and call. wazy derives the Wasm signature
from Go's types at compile time — there is no `reflect` on the call path, and dispatch allocates
nothing.

## Registering

```go
b := r.NewHostModuleBuilder("env")

wazy.HostFunc2(b.NewFunctionBuilder(), func(ctx context.Context, mod api.Module, x, y uint32) uint32 {
	return x + y
}).Export("add")

env, err := b.Instantiate(ctx)
if err != nil {
	return err
}
defer env.Close(ctx)
```

The guest then imports `env.add`. In Rust that is:

```rust
#[link(wasm_import_module = "env")]
unsafe extern "C" {
	fn add(x: u32, y: u32) -> u32;
}
```

| Helper | Shape |
| --- | --- |
| `HostFunc0`–`HostFunc16` | *N* parameters, **one** result |
| `HostProc0`–`HostProc16` | *N* parameters, **no** result |
| `WithGoModuleFunction` | everything else: multiple results, dynamic signatures |

Every helper takes `(ctx context.Context, mod api.Module, …)` as its leading parameters. Permitted
value types are `uint32`, `int32`, `uint64`, `int64`, `float32` and `float64` — the four Wasm value
types, in the Go spellings.

## Reading guest memory

The `mod` parameter is the *calling* module, so a host function can read and write the caller's
linear memory:

```go
wazy.HostProc2(b.NewFunctionBuilder(), func(ctx context.Context, mod api.Module, ptr, size uint32) {
	buf, ok := mod.Memory().Read(ptr, size)
	if !ok {
		panic("out of range") // traps the guest, surfaces to Call as an error
	}
	log.Println(string(buf))
}).Export("log")
```

`Read` returns a view, not a copy — valid only until the guest runs again. Copy anything you keep.

A `panic` inside a host function becomes a Wasm trap and comes back out of `Call` as an error, with
the guest's stack in the message. Returning an error is not part of the ABI, so a panic is the
mechanism; keep it to genuine failures.

## Multiple results and dynamic signatures

When the shape does not fit the typed helpers, drop to the stack API:

```go
b.NewFunctionBuilder().
	WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		x, y := api.DecodeI32(stack[0]), api.DecodeI32(stack[1])
		stack[0] = api.EncodeI32(x / y)
		stack[1] = api.EncodeI32(x % y)
	}),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
	).Export("divmod")
```

Parameters arrive in `stack`, results are written back over it in place.

## No reflection

wazero exposes `WithFunc`, which takes an `interface{}` and reflects over it on every call. wazy
deleted that path rather than optimizing it: `HostFunc*`/`HostProc*` replace it, and the
measurement for the same workload went from **1086 ns and 6 allocations to 74.9 ns and 0** —
14.5x, against a path that no longer exists here.

If you are porting from wazero, this is the one API break. See
[Coming from wazero](../../reference/from-wazero/).

## Host state

Passing per-call state is a `context.Context` value, as usual in Go:

```go
type tenantKey struct{}

ctx = context.WithValue(ctx, tenantKey{}, tenant)
results, err := fn.Call(ctx, args...)
```

The same `ctx` reaches every host function the guest calls during that invocation. For components
there is also `component.WithHostState(key, value)`, which binds state to the instance rather than
the call.

## Logging what the guest does

`experimental/logging` wraps a runtime and prints every host call — useful when a guest fails on an
import you did not expect:

```go
import (
	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/logging"
)

ctx = api.WithFunctionListenerFactory(ctx,
	logging.NewHostLoggingListenerFactory(os.Stderr, logging.LogScopeFilesystem))
```

The [CLI](../../reference/cli/) exposes the same thing as `-hostlogging=filesystem,clock,…`.
