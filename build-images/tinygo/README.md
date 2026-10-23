# wazy-build-tinygo

`ghcr.io/samyfodil/wazy-build-tinygo:0.43.0-go1.26.10`

| Tool | Version |
| --- | --- |
| TinyGo | 0.43.0 (WebAssembly targets only) |
| Go | 1.26.10 |
| wit-bindgen-go | 0.7.0 (`go.bytecodealliance.org/cmd/wit-bindgen-go`) |
| shared wasm tools | see [the index](../README.md#shared-tools) |

TinyGo's runtime for `wasip1` and `wasip2` is precompiled into the image, so
a build starts warm. Libraries and device packages for non-WebAssembly
targets are not shipped.

## Build

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-tinygo:0.43.0-go1.26.10 \
  tinygo build -target=wasip2 -o app.wasm .
```

## A minimal project

```sh
mkdir hello && cd hello
cat > main.go <<'GO'
package main

import "fmt"

func main() { fmt.Println("hello from tinygo") }
GO
printf 'module hello\n\ngo 1.24\n' > go.mod
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-tinygo:0.43.0-go1.26.10 \
  tinygo build -target=wasip2 -o hello.wasm .
go run github.com/samyfodil/wazy/build-images/runner@latest run hello.wasm
```

```
hello from tinygo
```

## Sample

[`sample/build.sh`](sample/build.sh) builds:

- `wasi:cli/command` with `-target=wasip2` (`hello/`),
- `wasi:http/proxy` from `wit-bindgen-go` bindings (`proxy/`),
- optimized variants: `wasm-opt -Oz` on the core module, then
  `wasm-tools component new`. The command goes through `-target=wasip1` and
  the preview1 adapter. The proxy uses
  [`wasip2-core.json`](sample/wasip2-core.json), TinyGo's `wasip2` target
  without its component step, so TinyGo emits the core module and
  `wasm-tools component embed` + `component new` finish it.

Two things that are not obvious:

- TinyGo's `wasip2` runtime imports WASI **0.2.0** (its bundled
  `lib/wasi-cli` WIT), whatever version your bindings target. The world the
  component is built against must include both: `build.sh` assembles
  `build/wit` with `include wasi:cli/imports@0.2.0` next to
  `wasi:http/proxy@0.2.12`.
- `wit-bindgen-go` cannot parse that mixed-version package set ("main
  package ... contains no worlds"), so it generates from `wit/` (0.2.12 only)
  and TinyGo builds against `build/wit`.
