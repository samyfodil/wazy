package version

import (
	"runtime/debug"
	"strings"
)

// Default is the default version value used when none was found.
const Default = "dev"

// version holds the current version from the go.mod of downstream users or set by ldflag for wazy CLI.
var version string

// GetWazyVersion returns the current version of wazy either in the go.mod or set by ldflag for wazy CLI.
//
// If this is not CLI, this assumes that downstream users of wazy imports wazy as "github.com/samyfodil/wazy".
// To be precise, the returned string matches the require statement there.
// For example, if the go.mod has "require github.com/samyfodil/wazy 0.1.2-12314124-abcd",
// then this returns "0.1.2-12314124-abcd".
//
// Note: this is tested in ./testdata/main_test.go with a separate go.mod to pretend as the wazy user.
func GetWazyVersion() (ret string) {
	if len(version) != 0 {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, dep := range info.Deps {
			// Note: here's the assumption that wazy is imported as github.com/samyfodil/wazy.
			if strings.Contains(dep.Path, "github.com/samyfodil/wazy") {
				ret = dep.Version
			}
		}

		// In wazy CLI, wazy is a main module, so we have to get the version info from info.Main.
		if versionMissing(ret) {
			ret = info.Main.Version
		}
	}
	if versionMissing(ret) {
		return Default // don't return parens
	}

	// Cache for the subsequent calls.
	version = ret
	return ret
}

func versionMissing(ret string) bool {
	return ret == "" || ret == "(devel)" // pkg.go defaults to (devel)
}
