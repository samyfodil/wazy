# ctenv fixtures

One `.wat` per branch of the component-level type checker (`ctenv.go`) that
no vendored suite reaches -- most of them components a validator refuses,
which `wasm-tools parse` (1.262.0, in `ghcr.io/samyfodil/wazy-build-c:34.0`)
encodes without validating. The `.wasm` beside each is that encoding;
`ctenv_test.go` names the error each must be refused with, or that it must
be accepted.
