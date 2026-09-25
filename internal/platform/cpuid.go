package platform

// CpuFeatureFlags exposes methods for querying CPU capabilities.
//
// The flags describe everything about the host CPU that changes the machine
// code the compiler emits -- capabilities it may use, and errata it must work
// around. Raw is folded into the compilation cache key (see fileCacheKey), so
// anything recorded here is automatically prevented from leaking across a
// heterogeneous fleet sharing one cache directory.
type CpuFeatureFlags uint64

const (
	// CpuFeatureAmd64SSE4_1 is the flag to query CpuFeatureFlags.Has for SSEv4.1 capabilities on amd64
	CpuFeatureAmd64SSE4_1 = 1 << iota
	// CpuFeatureAmd64BMI1 is the flag to query CpuFeatureFlags.Has for Bit Manipulation Instruction Set 1 (e.g. TZCNT) on amd64
	CpuFeatureAmd64BMI1
	// CpuExtraFeatureABM is the flag to query CpuFeatureFlags.Has for Advanced Bit Manipulation capabilities (e.g. LZCNT) on amd64
	CpuFeatureAmd64ABM
	// CpuFeatureAmd64JCCErratum is the flag to query CpuFeatureFlags.Has for a Skylake-family
	// amd64 core subject to Intel erratum SKX102, the "JCC erratum". Unlike the others this is
	// not a capability but a defect the code layout has to route around; see cpuid_jcc.go and
	// JCCErratumWorkaroundEnabled.
	CpuFeatureAmd64JCCErratum
)

const (
	// CpuFeatureArm64Atomic is the flag to query CpuFeatureFlags.Has for Large System Extensions capabilities on arm64
	CpuFeatureArm64Atomic CpuFeatureFlags = 1 << iota
)

const (
	// CpuFeatureRiscv64V is the flag to query CpuFeatureFlags.Has for the RVV
	// vector extension on riscv64. wasm's v128 needs it, and needs VLEN >= 128,
	// which every RVV 1.0 implementation provides.
	CpuFeatureRiscv64V CpuFeatureFlags = 1 << iota
)

func (c CpuFeatureFlags) Has(f CpuFeatureFlags) bool {
	return c&f != 0
}

func (c CpuFeatureFlags) Raw() uint64 {
	return uint64(c)
}
