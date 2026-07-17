package instance

import (
	"context"
	"fmt"
	"sync"

	"github.com/samyfodil/wazy/api"
)

// entryKind discriminates the kinds of values a handleTable can hold. Today
// there is exactly one (entryResource); the Canonical ABI's single
// per-instance handle table (testdata/definitions.py's `ComponentInstance
// .handles`, typed `Table[ResourceHandle | Waitable | WaitableSet |
// ErrorContext]`) also holds waitables, waitable sets, error contexts, and
// stream/future ends in that same index space. Those kinds are added by
// later Phase 1 work, not here -- entryKind exists now so the table's shape
// does not have to change again when they land.
type entryKind uint8

const (
	entryResource     entryKind = iota
	entryWaitableSet            // *waitableSet (canon waitable-set.new)
	entrySubtask                // *subtask, itself a Waitable (async-lowered call in flight)
	entryStreamEnd              // *streamEnd (Phase 2)
	entryFutureEnd              // *futureEnd (Phase 2)
	entryErrorContext           // *errorContext (Phase 2)
)

// tableEntry is anything a handleTable slot can hold. resourceEntry is the
// only implementer today.
type tableEntry interface {
	entryKind() entryKind
}

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

	// borrowScope mirrors ResourceHandle.borrow_scope (Phase 3): the callee
	// task this borrow handle is scoped to. nil for every own handle, and
	// for a borrow handle minted via NewBorrow (host-minted, unscoped -- see
	// NewBorrow's doc and the handleTable doc comment's retained-deviation
	// entry). Non-nil only for a borrow minted via NewBorrowScoped -- the
	// composition delegate's cross-instance lower_borrow (composition.go).
	borrowScope *task
}

// entryKind implements tableEntry.
func (*resourceEntry) entryKind() entryKind { return entryResource }

// handleTable is a per-instance table mapping an i32 handle to a tableEntry,
// mirroring the Canonical ABI's `Table` (testdata/definitions.py's Table and
// the canon_resource_* functions) -- the reference's single index space that
// also holds waitables, waitable sets, error contexts, and stream/future ends
// (see entryKind). Only resourceEntry exists today; add and lookup below
// deal exclusively in resourceEntry, so nothing else here changes behavior
// until a later Phase 1 change starts storing other kinds. One handleTable is
// shared by every resource type declared or used by a single Instance;
// resourceEntry values are tagged with the resource type they belong to
// (comp.Types index of the ResourceDesc) so cross-type handle confusion
// (canon_resource_rep/drop's `trap_if(h.rt is not rt)`) is detected.
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
//   - No borrow_scope / Task lifetime tracking for LEND accounting (the
//     num_lends bookkeeping above): lifting a borrow<T> HOST-IMPORT argument
//     increments the *lent-from* handle's num_lends for the duration of that
//     one call, and releases it automatically when the call returns. wazy
//     does not model a host-import call as a Task, so this package cannot
//     safely auto-release that lend at "the end of the call" -- there is no
//     such hook. Lend/Unlend below exist and are tested (so the accounting
//     primitive is ready), but nothing in the host-import lift/lower wiring
//     calls Lend automatically; a lend, once taken, must be released
//     explicitly via Unlend (async host imports instead use
//     subtask.addLender/deliverResolve -- see async_host_import.go).
//
//   - Host-minted borrows are deliberately unscoped (Phase 3's retained
//     deviation, docs/component-model-async-phase3-design.md §4.3): a
//     GUEST-facing borrow<T> handle minted for a cross-instance composition
//     call IS call-scoped (resourceEntry.borrowScope, NewBorrowScoped,
//     Drop's borrow arm below -- the reference's `lower_borrow` +
//     `canon_resource_drop`'s borrow arm), but a borrow<T> handle the HOST
//     mints directly (WASI results, resource hooks -- NewBorrow) is not tied
//     to any call and the guest holds it long-term; dropping one is refused
//     rather than guessed at (see Drop's borrowScope == nil arm).
//
//   - No destructors. canon_resource_drop's dtor-call step (running the
//     resource type's declared destructor when a fully-owned handle is
//     dropped with no outstanding lends) is not implemented; Drop just
//     removes the table entry. The host is responsible for freeing whatever
//     the rep names.
type handleTable struct {
	mu      sync.Mutex
	entries map[uint32]tableEntry
	next    uint32

	// free holds handle indices returned by Drop/TakeOwn, reused before next is
	// bumped -- mirroring the reference Table's free list (definitions.py's
	// Table.free). A guest may rely on this: e.g. the first resource.new after a
	// drop reclaims the just-freed index, so index numbering is deterministic
	// and dense per instance rather than monotonically growing.
	free []uint32

	// dtors maps a resource type tag to the destructor run when a handle of that
	// tag is dropped by canon resource.drop. A GUEST-defined resource's dtor
	// invokes its core func (registered lazily -- see resourceDtor -- because the
	// dtor's own module may not be instantiated when a `start` section triggers
	// the first drop); a HOST-provided resource's dtor is a Go callback (drop
	// accounting). nil callback means drop just removes the entry.
	dtors map[uint32]func(ctx context.Context, rep uint32) error
}

