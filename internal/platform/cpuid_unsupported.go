//go:build !(amd64 || arm64) || tinygo

package platform

const CpuFeatures CpuFeatureFlags = 0
