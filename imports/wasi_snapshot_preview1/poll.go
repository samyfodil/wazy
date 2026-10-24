package wasi_snapshot_preview1

import (
	"context"
	"math"
	"time"

	"github.com/samyfodil/wazy/api"
	internalsys "github.com/samyfodil/wazy/internal/sys"
	"github.com/samyfodil/wazy/internal/sysfs"
	"github.com/samyfodil/wazy/internal/wasip1"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/sys"
)

// pollOneoff is the WASI function named PollOneoffName that concurrently
// polls for the occurrence of a set of events.
//
// # Parameters
//
//   - in: pointer to the subscriptions (48 bytes each)
//   - out: pointer to the resulting events (32 bytes each)
//   - nsubscriptions: count of subscriptions, zero returns sys.EINVAL.
//   - resultNevents: count of events.
//
// Result (Errno)
//
// The return value is 0 except the following error conditions:
//   - sys.EINVAL: the parameters are invalid
//   - sys.ENOTSUP: a parameters is valid, but not yet supported.
//   - sys.EFAULT: there is not enough memory to read the subscriptions or
//     write results.
//
// # Notes
//
//   - Since the `out` pointer nests Errno, the result is always 0.
//   - This is similar to `poll` in POSIX.
//
// See https://github.com/WebAssembly/WASI/blob/snapshot-01/phases/snapshot/docs.md#poll_oneoff
// See https://linux.die.net/man/3/poll
var pollOneoff = newHostFunc(
	wasip1.PollOneoffName, pollOneoffFn,
	[]wasm.ValueType{i32, i32, i32, i32},
	"in", "out", "nsubscriptions", "result.nevents",
)

type event struct {
	eventType byte
	userData  []byte
	errno     wasip1.Errno
}

func pollOneoffFn(_ context.Context, mod api.Module, params []uint64) sys.Errno {
	in := uint32(params[0])
	out := uint32(params[1])
	nsubscriptions := uint32(params[2])
	resultNevents := uint32(params[3])

	if nsubscriptions == 0 {
		return sys.EINVAL
	}

	mem := mod.Memory()

	// Ensure capacity prior to the read loop to reduce error handling.
	inBuf, ok := mem.Read(in, nsubscriptions*48)
	if !ok {
		return sys.EFAULT
	}
	outBuf, ok := mem.Read(out, nsubscriptions*32)
	// zero-out all buffer before writing
	clear(outBuf)

	if !ok {
		return sys.EFAULT
	}

	// Eagerly write the number of events which will equal subscriptions unless
	// there's a fault in parsing (not processing).
	if !mod.Memory().WriteUint32Le(resultNevents, nsubscriptions) {
		return sys.EFAULT
	}

	// Relative clock timeouts count from here.
	start := time.Now()

	// Extract FS context, used in the body of the for loop for FS access.
	fsc := mod.(*wasm.ModuleInstance).Sys.FS()
	// blockingPollSubs are fd read subscriptions processed after the loop via polling.
	var blockingPollSubs []struct {
		evt  event
		file sys.File
	}
	// clocks are the clock subscriptions. None is reported up front: a clock
	// event means its timeout expired, so each is written only once it has.
	var clocks []clockSub
	// The timeout is initialized at max Duration, the loop will find the minimum.
	const noTimeout = time.Duration(1<<63 - 1)
	timeout := noTimeout
	// Count of all the subscriptions that have been already written back to outBuf.
	// nevents*32 returns at all times the offset where the next event should be written:
	// this way we ensure that there are no gaps between records.
	nevents := uint32(0)

	// Layout is subscription_u: Union
	// https://github.com/WebAssembly/WASI/blob/snapshot-01/phases/snapshot/docs.md#subscription_u
	for i := uint32(0); i < nsubscriptions; i++ {
		inOffset := i * 48
		outOffset := nevents * 32

		eventType := inBuf[inOffset+8] // +8 past userdata
		// +8 past userdata +8 contents_offset
		argBuf := inBuf[inOffset+8+8:]
		userData := inBuf[inOffset : inOffset+8]

		evt := event{
			eventType: eventType,
			userData:  userData,
			errno:     wasip1.ErrnoSuccess,
		}

		switch eventType {
		case wasip1.EventTypeClock: // handle later
			newTimeout, err := processClockEvent(argBuf)
			if err != 0 {
				return err
			}
			clocks = append(clocks, clockSub{evt, newTimeout})
			// Min timeout.
			if newTimeout < timeout {
				timeout = newTimeout
			}
		case wasip1.EventTypeFdRead:
			fd := int32(le.Uint32(argBuf))
			if fd < 0 {
				return sys.EBADF
			}
			if file, ok := fsc.LookupFile(fd); !ok {
				evt.errno = wasip1.ErrnoBadf
				writeEvent(outBuf[outOffset:], evt)
				nevents++
			} else if fd != internalsys.FdStdin && isNonblock(file.File) {
				writeEvent(outBuf[outOffset:], evt)
				nevents++
			} else {
				// The fd is in blocking mode; do not ack yet, append
				// to a slice for deferred polling evaluation.
				blockingPollSubs = append(blockingPollSubs, struct {
					evt  event
					file sys.File
				}{evt, file.File})
			}
		case wasip1.EventTypeFdWrite:
			fd := int32(le.Uint32(argBuf))
			if fd < 0 {
				return sys.EBADF
			}
			if _, ok := fsc.LookupFile(fd); ok {
				evt.errno = wasip1.ErrnoNotsup
			} else {
				evt.errno = wasip1.ErrnoBadf
			}
			nevents++
			writeEvent(outBuf[outOffset:], evt)
		default:
			return sys.EINVAL
		}
	}
	hasClock := len(clocks) > 0

	switch {
	case len(blockingPollSubs) > 0:
		// Wait for the timeout to expire, or for data to become available on
		// any blocking fd subscriber. An event already written means there is
		// something to report, so the fds are then only checked, not waited on.
		var errno sys.Errno
		if nevents, errno = pollBlocking(blockingPollSubs, outBuf, nevents, timeout-time.Since(start), hasClock); errno != 0 {
			return errno
		}
	case nevents == 0:
		// Only clocks: sleep until the earliest expires. Nothing else can
		// end the wait, so that one has expired when it does.
		if timeout > 0 {
			mod.(*wasm.ModuleInstance).Sys.Nanosleep(int64(timeout))
		}
	}

	// Report the clocks that have expired. When nothing else is reported the
	// call ended because the earliest clock did: the wait above is measured in
	// whole milliseconds, or by a sleeper the embedder configured, so the
	// elapsed time alone cannot be trusted to have reached it.
	expired := time.Since(start)
	if nevents == 0 && expired < timeout {
		expired = timeout
	}
	for _, c := range clocks {
		if c.timeout <= expired {
			writeEvent(outBuf[nevents*32:], c.evt)
			nevents++
		}
	}

	if nevents != nsubscriptions {
		if !mod.Memory().WriteUint32Le(resultNevents, nevents) {
			return sys.EFAULT
		}
	}
	return 0
}

