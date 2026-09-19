package platform

import "runtime"

// Intel erratum SKX102, commonly called the "JCC erratum".
//
// The uop cache (DSB) of a Skylake-family core is organised in 32-byte windows
// of instruction bytes. A window that contains a jump instruction whose bytes
// cross a 32-byte boundary, or end exactly on one, is silently not cached, so
// that code is fed by the legacy decoder instead. Results stay correct, but the
// affected loop loses most of its DSB delivery; on wazy's own compiled code the
// spread between a lucky and an unlucky placement has been measured at up to
// 32% of wall clock on a single workload.
//
// The mitigation (what LLVM's -mbranches-within-32B-boundaries does) is to pad
// with NOPs so that no jump crosses or ends on a 32-byte boundary. That costs
// code size and extra instructions, so it must only be paid on a core that is
// actually affected -- which is a property of the CPU model, not of GOARCH.
//
// The affected CPUID signatures are Intel's own, from "Mitigations for Jump
// Conditional Code Erratum" (Intel, November 2019): family 6, models 0x4E, 0x5E
// (Skylake client), 0x55 (Skylake-SP/X/W/D, Cascade Lake, Cooper Lake), 0x8E,
// 0x9E (Kaby Lake, Coffee Lake, Amber Lake, Whiskey Lake, Comet Lake mobile),
// 0xA5, 0xA6 (Comet Lake S/H/U). Ice Lake (0x7E, 0x6A, 0x6C) and everything
// after it, and every non-Intel part, are unaffected.

// The vendor string "GenuineIntel" as CPUID leaf 0 returns it: EBX="Genu",
// EDX="ineI", ECX="ntel", each little-endian.
const (
	vendorIntelEBX = 0x756e6547
	vendorIntelEDX = 0x49656e69
	vendorIntelECX = 0x6c65746e
)

// jccErratumFromCPUID reports whether the CPU described by the given CPUID
// results is subject to erratum SKX102: ebx/edx/ecx are the vendor-string
// registers from leaf 0, and sig is EAX from leaf 1 (the version information:
// extended family, extended model, family, model, stepping).
//
// It is kept free of any CPUID instruction so that it can be unit-tested with
// signatures of machines the test is not running on -- notably the negative
// cases, which are the ones that decide whether an unaffected core pays for a
// workaround it does not need.
func jccErratumFromCPUID(ebx, edx, ecx, sig uint32) bool {
	if ebx != vendorIntelEBX || edx != vendorIntelEDX || ecx != vendorIntelECX {
		return false
	}
	// Intel SDM Vol.2A, CPUID, "Figure 3-6. Version Information Returned by
	// CPUID in EAX": DisplayFamily is the base family plus the extended family
	// when the base family is 0xF, and DisplayModel takes the extended model as
	// its high nibble when the base family is 0x6 or 0xF.
	family := (sig >> 8) & 0xf
	model := (sig >> 4) & 0xf
	if family == 0xf {
		family += (sig >> 20) & 0xff
	}
	if family == 0x6 || family == 0xf {
		model += ((sig >> 16) & 0xf) << 4
	}
	if family != 0x6 {
		return false
	}
	switch model {
	case 0x4e, // Skylake-U/Y (client mobile)
		0x5e, // Skylake-S/H (client desktop)
		0x55, // Skylake-SP/X/W/D, Cascade Lake, Cooper Lake
		0x8e, // Kaby/Coffee/Amber/Whiskey/Comet Lake (mobile)
		0x9e, // Kaby/Coffee Lake (desktop, mobile H/S)
		0xa5, // Comet Lake S/H
		0xa6: // Comet Lake U62
		return true
	}
	return false
}

// JCCErratumWorkaroundEnabled reports whether compiled code must be laid out to
// avoid Intel erratum SKX102 (see above). It is the single source of truth for
// that decision: the amd64 encoder consults it to decide whether to pad before
// a branch, and the engine consults it to decide whether to align compiled
// functions to 32 bytes instead of 16 (which is what makes a buffer offset and
// the final address agree modulo 32 in the first place).
//
// The decision changes the machine code that is generated, so it is also part
// of the compilation cache key -- it rides CpuFeatures, which fileCacheKey
// already hashes, so a module compiled on a Skylake host is never loaded back
// on an Ice Lake one (or the reverse) out of a shared cache directory.
//
// The GOARCH test makes the arm64 (and every other) build a compile-time no-op:
// arm64 has fixed-width 32-bit instructions and no such erratum, and the amd64
// backend package is still compiled there for its cross-architecture tests.
func JCCErratumWorkaroundEnabled() bool {
	return runtime.GOARCH == "amd64" && CpuFeatures.Has(CpuFeatureAmd64JCCErratum)
}
