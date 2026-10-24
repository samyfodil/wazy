# component-model validation: instantiation

`instantiation.wast` is verbatim from the Component Model specification's own
test suite,
[WebAssembly/component-model@d095e2f6](https://github.com/WebAssembly/component-model/blob/d095e2f60ddae1f9a1e9b1e2fb547ec1114da677/test/validation/instantiation.wast)
(`test/validation/`): instantiation-time type checking -- every `with`
argument against the instantiated component's (or core module's) imports,
value types structurally, funcs exactly, instances export by export, the
sorts, and the index spaces arguments are taken from. The `.json` and `.wasm`
files are what `wasm-tools json-from-wast` (1.262.0, in
`ghcr.io/samyfodil/wazy-build-c:34.0`) makes of it, the form
`wast_validation_test.go` runs.

Licensed Apache-2.0, per the component-model repository; see `LICENSE-APACHE`.
