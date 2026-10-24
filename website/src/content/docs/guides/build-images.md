---
title: Build images
description: Pinned Docker images that compile Rust, Go, TinyGo, C/C++, Python, JavaScript and C# to WebAssembly components that run on wazy.
---

Getting a component toolchain right is most of the work of shipping a component: which rustc
target, which adapter, which `wasm-tools`, which versions agree with each other. The build images
pin all of that, one image per language, and every published tag has built real components that
ran on wazy.

## Images

| Image | Tag | Toolchain | Worlds |
| --- | --- | --- | --- |
| `ghcr.io/samyfodil/wazy-build-rust` | `1.99.1-cc0.21.1` | Rust 1.99.1 (`wasm32-wasip1`, `wasm32-wasip2`), cargo-component 0.21.1 | command, proxy |
| `ghcr.io/samyfodil/wazy-build-go` | `1.26.10` | Go 1.26.10 | command |
| `ghcr.io/samyfodil/wazy-build-tinygo` | `0.43.0-go1.26.10` | TinyGo 0.43.0, Go 1.26.10, wit-bindgen-go 0.7.0 | command, proxy |
| `ghcr.io/samyfodil/wazy-build-c` | `34.0` | wasi-sdk 34.0 (clang 23), C and C++ | command, proxy |
| `ghcr.io/samyfodil/wazy-build-python` | `0.25.1-py3.13.9` | componentize-py 0.25.1, Python 3.13.9 | command, proxy |
| `ghcr.io/samyfodil/wazy-build-js` | `1.39.0-cjs0.23.0-node22.23.4` | jco 1.39.0, componentize-js 0.23.0, Node 22.23.4 | command, proxy |
| `ghcr.io/samyfodil/wazy-build-csharp` | `0.8.0-preview00011-net10.0.402` | componentize-dotnet 0.8.0-preview00011, .NET 10.0.402, NativeAOT-LLVM | command, proxy |

"command" is `wasi:cli/command`, "proxy" is `wasi:http/proxy`. Every image is published for
`linux/amd64` and `linux/arm64`. Sizes, and the reasoning behind each image, are in
[`build-images/`](https://github.com/samyfodil/wazy/tree/main/build-images); each language has its
own README there with a minimal project.

Standard Go has no `wasi:http/proxy` image: `wit-bindgen-go` emits `//go:wasmimport` signatures
(`*string`, `*cm.List[...]`) that standard Go rejects, so its WIT bindings work only under TinyGo.

## Usage

Mount the project at `/src` and run as yourself:

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 \
  cargo build --release --target wasm32-wasip2
```

Any `--user` works: the toolchains are read-only and every cache (Cargo, Go, npm, pip, NuGet)
lives under `/tmp`, so nothing needs root and no root-owned files land in the project.

Each image also carries the same wasm toolset at the same versions: `wasm-tools` 1.262.0,
`wit-bindgen` 0.63.0, Binaryen version_133 (`wasm-opt`, `wasm-metadce`, `wasm-merge`,
`wasm-ctor-eval`), Wizer 11.0.3, and Wasmtime 50.0.0's preview1 adapters at
`$WASI_ADAPTER_COMMAND` and `$WASI_ADAPTER_REACTOR`.

Run the result on wazy as shown in [Components](/guides/components/), or try it with the runner the
images are tested with:

```sh
go run github.com/samyfodil/wazy/build-images/runner@latest run app.wasm        # wasi:cli/command
go run github.com/samyfodil/wazy/build-images/runner@latest http proxy.wasm /   # wasi:http/proxy
```

A Python component needs WebAssembly 3.0 features (CPython uses `extended-const`): configure the
runtime with `api.CoreFeaturesV3`.

## Optimizing

`wasm-opt` works on core modules, not components. Optimize the core module, then build the
component from it:

```sh
wasm-opt -Oz app.core.wasm -o app.core.wasm
wasm-tools component new app.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o app.wasm
```

Use `$WASI_ADAPTER_REACTOR` for a module without `main`, such as an HTTP handler. Toolchains that
link the component from several core modules themselves (componentize-py, componentize-js,
componentize-dotnet) leave no single core module to optimize; strip the finished component
instead with `wasm-tools strip --all`. Each image's sample shows the route for its language.

## What a tag guarantees

For every image, on both architectures, CI builds the image, then inside it, as an unprivileged
user, builds the sample components (plain and optimized), validates each with `wasm-tools`, checks
it exports the expected world, and runs it on wazy built from the same commit, asserting the exact
stdout of the command and the status and body of an HTTP request to the proxy. It also fails if an
image grows more than 10% over its recorded size.

That runs on every pull request that touches an image. On each wazy release every image is
re-tested against the release; an image is pushed only when its tag is new, and the release notes
list the tags that passed.

Tags name toolchain versions and never move. A rebuild with the same toolchains takes a `-N`
suffix (`34.0-2`). There is no `latest`. Base images are pinned by digest and every downloaded
tool by version and checksum.

Run the same test locally with Docker and Go:

```sh
make build-image-test IMAGE_LANG=rust
```
