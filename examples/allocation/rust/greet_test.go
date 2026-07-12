package main

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/maintester"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Test_main ensures the following will work:
//
//	go run greet.go wazy
func Test_main(t *testing.T) {
	stdout, _ := maintester.TestMain(t, main, "greet", "wazy")
	require.Equal(t, `wasm >> Hello, wazy!
go >> Hello, wazy!
`, stdout)
}
