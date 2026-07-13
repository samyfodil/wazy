package wazy

import (
	"context"

	"github.com/samyfodil/wazy/internal/expctxkeys"
)

// WithInterruptCheckInterval returns a context that overrides, for modules
// compiled under it, how often a loop checks for context cancellation when
// RuntimeConfig.WithCloseOnContextDone is enabled.
//
// The check is a Go round-trip that also serves as the scheduler/GC yield
// point for otherwise non-preemptible compiled loops, so performing it every
// iteration is expensive on tight loops. This option amortizes it:
//
//   - interval == 0: check on every iteration (lowest cancellation latency).
//   - interval == N (a power of two): check once every N loop iterations.
//     Cancellation is then observed within at most N iterations.
//
// interval must be 0 or a power of two; other values fail CompileModule. When
// this option is not set, wazy uses a non-zero default (currently 64), which
// makes WithCloseOnContextDone tight loops roughly an order of magnitude
// faster than checking every iteration.
//
// The interval is part of a module's compile identity: compiling the same
// binary under different intervals produces distinct cached variants, so a
// module can be re-lowered with a different interval by compiling it again
// under a context carrying the new value.
//
// Note: this only affects the optimizing compiler; it is a no-op for the
// interpreter.
func WithInterruptCheckInterval(ctx context.Context, interval uint64) context.Context {
	return context.WithValue(ctx, expctxkeys.InterruptCheckInterval{}, interval)
}
