# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f go/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

FROM golang:1.26.10-trixie@sha256:909771fef53e3dcb0950255f0c9a8b4728c3b841e61509c7b29b098de7ee02d4 AS go

# Prune what a build never reads: the Go test corpus, API lists and docs.
FROM go AS go-slim
RUN rm -rf /usr/local/go/test /usr/local/go/api /usr/local/go/doc

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=go-slim /usr/local/go /usr/local/go
COPY --from=tools /opt/wasm /opt/wasm
ENV PATH=/usr/local/go/bin:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    GOPATH=/tmp/go \
    GOCACHE=/tmp/go-cache \
    GOFLAGS=-modcacherw \
    GOOS=wasip1 \
    GOARCH=wasm \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
