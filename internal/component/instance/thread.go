package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file begins the thread.* execution runtime
// (docs/component-model-async-threads-design-fable.md, "Stage A"): a
// cooperative yield expressed entirely through the EXISTING taskBlocker
// machinery (stackfulTask.block / guestTask.block, task.go's taskBlocker
// interface) -- no new goroutine primitive yet (guestThread/the thread table
// are Stage D). Reference: canon_thread_yield (definitions.py ~2723-2727) ->
// Thread.yield_ (~411-412) -> wait_until(lambda: True, cancellable)
// (~402-409): a deliver-pending-cancel prologue, then an always-ready park.

// threadYieldHostFunc backs thread.yield (CanonKindThreadYield, 0x0c) --
// canon_thread_yield, definitions.py ~2723-2727. Core sig () -> i32
// (Cancelled: FALSE=0, TRUE=1, ~254-256).
//
// Binding any thread.* canon promotes the binding instance the same way a
// sync-lower-targeting-an-async-lift does (graph.go:1385-1387): a component
// that imports thread primitives declares itself a multi-fiber program whose
// callback tasks must be able to park mid-core-call (design §5.4) --
// cancellable's $C needs this to ever get a STARTED return to its caller.
// Also flags syncTaskNeeded so a plain sync lift that calls thread.yield
// still gets a syncImplicit task to resolve against (instance.go's
// invokeEntered), matching every other async builtin's bind-time flag
// (async_builtins.go).
func threadYieldHostFunc(in *Instance, canon binary.Canon) hostFuncDef {
	in.syncTaskNeeded, in.mayBlockSync = true, true
	cancellable := canon.Cancellable
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "thread.yield")
		t := in.activeTask
		var blk taskBlocker
		if t != nil {
			blk = t.blocker()
		}
		switch {
		case blk != nil:
			// Stackful implicit thread (sync-barges-in's $C.yielder), or a
			// promoted callback task's live segment: a genuine park with an
			// always-true ready predicate -- parks, then the driver's very
			// next step resumes it (behind any already-queued thunks/ready
			// tasks), exactly the reference's "yielded thread re-enters the
			// waiting list BEHIND already-ready threads".
			if blk.block(func() bool { return true }, cancellable) {
				stack[0] = 1 // Cancelled.TRUE
				return
			}
			stack[0] = 0

		case t != nil && !t.syncImplicit:
			// Unpromoted callback task's guest code, running on the driving
			// goroutine with no live segment to park (frame-free callback
			// loop, no suite in Stage A reaches this arm -- cancellable's
			// promotion gate (§5.4) means a bound thread.yield always gets a
			// promoted task; kept for parity with the reference, which never
			// distinguishes "callback" from "stackful" at Thread.yield_'s
			// level). One deterministic scheduler round, then the
			// pending-cancel check, mirroring wait_until's order as closely
			// as a frame-held caller allows.
			if t.deliverPendingCancel(cancellable) {
				stack[0] = 1
				return
			}
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0

		default:
			// Sync task (t == nil, or t.syncImplicit): the reference ALLOWS
			// a sync task to yield -- yield_ is a ready-waiting block, and
			// canon_lift's sync driver loop (~2202-2206) always finds a
			// candidate (the yielder itself, once re-armed) so there is
			// never a trap here, unlike thread.suspend/waitable-set.wait's
			// "cannot block a synchronous task" trap. wazy equivalent: one
			// scheduler round on the driving goroutine, then return NOT
			// cancelled (a sync task can never have a delivered
			// cancellation to observe). trap-if-block-and-sync's
			// yield-is-fine asserts 42, not a trap.
			if err := in.sched.pumpSnapshot(); err != nil {
				panic(fmt.Errorf("component/instance: thread.yield: %w", err))
			}
			stack[0] = 0
		}
	})
	return hostFuncDef{fn: fn, params: nil, results: []api.ValueType{api.ValueTypeI32}}
}
