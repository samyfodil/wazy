package nativeapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestPreemptWordLayout pins GStackguard0Offset and StackPreempt against the
// runtime sources of the toolchain building the test: g starts with
// stack (two words) and then stackguard0, and stackPreempt is -1314.
func TestPreemptWordLayout(t *testing.T) {
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skip("no go command:", err) // a scratch image ships the test binary alone
	}
	src := filepath.Join(strings.TrimSpace(string(goroot)), "src", "runtime")
	runtime2, err := os.ReadFile(filepath.Join(src, "runtime2.go"))
	if err != nil {
		t.Skip("no runtime sources:", err) // a scratch image ships the test binary alone
	}
	stack, err := os.ReadFile(filepath.Join(src, "stack.go"))
	require.NoError(t, err)

	require.True(t, regexp.MustCompile(`type g struct \{\n(?:\s*//[^\n]*\n)*\s*stack\s+stack\b[^\n]*\n\s*stackguard0\s+uintptr`).Match(runtime2),
		"runtime.g no longer starts with stack then stackguard0")
	require.True(t, regexp.MustCompile(`type stack struct \{\n\s*lo\s+uintptr\n\s*hi\s+uintptr\n\}`).Match(runtime2),
		"runtime.stack is no longer two words")
	require.Equal(t, uint64(2*8), uint64(GStackguard0Offset))

	require.True(t, regexp.MustCompile(`stackPreempt\s*=\s*uintptrMask & -1314\b`).Match(stack),
		"runtime.stackPreempt is no longer uintptrMask & -1314")
	m := int64(-1314)
	require.Equal(t, uint64(m), StackPreempt)
}
