# Build images

Docker images that compile source to WebAssembly components that run on wazy.
Each image is a pinned toolchain for one language. Every published tag has
passed the tests described below, on linux/amd64 and on linux/arm64.

| Image | Tag | Toolchain | Worlds tested | Size amd64 (compressed / unpacked) | Size arm64 (unpacked) |
| --- | --- | --- | --- | --- | --- |
| [`ghcr.io/samyfodil/wazy-build-rust`](rust/README.md) | `1.99.1-cc0.21.1` | Rust 1.99.1 (`wasm32-wasip1`, `wasm32-wasip2`), cargo-component 0.21.1 | command, proxy | 426 MB / 1222 MB | 1120 MB |
| [`ghcr.io/samyfodil/wazy-build-go`](go/README.md) | `1.26.10` | Go 1.26.10 | command | 134 MB / 399 MB | 403 MB |
| [`ghcr.io/samyfodil/wazy-build-tinygo`](tinygo/README.md) | `0.43.0-go1.26.10` | TinyGo 0.43.0, Go 1.26.10, wit-bindgen-go 0.7.0 | command, proxy | 222 MB / 635 MB | 625 MB |
| [`ghcr.io/samyfodil/wazy-build-c`](c/README.md) | `34.0` | wasi-sdk 34.0 (clang 23), C and C++ | command, proxy | 172 MB / 498 MB | 505 MB |
| [`ghcr.io/samyfodil/wazy-build-python`](python/README.md) | `0.25.1-py3.13.9` | componentize-py 0.25.1, Python 3.13.9 | command, proxy | 100 MB / 273 MB | 281 MB |
| [`ghcr.io/samyfodil/wazy-build-js`](js/README.md) | `1.39.0-cjs0.23.0-node22.23.4` | jco 1.39.0, componentize-js 0.23.0, Node 22.23.4 | command, proxy | 213 MB / 675 MB | 693 MB |
| [`ghcr.io/samyfodil/wazy-build-csharp`](csharp/README.md) | `0.8.0-preview00011-net10.0.402` | componentize-dotnet 0.8.0-preview00011, .NET SDK 10.0.402, NativeAOT-LLVM 10.0.0-rc.1.26306.1, wasi-sdk 29.0 | command, proxy | 560 MB / 1570 MB | 1606 MB |

Compressed is `docker save | gzip`, close to the pull size; unpacked is the
image's size on disk (MiB). Exact byte counts are in each image's `size.<arch>`.

## Usage

Mount your project at `/src` and run as yourself:

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 \
  cargo build --release --target wasm32-wasip2
```

Any `--user` works. The toolchains are read-only and every cache (Cargo
registry, Go build and module cache, npm, pip, NuGet) points under `/tmp`, so
nothing needs root and nothing root-owned lands in your tree.

To run a component, embed wazy (see the [component docs](../component)) or use
the runner these images are tested with:

```sh
go run github.com/samyfodil/wazy/build-images/runner@latest run app.wasm      # wasi:cli/command
go run github.com/samyfodil/wazy/build-images/runner@latest http proxy.wasm / # wasi:http/proxy, one GET
```

## Shared tools

Every image ships the same wasm toolset at the same versions, installed by one
script ([`common/install-tools.sh`](common/install-tools.sh)), so they cannot
drift apart:

| Tool | Version | In the image |
| --- | --- | --- |
| [wasm-tools](https://github.com/bytecodealliance/wasm-tools) | 1.262.0 | `wasm-tools` |
| [wit-bindgen](https://github.com/bytecodealliance/wit-bindgen) CLI | 0.63.0 | `wit-bindgen` |
| [Binaryen](https://github.com/WebAssembly/binaryen) | version_133 | `wasm-opt`, `wasm-metadce`, `wasm-merge`, `wasm-ctor-eval` |
| [Wizer](https://github.com/bytecodealliance/wizer) | 11.0.3 | `wizer` |
| WASI preview1 adapters (from [Wasmtime](https://github.com/bytecodealliance/wasmtime)) | 50.0.0 | `$WASI_ADAPTER_COMMAND`, `$WASI_ADAPTER_REACTOR` |

Each Binaryen tool is a ~17 MB static binary, so only the optimizers ship;
`wasm-tools` covers printing, parsing and the rest.

## Optimizing a component

`wasm-opt` works on core modules, not components. Optimize the core module,
then build the component from it:

```sh
wasm-opt -Oz app.core.wasm -o app.core.wasm
wasm-tools component new app.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o app.wasm
```

Use `$WASI_ADAPTER_REACTOR` for a module with no `main` (a library, or a
`wasi:http/proxy` handler). A module that already imports WASI 0.2 directly
needs no adapter, only `wasm-tools component embed <wit> --world <world>` if it
lacks the component-type section. Where the toolchain links the component
itself out of several core modules (componentize-py, componentize-js,
componentize-dotnet), strip the finished component instead:

```sh
wasm-tools strip --all app.wasm -o app.wasm
```

Each sample's `build.sh` shows the optimized route for its language, and the
tests run the optimized output too.

## What a published tag guarantees

For every image, on both architectures, CI:

1. builds the image from this repository,
2. runs its `sample/build.sh` inside it as an unprivileged `--user`, producing
   a `wasi:cli/command` and (where the language has one) a `wasi:http/proxy`
   component, each plain and optimized,
3. checks each with `wasm-tools validate` and `wasm-tools component wit` for
   the expected export,
4. runs each under wazy built from the same commit and asserts the output:
   the exact stdout for a command, the status and body of a GET for a proxy,
5. fails if the image grew more than 10% over its recorded `size.<arch>`.

On a pull request this gates every image whose inputs changed. On a release
tag (`v*`) every image is re-tested against that release; an image is pushed
only if its tag is not in the registry yet, and the release notes list every
tag that passed. Published tags are never overwritten.

Run the same test locally (Docker and Go required):

```sh
make build-image-test IMAGE_LANG=rust
```

## Tags

A tag names the image's toolchain versions and is immutable. When an image
changes without a toolchain version changing (a shared tool, a base image
digest, a Dockerfile fix), its tag gets the next rebuild suffix: `34.0`, then
`34.0-2`, `34.0-3`. A pull request that changes an image whose tag is already
published fails until `TAG` is bumped. There is no `latest`.

Base images are pinned by digest and every downloaded tool by version and
SHA-256.

## Not shipped

- **Standard Go `wasi:http/proxy`.** `wit-bindgen-go` 0.7.0 emits
  `//go:wasmimport` signatures with `*string` and `*cm.List[...]` parameters;
  standard Go (1.24 through 1.26) rejects those ("go:wasmimport: unsupported
  parameter type"), since it accepts only numeric types, `unsafe.Pointer` and
  pointers to `structs.HostLayout` structs. Standard Go builds
  `wasi:cli/command` components; use the TinyGo image for WIT bindings.

## Layout

```
build-images/
  common/install-tools.sh   the shared wasm toolset
  wit/deps/                 WASI 0.2.12 WIT for samples that generate bindings
  runner/                   runs a component under wazy (used by test.sh)
  test.sh                   builds and tests one image
  <lang>/Dockerfile         build context is build-images/
  <lang>/TAG                the image tag
  <lang>/sample/            build.sh plus the sample sources
  <lang>/test.env           the worlds the sample covers
  <lang>/size.<arch>        recorded unpacked size, in bytes
```
