package interpreter

import "github.com/samyfodil/wazy/api"

// HoldsBuffersForTest reports whether f keeps an operand stack between calls, for the tests in interpreter_test,
// which is where a real, callable api.Function can be built. See callEngine.buffers.
func HoldsBuffersForTest(f api.Function) bool {
	return cap(f.(*callEngine).stack) > 0
}

// BuffersReleasesBeforeKeepForTest is buffersReleasesBeforeKeep, for interpreter_test.
const BuffersReleasesBeforeKeepForTest = buffersReleasesBeforeKeep
