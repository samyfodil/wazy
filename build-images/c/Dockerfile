# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f c/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

# wasi-sdk, cut down to what builds wasm32-wasip1/wasip2 C and C++: the
# compiler, linkers and binutils, and those two targets' sysroots.
FROM tools AS wasi-sdk
ARG WASI_SDK_VERSION=34
RUN set -eu; \
    case "$(uname -m)" in \
      x86_64) arch=x86_64 sha=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4 ;; \
      aarch64) arch=arm64 sha=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971 ;; \
    esac; \
    curl -fsSL --retry 5 -o /sdk.tgz "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-$WASI_SDK_VERSION/wasi-sdk-$WASI_SDK_VERSION.0-$arch-linux.tar.gz"; \
    echo "$sha  /sdk.tgz" | sha256sum -c -; \
    mkdir /opt/wasi-sdk; tar -xzf /sdk.tgz -C /opt/wasi-sdk --strip-components=1; rm /sdk.tgz
WORKDIR /opt/wasi-sdk
# Drop the threads/wasip3/coop-threads targets, the debugger, and the clang
# tooling (format/tidy) that a build never runs.
RUN rm -rf share/wasi-sysroot/experimental-coop-threads share/man share/misc \
      share/wasi-sysroot/*/wasm32-wasip1-threads share/wasi-sysroot/*/wasm32-wasip3 \
      lib/clang/*/lib/*-threads lib/clang/*/lib/*-wasip3 \
      bin/*-threads-* bin/wasm32-wasip3-* bin/lldb* lib/liblldb* lib/libxml2* lib/pkgconfig lib/cmake \
      bin/clang-apply-replacements bin/clang-format bin/git-clang-format bin/clang-tidy bin/run-clang-tidy \
      bin/clang-scan-deps bin/clang-cl bin/lld-link bin/ld64.lld bin/llvm-dwp bin/llvm-config bin/xml2-config \
 && find bin lib -type f -exec sh -c 'head -c4 "$1" | grep -q ELF && strip --strip-unneeded "$1"' _ {} \; ; \
    du -sh /opt/wasi-sdk

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update && apt-get install -y --no-install-recommends make \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=wasi-sdk /opt/wasi-sdk /opt/wasi-sdk
COPY --from=tools /opt/wasm /opt/wasm
ENV PATH=/opt/wasi-sdk/bin:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    WASI_SDK_PATH=/opt/wasi-sdk \
    CC=/opt/wasi-sdk/bin/clang \
    CXX=/opt/wasi-sdk/bin/clang++ \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
