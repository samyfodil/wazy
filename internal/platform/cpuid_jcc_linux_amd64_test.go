//go:build linux && amd64 && !tinygo

package platform

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestJccErratumDetectionMatchesHost cross-checks the CPUID-based detection
// against the kernel's own view of the running CPU. It is the only check that
// exercises the CPUID assembly itself against an independent source of truth,
// and it asserts the negative case as strongly as the positive one: on a host
// that is not Skylake-family the flag must be clear, which is what keeps an
// unaffected machine from paying for the workaround.
func TestJccErratumDetectionMatchesHost(t *testing.T) {
	vendor, family, model, ok := procCPUInfo(t)
	if !ok {
		t.Skip("/proc/cpuinfo did not report vendor/family/model")
	}
	t.Logf("host: vendor=%s family=%d model=0x%x", vendor, family, model)

	affected := vendor == "GenuineIntel" && family == 6
	if affected {
		switch model {
		case 0x4e, 0x5e, 0x55, 0x8e, 0x9e, 0xa5, 0xa6:
		default:
			affected = false
		}
	}
	require.Equal(t, affected, CpuFeatures.Has(CpuFeatureAmd64JCCErratum),
		"CPUID says %v for vendor=%s family=%d model=0x%x",
		CpuFeatures.Has(CpuFeatureAmd64JCCErratum), vendor, family, model)
	require.Equal(t, affected, JCCErratumWorkaroundEnabled())
}

// TestCpuidVendorAndMaxLeaf asserts the raw CPUID helper returns something
// sane, so that a broken assembly stub shows up as a failure here rather than
// as a silently-disabled workaround.
func TestCpuidVendorAndMaxLeaf(t *testing.T) {
	maxLeaf, ebx, ecx, edx := cpuid(0, 0)
	require.True(t, maxLeaf >= 1, "CPUID leaf 0 reported max leaf %d", maxLeaf)

	var vendor [12]byte
	for i, r := range [3]uint32{ebx, edx, ecx} {
		vendor[i*4+0] = byte(r)
		vendor[i*4+1] = byte(r >> 8)
		vendor[i*4+2] = byte(r >> 16)
		vendor[i*4+3] = byte(r >> 24)
	}
	procVendor, _, _, ok := procCPUInfo(t)
	if !ok {
		t.Skip("/proc/cpuinfo did not report vendor_id")
	}
	require.Equal(t, procVendor, string(vendor[:]))
}

func procCPUInfo(t *testing.T) (vendor string, family, model uint32, ok bool) {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		t.Skipf("cannot read /proc/cpuinfo: %v", err)
	}
	var haveFamily, haveModel bool
	for _, line := range strings.Split(string(b), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "vendor_id":
			vendor = value
		case "cpu family":
			if n, err := strconv.ParseUint(value, 10, 32); err == nil {
				family, haveFamily = uint32(n), true
			}
		case "model":
			if n, err := strconv.ParseUint(value, 10, 32); err == nil {
				model, haveModel = uint32(n), true
			}
		}
		if vendor != "" && haveFamily && haveModel {
			return vendor, family, model, true
		}
	}
	return vendor, family, model, vendor != "" && haveFamily && haveModel
}
