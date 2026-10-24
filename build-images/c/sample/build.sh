#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized. wit/deps holds the WASI WIT
# (build-images/wit/deps in the wazy repo).
set -eu
mkdir -p out

# wasi:cli/command: wasi-sdk's wasm32-wasip2 target links a component directly.
clang --target=wasm32-wasip2 -O2 hello.c -o out/command.wasm

# wasi:http/proxy: generate bindings for the world, then build a reactor.
wit-bindgen c --out-dir gen wit
clang --target=wasm32-wasip2 -O2 -mexec-model=reactor \
	proxy.c gen/proxy.c gen/proxy_component_type.o -o out/proxy.wasm

# Optimized: wasm-opt works on core modules, so build a wasip1 core module,
# optimize it, then wrap it in a component with the preview1 adapter.
clang --target=wasm32-wasip1 -Oz hello.c -o out/hello.core.wasm
wasm-opt -Oz out/hello.core.wasm -o out/hello.core.wasm
wasm-tools component new out/hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o out/command.opt.wasm

# The proxy imports WASI 0.2 directly, so its optimized core module is linked
# for wasip2 without the component step (-Wl,--skip-wit-component), optimized,
# then turned into a component by wasm-tools; the component type comes from
# the custom section wit-bindgen embedded.
clang --target=wasm32-wasip2 -Oz -mexec-model=reactor -Wl,--skip-wit-component \
	proxy.c gen/proxy.c gen/proxy_component_type.o -o out/proxy.core.wasm
wasm-opt -Oz out/proxy.core.wasm -o out/proxy.core.wasm
wasm-tools component new out/proxy.core.wasm --adapt "$WASI_ADAPTER_REACTOR" -o out/proxy.opt.wasm
rm out/*.core.wasm