// clockSub is a clock subscription, and the relative timeout after which its
// event is due.
type clockSub struct {
	evt     event
	timeout time.Duration
}

// pollBlocking polls the blocking fd subscriptions for readiness, waiting up
// to timeout (forever without a clock) unless nevents says an event is
// already written. It writes an event for each ready fd, and for each that
// cannot be polled, after the nevents already in outBuf, and returns the new
// count.
func pollBlocking(subs []struct {
	evt  event
	file sys.File
}, outBuf []byte, nevents uint32, timeout time.Duration, hasClock bool,
) (uint32, sys.Errno) {
	// pollSubs are the subs that can be polled, in the same order as pollables.
	pollables := make([]sys.Pollable, 0, len(subs))
	pollSubs := subs[:0:0]
	for _, sub := range subs {
		p, ok := sub.file.(sys.Pollable)
		if !ok {
			// Not pollable: report it as unsupported, which also means there
			// is now an event to return, so nothing below waits.
			sub.evt.errno = wasip1.ErrnoNotsup
			writeEvent(outBuf[nevents*32:], sub.evt)
			nevents++
			continue
		}
		pollables = append(pollables, p)
		pollSubs = append(pollSubs, sub)
	}
	if len(pollables) == 0 {
		return nevents, 0
	}

	report := func(i int, errno sys.Errno) {
		sub := pollSubs[i]
		sub.evt.errno = 0
		if errno != 0 {
			sub.evt.errno = wasip1.ErrnoNotsup
		}
		writeEvent(outBuf[nevents*32:], sub.evt)
		nevents++
	}

	// With more than one fd, a single batched poll honors poll_oneoff's
	// any-ready semantics: it returns as soon as ANY fd is ready, where
	// waiting on each fd in turn would block on the first for the whole
	// timeout while a later one is ready.
	if len(pollables) > 1 {
		if ready, errno, batched := sysfs.PollReadiness(pollables, pollMillis(timeout, hasClock, nevents > 0)); batched {
			switch errno {
			case 0:
				for i := range pollables {
					if ready != nil && ready[i] {
						report(i, 0)
					}
				}
			case sys.ENOSYS, sys.ENOTSUP:
				for i := range pollables {
					report(i, errno)
				}
			default:
				return nevents, errno
			}
			return nevents, 0
		}
	}

	// One fd, or several that cannot be polled together: wait on a single fd
	// directly, and on several by checking them all without blocking until
	// one is ready or the timeout expires, so no fd is waited on while
	// another is ready.
	if len(pollables) == 1 {
		ready, errno := pollables[0].Poll(sys.POLLIN, pollMillis(timeout, hasClock, nevents > 0))
		switch errno {
		case 0:
			if ready {
				report(0, 0)
			}
		case sys.ENOSYS, sys.ENOTSUP:
			report(0, errno)
		default:
			return nevents, errno
		}
		return nevents, 0
	}

	deadline := time.Now().Add(timeout)
	wait := minPollInterval
	for {
		before := nevents
		for i, p := range pollables {
			ready, errno := p.Poll(sys.POLLIN, 0)
			switch errno {
			case 0:
				if ready {
					report(i, 0)
				}
			case sys.ENOSYS, sys.ENOTSUP:
				report(i, errno)
			default:
				return nevents, errno
			}
		}
		if nevents > before || before > 0 {
			return nevents, 0
		}
		remaining := time.Until(deadline)
		if hasClock && remaining <= 0 {
			return nevents, 0
		}
		sleep := wait
		if hasClock && remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
		if wait < maxPollInterval {
			wait *= 2
		}
	}
}

