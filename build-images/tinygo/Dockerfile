# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f tinygo/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

FROM golang:1.26.10-trixie@sha256:909771fef53e3dcb0950255f0c9a8b4728c3b841e61509c7b29b098de7ee02d4 AS go

# TinyGo needs a Go toolchain for package loading; prune what a build never
# reads: the Go test corpus, API lists and docs.
FROM go AS go-slim
RUN rm -rf /usr/local/go/test /usr/local/go/api /usr/local/go/doc

# TinyGo, without the libraries and device packages for targets other than
# WebAssembly (Windows, macOS, musl, microcontrollers): src/device alone is
# 900 MB of register definitions no wasm build imports. src/device/arm stays:
# TinyGo recognises its root by it.
FROM tools AS tinygo
ARG TINYGO_VERSION=0.43.0
RUN set -eu; \
    case "$(uname -m)" in \
      x86_64) arch=amd64 sha=ca9fc6d055e3889462dea4450a2e42a92242d88b5c362a7ec39bdb563413d78a ;; \
      aarch64) arch=arm64 sha=a2165b9f46aff9a895a0abed54aedffab1e93317a387c6c2e9dd839425527cbd ;; \
    esac; \
    curl -fsSL --retry 5 -o /tg.tgz "https://github.com/tinygo-org/tinygo/releases/download/v$TINYGO_VERSION/tinygo$TINYGO_VERSION.linux-$arch.tar.gz"; \
    echo "$sha  /tg.tgz" | sha256sum -c -; \
    tar -xzf /tg.tgz -C /usr/local; rm /tg.tgz; \
    cd /usr/local/tinygo; \
    rm -rf lib/mingw-w64 lib/musl lib/picolibc lib/nrfx lib/macos-minimal-sdk lib/CMSIS lib/xtensa src/examples; \
    find src/device -mindepth 1 -maxdepth 1 ! -name arm -exec rm -rf {} +

# wit-bindgen-go, built with the image's Go. The module cache is a tmpfs: a
# dependency ships ".wh.*" test files, which overlayfs rejects as whiteout names.
FROM go AS wit-bindgen-go
RUN --mount=type=tmpfs,target=/go/pkg/mod GOBIN=/out go install -trimpath -ldflags=-s go.bytecodealliance.org/cmd/wit-bindgen-go@v0.7.0

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=go-slim /usr/local/go /usr/local/go
COPY --from=tinygo /usr/local/tinygo /usr/local/tinygo
COPY --from=wit-bindgen-go /out/wit-bindgen-go /usr/local/bin/
COPY --from=tools /opt/wasm /opt/wasm
ENV PATH=/usr/local/tinygo/bin:/usr/local/go/bin:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    GOPATH=/tmp/go \
    GOCACHE=/tmp/go-cache \
    GOFLAGS=-modcacherw \
    XDG_CACHE_HOME=/opt/cache \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
# TinyGo compiles wasi-libc and its runtime on first use. Do it now for both
# WebAssembly targets so a build starts warm; the cache stays writable for any
# --user so other flag combinations can add to it.
RUN mkdir -p /tmp/warm && cd /tmp/warm \
 && printf 'package main\n\nfunc main() { println("warm") }\n' > main.go \
 && tinygo build -target=wasip2 -o /dev/null main.go \
 && tinygo build -target=wasip1 -o /dev/null main.go \
 && cd / && rm -rf /tmp/warm /tmp/go-cache /tmp/go \
 && chmod -R a+rwX /opt/cache
WORKDIR /src
