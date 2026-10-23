# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f js/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

FROM node:22.23.4-trixie-slim@sha256:8eb619e402eaede1748a3c8173bffab391592812e85136452f781741c7242ddf AS jco
RUN npm install --prefix /opt/js --omit=dev --no-fund --no-audit \
      @bytecodealliance/jco@1.39.0 @bytecodealliance/componentize-js@0.23.0 \
 && npm cache clean --force

FROM node:22.23.4-trixie-slim@sha256:8eb619e402eaede1748a3c8173bffab391592812e85136452f781741c7242ddf
RUN rm -rf /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=jco /opt/js /opt/js
COPY --from=tools /opt/wasm /opt/wasm
# componentize-js runs wizer with an environment that drops HOME, so wasmtime
# falls back to the passwd entry for its config dir. An arbitrary --user uid
# has none, and wizer fails ("config file not specified and failed to get the
# default"). Wrap the binary to give it a HOME.
RUN w=$(echo /opt/js/node_modules/@bytecodealliance/wizer-linux-*/wizer) \
 && mv "$w" "$w.real" \
 && printf '#!/bin/sh\nHOME=${HOME:-/tmp} exec "$0.real" "$@"\n' > "$w" \
 && chmod 755 "$w"
ENV PATH=/opt/js/node_modules/.bin:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    npm_config_cache=/tmp/npm-cache \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
