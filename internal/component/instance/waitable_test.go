package instance

import (
	"strings"
	"testing"
)

func ev(c eventCode, p1, p2 uint32) func() eventTuple {
	return func() eventTuple { return eventTuple{c, p1, p2} }
}

func TestWaitable_JoinMoveDrop(t *testing.T) {
	a, b := &waitableSet{}, &waitableSet{}
	w := &waitable{}

	w.join(a)
	if w.wset != a || len(a.elems) != 1 || a.elems[0] != w {
		t.Fatal("join(a) did not register membership")
	}
	// Moving to b must remove from a.
	w.join(b)
	if w.wset != b || len(a.elems) != 0 || len(b.elems) != 1 {
		t.Fatalf("join(b) did not move membership: a=%d b=%d", len(a.elems), len(b.elems))
	}
	// drop (no pending event, leaves the set).
	w.dropWaitable()
	if w.wset != nil || len(b.elems) != 0 {
		t.Fatal("dropWaitable did not clear membership")
	}
}

func TestWaitable_DropWithPendingEventPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic dropping a waitable with a pending event")
		}
	}()
	w := &waitable{}
	w.setPendingEvent(ev(eventSubtask, 3, 1))
	w.dropWaitable()
}

func TestWaitable_GetPendingEventClears(t *testing.T) {
	w := &waitable{}
	if w.hasPendingEvent() {
		t.Fatal("fresh waitable should have no event")
	}
	w.setPendingEvent(ev(eventSubtask, 7, uint32(subtaskReturned)))
	if !w.hasPendingEvent() {
		t.Fatal("expected pending event after set")
	}
	got := w.getPendingEvent()
	if got != (eventTuple{eventSubtask, 7, uint32(subtaskReturned)}) {
		t.Fatalf("unexpected event %+v", got)
	}
	if w.hasPendingEvent() {
		t.Fatal("getPendingEvent must clear the event")
	}
}

func TestWaitableSet_GetPendingEventFIFO(t *testing.T) {
	s := &waitableSet{}
	w1, w2 := &waitable{}, &waitable{}
	w1.join(s)
	w2.join(s)
	// Only the second has an event; get must find it despite order.
	w2.setPendingEvent(ev(eventSubtask, 2, 0))
	if !s.hasPendingEvent() {
		t.Fatal("set should report the pending event")
	}
	if got := s.getPendingEvent(); got.p1 != 2 {
		t.Fatalf("unexpected event %+v", got)
	}
	// Now both: FIFO (insertion order) picks w1 first.
	w1.setPendingEvent(ev(eventSubtask, 1, 0))
	w2.setPendingEvent(ev(eventSubtask, 2, 0))
	if got := s.getPendingEvent(); got.p1 != 1 {
		t.Fatalf("FIFO should pick w1 first, got %+v", got)
	}
}

func TestWaitableSet_PollAndDrop(t *testing.T) {
	s := &waitableSet{}
	if got := s.poll(); got.code != eventNone {
		t.Fatalf("empty poll should be NONE, got %+v", got)
	}
	w := &waitable{}
	w.join(s)
	if err := s.dropSet(); err == nil || !strings.Contains(err.Error(), "still joined") {
		t.Fatalf("dropSet with a member must trap, got %v", err)
	}
	w.join(nil) // leave
	if err := s.dropSet(); err != nil {
		t.Fatalf("empty dropSet should succeed, got %v", err)
	}
}

func TestSubtask_StateMachineAndDrop(t *testing.T) {
	st := newSubtask()
	if st.resolved() || st.resolveDelivered() {
		t.Fatal("fresh subtask is unresolved and undelivered")
	}
	if err := st.dropSubtask(); err == nil {
		t.Fatal("dropping an unresolved subtask must trap")
	}
	// A lent borrow is released on deliver.
	h := &resourceEntry{}
	st.addLender(h)
	if h.lendCount != 1 {
		t.Fatalf("addLender should bump lendCount, got %d", h.lendCount)
	}
	st.resolve(subtaskReturned, []uint64{42})
	if !st.resolved() {
		t.Fatal("resolve(RETURNED) should mark resolved")
	}
	if err := st.dropSubtask(); err == nil {
		t.Fatal("drop before deliverResolve must still trap")
	}
	st.deliverResolve()
	if !st.resolveDelivered() || h.lendCount != 0 {
		t.Fatalf("deliverResolve must release lends: delivered=%v lend=%d", st.resolveDelivered(), h.lendCount)
	}
	if err := st.dropSubtask(); err != nil {
		t.Fatalf("drop after deliver should succeed, got %v", err)
	}
}

func TestSubtask_ResolveNonReturnedWithResultsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic resolving a cancelled subtask with flat results")
		}
	}()
	newSubtask().resolve(subtaskCancelledBeforeStarted, []uint64{1})
}
