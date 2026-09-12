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
	// riscv64 is Linux and nothing else. golang.org/x/sys/cpu reads the vector
	// extension out of AT_HWCAP, which is a Linux interface and reports nothing
	// elsewhere -- so on another OS every module would silently fall back to the
	// interpreter with no way to tell that from a CPU without RVV. Nor has this
	// backend been run anywhere else. The interpreter still serves those.
	if runtime.GOARCH == "riscv64" {
		return runtime.GOOS == "linux" && riscv64CompilerSupports(features)
	}

	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd", "windows":
		if runtime.GOARCH == "arm64" {
			if features.IsEnabled(experimental.CoreFeaturesThreads) {
				return CpuFeatures.Has(CpuFeatureArm64Atomic)
			}
			return true
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
// Threads needs the A extension, which RV64GC includes and the baseline the
// backend targets assumes. What the baseline does not include is Zabha or
// Zacas, so the sub-word and compare-exchange forms are LR/SC loops rather than
// single instructions -- a code-size question, not a correctness one.
//
// What the spec suites under qemu-user do not establish is the *ordering*: it
// runs guest threads as host threads, so an x86 host's stronger model hides the
// reorderings a weakly ordered machine would expose. The sequences carry the
// fences and the aq/rl bits the memory model asks for; confirming that on
// silicon is still worth doing.
func riscv64CompilerSupports(features api.CoreFeatures) bool {
	if features.IsEnabled(api.CoreFeatureSIMD) {
		return CpuFeatures.Has(CpuFeatureRiscv64V)
	}
	return true
}
