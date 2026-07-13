package wazy

import (
	"context"
	"errors"

	"github.com/samyfodil/wazy/api"
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

// interruptCheckIntervalSetter is implemented by api.Function values whose
// engine supports retuning the loop interrupt-check interval at runtime. The
// optimizing (native) engine implements it; the interpreter does not.
type interruptCheckIntervalSetter interface {
	SetInterruptCheckInterval(interval uint64) error
}

// SetInterruptCheckInterval retunes, without recompiling, how often fn's loops
// perform the cancellation/GC-yield check — the runtime counterpart of the
// compile-time WithInterruptCheckInterval. It writes a per-callEngine mask, so
// the same binary can run different functions (or the same function at different
// times) at different check frequencies. interval must be 0 (check every
// iteration) or a power of two.
//
// A larger interval lowers per-iteration overhead on a loop you know is hot and
// bounded; the change takes effect the next time one of fn's loops is entered.
// It is safe to call from another goroutine while fn runs — the mask affects
// only how often the check fires, never correctness.
//
// NOTE — be aware of these before raising an interval:
//
//   - It only works when fn's module was compiled with
//     RuntimeConfig.WithCloseOnContextDone AND a non-zero
//     WithInterruptCheckInterval (the default interval is non-zero). That is the
//     only configuration in which any interrupt-check code is emitted; otherwise
//     there is nothing to tune and this returns an error (as does the
//     interpreter engine, which has no support).
//   - The check is the ONLY scheduler/GC yield and cancellation point in an
//     otherwise non-preemptible compiled loop. Raising the interval means fn will
//     not observe context cancellation — and will not yield to Go's GC
//     stop-the-world — for up to `interval` iterations. Only raise it for a loop
//     you are confident is bounded/short; a runaway loop at a large interval can
//     hang uninterruptibly and stall GC.
//   - It does not reach the speed of a module compiled without
//     WithCloseOnContextDone: the per-iteration counter bookkeeping remains, only
//     the (expensive) Go round-trip is made less frequent. For a function that
//     never needs cancellation, compiling it under a runtime without
//     WithCloseOnContextDone is faster still.
//   - The mask lives on the api.Function handle (its callEngine). Set it on the
//     same handle you call; a fresh ExportedFunction lookup re-seeds the compiled
//     default.
//
// Like WithInterruptCheckInterval, this affects only the optimizing compiler.
func SetInterruptCheckInterval(fn api.Function, interval uint64) error {
	s, ok := fn.(interruptCheckIntervalSetter)
	if !ok {
		return errors.New("the runtime engine does not support runtime interrupt-check interval retuning")
	}
	return s.SetInterruptCheckInterval(interval)
}
