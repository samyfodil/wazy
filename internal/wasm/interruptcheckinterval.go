package wasm

import (
	"context"
	"fmt"
	"math/bits"

	"github.com/samyfodil/wazy/internal/expctxkeys"
)

// DefaultInterruptCheckInterval is the interrupt-check interval used when a
// module is compiled under WithCloseOnContextDone and the context does not
// carry an explicit wazy.WithInterruptCheckInterval override.
//
// wazy defaults to a non-zero value: the per-loop-iteration module-exit-code
// check is a full Go round-trip (also the scheduler/GC yield point), so
// checking every iteration is expensive on tight loops. 64 amortizes that
// ~64x while keeping cancellation latency and GC-yield spacing to at most 64
// (cheap) iterations.
const DefaultInterruptCheckInterval uint64 = 64

// InterruptCheckIntervalFromContext returns the per-compile interrupt-check
// interval carried by ctx (set via wazy.WithInterruptCheckInterval),
// or DefaultInterruptCheckInterval when unset. The value is folded into the
// module ID (see AssignModuleID) so distinct intervals produce distinct
// compiled variants.
func InterruptCheckIntervalFromContext(ctx context.Context) uint64 {
	if v, ok := ctx.Value(expctxkeys.InterruptCheckInterval{}).(uint64); ok {
		return v
	}
	return DefaultInterruptCheckInterval
}

// ValidateInterruptCheckInterval reports whether interval is a legal
// interrupt-check interval: 0 (check every iteration) or a power of two.
func ValidateInterruptCheckInterval(interval uint64) error {
	if interval == 0 || bits.OnesCount64(interval) == 1 {
		return nil
	}
	return fmt.Errorf("interrupt check interval must be 0 or a power of two, got %d", interval)
}
