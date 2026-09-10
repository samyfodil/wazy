// Package platform includes runtime-specific code needed for the compiler or otherwise.
package platform

import (
	"runtime"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
)

// CompilerSupported includes constraints here and also the assembler.
func CompilerSupported() bool {
	return CompilerSupports(api.CoreFeaturesV2)
}

func CompilerSupports(features api.CoreFeatures) bool {
	if !nativeCompilerAvailable {
		return false
	}
	if !compilerPlatformSupports(features) {
		return false
	}
	// Won't panic
	return executableMmapSupported()
}

func compilerPlatformSupports(features api.CoreFeatures) bool {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "windows":
		if runtime.GOARCH == "arm64" {
			if features.IsEnabled(experimental.CoreFeaturesThreads) {
				return CpuFeatures.Has(CpuFeatureArm64Atomic)
			}
			return true
		}
		if runtime.GOARCH == "riscv64" {
			return riscv64CompilerSupports(features)
		}
		fallthrough
	case "dragonfly", "solaris", "illumos":
		return runtime.GOARCH == "amd64" && CpuFeatures.Has(CpuFeatureAmd64SSE4_1)
	default:
		return false
	}
}

// MmapCodeSegment allocates and returns a byte slice to copy executable code into.
//
// See https://man7.org/linux/man-pages/man2/mmap.2.html for mmap API and flags.
func MmapCodeSegment(size int) ([]byte, error) {
	if size == 0 {
		panic("BUG: MmapCodeSegment with zero length")
	}
	return mmapCodeSegment(size)
}

// MunmapCodeSegment unmaps the given memory region.
func MunmapCodeSegment(code []byte) error {
	if len(code) == 0 {
		panic("BUG: MunmapCodeSegment with zero length")
	}
	return munmapCodeSegment(code)
}

func executableMmapSupported() bool {
	seg, err := MmapCodeSegment(1)
	if err != nil {
		return false
	}
	defer func() {
		_ = MunmapCodeSegment(seg)
	}()
	if err := MprotectCodeSegment(seg); err != nil {
		return false
	}
	return true
}

// riscv64CompilerSupports reports whether the riscv64 backend can compile a
// module with the given feature set.
//
// SIMD needs RVV, and unlike SSE4.1 on amd64 that genuinely varies: the vector
// extension is optional on RISC-V and a good deal of shipping hardware has
// none. Where it is absent the interpreter takes SIMD modules, exactly as it
// does on an amd64 without SSE4.1.
//
// Threads is still withheld, and for a reason that is not about hardware: the
// atomic lowering does not exist yet. It also wants verifying on real silicon
// rather than under emulation, because RISC-V is weakly ordered and qemu-user
// runs guest threads as host threads -- so an x86 host's stronger model hides
// exactly the reorderings that would matter.
func riscv64CompilerSupports(features api.CoreFeatures) bool {
	if features.IsEnabled(experimental.CoreFeaturesThreads) {
		return false
	}
	if features.IsEnabled(api.CoreFeatureSIMD) {
		return CpuFeatures.Has(CpuFeatureRiscv64V)
	}
	return true
}
