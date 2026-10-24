//go:build !tinygo

package platform

import "golang.org/x/sys/cpu"

// CpuFeatures exposes the capabilities for this CPU, queried via the Has method.
var CpuFeatures = loadCpuFeatureFlags()

func loadCpuFeatureFlags() (flags CpuFeatureFlags) {
	if cpu.X86.HasSSE41 {
		flags |= CpuFeatureAmd64SSE4_1
	}
	if cpu.X86.HasBMI1 {
		flags |= CpuFeatureAmd64BMI1
	}
	// x/sys/cpu does not track the ABM explicitly.
	// LZCNT combined with BMI1 and BMI2 completes the expanded ABM instruction set.
	// Intel includes LZCNT in BMI1, and all AMD CPUs with POPCNT also have LZCNT.
	if cpu.X86.HasBMI1 && cpu.X86.HasBMI2 && cpu.X86.HasPOPCNT {
		flags |= CpuFeatureAmd64ABM
	}
	if jccErratumAffected() {
		flags |= CpuFeatureAmd64JCCErratum
	}
	return
}

// cpuid executes the CPUID instruction with the given EAX/ECX inputs and
// returns EAX, EBX, ECX and EDX. Implemented in cpuid_jcc_amd64.s.
func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)

// jccErratumAffected reports whether this CPU is subject to Intel erratum
// SKX102 (see cpuid_jcc.go), by reading the vendor string from CPUID leaf 0 and
// the family/model signature from leaf 1.
func jccErratumAffected() bool {
	maxID, ebx, ecx, edx := cpuid(0, 0)
	if maxID < 1 {
		return false
	}
	sig, _, _, _ := cpuid(1, 0)
	return jccErratumFromCPUID(ebx, edx, ecx, sig)
}
