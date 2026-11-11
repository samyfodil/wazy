#!/bin/sh
# Rebuilds ../async.wasm: an async export, fetch, awaiting an async import,
# slow, and returning what it gives plus one. Built as kitchen's is; see
# component/bindgen/internal/kitchen/guest/build.sh.
set -eu
cargo build --release --target wasm32-unknown-unknown
wasm-tools component new target/wasm32-unknown-unknown/release/witgo_async_guest.wasm -o ../async.wasm
wasm-tools component wit ../async.wasm
