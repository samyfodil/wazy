module github.com/samyfodil/wazy/benchmarks/vs-wazero

go 1.25.0

require (
	github.com/bytecodealliance/wasmtime-go/v34 v34.0.0
	github.com/samyfodil/wazy v0.0.0-00010101000000-000000000000
	github.com/tetratelabs/wazero v1.12.1-0.20260829084255-f4779551afb4
)

require golang.org/x/sys v0.44.0 // indirect

replace github.com/samyfodil/wazy => ../..
