#!/bin/sh
# Rebuilds ../fixedlistguest.wasm in the pinned Rust build image
# (Rust 1.99.1, wasm32-wasip2; wit-bindgen pinned in Cargo.toml/Cargo.lock):
#
#   docker run --rm -v "$PWD":/src -w /src ghcr.io/samyfodil/wazy-build-rust:1.99.1-cc0.21.1 sh build.sh
set -eu
cargo build --release --target wasm32-wasip2
cp target/wasm32-wasip2/release/fixed_list_guest.wasm ../fixedlistguest.wasm
wasm-tools component wit ../fixedlistguest.wasm
