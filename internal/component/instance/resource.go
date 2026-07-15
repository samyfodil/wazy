package instance

import (
	"fmt"
	"sync"
)

// resourceEntry is one live handle in a handleTable: the host-side
// representation it refers to, whether the handle owns that representation
// (as opposed to merely borrowing it), and how many outstanding loans have
// been lent out from it (mirrors the Canonical ABI's ResourceHandle --
// testdata/definitions.py's `rep`, `own`, and `num_lends` fields).
type resourceEntry struct {
	typeIdx   uint32 // resource type this handle was minted for
	rep       uint32
	own       bool
	lendCount int
}

// handleTable is a per-instance table mapping an i32 handle to a
// resourceEntry, mirroring the Canonical ABI's `Table` + `ResourceHandle`
// (testdata/definitions.py's Table/ResourceHandle and the canon_resource_*
// functions). One handleTable is shared by every resource type declared or
// used by a single Instance; entries are tagged with the resource type they
// belong to (comp.Types index of the ResourceDesc) so cross-type handle
// confusion (canon_resource_rep/drop's `trap_if(h.rt is not rt)`) is
// detected.
//
// Handle numbering starts at 1 (0 is never allocated), matching the
// reference Table, which reserves index 0 by seeding its backing array with
// a single nil entry.
//
// ponytail: this is the M4 STEP 1 ceiling, not the full spec:
//
//   - No cross-instance handle transfer. The Canonical ABI lets an own<T>
//     handle move between component instances (lift_own/lower_own operate on
//     whichever instance's table the current call context names). wazy does
//     not yet track "which instance implements resource type T" across
//     instance boundaries, so every handle a given Instance's table hands
//     out is only ever resolved against that same Instance's table -- there
//     is no operation that moves a handle, or the rep it names, to another
//     Instance's table.
//
//   - No borrow_scope / Task lifetime tracking. The spec ties a borrow
//     handle's lend accounting to the enclosing call ("Task"/"Subtask"):
//     lifting a borrow<T> argument increments the *lent-from* handle's
//     num_lends for the duration of that one call, and releases it
//     automatically when the call returns. wazy does not model calls as
//     Tasks, so this package cannot safely auto-release a lend at "the end
//     of the call" -- there is no such hook. Lend/Unlend below exist and are
//     tested (so the accounting primitive is ready), but nothing in the
//     lift/lower or canon wiring calls Lend automatically; a lend, once
//     taken, must be released explicitly via Unlend.
//
//   - Dropping a borrow handle is rejected outright, rather than the spec's
//     "release the loan" behavor (canon_resource_drop's `else:
//     h.borrow_scope.num_borrows -= 1` branch). Without call-scoped
//     tracking this package cannot correctly identify *which* loan a given
//     borrow handle corresponds to, so the safe, fail-loud choice is to
//     refuse the operation rather than guess.
//
//   - No destructors. canon_resource_drop's dtor-call step (running the
//     resource type's declared destructor when a fully-owned handle is
//     dropped with no outstanding lends) is not implemented; Drop just
//     removes the table entry. The host is responsible for freeing whatever
//     the rep names.
type handleTable struct {
	mu      sync.Mutex
	entries map[uint32]*resourceEntry
	next    uint32
}

// newHandleTable returns an empty handleTable, ready to allocate handles
// starting at 1.
func newHandleTable() *handleTable {
	return &handleTable{entries: make(map[uint32]*resourceEntry), next: 1}
}

func (t *handleTable) add(typeIdx, rep uint32, own bool) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := t.next
	t.next++
	t.entries[h] = &resourceEntry{typeIdx: typeIdx, rep: rep, own: own}
	return h
}

// NewOwn allocates a new owning handle for rep under resource type typeIdx.
// Mirrors canon_resource_new (and lower_own, when a host result is lowered
// back into an own<T>).
func (t *handleTable) NewOwn(typeIdx, rep uint32) uint32 { return t.add(typeIdx, rep, true) }

// NewBorrow allocates a new non-owning handle for rep under resource type
// typeIdx. See the handleTable doc comment: unlike the full Canonical ABI,
// this handle is not tied to a call/borrow scope. It can be read via Rep but
// -- deliberately, see Drop -- can never be dropped through this table; the
// host that minted it is responsible for its lifetime.
func (t *handleTable) NewBorrow(typeIdx, rep uint32) uint32 { return t.add(typeIdx, rep, false) }

// lookup resolves handle h, requiring it to exist and belong to resource
// type typeIdx. Callers must hold t.mu.
func (t *handleTable) lookup(typeIdx, h uint32) (*resourceEntry, error) {
	e, ok := t.entries[h]
	if !ok {
		return nil, fmt.Errorf("unknown handle %d", h)
	}
	if e.typeIdx != typeIdx {
		return nil, fmt.Errorf("handle %d belongs to resource type %d, not %d", h, e.typeIdx, typeIdx)
	}
	return e, nil
}

// Rep returns the host representation handle h refers to, without consuming
// it. Works for both own and borrow handles (mirrors canon_resource_rep,
// which does not distinguish). Fails loud on an unknown handle -- which also
// covers a handle that has already been dropped, since Drop/TakeOwn remove
// the entry entirely, matching the reference Table.remove -- or a handle
// minted for a different resource type.
func (t *handleTable) Rep(typeIdx, h uint32) (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return 0, err
	}
	return e.rep, nil
}

// TakeOwn consumes an owning handle, removing it from the table and
// returning the rep it named (mirrors lift_own: ownership transfers to the
// caller). Fails loud if the handle is unknown, belongs to a different
// resource type, is a borrow (not own) handle, or has outstanding lends.
func (t *handleTable) TakeOwn(typeIdx, h uint32) (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return 0, err
	}
	if !e.own {
		return 0, fmt.Errorf("handle %d is a borrow, not an own handle", h)
	}
	if e.lendCount != 0 {
		return 0, fmt.Errorf("handle %d has %d outstanding borrow(s)", h, e.lendCount)
	}
	delete(t.entries, h)
	return e.rep, nil
}

// Drop removes handle h (mirrors canon_resource_drop). It fails loud if the
// handle is unknown, belongs to a different resource type, has outstanding
// lends, or -- see the handleTable doc comment's ceiling list -- is a borrow
// handle (this package does not implement releasing a loan, so it refuses
// rather than silently doing nothing).
func (t *handleTable) Drop(typeIdx, h uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return err
	}
	if !e.own {
		return fmt.Errorf("handle %d is a borrow; borrow handles cannot be dropped by the receiver (not supported by this milestone)", h)
	}
	if e.lendCount != 0 {
		return fmt.Errorf("handle %d has %d outstanding borrow(s), cannot drop", h, e.lendCount)
	}
	delete(t.entries, h)
	return nil
}

// Lend increments h's outstanding-lend count, blocking TakeOwn/Drop until
// released via Unlend. This is the accounting primitive canon_resource_drop
// and lift_own check (`trap_if(h.num_lends != 0)`); see the handleTable doc
// comment for why nothing in this package calls it automatically yet.
func (t *handleTable) Lend(typeIdx, h uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return err
	}
	e.lendCount++
	return nil
}

// Unlend reverses a Lend. Fails loud if h has no outstanding lends to
// release.
func (t *handleTable) Unlend(typeIdx, h uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return err
	}
	if e.lendCount == 0 {
		return fmt.Errorf("handle %d has no outstanding lends to release", h)
	}
	e.lendCount--
	return nil
}
