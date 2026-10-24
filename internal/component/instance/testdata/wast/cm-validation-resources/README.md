# component-model validation: resources

`resources.wast` is verbatim from the Component Model specification's own
test suite,
[WebAssembly/component-model@d095e2f6](https://github.com/WebAssembly/component-model/blob/d095e2f60ddae1f9a1e9b1e2fb547ec1114da677/test/validation/resources.wast)
(`test/validation/`): resource types as abstract types -- which resources a
`(sub resource)` import or export makes fresh, how instantiation substitutes
the supplied resources into a component's exports (and gives each
instantiation fresh copies of the ones it defines), resource subtyping across
component-typed imports, and the rules on `own`, `borrow` and the resource
built-ins. The `.json` and `.wasm` files are what `wasm-tools json-from-wast`
(1.262.0, in `ghcr.io/samyfodil/wazy-build-c:34.0`) makes of it, the form
`wast_validation_test.go` runs.

Licensed Apache-2.0, per the component-model repository; see `LICENSE-APACHE`.
