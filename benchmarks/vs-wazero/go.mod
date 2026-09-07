module github.com/samyfodil/wazy/benchmarks/vs-wazero

go 1.25.0

require (
	github.com/bytecodealliance/wasmtime-go/v34 v34.0.0
	github.com/samyfodil/wazy v0.0.0-00010101000000-000000000000
	github.com/tetratelabs/wazero v1.12.1-0.20260904184217-6edbb8c01a5f
)

require golang.org/x/sys v0.44.0 // indirect

replace github.com/samyfodil/wazy => ../..
