# wazy-build-rust

`ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1`

| Tool | Version |
| --- | --- |
| Rust (minimal profile) | 1.99.1, targets `wasm32-wasip1`, `wasm32-wasip2` |
| cargo-component | 0.21.1 |
| shared wasm tools | see [the index](../README.md#shared-tools) |

## Build

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 \
  cargo build --release --target wasm32-wasip2
```

`wasm32-wasip2` makes rustc emit a component directly. `cargo component build`
works too. Cargo's registry lives under `/tmp/cargo` in the container; mount
a volume there to keep it between runs.

## A minimal project

```sh
cargo new --bin hello && cd hello   # or write Cargo.toml and src/main.rs by hand
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 \
  cargo build --release --target wasm32-wasip2
go run github.com/samyfodil/wazy/build-images/runner@latest run target/wasm32-wasip2/release/hello.wasm
```

```
Hello, world!
```

## Sample

[`sample/`](sample/) builds, with [`build.sh`](sample/build.sh):

- `wasi:cli/command` with cargo-component (`hello/`),
- `wasi:http/proxy` from rustc's `wasm32-wasip2` target and the `wasi` crate
  (`proxy/`),
- an optimized variant of each: a `wasm32-wasip1` core module through
  `wasm-opt -Oz`, then `wasm-tools component new` with the preview1 adapter.

`strip = true` in a release profile drops rustc's `target_features` section,
so pass `wasm-opt` the WebAssembly 2.0 features explicitly (`build.sh` shows
the list) or it assumes the MVP and rejects the module.
