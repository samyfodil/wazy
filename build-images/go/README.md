# wazy-build-go

`ghcr.io/samyfodil/wazy-build-go:1.26.10`

| Tool | Version |
| --- | --- |
| Go | 1.26.10 |
| shared wasm tools | see [the index](../README.md#shared-tools) |

The image sets `GOOS=wasip1 GOARCH=wasm`, so `go build` targets WebAssembly
unless you override them. Want WIT bindings? Use
[wazy-build-tinygo](../tinygo/README.md).

## Build

Standard Go has no component target: it builds a `wasip1` core module and
`wasm-tools` wraps it with the preview1 adapter.

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-go:1.26.10 \
  sh -c 'go build -o app.core.wasm . && wasm-tools component new app.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o app.wasm'
```

## A minimal project

```sh
mkdir hello && cd hello
cat > main.go <<'GO'
package main

import "fmt"

func main() { fmt.Println("hello from go") }
GO
printf 'module hello\n\ngo 1.24\n' > go.mod
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-go:1.26.10 \
  sh -c 'go build -o hello.core.wasm . && wasm-tools component new hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o hello.wasm'
go run github.com/samyfodil/wazy/build-images/runner@latest run hello.wasm
```

```
hello from go
```

## Sample

[`sample/build.sh`](sample/build.sh) builds a `wasi:cli/command`, plain and
optimized (`-ldflags=-s`, then `wasm-opt -Oz` on the core module before
`wasm-tools component new`).

## No `wasi:http/proxy`

Standard Go cannot use generated WIT bindings yet. `wit-bindgen-go` 0.7.0
emits `//go:wasmimport` declarations with `*string` and `*cm.List[...]`
parameters, and Go 1.24 through 1.26 reject them ("go:wasmimport: unsupported
parameter type"): `go:wasmimport` accepts numeric types, `unsafe.Pointer` and
pointers to `structs.HostLayout` structs only. A proxy component in standard
Go means writing the canonical ABI by hand (as
[examples/componentize/go](../../examples/componentize/go) does for a small
world). Use TinyGo for `wasi:http`.

A command calls `wasi:cli/exit` with `ok` once `main` returns. wazy reports
that as a `*sys.ExitError` with code 0, which a host treats as success.
