# wazy: a performance-focused WebAssembly runtime for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/samyfodil/wazy.svg)](https://pkg.go.dev/github.com/samyfodil/wazy) [![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)

wazy is a WebAssembly Core Specification [1.0][1] and [2.0][2] compliant
runtime written in Go. It is a deliberately diverging derivative of
[wazero](https://github.com/tetratelabs/wazero), forked from upstream commit
[`c0f3a4e`](https://github.com/tetratelabs/wazero/commit/c0f3a4e), focused on
pushing runtime performance further while keeping wazero's zero-dependency,
no-CGO design.

Import wazy and extend your Go application with code written in any language!

```bash
go get github.com/samyfodil/wazy@latest
```

## Relationship to wazero

wazy started as a byte-for-byte import of wazero and is not a clean-room
project. All credit for the runtime's architecture, WebAssembly semantics,
WASI implementation, and the enormous compliance/fuzz test suite belongs to
[Tetrate](https://tetrate.io) and the wazero authors and contributors. See
[RATIONALE.md](RATIONALE.md) for the original design rationale, most of which
still applies here, and [NOTICE](NOTICE)/[LICENSE](LICENSE) for the Apache 2.0
attribution, which is preserved unmodified.

What wazy changes going forward is documented in [OPTIMIZATIONS.md](OPTIMIZATIONS.md),
a running scan of performance opportunities across the compiler backend,
interpreter, runtime core, WASI/sysfs layer, and host-call mechanism, together
with which of them have been resolved. If you want a battle-tested,
API-stable runtime with a large user base, use upstream
[wazero](https://github.com/tetratelabs/wazero) instead. If you want to track
an actively-diverging performance fork, you're in the right place — but
expect breaking changes without the same stability promises upstream makes.

## What's already different

**Reflection has been removed entirely from host function registration.**
Upstream wazero's original `WithFunc` path registered arbitrary Go functions
via `reflect.Value.Call`, `reflect.New`, and similar — convenient, but roughly
14x slower than a direct call. wazy deleted that path completely. Host
functions are now registered one of two ways:

* Typed generic helpers — `wazy.HostFunc0` through `wazy.HostFunc8` (functions
  that return a value) and `wazy.HostProc0` through `wazy.HostProc8`
  (functions with no return value), defined in [`host_typed.go`](host_typed.go).
  These derive the WebAssembly signature from Go's type system at compile time
  and encode/decode the value stack directly — no reflection, no allocation.
* `WithGoModuleFunction` (or `WithGoFunction`, when the calling module isn't
  needed) on `HostFunctionBuilder`, for cases the typed helpers don't cover.

Both paths compile down to the same zero-allocation stack-based calling
convention as wazero's internal `api.GoModuleFunc`. See
[`host_typed.go`](host_typed.go) and [`builder.go`](builder.go) for the API,
and [OPTIMIZATIONS.md](OPTIMIZATIONS.md) for the measurements motivating the
change, plus what's next on the performance roadmap.

## Example

The best way to learn wazy is by trying one of our [examples](examples/README.md).
The most [basic example](examples/basic) extends a Go application with an
addition function defined in WebAssembly.

## Runtime

There are two runtime configurations supported in wazy: _Compiler_ is default:

By default, ex `wazy.NewRuntime(ctx)`, the Compiler is used if supported. You
can also force the interpreter like so:
```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigInterpreter())
```

### Interpreter
Interpreter is a naive interpreter-based implementation of Wasm virtual
machine. Its implementation doesn't have any platform (GOARCH, GOOS) specific
code, therefore _interpreter_ can be used for any compilation target available
for Go (such as `riscv64`).

### Compiler
Compiler compiles WebAssembly modules into machine code ahead of time (AOT),
during `Runtime.CompileModule`. This means your WebAssembly functions execute
natively at runtime. Compiler is faster than Interpreter, often by order of
magnitude (10x) or more. This is done without host-specific dependencies.

## License

Apache 2.0, same as upstream. [LICENSE](LICENSE) and [NOTICE](NOTICE) are
unmodified from wazero.

[1]: https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/
[2]: https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/
