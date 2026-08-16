package wasmedge

import (
	"errors"
	"net"
	"os"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasip1"
)

// wrapSyscallError wraps an errno the way net does, since that is how it
// arrives at the mapping.
func wrapSyscallError(e error) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: e}}
}

// The conditions that do not come from a platform errno, so they map the same
// way everywhere. The platform-specific arms are in the build-tagged files
// beside this one.
func TestToWasiErrno(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected wasip1.Errno
	}{
		{name: "nil is success", err: nil, expected: wasip1.ErrnoSuccess},
		{name: "closed is bad descriptor", err: net.ErrClosed, expected: wasip1.ErrnoBadf},
		{name: "wrapped closed", err: wrapSyscallError(net.ErrClosed), expected: wasip1.ErrnoBadf},
		{
			// The non-blocking emulation sets a deadline in the past, so a
			// timeout is EAGAIN rather than ETIMEDOUT.
			name: "timeout is EAGAIN", err: os.ErrDeadlineExceeded, expected: wasip1.ErrnoAgain,
		},
		{
			name: "dns failure", err: &net.DNSError{Err: "no such host", IsNotFound: true},
			expected: wasip1.ErrnoNoent,
		},
		{name: "anything else", err: errors.New("boom"), expected: wasip1.ErrnoIo},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, toWasiErrno(tc.err))
		})
	}
}
