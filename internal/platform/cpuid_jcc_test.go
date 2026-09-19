package platform

import (
	"runtime"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// sig builds a CPUID leaf-1 EAX value (Intel SDM Vol.2A, Figure 3-6) from the
// display family and display model, so that the table below can be read against
// Intel's own "06_55H" style notation.
func sig(family, model, stepping uint32) uint32 {
	var baseFamily, extFamily uint32
	if family >= 0xf {
		baseFamily, extFamily = 0xf, family-0xf
	} else {
		baseFamily = family
	}
	var baseModel, extModel uint32
	if baseFamily == 0x6 || baseFamily == 0xf {
		baseModel, extModel = model&0xf, model>>4
	} else {
		baseModel = model & 0xf
	}
	return extFamily<<20 | extModel<<16 | baseFamily<<8 | baseModel<<4 | stepping
}

func TestJccErratumFromCPUID(t *testing.T) {
	const (
		amdEBX = 0x68747541 // "Auth"
		amdEDX = 0x69746e65 // "enti"
		amdECX = 0x444d4163 // "cAMD"
	)
	for _, tc := range []struct {
		name          string
		ebx, edx, ecx uint32
		sig           uint32
		exp           bool
	}{
		// Affected: every signature in Intel's erratum table.
		{"Skylake-U/Y 06_4E", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x4e, 3), true},
		{"Skylake-S/H 06_5E", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x5e, 3), true},
		{"Skylake-SP/D 06_55", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x55, 4), true},
		{"Cascade Lake 06_55", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x55, 7), true},
		{"Kaby/Whiskey/Comet mobile 06_8E", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x8e, 9), true},
		{"Kaby/Coffee 06_9E", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x9e, 0xd), true},
		{"Comet Lake S/H 06_A5", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0xa5, 3), true},
		{"Comet Lake U62 06_A6", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0xa6, 0), true},

		// Unaffected Intel: this is the half that decides whether a modern core
		// pays +2.5% code size and extra NOPs for nothing.
		{"Broadwell 06_3D", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x3d, 4), false},
		{"Haswell 06_3C", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x3c, 3), false},
		{"Ice Lake client 06_7E", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x7e, 5), false},
		{"Ice Lake server 06_6A", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x6a, 6), false},
		{"Ice Lake server 06_6C", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x6c, 0), false},
		{"Tiger Lake 06_8C", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x8c, 1), false},
		{"Rocket Lake 06_A7", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0xa7, 1), false},
		{"Alder Lake 06_97", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x97, 2), false},
		{"Sapphire Rapids 06_8F", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0x8f, 4), false},
		{"Emerald Rapids 06_CF", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(6, 0xcf, 2), false},
		{"NetBurst family 15", vendorIntelEBX, vendorIntelEDX, vendorIntelECX, sig(15, 0x55, 1), false},

		// Non-Intel: a model number alone means nothing, so the vendor string
		// has to gate it. AMD family 0x19 model 0x55 would otherwise match.
		{"AMD Zen3 19_55", amdEBX, amdEDX, amdECX, sig(0x19, 0x55, 0), false},
		{"AMD family 6 model 0x5E", amdEBX, amdEDX, amdECX, sig(6, 0x5e, 0), false},
		{"AMD with Skylake signature", amdEBX, amdEDX, amdECX, sig(6, 0x55, 4), false},
		{"zero/unknown vendor", 0, 0, 0, sig(6, 0x55, 4), false},
		// Partially-matching vendor strings must not pass.
		{"vendor EBX only", vendorIntelEBX, 0, 0, sig(6, 0x55, 4), false},
		{"vendor EDX/ECX only", 0, vendorIntelEDX, vendorIntelECX, sig(6, 0x55, 4), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.exp, jccErratumFromCPUID(tc.ebx, tc.edx, tc.ecx, tc.sig))
		})
	}
}

// TestJccErratumFromCPUID_realSignatures pins a handful of signatures exactly as
// /proc/cpuinfo and Intel's microcode tables spell them, rather than through the
// sig helper, so a bug in the helper cannot make the table above vacuous.
func TestJccErratumFromCPUID_realSignatures(t *testing.T) {
	for _, tc := range []struct {
		name string
		eax  uint32
		exp  bool
	}{
		{"Xeon D-2123IT (Skylake-D, 0x50654)", 0x00050654, true},
		{"Xeon Gold 6148 (Skylake-SP, 0x50657)", 0x00050657, true},
		{"i7-8565U (Whiskey Lake, 0x806EC)", 0x000806ec, true},
		{"i7-6700K (Skylake-S, 0x506E3)", 0x000506e3, true},
		{"i7-1065G7 (Ice Lake, 0x706E5)", 0x000706e5, false},
		{"i7-1185G7 (Tiger Lake, 0x806C1)", 0x000806c1, false},
		{"Xeon Platinum 8480+ (Sapphire Rapids, 0x806F8)", 0x000806f8, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.exp,
				jccErratumFromCPUID(vendorIntelEBX, vendorIntelEDX, vendorIntelECX, tc.eax))
		})
	}
}

// TestJCCErratumWorkaroundEnabled_archGate asserts the workaround can never turn
// itself on outside amd64, even if the flag bit (which the arm64 flag namespace
// reuses for its own numbering) happened to be set.
func TestJCCErratumWorkaroundEnabled_archGate(t *testing.T) {
	saved := CpuFeatures
	t.Cleanup(func() { CpuFeatures = saved })

	CpuFeatures = saved | CpuFeatureAmd64JCCErratum
	require.Equal(t, runtime.GOARCH == "amd64", JCCErratumWorkaroundEnabled())

	CpuFeatures = saved &^ CpuFeatureAmd64JCCErratum
	require.False(t, JCCErratumWorkaroundEnabled())
}