// minPollInterval and maxPollInterval bound how long pollBlocking waits
// between checks of fds it cannot poll together. The wait starts short, so
// an fd that becomes ready soon is noticed soon, and doubles up to the cap,
// so a long wait does not spin.
const (
	minPollInterval = time.Millisecond
	maxPollInterval = 50 * time.Millisecond
)

// pollMillis is the timeout to poll the blocking fds with. An event already
// written (a bad fd, a ready one) means poll_oneoff has something to report,
// so the fds are only checked, never waited on. Without a clock the wait is
// unbounded, which a negative timeout means to every Pollable. The timeout is
// rounded up, so the poll never ends before the earliest clock is due.
func pollMillis(d time.Duration, hasClock, haveEvents bool) int32 {
	switch {
	case haveEvents:
		return 0
	case !hasClock:
		return -1
	case d <= 0:
		return 0
	case d >= math.MaxInt32*time.Millisecond:
		return math.MaxInt32
	}
	return int32((d + time.Millisecond - 1) / time.Millisecond)
}

// processClockEvent supports only relative name events, as that's what's used
// to implement sleep in various compilers including Rust, Zig and TinyGo.
func processClockEvent(inBuf []byte) (time.Duration, sys.Errno) {
	_ /* ID */ = le.Uint32(inBuf[0:8])          // See below
	timeout := le.Uint64(inBuf[8:16])           // nanos if relative
	_ /* precision */ = le.Uint64(inBuf[16:24]) // Unused
	flags := le.Uint16(inBuf[24:32])

	var err sys.Errno
	// subclockflags has only one flag defined:  subscription_clock_abstime
	switch flags {
	case 0: // relative time
	case 1: // subscription_clock_abstime
		err = sys.ENOTSUP
	default: // subclockflags has only one flag defined.
		err = sys.EINVAL
	}

	if err != 0 {
		return 0, err
	} else {
		// https://linux.die.net/man/3/clock_settime says relative timers are
		// unaffected. Since this function only supports relative timeout, we can
		// skip name ID validation and use a single sleep function.

		return time.Duration(timeout), 0
	}
}

// isNonblock returns true if the file implements PollableFile and is in
// non-blocking mode.
func isNonblock(f sys.File) bool {
	if pf, ok := f.(sys.PollableFile); ok {
		return pf.IsNonblock()
	}
	return false
}

// writeEvent writes the event corresponding to the processed subscription.
// evt is passed by value (~40 bytes; userData slices guest memory) so callers
// never heap-allocate an *event per subscription (W8).
// https://github.com/WebAssembly/WASI/blob/snapshot-01/phases/snapshot/docs.md#-event-struct
func writeEvent(outBuf []byte, evt event) {
	copy(outBuf, evt.userData)  // userdata
	outBuf[8] = byte(evt.errno) // uint16, but safe as < 255
	outBuf[9] = 0
	le.PutUint32(outBuf[10:], uint32(evt.eventType))
	// TODO: When FD events are supported, write outOffset+16
}
