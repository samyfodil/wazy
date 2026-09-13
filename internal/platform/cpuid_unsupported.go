//go:build !(amd64 || arm64 || riscv64) || tinygo

package platform

const CpuFeatures CpuFeatureFlags = 0
