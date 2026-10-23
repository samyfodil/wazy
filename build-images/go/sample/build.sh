#!/bin/sh
# Builds the sample wasi:cli/command component into out/, plain and
# size-optimized. The image sets GOOS=wasip1 GOARCH=wasm, so every go build
# here targets WebAssembly.
#
# There is no wasi:http/proxy sample for standard Go: see ../README.md.
set -eu
mkdir -p out

# Standard Go has no component target: it builds a wasip1 core module, and
# wasm-tools wraps it with the preview1 command adapter.
go build -o out/hello.core.wasm ./hello
wasm-tools component new out/hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o out/command.wasm

# Optimized: wasm-opt works on core modules, so optimize the core module
# before the component step. -ldflags=-s drops Go's symbol table first.
go build -ldflags=-s -o out/hello.core.wasm ./hello
wasm-opt -Oz --enable-bulk-memory --enable-sign-ext --enable-nontrapping-float-to-int \
	out/hello.core.wasm -o out/hello.core.wasm
wasm-tools component new out/hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o out/command.opt.wasm
rm out/*.core.wasm
