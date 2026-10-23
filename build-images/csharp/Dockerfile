# syntax=docker/dockerfile:1
# Build context: build-images/ (docker build -f csharp/Dockerfile build-images)

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad AS tools
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
COPY common/install-tools.sh /install-tools.sh
RUN sh /install-tools.sh

# wasi-sdk 29 is the version NativeAOT-LLVM 10.0.0-rc.1 expects. componentize-dotnet
# ignores WASI_SDK_PATH: it always uses $HOME/.wasi-sdk/wasi-sdk-29.0 and, when
# that is missing, downloads the x86_64 build there (wrong on arm64, and slow on
# every fresh container). So the SDK installed here is linked into that path
# below. Cut down to the wasm32-wasip2 C toolchain it uses.
FROM tools AS wasi-sdk
RUN set -eu; \
    case "$(uname -m)" in \
      x86_64) arch=x86_64 sha=87d1d1a2879d139cdc624b968efad3d4a97b8078cdff95e63ac88ecafd1a0171 ;; \
      aarch64) arch=arm64 sha=052ad773397dc9e5aa99fb4cfef694175e6b1e81bb2ad1d3c8e7b3fc81441b7c ;; \
    esac; \
    curl -fsSL --retry 5 -o /sdk.tgz "https://github.com/WebAssembly/wasi-sdk/releases/download/wasi-sdk-29/wasi-sdk-29.0-$arch-linux.tar.gz"; \
    echo "$sha  /sdk.tgz" | sha256sum -c -; \
    mkdir /opt/wasi-sdk; tar -xzf /sdk.tgz -C /opt/wasi-sdk --strip-components=1; rm /sdk.tgz
WORKDIR /opt/wasi-sdk
RUN rm -rf share/wasi-sysroot/experimental-coop-threads share/man share/misc \
      share/wasi-sysroot/*/wasm32-wasi share/wasi-sysroot/*/wasm32-wasip1-threads share/wasi-sysroot/*/wasm32-wasip3 \
      lib/clang/*/lib/*-threads lib/clang/*/lib/*-wasip3 \
      bin/*-threads-* bin/wasm32-wasip3-* bin/lldb* lib/liblldb* lib/libxml2* lib/pkgconfig lib/cmake \
      bin/clang-apply-replacements bin/clang-format bin/git-clang-format bin/clang-tidy bin/run-clang-tidy \
      bin/clang-scan-deps bin/clang-cl bin/lld-link bin/ld64.lld bin/llvm-dwp bin/llvm-config bin/xml2-config \
 && find bin lib -type f -exec sh -c 'head -c4 "$1" | grep -q ELF && strip --strip-unneeded "$1"' _ {} \;

# The .NET SDK, plus a NuGet cache warmed with componentize-dotnet and the
# NativeAOT-LLVM compiler for this architecture by building the sample once.
FROM mcr.microsoft.com/dotnet/sdk:10.0.402-noble@sha256:747cbade5bfd918a175ac32f8f1d931edec2c7d0d5397a3de72eaef1ac1f66d2 AS dotnet
COPY --from=wasi-sdk /opt/wasi-sdk /opt/wasi-sdk
ENV NUGET_PACKAGES=/opt/nuget WASI_SDK_PATH=/opt/wasi-sdk DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1
COPY csharp/sample /warm
RUN mkdir -p /root/.wasi-sdk && ln -s /opt/wasi-sdk /root/.wasi-sdk/wasi-sdk-29.0 \
 && cd /warm && dotnet build -c Release hello \
 && rm -rf /warm /opt/nuget/*/*/*.nupkg \
 && chmod -R a+rwX /opt/nuget

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3t64 \
 && rm -rf /var/lib/apt/lists/* /usr/share/doc /usr/share/man /usr/share/info /usr/share/locale
COPY --from=dotnet /usr/share/dotnet /usr/share/dotnet
COPY --from=dotnet /opt/nuget /opt/nuget
COPY --from=wasi-sdk /opt/wasi-sdk /opt/wasi-sdk
COPY --from=tools /opt/wasm /opt/wasm
RUN mkdir -p /tmp/.wasi-sdk && ln -s /opt/wasi-sdk /tmp/.wasi-sdk/wasi-sdk-29.0
ENV PATH=/usr/share/dotnet:/opt/wasm/bin:$PATH \
    HOME=/tmp \
    DOTNET_ROOT=/usr/share/dotnet \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1 \
    DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 \
    NUGET_PACKAGES=/opt/nuget \
    WASI_SDK_PATH=/opt/wasi-sdk \
    WASI_ADAPTER_COMMAND=/opt/wasm/adapters/wasi_snapshot_preview1.command.wasm \
    WASI_ADAPTER_REACTOR=/opt/wasm/adapters/wasi_snapshot_preview1.reactor.wasm
WORKDIR /src
