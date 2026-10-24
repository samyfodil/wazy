# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f python/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

FROM python:3.13.9-slim-trixie@sha256:326df678c20c78d465db501563f3492d17c42a4afe33a1f2bf5406a1d56b0e86 AS componentize-py
RUN python -m venv /opt/venv \
 && /opt/venv/bin/pip install --no-cache-dir componentize-py==0.25.1 \
 && /opt/venv/bin/pip uninstall -y pip \
 && find /opt/venv -name __pycache__ -prune -exec rm -rf {} +

FROM python:3.13.9-slim-trixie@sha256:326df678c20c78d465db501563f3492d17c42a4afe33a1f2bf5406a1d56b0e86
RUN rm -rf /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=componentize-py /opt/venv /opt/venv
COPY --from=tools /opt/wasm /opt/wasm
ENV PATH=/opt/venv/bin:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_CACHE_DIR=/tmp/pip-cache \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
