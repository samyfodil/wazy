---
title: Core modules and WASI 0.1
description: Compile, instantiate and call a plain WebAssembly module, with or without WASI preview 1.
---

A *core module* is a plain `.wasm` — the thing `tinygo build -target=wasi`, `zig build-exe -target
wasm32-wasi` or `clang --target=wasm32-wasi` produces. Its interface is integers and a linear
memory. If your guest carries its own typed interface, you want
[components](../components/) instead.

## Compile once, instantiate many

`CompileModule` translates the module to machine code. `Instantiate` gives you one live instance
with its own linear memory. Compilation is the expensive half, so hoist it out of the request path.

```go
r := wazy.NewRuntime(ctx)
defer r.Close(ctx)

compiled, err := r.CompileModule(ctx, guestWasm)
if err != nil {
	return err
}

// Per request: a fresh instance, a fresh linear memory.
mod, err := r.InstantiateModule(ctx, compiled, wazy.NewModuleConfig().WithName(""))
if err != nil {
	return err
}
defer mod.Close(ctx)
```

`WithName("")` matters when you instantiate the same module concurrently: a named module is
registered in the runtime's namespace and the second instantiation collides. An empty name makes
the instance anonymous.

`r.Instantiate(ctx, guestWasm)` is the one-shot form — compile and instantiate together — which is
what you want in tests and CLIs.

## Calling exports

```go
add := mod.ExportedFunction("add")

results, err := add.Call(ctx, 1, 2)
if err != nil {
	return err
}
fmt.Println(api.DecodeI32(results[0]))
```

Every parameter and result is a `uint64` on the wire. The `api` package has the encoders you need
for the other value types: `api.EncodeI32`, `api.EncodeF64`, `api.DecodeF32` and friends.

`Call` allocates the slice it returns results in. A hot loop should reuse one:

```go
stack := make([]uint64, 2)
for i := range n {
	stack[0], stack[1] = uint64(i), 2
	if err := add.CallWithStack(ctx, stack); err != nil {
		return err
	}
	// stack[0] now holds the result.
}
```

That is the 48 ns path. See [Host functions](../host-functions/) for the other direction.

## Memory

```go
mem := mod.Memory()

buf, ok := mem.Read(offset, length)   // a copy-free view into the guest's memory
ok = mem.Write(offset, []byte("hi"))
```

`Read` hands back a slice aliasing the guest's linear memory. It is only valid until the guest runs
again — `memory.grow` can reallocate the backing array. Copy it if you need to keep it.

To pass a string in, you generally call the guest's own allocator:

```go
malloc := mod.ExportedFunction("malloc")
res, err := malloc.Call(ctx, uint64(len(s)))
ptr := uint32(res[0])
mod.Memory().Write(ptr, []byte(s))
```

Which is exactly the bookkeeping the [Component Model](../components/) exists to delete.

## WASI 0.1

TinyGo, Zig and Rust's `wasm32-wasip1` targets emit imports from the `wasi_snapshot_preview1`
module — for `panic`, for stdio, for the clock. Register it before instantiating the guest:

```go
import "github.com/samyfodil/wazy/imports/wasi_snapshot_preview1"

wasi_snapshot_preview1.MustInstantiate(ctx, r)
```

Then configure what the guest can see through `ModuleConfig`:

```go
cfg := wazy.NewModuleConfig().
	WithStdout(os.Stdout).
	WithStderr(os.Stderr).
	WithArgs("app", "--flag").
	WithEnv("HOME", "/").
	WithFSConfig(wazy.NewFSConfig().WithReadOnlyDirMount("./data", "/")).
	WithSysWalltime().
	WithSysNanotime()

mod, err := r.InstantiateWithConfig(ctx, guestWasm, cfg)
```

Everything on that list is opt-in. Without `WithStdout` the guest's writes go nowhere; without an
`FSConfig` mount it has no filesystem at all; without `WithSysWalltime` it reads a **fake** clock
that always returns the same instant. That default is deliberate — see
[Sandboxing and limits](../sandboxing/).

### Command vs reactor

A *command* module exports `_start` and runs to completion; a *reactor* exports `_initialize` and
then waits to be called. wazy runs `_start` by default. For a reactor, say so:

```go
wazy.NewModuleConfig().WithStartFunctions("_initialize")
```

A command that has already run `_start` has exited: calling its exports afterwards fails. If you
need repeated calls, build a reactor.

### Exit codes

A guest calling `proc_exit` unwinds as an error, not a panic:

```go
if _, err := mod.ExportedFunction("run").Call(ctx); err != nil {
	var exitErr *sys.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != 0 {
		return fmt.Errorf("guest exited: %d", exitErr.ExitCode())
	}
	return err
}
```

Exit code 0 is still returned as an `*sys.ExitError` — a normal `_start` completion, not a failure.
