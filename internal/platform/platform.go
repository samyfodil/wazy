// Package platform includes runtime-specific code needed for the compiler or otherwise.
package platform

import (
	"fmt"
	"os"
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

// CodeFileAlignment is the alignment MapCodeSegmentFromFile needs of a file
// offset: a multiple of the page size on every platform wazy runs on.
const CodeFileAlignment = 64 << 10

// MapCodeSegmentFromFile maps size bytes of f, starting at offset, as
// read-only executable memory, in place of MmapCodeSegment followed by a copy
// and MprotectCodeSegment. offset must be a multiple of CodeFileAlignment.
//
// Pages are read from the file on first use and shared, through the page
// cache, with every process mapping the same file, so code that never runs
// never becomes resident. The mapping outlives f: the caller may close f, and
// the file may be renamed over or unlinked, as long as it is not truncated
// or rewritten in place. Release it with MunmapCodeSegment.
//
// This fails, and the caller must copy the code instead, where a file can't
// be mapped executable: a noexec mount, or macOS, Windows and platforms
// without mmap (see mmap_file_unsupported.go).
func MapCodeSegmentFromFile(f *os.File, offset int64, size int) ([]byte, error) {
	if size == 0 {
		panic("BUG: MapCodeSegmentFromFile with zero length")
	}
	if offset%CodeFileAlignment != 0 {
		return nil, fmt.Errorf("unaligned code offset %d", offset)
	}
	return mapCodeSegmentFromFile(f, offset, size)
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
