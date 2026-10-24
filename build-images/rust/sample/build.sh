#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized.
set -eu
mkdir -p out

# wasi:cli/command with cargo-component (wasm32-wasip1 + the command adapter).
cargo component build --release -p hello
cp target/wasm32-wasip1/release/hello.wasm out/command.wasm

# wasi:http/proxy straight from rustc's wasm32-wasip2 target.
cargo build --release -p proxy --target wasm32-wasip2
cp target/wasm32-wasip2/release/proxy.wasm out/proxy.wasm

# Optimized: wasm-opt works on core modules, so build each as a wasip1 core
# module, optimize it, then wrap it in a component with the preview1 adapter.
# strip = true drops rustc's target_features section, so name the WebAssembly
# 2.0 features rustc emits rather than let wasm-opt assume the MVP.
features="--enable-bulk-memory --enable-nontrapping-float-to-int --enable-sign-ext --enable-mutable-globals --enable-multivalue --enable-reference-types"
cargo build --release -p hello --target wasm32-wasip1
wasm-opt -Oz $features target/wasm32-wasip1/release/hello.wasm -o out/hello.core.wasm
wasm-tools component new out/hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o out/command.opt.wasm

cargo build --release -p proxy --target wasm32-wasip1
wasm-opt -Oz $features target/wasm32-wasip1/release/proxy.wasm -o out/proxy.core.wasm
wasm-tools component new out/proxy.core.wasm --adapt "$WASI_ADAPTER_REACTOR" -o out/proxy.opt.wasm
rm out/*.core.wasm
