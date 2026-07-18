//go:build windows || linux || darwin

package sysfs

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/sys"
)

func Test_poll(t *testing.T) {
	t.Run("should return immediately with no fds and duration 0", func(t *testing.T) {
		for {
			n, err := _poll([]pollFd{}, 0)
			if err == sys.EINTR {
				t.Logf("Select interrupted")
				continue
			}
			require.EqualErrno(t, 0, err)
			require.Equal(t, 0, n)
			return
		}
	})

	t.Run("should wait for the given duration", func(t *testing.T) {
		dur := int32(250)
		var took time.Duration
		for {

			// On some platforms (e.g. Linux), the passed-in timeval is
			// updated by select(2). We are not accounting for this
			// in our implementation.
			start := time.Now()
			n, err := _poll([]pollFd{}, dur)
			took = time.Since(start)
			if err == sys.EINTR {
				t.Logf("Select interrupted after %v", took)
				continue
			}
			require.EqualErrno(t, 0, err)
			require.Equal(t, 0, n)

			// On some platforms the actual timeout might be arbitrarily
			// less than requested.
			if took < time.Duration(dur)*time.Millisecond {
				if runtime.GOOS == "linux" {
					// Linux promises to only return early if a file descriptor
					// becomes ready (not applicable here), or the call
					// is interrupted by a signal handler (explicitly retried in the loop above),
					// or the timeout expires.
					t.Errorf("Select: slept for %v, expected %v", took, dur)
				} else {
					t.Logf("Select: slept for %v, requested %v", took, dur)
				}
			}
			return
		}
	})

	t.Run("should return 1 if a given FD has data", func(t *testing.T) {
		rr, ww, err := os.Pipe()
		require.NoError(t, err)
		defer rr.Close()
		defer ww.Close()

		_, err = ww.Write([]byte("TEST"))
		require.NoError(t, err)

		for {
			fds := []pollFd{newPollFd(rr.Fd(), _POLLIN, 0)}
			if err == sys.EINTR {
				t.Log("Select interrupted")
				continue
			}
			n, err := _poll(fds, 0)
			require.EqualErrno(t, 0, err)
			require.Equal(t, 1, n)
			return
		}
	})
}

// TestPollReadiness proves the W3 any-ready fix: with one ready and one empty
// blocking fd, PollReadiness returns the ready one promptly rather than blocking
// on the empty fd for the full timeout (the bug a sequential loop had).
func TestPollReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("batched PollReadiness is linux/darwin only; Windows uses the sequential fallback")
	}
	ra, wa, err := os.Pipe()
	require.NoError(t, err)
	defer ra.Close()
	defer wa.Close()
	rb, wb, err := os.Pipe()
	require.NoError(t, err)
	defer rb.Close()
	defer wb.Close()

	_, err = wa.Write([]byte("data")) // pipe A ready; pipe B stays empty
	require.NoError(t, err)

	fileA := newOsFile("", sys.O_RDONLY, 0, ra)
	fileB := newOsFile("", sys.O_RDONLY, 0, rb)
	files := []sys.Pollable{fileA.(sys.Pollable), fileB.(sys.Pollable)}

	// A 10s timeout would hang if we blocked on the empty fd first.
	start := time.Now()
	ready, errno, ok := PollReadiness(files, 10_000)
	require.True(t, ok)
	require.EqualErrno(t, 0, errno)
	require.True(t, time.Since(start) < time.Second, "should return promptly, not block on empty fd")
	require.NotNil(t, ready)
	require.True(t, ready[0], "fd A had data")
	require.False(t, ready[1], "fd B was empty")
}
