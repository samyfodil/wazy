#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized. wit/deps holds the WASI WIT
# (build-images/wit/deps in the wazy repo).
set -eu
mkdir -p out

jco componentize hello.js --wit wit --world-name command -o out/command.wasm
jco componentize proxy.js --wit wit --world-name proxy -o out/proxy.wasm

# Optimized: the component embeds the StarlingMonkey engine across several
# core modules, so there is no single core module to run wasm-opt over. Strip
# the finished component's custom sections instead.
wasm-tools strip --all out/command.wasm -o out/command.opt.wasm
wasm-tools strip --all out/proxy.wasm -o out/proxy.opt.wasm
