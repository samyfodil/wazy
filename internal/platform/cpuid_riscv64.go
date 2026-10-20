//go:build !tinygo

package platform

import "golang.org/x/sys/cpu"

// CpuFeatures exposes the capabilities for this CPU, queried via the Has method.
var CpuFeatures = loadCpuFeatureFlags()

func loadCpuFeatureFlags() (flags CpuFeatureFlags) {
	// RVV is what wasm's v128 needs. It is optional on RISC-V -- plenty of
	// shipping hardware has none -- so unlike SSE4.1 on amd64 this genuinely
	// varies, and the compiler backend is withheld when it is absent.
	if cpu.RISCV64.HasV {
		flags |= CpuFeatureRiscv64V
	}
	return
}
