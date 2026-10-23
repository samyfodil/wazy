# wazy-build-python

`ghcr.io/samyfodil/wazy-build-python:0.25.1-py3.13.9`

| Tool | Version |
| --- | --- |
| componentize-py | 0.25.1 |
| Python | 3.13.9 |
| shared wasm tools | see [the index](../README.md#shared-tools) |

## Build

componentize-py builds against a WIT world, so a project carries its world
and the WASI WIT as `wit/deps` (copy [`../wit/deps`](../wit/deps)).

```sh
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-python:0.25.1-py3.13.9 \
  componentize-py -d wit -w command componentize app -o app.wasm
```

## A minimal project

```sh
mkdir -p hello/wit && cd hello
cp -R <wazy>/build-images/wit/deps wit/deps
printf 'package example:hello;\n\nworld command {\n  include wasi:cli/command@0.2.12;\n}\n' > wit/world.wit
printf 'from wit_world import exports\n\n\nclass Run(exports.Run):\n    def run(self) -> None:\n        print("hello from python")\n' > app.py
docker run --rm -v "$PWD":/src -w /src --user "$(id -u):$(id -g)" \
  ghcr.io/samyfodil/wazy-build-python:0.25.1-py3.13.9 \
  componentize-py -d wit -w command componentize app -o hello.wasm
go run github.com/samyfodil/wazy/build-images/runner@latest run hello.wasm
```

```
hello from python
```

## Running it

componentize-py's CPython uses the `extended-const` proposal, which is
WebAssembly 3.0. Enable it in the runtime that instantiates the component:

```go
r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV3))
```

## Sample

[`sample/build.sh`](sample/build.sh) builds `wasi:cli/command` (`hello.py`)
and `wasi:http/proxy` (`proxy.py`). The component links CPython from several
core modules, so there is no single core module for `wasm-opt`; the optimized
variants are `wasm-tools strip --all` of the finished components.
