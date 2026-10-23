# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f rust/Dockerfile build-images)

# Shared wasm toolset (wasm-tools, wit-bindgen, binaryen, wizer, preview1 adapters).
FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

FROM rust:1.99.1-slim-trixie@sha256:aab42391a986424e22d6a3ec14f37bc6dd3dacff4d457ca7cccd781e02297cc6 AS cargo-component
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev \
 && cargo install cargo-component --version 0.21.1 --locked --root /out \
 && strip /out/bin/cargo-component

FROM rust:1.99.1-slim-trixie@sha256:aab42391a986424e22d6a3ec14f37bc6dd3dacff4d457ca7cccd781e02297cc6
RUN rustup target add wasm32-wasip1 wasm32-wasip2 \
 && rm -rf /usr/local/rustup/downloads /usr/local/rustup/tmp /usr/local/rustup/toolchains/*/share/doc \
    /usr/local/cargo/registry /usr/share/doc /usr/share/man
COPY --from=cargo-component /out/bin/cargo-component /usr/local/cargo/bin/
COPY --from=tools /opt/wasm /opt/wasm
# Any --user uid:gid works: the toolchain is read-only, and everything cargo
# writes (registry, git checkouts) goes under world-writable /tmp.
ENV PATH=/opt/wasm/bin:$PATH \
    HOME=/tmp \
    CARGO_HOME=/tmp/cargo \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
