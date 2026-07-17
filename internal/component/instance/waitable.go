package instance

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file transliterates the Canonical ABI's Waitable / WaitableSet /
// Subtask (testdata/definitions.py ~745-900) for wazy's single-active-task
// callback-lift MVP (docs/component-model-async-runtime-design.md §1.4).
//
// The reference's green-thread machinery collapses here exactly as it does in
// task.go/sched.go: with one task ever active, "block this thread until a
// waitable has an event" is not a park-and-reschedule -- it is the WAIT
// driver pumping `sched` until an event is pending (see invokeAsyncCallback's
// callbackWait arm, Phase 1c). So Waitable.has_sync_waiter, wait_for_pending
// _event, and WaitableSet.num_waiting / wait_for_event* have no analog here;
// their role is played by sched.drive(pred). Cancellation fields (Phase 3)
// are likewise omitted until CallAsync makes them reachable.

// eventCode is the reference EventCode (definitions.py ~745): the discriminant
// a callback receives as its first argument. Only NONE (yield resume) and
// SUBTASK (an async-lowered call resolved) are reachable in this milestone;
// the stream/future/cancel codes are numbered to match for Phase 2/3.
type eventCode uint32

const (
	eventNone          eventCode = 0
	eventSubtask       eventCode = 1
	eventStreamRead    eventCode = 2
	eventStreamWrite   eventCode = 3
	eventFutureRead    eventCode = 4
	eventFutureWrite   eventCode = 5
	eventTaskCancelled eventCode = 6
)

// eventTuple is the reference EventTuple: (code, p1, p2), delivered to a
// callback as its three i32 arguments. For a SUBTASK event, p1 is the
// subtask's table index and p2 is its (packed) state.
type eventTuple struct {
	code   eventCode
	p1, p2 uint32
}

// waitable is the reference Waitable base (definitions.py ~756), embedded by
// every table entry that can carry a pending event (subtask now;
// stream/future ends in Phase 2). A waitable is a member of at most one
// waitableSet at a time (wset), and holds at most one deferred event
// (pendingEvent, a thunk so the payload -- e.g. the live subtask state -- is
// read at delivery, not at arming).
type waitable struct {
	pendingEvent func() eventTuple
	wset         *waitableSet
}

// waitableEntry is a table entry that embeds a waitable (subtask now;
// stream/future ends in Phase 2). It lets the waitable.join / waitable-set.*
// builtins recover the embedded *waitable from a table entry of unknown kind,
// mirroring the reference's isinstance(x, Waitable) check.
type waitableEntry interface {
	tableEntry
	waitablePtr() *waitable
}

// setPendingEvent mirrors Waitable.set_pending_event.
func (w *waitable) setPendingEvent(ev func() eventTuple) { w.pendingEvent = ev }

// hasPendingEvent mirrors Waitable.has_pending_event.
func (w *waitable) hasPendingEvent() bool { return w.pendingEvent != nil }

// getPendingEvent mirrors Waitable.get_pending_event: take and clear the
// deferred event, evaluating the thunk now.
func (w *waitable) getPendingEvent() eventTuple {
	ev := w.pendingEvent
	w.pendingEvent = nil
	return ev()
}

// join mirrors Waitable.join: move this waitable into wset (or out of any set
// when wset is nil), keeping both sides' membership consistent.
func (w *waitable) join(wset *waitableSet) {
	if w.wset != nil {
		w.wset.remove(w)
	}
	w.wset = wset
	if wset != nil {
		wset.elems = append(wset.elems, w)
	}
}

// dropWaitable mirrors Waitable.drop: only legal with no undelivered event and
// no set membership. Callers that can violate this (subtask.drop) check their
// own trap_if first; this asserts the invariant the reference asserts.
func (w *waitable) dropWaitable() {
	if w.hasPendingEvent() {
		panic("BUG: dropping a waitable with a pending event")
	}
	w.join(nil)
}

// waitableSet is the reference WaitableSet (definitions.py ~799): a bag of
// waitables a task can wait on as a unit (canon waitable-set.new). It lives in
// the handle table (entryWaitableSet).
type waitableSet struct {
	elems []*waitable

	// numWaiting mirrors WaitableSet.num_waiting: how many drivers are
	// currently blocked in wait_for_event_and against this set. Bracketed
	// (++/--) around every sched.drive call that can observe this set --
	// invokeAsyncCallback's callbackWait arm and waitable-set.wait
	// (async_builtins.go) -- so dropSet's trap_if(num_waiting > 0) is a real
	// check, not always-zero bookkeeping: with one task it can only be
	// nonzero during a NESTED drive (the callback loop's own WAIT is itself
	// pumping the scheduler when a queued thunk turns around and drops the
	// very set it's blocked on), which is a real, if unusual, program.
	numWaiting int
}

func (*waitableSet) entryKind() entryKind { return entryWaitableSet }

func (s *waitableSet) remove(w *waitable) {
	for i, e := range s.elems {
		if e == w {
			s.elems = append(s.elems[:i], s.elems[i+1:]...)
			return
		}
	}
}

// hasPendingEvent mirrors WaitableSet.has_pending_event.
func (s *waitableSet) hasPendingEvent() bool {
	for _, w := range s.elems {
		if w.hasPendingEvent() {
			return true
		}
	}
	return false
}

