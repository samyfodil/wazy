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
// The scalar backend is complete. Two feature sets are still withheld, and
// both are withheld because the lowering does not exist yet rather than
// because the hardware cannot do it:
//
//   - SIMD needs the RVV lowering. When it lands, this becomes a check for
//     CpuFeatureRiscv64V, since RVV is optional on RISC-V and a good deal of
//     shipping hardware has none -- unlike SSE4.1 on amd64, this genuinely
//     varies.
//   - Threads needs the atomic lowering, and separately needs verifying on
//     hardware: RISC-V is weakly ordered, and qemu-user runs guest threads as
//     host threads, so an x86 host's stronger model hides exactly the bugs
//     that matter.
//
// Until then a default runtime -- CoreFeaturesV2 includes SIMD -- falls back
// to the interpreter on riscv64, which is correct but means the compiler is
// only reached by a caller that asks for a narrower feature set.
func riscv64CompilerSupports(features api.CoreFeatures) bool {
	if features.IsEnabled(api.CoreFeatureSIMD) {
		return false
	}
	if features.IsEnabled(experimental.CoreFeaturesThreads) {
		return false
	}
	return true
}
