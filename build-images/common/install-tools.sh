#!/bin/sh
# Installs the wasm toolset every wazy build image ships, at one pinned set of
# versions, into $PREFIX (default /opt/wasm). Each image's Dockerfile runs this
# in a builder stage and copies $PREFIX into its final stage, so the versions
# cannot drift between images. Bumping a version here changes every image.
set -eu

PREFIX=${PREFIX:-/opt/wasm}

WASM_TOOLS_VERSION=1.262.0
WIT_BINDGEN_VERSION=0.63.0
BINARYEN_VERSION=133
WIZER_VERSION=11.0.3
WASMTIME_ADAPTER_VERSION=50.0.0

case "$(uname -m)" in
x86_64)
	arch=x86_64
	wasm_tools_sha=655b8df67d54c3ef9b52fc237c4e94bbfbb5f5f43825245cb0940a7002a9ed65
	wit_bindgen_sha=bf4efe883ab70d34a4740253911dda20caf895d58a003af58369506b04684cbe
	binaryen_sha=2dc9c7813f5375db93d96ead4b78222fcc3e2677bbb832297af4797782a37489
	wizer_sha=36016e7f846f38ad09f22eb4bafb99068f038c431eb9613ddf59e31ad8a21414
	;;
aarch64)
	arch=aarch64
	wasm_tools_sha=729dc47a114305ed60873ce6199d403eed1da89c3aa2367eef292da358b1e8d3
	wit_bindgen_sha=1f9b4fb9f3af15374e2a2d026f6a1fcd8b48f37655f59f48a32cb421e169cbe4
	binaryen_sha=89c07ea56faf38d0fbecf36ca8ec0721756716185f265b568e133d427f299bf8
	wizer_sha=73bffcbaf736ef24109a48cac431d070c43cb2ee8a14a840576f7aec0133d8d6
	;;
*)
	echo "unsupported architecture: $(uname -m)" >&2
	exit 1
	;;
esac
adapter_command_sha=b3de00ac47528237211c6404b4a7e4235a67878665dc3e8853071908c3127a85
adapter_reactor_sha=73b01f85e42e99c890efbbf619cd765e04e5809da36d976b87fc64abd87f5812

fetch() { # url sha256 dest
	curl -fsSL --retry 5 -o "$3" "$1"
	echo "$2  $3" | sha256sum -c - >/dev/null
}

tmp=$(mktemp -d)
mkdir -p "$PREFIX/bin" "$PREFIX/adapters"

fetch "https://github.com/bytecodealliance/wasm-tools/releases/download/v$WASM_TOOLS_VERSION/wasm-tools-$WASM_TOOLS_VERSION-$arch-linux.tar.gz" "$wasm_tools_sha" "$tmp/wt.tgz"
tar -xzf "$tmp/wt.tgz" -C "$tmp" --strip-components=1 "wasm-tools-$WASM_TOOLS_VERSION-$arch-linux/wasm-tools"
mv "$tmp/wasm-tools" "$PREFIX/bin/"

fetch "https://github.com/bytecodealliance/wit-bindgen/releases/download/v$WIT_BINDGEN_VERSION/wit-bindgen-$WIT_BINDGEN_VERSION-$arch-linux.tar.gz" "$wit_bindgen_sha" "$tmp/wb.tgz"
tar -xzf "$tmp/wb.tgz" -C "$tmp" --strip-components=1 "wit-bindgen-$WIT_BINDGEN_VERSION-$arch-linux/wit-bindgen"
mv "$tmp/wit-bindgen" "$PREFIX/bin/"

# Binaryen's release binaries are static; ship the user-facing tools only (no
# unit tests, fuzzers, reducer, or the static library). Each is a ~17 MB static
# binary, so only the optimizers ship: wasm-tools covers print/parse/split.
fetch "https://github.com/WebAssembly/binaryen/releases/download/version_$BINARYEN_VERSION/binaryen-version_$BINARYEN_VERSION-$arch-linux.tar.gz" "$binaryen_sha" "$tmp/by.tgz"
tar -xzf "$tmp/by.tgz" -C "$tmp"
for t in wasm-opt wasm-metadce wasm-merge wasm-ctor-eval; do
	mv "$tmp/binaryen-version_$BINARYEN_VERSION/bin/$t" "$PREFIX/bin/"
done

fetch "https://github.com/bytecodealliance/wizer/releases/download/v$WIZER_VERSION/wizer-v$WIZER_VERSION-$arch-linux.tar.xz" "$wizer_sha" "$tmp/wz.txz"
tar -xJf "$tmp/wz.txz" -C "$tmp"
mv "$tmp/wizer-v$WIZER_VERSION-$arch-linux/wizer" "$PREFIX/bin/"

for kind in command reactor; do
	eval sha=\$adapter_${kind}_sha
	fetch "https://github.com/bytecodealliance/wasmtime/releases/download/v$WASMTIME_ADAPTER_VERSION/wasi_snapshot_preview1.$kind.wasm" "$sha" "$PREFIX/adapters/wasi_snapshot_preview1.$kind.wasm"
done

rm -rf "$tmp"
strip "$PREFIX"/bin/*
chmod -R a+rX "$PREFIX"
