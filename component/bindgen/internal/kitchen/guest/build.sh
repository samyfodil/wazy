#!/bin/sh
# Rebuilds ../kitchen.wasm (wit-bindgen pinned in Cargo.toml/Cargo.lock). The
# guest needs no WASI, so it is a wasm32-unknown-unknown core module that
# wasm-tools componentizes: that path understands async function types, which
# older wasm32-wasip2 linkers do not.
#
#   docker run --rm -v "$PWD":/src -w /src ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 sh build.sh
set -eu
cargo build --release --target wasm32-unknown-unknown
wasm-tools component new target/wasm32-unknown-unknown/release/bindgen_kitchen_guest.wasm -o ../kitchen.wasm
wasm-tools component wit ../kitchen.wasm