// registerDtor records the destructor callback for a resource type tag.
func (t *handleTable) registerDtor(typeIdx uint32, dtor func(ctx context.Context, rep uint32) error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dtors == nil {
		t.dtors = make(map[uint32]func(ctx context.Context, rep uint32) error)
	}
	t.dtors[typeIdx] = dtor
}

// dtorFor returns the destructor callback registered for a resource type tag,
// or nil.
func (t *handleTable) dtorFor(typeIdx uint32) func(ctx context.Context, rep uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dtors[typeIdx]
}

// resourceDtor wraps a lazily-resolved guest destructor core func as a dtor
// callback. resolve is called at drop time (its module is up by then); a nil
// resolution means the dtor can't run, so the drop just removes the entry.
func resourceDtor(resolve func() api.Function) func(context.Context, uint32) error {
	return func(ctx context.Context, rep uint32) error {
		fn := resolve()
		if fn == nil {
			return nil
		}
		_, err := fn.Call(ctx, uint64(rep))
		return err
	}
}

// newHandleTable returns an empty handleTable, ready to allocate handles
// starting at 1.
func newHandleTable() *handleTable {
	return &handleTable{entries: make(map[uint32]tableEntry), next: 1}
}

// add allocates a new resourceEntry via addEntry. It is a thin, kind-specific
// wrapper: every entry kind the table can hold shares the same allocation
// (free-list) policy, implemented once in addEntry.
func (t *handleTable) add(typeIdx, rep uint32, own bool) uint32 {
	return t.addEntry(&resourceEntry{typeIdx: typeIdx, rep: rep, own: own})
}

// addEntry allocates a handle index -- reusing a freed index first (reference
// Table.free), else bumping next -- and stores e under it. This is the one
// place index allocation happens, so later entry kinds reuse it instead of
// duplicating the free-list logic add used to inline.
func (t *handleTable) addEntry(e tableEntry) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var h uint32
	if n := len(t.free); n > 0 { // reuse a freed index (reference Table.free)
		h = t.free[n-1]
		t.free = t.free[:n-1]
	} else {
		h = t.next
		t.next++
	}
	t.entries[h] = e
	return h
}

// entryAt returns the raw table entry at handle h, regardless of kind, or
// false if h is not currently allocated. Like lookup, callers must hold
// t.mu.
func (t *handleTable) entryAt(h uint32) (tableEntry, bool) {
	e, ok := t.entries[h]
	return e, ok
}

// getEntry returns the entry at h without removing it (reference Table.get),
// taking the lock itself. Unlike entryAt, safe to call without holding t.mu.
func (t *handleTable) getEntry(h uint32) (tableEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[h]
	return e, ok
}

