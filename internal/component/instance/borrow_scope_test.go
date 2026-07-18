package instance

import (
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// This file is the Phase 3 borrow-scope named-trap inventory (docs/
// component-model-async-phase3-design.md §4.2/§7): direct Go-level tests
// for handleTable.NewBorrowScoped/Drop's borrow arm and the task-exit
// numBorrows traps -- the ceiling resource.go's doc comment used to
// document as "dropping a borrow handle is rejected outright" is retired
// for call-scoped borrows here.

func TestNewBorrowScoped_IncrementsNumBorrows(t *testing.T) {
	tbl := newHandleTable()
	scope := &task{}
	h := tbl.NewBorrowScoped(1, 42, scope)
	if scope.numBorrows != 1 {
		t.Fatalf("scope.numBorrows = %d, want 1", scope.numBorrows)
	}
	rep, err := tbl.Rep(1, h)
	if err != nil || rep != 42 {
		t.Fatalf("Rep(1, h) = (%d, %v), want (42, nil)", rep, err)
	}
}

func TestNewBorrowScoped_NilScopePanics(t *testing.T) {
	tbl := newHandleTable()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic (BUG) minting a scoped borrow with a nil scope")
		}
	}()
	tbl.NewBorrowScoped(1, 42, nil)
}

// TestBorrowScopedDrop_ReleasesTheLoanAndRemovesTheEntry pins Drop's borrow
// arm: dropping a call-scoped borrow decrements the scope's numBorrows and
// removes the table entry (unlike the retained host-minted-unscoped
// deviation, which stays rejected -- resource_test.go's
// TestHandleTable_BorrowCannotBeDropped).
func TestBorrowScopedDrop_ReleasesTheLoanAndRemovesTheEntry(t *testing.T) {
	tbl := newHandleTable()
	scope := &task{}
	h := tbl.NewBorrowScoped(1, 42, scope)

	if err := tbl.Drop(1, h); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if scope.numBorrows != 0 {
		t.Fatalf("scope.numBorrows = %d after drop, want 0", scope.numBorrows)
	}
	if _, err := tbl.Rep(1, h); err == nil {
		t.Fatal("expected the dropped handle to be unknown")
	}
}

// TestBorrowScopedDrop_DoubleDropFails pins the "same observable as an own
// double-drop" gate note: a second drop of an already-dropped scoped borrow
// is "unknown handle", not a crash or a double-decrement of numBorrows.
func TestBorrowScopedDrop_DoubleDropFails(t *testing.T) {
	tbl := newHandleTable()
	scope := &task{}
	h := tbl.NewBorrowScoped(1, 42, scope)
	if err := tbl.Drop(1, h); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	err := tbl.Drop(1, h)
	if err == nil {
		t.Fatal("expected the second drop to fail")
	}
	requireErrContains(t, err, "unknown handle")
	if scope.numBorrows != 0 {
		t.Fatalf("scope.numBorrows = %d after double-drop, want 0 (unchanged)", scope.numBorrows)
	}
}

// TestBorrowScopedDrop_LentBorrowCannotBeDropped pins the lend-guard order
// gate: checking lends before the own/borrow split (handleTable.Drop's doc)
// means a lent-out scoped borrow cannot be dropped out from under its
// lender, exactly like a lent own handle.
func TestBorrowScopedDrop_LentBorrowCannotBeDropped(t *testing.T) {
	tbl := newHandleTable()
	scope := &task{}
	h := tbl.NewBorrowScoped(1, 42, scope)
	if err := tbl.Lend(1, h); err != nil {
		t.Fatalf("Lend: %v", err)
	}
	if err := tbl.Drop(1, h); err == nil {
		t.Fatal("expected Drop to fail while a lend is outstanding")
	} else {
		requireErrContains(t, err, "outstanding")
	}
	if scope.numBorrows != 1 {
		t.Fatalf("scope.numBorrows = %d after a rejected drop, want 1 (unchanged)", scope.numBorrows)
	}
}

// TestBorrowScopedDrop_DoesNotRunDtor pins §4.2's dtor-non-run gate: a
// borrow drop must never run the resource's destructor (only an own drop
// does) -- resourceCanonHostFuncGraph's drop case asks IsOwn before any
// dtor lookup. This tests the handleTable-level IsOwn helper that dispatch
// depends on directly (the graph-level dtor-skip is exercised by
// TestGuestGuestBorrow_DropDoesNotRunDtor in the composition suite).
func TestHandleTable_IsOwn(t *testing.T) {
	tbl := newHandleTable()
	ownH := tbl.NewOwn(1, 1)
	scope := &task{}
	borrowH := tbl.NewBorrowScoped(1, 2, scope)

	if own, err := tbl.IsOwn(1, ownH); err != nil || !own {
		t.Fatalf("IsOwn(own) = (%v, %v), want (true, nil)", own, err)
	}
	if own, err := tbl.IsOwn(1, borrowH); err != nil || own {
		t.Fatalf("IsOwn(borrow) = (%v, %v), want (false, nil)", own, err)
	}
	if _, err := tbl.IsOwn(1, 999); err == nil {
		t.Fatal("expected an error for an unknown handle")
	}
}

// ------- task-exit numBorrows traps (site 3) -------

// TestReturnValues_ScopedBorrowOutstandingTraps and
// TestCancelResolve_ScopedBorrowOutstandingTraps pin the two task-exit
// checkpoints (task.return and task.cancel) against a REAL
// NewBorrowScoped-minted handle, not just a hand-set numBorrows int --
// confirming the mint and the exit trap are wired to the same field.
func TestReturnValues_ScopedBorrowOutstandingTraps(t *testing.T) {
	tbl := newHandleTable()
	tk := &task{}
	tbl.NewBorrowScoped(1, 7, tk)
	err := tk.returnValues(nil)
	requireErrContains(t, err, "borrow handles still remain")
}

func TestCancelResolve_ScopedBorrowOutstandingTraps(t *testing.T) {
	tbl := newHandleTable()
	tk := &task{state: taskCancelDelivered}
	tbl.NewBorrowScoped(1, 7, tk)
	err := tk.cancelResolve()
	requireErrContains(t, err, "borrow handles still remain")
}

// TestReturnValues_ScopedBorrowDroppedSucceeds is the companion: dropping
// the borrow before return clears numBorrows and task.return succeeds.
func TestReturnValues_ScopedBorrowDroppedSucceeds(t *testing.T) {
	tbl := newHandleTable()
	tk := &task{onResolve: func([]abi.Value, bool) {}}
	h := tbl.NewBorrowScoped(1, 7, tk)
	if err := tbl.Drop(1, h); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	if err := tk.returnValues(nil); err != nil {
		t.Fatalf("returnValues after drop: %v", err)
	}
}
