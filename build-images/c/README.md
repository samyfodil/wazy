# wazy-build-c

`ghcr.io/samyfodil/wazy-build-c:34.0`

| Tool | Version |
| --- | --- |
| wasi-sdk | 34.0 (clang 23.1, C and C++), sysroots for `wasm32-wasip1` and `wasm32-wasip2` |
| shared wasm tools | see [the index](../README.md#shared-tools) |

`CC`, `CXX` and `WASI_SDK_PATH` are set. The threads and `wasip3` sysroots,
the debugger and clang-format/tidy are not shipped.

## Build

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-c:34.0 \
  clang --target=wasm32-wasip2 -O2 main.c -o app.wasm
```

`--target=wasm32-wasip2` links through `wasm-component-ld`, so the output is
a component. Add `-mexec-model=reactor` for a library with no `main`.

## A minimal project

```sh
mkdir hello && cd hello
printf '#include <stdio.h>\n\nint main(void) {\n\tprintf("hello from c\\n");\n\treturn 0;\n}\n' > hello.c
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-c:34.0 \
  clang --target=wasm32-wasip2 -O2 hello.c -o hello.wasm
go run github.com/samyfodil/wazy/build-images/runner@latest run hello.wasm
```

```
hello from c
```

## Sample

[`sample/build.sh`](sample/build.sh) builds:

- `wasi:cli/command` (`hello.c`),
- `wasi:http/proxy` from `wit-bindgen c` bindings (`proxy.c`, `wit/`),
- optimized variants: the command as a `wasm32-wasip1` core module through
  `wasm-opt -Oz` and the preview1 adapter; the proxy linked with
  `-Wl,--skip-wit-component` (a core module importing WASI 0.2), through
  `wasm-opt -Oz`, then `wasm-tools component new`.
