package version

import (
	"sync"
	"testing"
)

// TestGetWazyVersion_Concurrent guards the memoization against the data race
// that a plain `if version == "" { ... version = ret }` had: concurrent runtime
// construction calls this from several goroutines at once. Only meaningful
// under -race.
func TestGetWazyVersion_Concurrent(t *testing.T) {
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

	for i, v := range got {
		if v == "" {
			t.Fatalf("goroutine %d got an empty version", i)
		}
		if v != got[0] {
			t.Fatalf("goroutine %d got %q, want %q", i, v, got[0])
		}
	}
}
