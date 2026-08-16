//go:build !(plan9 || aix || windows)

package wasmedge

import (
	"errors"
	"syscall"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
)

// The errno mapping is what a guest branches on -- Rust's std turns
// ECONNREFUSED into a distinct error kind -- so each arm is checked directly
// rather than through whichever conditions a test happens to provoke.
func TestSyscallToWasiErrno(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected wasip1.Errno
	}{
		{name: "connection refused", err: syscall.ECONNREFUSED, expected: wasip1.ErrnoConnrefused},
		{name: "connection reset", err: syscall.ECONNRESET, expected: wasip1.ErrnoConnreset},
		{name: "connection aborted", err: syscall.ECONNABORTED, expected: wasip1.ErrnoConnaborted},
		{name: "address in use", err: syscall.EADDRINUSE, expected: wasip1.ErrnoAddrinuse},
		{name: "address not available", err: syscall.EADDRNOTAVAIL, expected: wasip1.ErrnoAddrnotavail},
		{name: "host unreachable", err: syscall.EHOSTUNREACH, expected: wasip1.ErrnoHostunreach},
		{name: "network unreachable", err: syscall.ENETUNREACH, expected: wasip1.ErrnoNetunreach},
		{name: "broken pipe", err: syscall.EPIPE, expected: wasip1.ErrnoPipe},
		{name: "permission denied", err: syscall.EACCES, expected: wasip1.ErrnoAcces},
		{name: "would block", err: syscall.EAGAIN, expected: wasip1.ErrnoAgain},
		{name: "invalid", err: syscall.EINVAL, expected: wasip1.ErrnoInval},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Both bare and wrapped the way net delivers it.
			errno, ok := syscallToWasiErrno(tc.err)
			require.True(t, ok)
			require.Equal(t, tc.expected, errno)
			require.Equal(t, tc.expected, toWasiErrno(wrapSyscallError(tc.err)))
		})
	}

	t.Run("unmapped errno", func(t *testing.T) {
		// An errno with no mapping of its own is not silently turned into
		// something specific.
		_, ok := syscallToWasiErrno(syscall.ENOTTY)
		require.False(t, ok)
		require.Equal(t, wasip1.ErrnoIo, toWasiErrno(wrapSyscallError(syscall.ENOTTY)))
	})

	t.Run("not a syscall error", func(t *testing.T) {
		_, ok := syscallToWasiErrno(errors.New("boom"))
		require.False(t, ok)
	})
}
