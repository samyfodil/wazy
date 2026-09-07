package version

import (
	"sync"
	"testing"
)

// TestGetWazyVersion_Concurrent pins the property that makes wazy immune to
// wazero#2532: resolution never writes shared state, so concurrent runtime
// construction -- which calls this from several goroutines at once -- has
// nothing to race on. A reintroduced `if version == "" { ...; version = ret }`
// cache fails the "unchanged" assertion outright, not just under -race; the
// race detector alone would not catch it here, because in this package's own
// test binary the build info names no wazy dependency and the racy write is
// never reached.
func TestGetWazyVersion_Concurrent(t *testing.T) {
	before := version

	const goroutines = 8
	got := make([]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range got {
		go func() {
			defer wg.Done()
			got[i] = GetWazyVersion()
		}()
	}
	wg.Wait()

	if version != before {
		t.Fatalf("GetWazyVersion wrote the package-level version: %q -> %q", before, version)
	}
	for i, v := range got {
		if v == "" {
			t.Fatalf("goroutine %d got an empty version", i)
		}
		if v != got[0] {
			t.Fatalf("goroutine %d got %q, want %q", i, v, got[0])
		}
	}
}

// TestGetWazyVersion_Ldflag covers the other path: a version stamped at link
// time short-circuits build-info resolution.
func TestGetWazyVersion_Ldflag(t *testing.T) {
	before := version
	t.Cleanup(func() { version = before })

	version = "1.2.3-ldflag"
	if got := GetWazyVersion(); got != "1.2.3-ldflag" {
		t.Fatalf("got %q, want the linker-stamped version", got)
	}
}

// TestVersionMissing covers the values pkg.go and an unstamped build produce.
func TestVersionMissing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{{"", true}, {"(devel)", true}, {"v1.2.3", false}, {Default, false}} {
		if got := versionMissing(tc.in); got != tc.want {
			t.Fatalf("versionMissing(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
