package main

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/maintester"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// Test_main ensures `go run .` drives the guest through every shape the
// custom interface declares: the resource constructor (which prints the rep
// the host assigned), a value round-tripped through put/get, an option::none
// for a missing key, and the ok arm of the result<list<string>, error>.
func Test_main(t *testing.T) {
	stdout, _ := maintester.TestMain(t, main, "custom-wit")
	require.Equal(t, `host: created bucket "cache" (rep 1)
guest: hello wazy|missing=true|keys=greeting,target
`, stdout)
}
