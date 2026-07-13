module github.com/samyfodil/wazy/benchmarks/vs-wazero

go 1.25.0

require (
	github.com/samyfodil/wazy v0.0.0-00010101000000-000000000000
	github.com/tetratelabs/wazero v1.12.1-0.20260630042819-c0f3a4ec6411
)

require (
	github.com/bytecodealliance/wasmtime-go/v34 v34.0.0 // indirect
	golang.org/x/sys v0.44.0 // indirect
)

replace github.com/samyfodil/wazy => ../..
