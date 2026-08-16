package wasmedge

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
)

// Winsock reports its own errnos rather than the POSIX ones of the same
// meaning, so a guest on Windows would see EIO for every condition it wants to
// tell apart unless these are mapped. A refused connection arrives as
// WSAECONNREFUSED (10061), not ECONNREFUSED (107).
func TestSyscallToWasiErrno(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected wasip1.Errno
	}{
		{name: "connection refused", err: windows.WSAECONNREFUSED, expected: wasip1.ErrnoConnrefused},
		{name: "connection reset", err: windows.WSAECONNRESET, expected: wasip1.ErrnoConnreset},
		{name: "connection aborted", err: windows.WSAECONNABORTED, expected: wasip1.ErrnoConnaborted},
		{name: "address in use", err: windows.WSAEADDRINUSE, expected: wasip1.ErrnoAddrinuse},
		{name: "address not available", err: windows.WSAEADDRNOTAVAIL, expected: wasip1.ErrnoAddrnotavail},
		{name: "host unreachable", err: windows.WSAEHOSTUNREACH, expected: wasip1.ErrnoHostunreach},
		{name: "network unreachable", err: windows.WSAENETUNREACH, expected: wasip1.ErrnoNetunreach},
		{name: "shutdown is a broken pipe", err: windows.WSAESHUTDOWN, expected: wasip1.ErrnoPipe},
		{name: "permission denied", err: windows.WSAEACCES, expected: wasip1.ErrnoAcces},
		{name: "would block", err: windows.WSAEWOULDBLOCK, expected: wasip1.ErrnoAgain},
		{name: "invalid", err: windows.WSAEINVAL, expected: wasip1.ErrnoInval},
		{name: "broken pipe", err: syscall.EPIPE, expected: wasip1.ErrnoPipe},
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
		_, ok := syscallToWasiErrno(windows.WSAEPROTOTYPE)
		require.False(t, ok)
		require.Equal(t, wasip1.ErrnoIo, toWasiErrno(wrapSyscallError(windows.WSAEPROTOTYPE)))
	})

	t.Run("not a syscall error", func(t *testing.T) {
		_, ok := syscallToWasiErrno(errors.New("boom"))
		require.False(t, ok)
	})
}
