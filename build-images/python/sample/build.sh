#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized. wit/deps holds the WASI WIT
# (build-images/wit/deps in the wazy repo).
set -eu
mkdir -p out

componentize-py -d wit -w command componentize hello -o out/command.wasm
componentize-py -d wit -w proxy componentize proxy -o out/proxy.wasm

# Optimized: a componentize-py component links CPython from several core
# modules, so there is no single core module to run wasm-opt over. Strip the
# finished component's custom sections (debug info, names) instead.
wasm-tools strip --all out/command.wasm -o out/command.opt.wasm
wasm-tools strip --all out/proxy.wasm -o out/proxy.opt.wasm