// getPendingEvent mirrors WaitableSet.get_pending_event. The reference
// random.shuffle(elems)es first; the conformance oracle monkeypatches shuffle
// to a no-op for determinism matching wazy's FIFO scheduler (plan §6), so this
// returns the first member (in insertion order) that has an event.
func (s *waitableSet) getPendingEvent() eventTuple {
	for _, w := range s.elems {
		if w.hasPendingEvent() {
			return w.getPendingEvent()
		}
	}
	panic("BUG: getPendingEvent with no pending event")
}

// poll mirrors WaitableSet.poll (the non-blocking canon waitable-set.poll):
// NONE when nothing is ready, otherwise the next event. (deliver_pending_cancel
// is Phase 3; no task can be cancelled in this milestone.)
func (s *waitableSet) poll() eventTuple {
	if !s.hasPendingEvent() {
		return eventTuple{code: eventNone}
	}
	return s.getPendingEvent()
}

// dropSet mirrors WaitableSet.drop: trap if anything still joined, or if a
// driver is currently blocked waiting on this set (see numWaiting's doc --
// unreachable while dropSet itself runs on the one driving goroutine calling
// in, since nothing can be BOTH inside sched.drive(this set) AND calling
// waitable-set.drop(this set) at once, but kept for oracle parity, exactly
// like the reference keeps the check even though its own single-process
// deterministic profile rarely exercises it).
func (s *waitableSet) dropSet() error {
	if len(s.elems) > 0 {
		return fmt.Errorf("waitable-set.drop: %d waitable(s) still joined to the set", len(s.elems))
	}
	if s.numWaiting > 0 {
		return fmt.Errorf("waitable-set.drop: %d waiter(s) still blocked on the set", s.numWaiting)
	}
	return nil
}

// subtaskState is the reference Subtask.State (definitions.py ~858).
type subtaskState uint8

const (
	subtaskStarting subtaskState = iota
	subtaskStarted
	subtaskReturned
	subtaskCancelledBeforeStarted
	subtaskCancelledBeforeReturned
)

// subtask is the reference Subtask (definitions.py ~847): an async-lowered
// call in flight, itself a waitable so the caller can wait on its resolution.
// It lives in the handle table (entrySubtask).
//
// lenders holds the resource handles whose borrows were lent to this call for
// its duration (Lend/Unlend); it is released by deliverResolve. The MVP host
// import path lends nothing yet, but the resolve/deliver/drop gating is wired
// against it now so Phase 3 borrow scopes only have to start calling addLender.
type subtask struct {
	waitable
	state       subtaskState
	lenders     []*resourceEntry
	flatResults []uint64

	// applyResolve lowers an async host import's results into the guest
	// memory captured at call time (through the retptr an async-lowered
	// import's core signature always carries for a non-empty result -- see
	// async_host_import.go's buildAsyncHostWrapper) and flips state to
	// RETURNED. Set by buildAsyncHostWrapper for a host-import subtask;
	// nil for anything else (no other subtask source exists yet -- Phase
	// 2/3 add guest->guest lowers and streams/futures).
	applyResolve func(results []abi.Value) error
}

func (*subtask) entryKind() entryKind     { return entrySubtask }
func (s *subtask) waitablePtr() *waitable { return &s.waitable }

// newSubtask constructs a subtask in the reference's initial state. lenders is
// a non-nil empty slice on purpose: resolveDelivered() uses lenders==nil as the
// "resolve has been delivered" sentinel (mirroring the reference setting
// lenders=None in deliver_resolve), so a fresh subtask must NOT read as
// delivered. Zero-value &subtask{} would, so always build via newSubtask.
func newSubtask() *subtask {
	return &subtask{state: subtaskStarting, lenders: []*resourceEntry{}}
}

// resolved mirrors Subtask.resolved.
func (s *subtask) resolved() bool { return s.state >= subtaskReturned }

// addLender mirrors Subtask.add_lender.
func (s *subtask) addLender(h *resourceEntry) {
	h.lendCount++
	s.lenders = append(s.lenders, h)
}

// resolve mirrors Subtask.resolve: the call finished (RETURNED) or was
// cancelled; only a RETURNED subtask carries flat results.
func (s *subtask) resolve(state subtaskState, flatResults []uint64) {
	if state != subtaskReturned && len(flatResults) != 0 {
		panic("BUG: non-RETURNED subtask resolved with flat results")
	}
	s.state = state
	s.flatResults = flatResults
}

// deliverResolve mirrors Subtask.deliver_resolve: release the lent borrows.
// resolveDelivered reports whether this has happened (lenders == nil).
func (s *subtask) deliverResolve() {
	for _, h := range s.lenders {
		h.lendCount--
	}
	s.lenders = nil
}

func (s *subtask) resolveDelivered() bool { return s.lenders == nil }

// dropSubtask mirrors Subtask.drop: trap unless the resolve has been delivered.
func (s *subtask) dropSubtask() error {
	if !s.resolveDelivered() {
		return fmt.Errorf("subtask.drop: subtask not yet resolved-and-delivered")
	}
	s.dropWaitable()
	return nil
}
