//go:build !(amd64 || arm64) || tinygo

package platform

// CpuFeatures is a constant here, not a variable as on amd64 and arm64: ppc64's compiler turns the
// little-endian copy of a variable that the compilation cache key takes into a byte-reversed load from the
// variable's address, which it then cannot encode. The tests that fake a CPU are limited to the variable.
const CpuFeatures CpuFeatureFlags = 0