// removeEntry removes and returns the entry at h (reference Table.remove),
// pushing h onto the free list for guest-observable reuse. Used by the async
// builtins that retire a table entry regardless of kind (waitable-set.drop,
// subtask.drop); resource handles retire through Drop/DropOwned, which also
// enforce their own-only/lend guards.
func (t *handleTable) removeEntry(h uint32) (tableEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[h]
	if !ok {
		return nil, false
	}
	delete(t.entries, h)
	t.free = append(t.free, h)
	return e, true
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

// NewBorrowScoped mints a borrow handle in THIS (the callee's) table, scoped
// to scope -- mirrors lower_borrow's minting arm (definitions.py ~1813-1815).
// Unlike NewBorrow, this handle CAN be dropped by the guest (Drop's borrow
// arm below); dropping it releases the loan by decrementing scope.numBorrows,
// and scope's own exit (task.return / task.cancel) traps if any minted-here
// borrow is still outstanding when the callee tries to resolve. scope must be
// non-nil -- a nil scope would be indistinguishable from a host-minted
// unscoped borrow (see NewBorrow), silently defeating the exit trap.
func (t *handleTable) NewBorrowScoped(typeIdx, rep uint32, scope *task) uint32 {
	if scope == nil {
		panic("BUG: NewBorrowScoped called with a nil scope")
	}
	scope.numBorrows++
	return t.addEntry(&resourceEntry{typeIdx: typeIdx, rep: rep, own: false, borrowScope: scope})
}

// lookup resolves handle h, requiring it to exist, name a resourceEntry (as
// opposed to some other entryKind a later phase adds), and belong to
// resource type typeIdx. Callers must hold t.mu.
func (t *handleTable) lookup(typeIdx, h uint32) (*resourceEntry, error) {
	raw, ok := t.entryAt(h)
	if !ok {
		return nil, fmt.Errorf("unknown handle %d", h)
	}
	e, ok := raw.(*resourceEntry)
	if !ok {
		return nil, fmt.Errorf("handle %d is not a resource handle", h)
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
	t.free = append(t.free, h)
	return e.rep, nil
}

// Drop removes handle h (mirrors canon_resource_drop). It fails loud if the
// handle is unknown, belongs to a different resource type, or has
// outstanding lends -- checked before the own/borrow split, matching the
// reference's own check order (definitions.py canon_resource_drop ~2314,
// before its own/else branch at ~2317-2324) -- so a lent-out borrow cannot be
// dropped out from under its lender.
//
// A borrow handle (Phase 3) is then dropped by releasing its call scope's
// loan (borrowScope.numBorrows--) rather than running a destructor -- see
// resourceCanonHostFuncGraph's drop case, which must ask IsOwn before doing
// any dtor lookup so a borrow drop never runs the owning resource's dtor. A
// host-minted, unscoped borrow (NewBorrow, borrowScope == nil) still cannot
// be dropped through this table -- see the handleTable doc comment.
func (t *handleTable) Drop(typeIdx, h uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return err
	}
	if e.lendCount != 0 {
		return fmt.Errorf("handle %d has %d outstanding borrow(s), cannot drop", h, e.lendCount)
	}
	if !e.own {
		if e.borrowScope == nil {
			return fmt.Errorf("handle %d is a borrow with no owning call scope; host-minted borrows cannot be dropped by the guest", h)
		}
		e.borrowScope.numBorrows--
		delete(t.entries, h)
		t.free = append(t.free, h)
		return nil
	}
	delete(t.entries, h)
	t.free = append(t.free, h)
	return nil
}

// IsOwn reports whether handle h (of resource type typeIdx) is an owning
// handle, without consuming or otherwise mutating it. Used by
// resourceCanonHostFuncGraph's drop case to decide own/borrow dispatch
// BEFORE any destructor lookup -- see Drop's doc: a borrow drop must never
// run the resource's dtor.
func (t *handleTable) IsOwn(typeIdx, h uint32) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, err := t.lookup(typeIdx, h)
	if err != nil {
		return false, err
	}
	return e.own, nil
}

// DropOwned removes an owning handle by handle alone (the resource type is not
// known to the caller -- a host dropping a guest resource it received), first
// returning the rep so the caller can run the guest destructor against it. It
// fails loud if the handle is unknown, is a borrow, or has outstanding lends
// (the same guards as Drop). The removal happens only on success.
func (t *handleTable) DropOwned(h uint32) (rep uint32, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	raw, ok := t.entryAt(h)
	if !ok {
		return 0, fmt.Errorf("handle %d does not name a live resource", h)
	}
	e, ok := raw.(*resourceEntry)
	if !ok {
		return 0, fmt.Errorf("handle %d is not a resource handle", h)
	}
	if !e.own {
		return 0, fmt.Errorf("handle %d is a borrow, not an own handle", h)
	}
	if e.lendCount != 0 {
		return 0, fmt.Errorf("handle %d has %d outstanding borrow(s), cannot drop", h, e.lendCount)
	}
	delete(t.entries, h)
	t.free = append(t.free, h)
	return e.rep, nil
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

// entryForLend resolves handle h (of resource type typeIdx) to its
// *resourceEntry for a caller that will manage a lend's lifetime itself via
// subtask.addLender/deliverResolve, rather than releasing it through Unlend
// keyed by (typeIdx, handle) once the immediate call returns -- the sync host
// import wrapper's pattern (buildHostWrapper's defer Unlend). An async host
// import's borrow<T> arg must instead stay lent until its subtask's resolve
// is DELIVERED (liftAsyncHostArgsPlanned, async_host_import.go), which can be
// long after this call returns, so the caller needs the live *resourceEntry
// pointer to release later rather than a (typeIdx, handle) pair that could,
// by then, name a different handle entirely (index reuse via the free list).
// Does not touch lendCount itself -- the caller increments it via addLender.
func (t *handleTable) entryForLend(typeIdx, h uint32) (*resourceEntry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lookup(typeIdx, h)
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
