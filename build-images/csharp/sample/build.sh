#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized. proxy/wit/deps holds the
# WASI WIT (build-images/wit/deps in the wazy repo).
set -eu
mkdir -p out

# componentize-dotnet compiles with NativeAOT-LLVM and links a wasip2
# component directly; for proxy it also generates the WIT bindings.
dotnet build -c Release hello
cp hello/bin/Release/net10.0/wasi-wasm/native/hello.wasm out/command.wasm
dotnet build -c Release proxy
cp proxy/bin/Release/net10.0/wasi-wasm/native/proxy.wasm out/proxy.wasm

# Optimized: NativeAOT already optimizes at link time and the core modules
# are linked inside the SDK, so strip the component's custom sections (debug
# info, names), which are most of its size.
wasm-tools strip --all out/command.wasm -o out/command.opt.wasm
wasm-tools strip --all out/proxy.wasm -o out/proxy.opt.wasm
