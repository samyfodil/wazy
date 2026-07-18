package main

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/maintester"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Test_main ensures `go run .` prints the three component results.
func Test_main(t *testing.T) {
	stdout, _ := maintester.TestMain(t, main, "component")
	require.Equal(t, `component:adder/calc add(2, 3) = 5
wasi:cli hello: hello world
async run-async() = 42
`, stdout)
}
