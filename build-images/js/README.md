# wazy-build-js

`ghcr.io/samyfodil/wazy-build-js:1.39.0-cjs0.23.0-node22.23.4`

| Tool | Version |
| --- | --- |
| jco | 1.39.0 |
| componentize-js | 0.23.0 |
| Node.js | 22.23.4 |
| shared wasm tools | see [the index](../README.md#shared-tools) |

## Build

Like componentize-py, componentize-js builds against a WIT world: a project
carries its world and the WASI WIT as `wit/deps` (copy
[`../wit/deps`](../wit/deps)).

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-js:1.39.0-cjs0.23.0-node22.23.4 \
  jco componentize app.js --wit wit --world-name command -o app.wasm
```

## A minimal project

```sh
mkdir -p hello/wit && cd hello
cp -R <wazy>/build-images/wit/deps wit/deps
printf 'package example:hello;\n\nworld command {\n  include wasi:cli/command@0.2.12;\n}\n' > wit/world.wit
printf 'export const run = {\n  run() {\n    console.log("hello from js");\n  },\n};\n' > app.js
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-js:1.39.0-cjs0.23.0-node22.23.4 \
  jco componentize app.js --wit wit --world-name command -o hello.wasm
go run github.com/samyfodil/wazy/build-images/runner@latest run hello.wasm
```

```
hello from js
```

## Sample

[`sample/build.sh`](sample/build.sh) builds `wasi:cli/command` (`hello.js`)
and `wasi:http/proxy` (`proxy.js`, an explicit `incomingHandler` export). A
component embeds the StarlingMonkey engine (about 13 MB) across several core
modules, so the optimized variants are `wasm-tools strip --all` of the
finished components.
