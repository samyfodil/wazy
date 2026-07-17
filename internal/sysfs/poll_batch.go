//go:build linux || darwin

package sysfs

import "github.com/samyfodil/wazy/experimental/sys"

// PollReadiness polls files for POLLIN readiness with a single _poll syscall
// over all of them, so it returns as soon as ANY file is ready — the any-ready
// semantics poll_oneoff requires — instead of blocking on each file in turn.
//
// ready[i] reports whether files[i] became readable (nil if none did before the
// timeout). ok is false when any file cannot expose a raw fd (e.g. sockets); the
// caller must then fall back to sequential Poll. timeoutMillis matches _poll: a
// negative value blocks indefinitely.
func PollReadiness(files []sys.Pollable, timeoutMillis int32) (ready []bool, errno sys.Errno, ok bool) {
	fds := make([]pollFd, len(files))
	for i, f := range files {
		p, isFd := f.(fdPoller)
		if !isFd {
			return nil, 0, false
		}
		fd, hasFd := p.pollFd()
		if !hasFd {
			return nil, 0, false
		}
		fds[i] = newPollFd(fd, _POLLIN, 0)
	}
	n, errno := _poll(fds, timeoutMillis)
	if errno != 0 || n == 0 {
		return nil, errno, true
	}
	ready = make([]bool, len(files))
	for i := range fds {
		ready[i] = fds[i].revents&_POLLIN != 0
	}
	return ready, 0, true
}
