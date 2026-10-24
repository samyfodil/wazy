#!/bin/sh
# Builds the sample components into out/: a wasi:cli/command and a
# wasi:http/proxy, each plain and size-optimized. wit/deps holds the WASI WIT
# (build-images/wit/deps in the wazy repo).
set -eu
mkdir -p out

# wasi:cli/command: TinyGo's wasip2 target emits a component directly.
tinygo build -target=wasip2 -o out/command.wasm ./hello

# wasi:http/proxy: generate Go bindings for the world, then build against it.
wit-bindgen-go generate --world example:proxy/proxy -o gen ./wit

# TinyGo's wasip2 runtime imports WASI 0.2.0 (its bundled lib/wasi-cli WIT)
# next to the bindings' wasi:http 0.2.12, so the world the component is built
# against names both. wit-bindgen-go cannot parse a mixed-version package
# set, which is why this lives in build/wit rather than wit/.
mkdir -p build
cp -R wit build/
cp -R "$(tinygo env TINYGOROOT)/lib/wasi-cli/wit" build/wit/deps/tinygo-cli
for d in build/wit/deps/tinygo-cli/deps/*; do
	mv "$d" "build/wit/deps/tinygo-${d##*/}"
done
rmdir build/wit/deps/tinygo-cli/deps
cat >build/wit/world.wit <<'WIT'
package example:proxy;

world proxy {
  include wasi:http/proxy@0.2.12;
  include wasi:cli/imports@0.2.0;
}
WIT
tinygo build -target=wasip2 --wit-package ./build/wit --wit-world example:proxy/proxy -o out/proxy.wasm ./proxy

# Optimized: wasm-opt works on core modules, so build a wasip1 core module,
# optimize it, then wrap it in a component with the preview1 adapter.
tinygo build -target=wasip1 -opt=z -no-debug -o out/hello.core.wasm ./hello
wasm-opt -Oz out/hello.core.wasm -o out/hello.core.wasm
wasm-tools component new out/hello.core.wasm --adapt "$WASI_ADAPTER_COMMAND" -o out/command.opt.wasm

# The proxy: wasip2-core.json is TinyGo 0.43's targets/wasip2.json without
# its wit-package/wit-world/emulator keys, so TinyGo skips the component step
# and emits the core module (WASI 0.2 imports, cabi_realloc and all). It is a
# copy rather than "inherits" because an inherited wit-package cannot be
# cleared.
# Optimize that, embed the world's component type, then componentize; no
# adapter is involved since nothing imports preview1.
tinygo build -target=./wasip2-core.json -opt=z -no-debug -o out/proxy.core.wasm ./proxy
wasm-opt -Oz out/proxy.core.wasm -o out/proxy.core.wasm
wasm-tools component embed ./build/wit --world example:proxy/proxy out/proxy.core.wasm -o out/proxy.core.wasm
wasm-tools component new out/proxy.core.wasm -o out/proxy.opt.wasm
rm out/*.core.wasm
