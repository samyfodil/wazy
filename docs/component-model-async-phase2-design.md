# wazy async Phase 2 — streams / futures / error-context runtime design

Scope: Phase 2 of `docs/component-model-async-plan.md`. Builds strictly on the Phase 1
runtime (`instance/sched.go`, `waitable.go`, `task.go`, `async_lift.go`,
`async_builtins.go`, the unified `handleTable`) — nothing in Phase 1 is reshaped, only
extended at the seams Phase 1 left open (`entryKind` constants, `waitableEntry`,
`canonToDef` cases, `resolveHandleArg`/`allocHandleResult` type switches).
Behavior source: `internal/component/abi/testdata/definitions.py` —
Buffer ~907-960, SharedStreamImpl ~986-1056, CopyEnd ~1069-1098, SharedFutureImpl
~1108-1175, `stream_copy` ~2526, `future_copy` ~2580, `cancel_copy` ~2632,
`drop` ~2666, `lift_stream/lower_stream` ~1513/1817, `canon_error_context_*` ~2770.

Design rule (unchanged from Phase 1): **transliterate every `trap_if` and every state
transition; collapse every mechanism whose only job is inter-thread scheduling.** The
one Phase-2-specific instance of the rule: the reference's
`Waitable.wait_for_pending_event()` (park a green thread until this one waitable has
an event) becomes `in.sched.drive(end.hasPendingEvent)` — exactly the WAIT driver's
mechanism, pointed at one end instead of a set.

New files: `instance/stream.go` (shared impls + ends + buffers),
`instance/stream_builtins.go` (the 17 canonToDef cases),
`instance/stream_host.go` (StreamReader/Writer + future twins),
`instance/errorcontext.go`. Modified (all additive): `resource.go` (three
`entryKind` constants), `graph.go` (`computeCanonHostFunc` cases),
`host_import.go`/`async_host_import.go`/`composition.go`/`instance.go` (stream/future
cases in the four existing handle-translation switches). **The `abi` package does not
change** (§2.4).

---

## 0. The core objects, stated once

The reference splits stream state across four layers: a *shared* object (one per
`stream.new`, holding the single pending buffer — the rendezvous point), two *end*
objects (readable/writable, each a `Waitable` living in some instance's handle table,
each with a 4-state copy machine), and per-call *buffer* objects (a typed window into
guest memory, or a host-value list). All four layers are load-bearing and all four are
kept. What collapses is only *waiting*: the reference parks threads inside
`wait_for_pending_event`; wazy pumps `sched` (drive with deadlock trap), exactly as
Phase 1 pinned.

Ownership picture (matters for §2's transfer semantics):

```
guest A table                 heap (no table)                guest B table / host
[entryStreamEnd W]──────┐   ┌────────────────┐   ┌──────[entryStreamEnd R]
  state: copyState      ├──►│  sharedStream   │◄──┤        state: copyState
  waitable (events)     │   │  pending buffer │   │        waitable (events)
                        │   └────────────────┘   │   or:  hostStreamEnd (no waitable)
```

The shared object is never in a table and never moves. VALUE transfer moves only the
*readable end* between tables; the writable end is pinned to its creating instance for
life (spec rule; the writable-end handle type isn't even transferable in the type
system).

---

## 1. The rendezvous copy state machine

### 1.1 Buffers

The reference's `Buffer` hierarchy has exactly two concrete shapes we need: a guest
buffer (`BufferGuestImpl`: cx + ptr + length + progress over linear memory) and a host
buffer (Go values). One interface, two implementations:

```go
// stream.go

// maxBufferLen is Buffer.MAX_LENGTH = 2^28 - 1. Also guarantees
// progress<<4 in packCopyResult cannot overflow 32 bits (progress <= 2^28-1).
const maxBufferLen = 1<<28 - 1

// copyBuffer is the reference Buffer: one side of one read/write call.
// remain/progress are element counts, not bytes.
type copyBuffer interface {
	remain() uint32
	isZeroLength() bool // length == 0 (NOT remain == 0)
	progressed() uint32 // .progress — for packCopyResult
	// read consumes n elements (n <= remain) as host values.
	read(n uint32) ([]abi.Value, error)
	// write appends host values (len(vs) <= remain).
	write(vs []abi.Value) error
}

// guestBuffer is BufferGuestImpl (+Readable/Writable, which differ only in
// which of read/write is legal — Go doesn't need the split; misuse is a
// programming bug caught by the copy direction, not a guest-reachable state).
type guestBuffer struct {
	memMod  api.Module      // bytes fetched FRESH at every access — memory can
	                        // grow between the parking call and the rendezvous
	realloc abi.Realloc     // dst-side realloc for indirect element payloads
	resolve abi.Resolver    // the OWNING instance's resolver
	elem    binary.TypeDesc // nil for bare stream/future (no element)
	elemSz  uint32          // 0 when elem == nil
	numeric bool            // none_or_number_type(elem) — the memmove fast path
	ptr     uint32          // advances elemSz per element copied
	length  uint32
	progress uint32
}

// newGuestBuffer transliterates BufferGuestImpl.__init__'s traps:
//   trap_if(length > maxBufferLen)
//   if elem != nil && length > 0:
//     trap_if(ptr != align_to(ptr, alignment(elem)))
//     trap_if(ptr + length*elemSz > len(mem))     // checked against CURRENT mem
func newGuestBuffer(memMod api.Module, realloc abi.Realloc, resolve abi.Resolver,
	elem binary.TypeDesc, elemSz, align uint32, numeric bool,
	ptr, length uint32) (*guestBuffer, error)

func (b *guestBuffer) read(n uint32) ([]abi.Value, error) {
	// ReadableBufferGuestImpl.read: elem==nil => n empty tuples (abi.Value(nil));
	// else Load each element at b.ptr + i*elemSz, advance ptr, progress += n.
	// Bounds were validated at construction against the memory length THEN;
	// re-check ptr+n*elemSz <= len(mem) here (memory cannot shrink, so this
	// only re-fails if construction was skipped — panic-assert, not trap).
}
func (b *guestBuffer) write(vs []abi.Value) error {
	// WritableBufferGuestImpl.write: Store each element (realloc for indirect
	// payloads), advance ptr, progress += len(vs).
}

// hostBuffer is the host-side buffer (§3): values live in Go, no memory.
type hostBuffer struct {
	vals     []abi.Value // read side: source; write side: filled up to cap
	progress uint32
	length   uint32
}
```

`read`/`write` in host values is the reference's own copy discipline —
`BufferGuestImpl` lifts to host values and lowers back (§4.2 discusses the
two-memories consequence and the fast path that bypasses it).

### 1.2 Shared objects

```go
// copyResult is the reference CopyResult.
type copyResult uint32

const (
	copyCompleted copyResult = 0
	copyDropped   copyResult = 1
	copyCancelled copyResult = 2
)

// blockedSentinel is the reference BLOCKED = 0xffff_ffff: the i32 an
// async-opt copy/cancel builtin returns when the operation parked.
const blockedSentinel = 0xffff_ffff

// onCopyFn / onCopyDoneFn are the reference OnCopy/OnCopyDone callback types.
// reclaim is the reference ReclaimBuffer: the shared object hands the parked
// side a thunk that un-parks its pending buffer; it MUST be called before the
// event payload is read (stream_event calls reclaim_buffer() first).
type onCopyFn func(reclaim func())
type onCopyDoneFn func(result copyResult)

// sharedStream is SharedStreamImpl: the rendezvous point. At most ONE pending
// buffer at a time — whichever side calls read/write first parks; the second
// side copies directly between the two buffers. No elastic host buffer exists,
// so backpressure is structural, exactly as the plan pinned.
type sharedStream struct {
	elem    binary.TypeDesc // resolved element type (nil = bare stream)
	elemSz  uint32
	align   uint32
	numeric bool // none_or_number_type(elem)

	dropped bool

	// The pending side. pendingInst identifies the guest instance whose
	// buffer is parked (nil for a host buffer) — consumed ONLY by the
	// "temporary" same-instance trap below.
	pendingInst       *Instance
	pendingBuffer     copyBuffer
	pendingOnCopy     onCopyFn     // nil for the future impl
	pendingOnCopyDone onCopyDoneFn
}

func (s *sharedStream) resetPending() { s.setPending(nil, nil, nil, nil) }
func (s *sharedStream) setPending(inst *Instance, b copyBuffer, oc onCopyFn, ocd onCopyDoneFn)

// resetAndNotifyPending: MUST clear pending BEFORE invoking the callback
// (reference order — the callback may immediately re-park).
func (s *sharedStream) resetAndNotifyPending(result copyResult) {
	ocd := s.pendingOnCopyDone
	s.resetPending()
	ocd(result)
}

func (s *sharedStream) cancel() { s.resetAndNotifyPending(copyCancelled) }

func (s *sharedStream) drop() {
	if !s.dropped {
		s.dropped = true
		if s.pendingBuffer != nil {
			s.resetAndNotifyPending(copyDropped)
		}
	}
}
```

`read` and `write` are branch-for-branch transliterations. `write` shown in full
(read is its mirror minus the zero-length elif — the asymmetry is the reference's,
keep it):

```go
// write: a writable-end (or host-writer) call with src as the source buffer.
func (s *sharedStream) write(inst *Instance, src copyBuffer,
	onCopy onCopyFn, onCopyDone onCopyDoneFn) error {

	switch {
	case s.dropped:
		onCopyDone(copyDropped)

	case s.pendingBuffer == nil:
		s.setPending(inst, src, onCopy, onCopyDone) // park; rendezvous later

	default:
		// trap_if(inst is pending_inst and not none_or_number_type(t)):
		// the spec's TEMPORARY same-instance restriction — an intra-instance
		// copy of a non-numeric element would alias the source and
		// destination memory mid-lift. inst==nil (host side) never trips it.
		if inst != nil && inst == s.pendingInst && !s.numeric {
			return errors.New("stream: intra-instance copy of a non-numeric element type (spec restriction)")
		}
		if s.pendingBuffer.remain() > 0 {
			if src.remain() > 0 {
				n := min(src.remain(), s.pendingBuffer.remain())
				if err := copyElements(s, s.pendingBuffer, src, n); err != nil {
					return err // element lift/lower trap
				}
				// Notify the parked reader mid-copy: it re-arms or reclaims.
				s.pendingOnCopy(s.resetPending)
			}
			onCopyDone(copyCompleted) // the WRITER completes immediately
		} else if src.isZeroLength() && s.pendingBuffer.isZeroLength() {
			// Zero-length handshake, writer side: a 0-length write against a
			// parked 0-length read COMPLETEs the write WITHOUT waking the
			// reader — the read stays parked as the "ready to receive"
			// signal. (§4.1 item 3.)
			onCopyDone(copyCompleted)
		} else {
			// Parked reader has remain()==0 (it already received elements
			// via an earlier partial rendezvous and is only parked awaiting
			// delivery): complete it, then park ourselves in its place.
			s.resetAndNotifyPending(copyCompleted)
			s.setPending(inst, src, onCopy, onCopyDone)
		}
	}
	return nil
}

// read is the mirror image: dst receives, s.pendingBuffer is the parked
// WRITER's source. Branches: dropped => onCopyDone(DROPPED);
// no pending => park; else same-instance trap; pending.remain()>0 =>
// { dst.remain()>0 => copy pending->dst, pendingOnCopy(reset), }
// onCopyDone(COMPLETED); else => resetAndNotify(COMPLETED) + park dst.
// NOTE: read has NO zero-length elif — a 0-length read against a parked
// 0-length write takes the remain()==0 else-branch (completes the parked
// writer, parks the reader). Reference asymmetry, kept verbatim.
func (s *sharedStream) read(inst *Instance, dst copyBuffer,
	onCopy onCopyFn, onCopyDone onCopyDoneFn) error
```

```go
// sharedFuture is SharedFutureImpl: a stream of exactly one element with a
// simpler protocol — no onCopy (no partial progress), buffers always length 1.
type sharedFuture struct {
	elem    binary.TypeDesc
	elemSz  uint32
	align   uint32
	numeric bool

	dropped           bool
	pendingInst       *Instance
	pendingBuffer     copyBuffer
	pendingOnCopyDone onCopyDoneFn
}

func (f *sharedFuture) read(inst *Instance, dst copyBuffer, onCopyDone onCopyDoneFn) error {
	// assert(!f.dropped && dst.remain() == 1) — reference assert, not trap:
	// unreachable through the builtins (§4.1 item 5). Go: panic-assert.
	if f.pendingBuffer == nil {
		f.setPending(inst, dst, onCopyDone)
		return nil
	}
	// same-instance non-numeric trap (as stream)
	vs, err := f.pendingBuffer.read(1)  // ...
	dst.write(vs)                        // ...
	f.resetAndNotifyPending(copyCompleted) // completes the parked WRITER
	onCopyDone(copyCompleted)              // completes the reader
	return nil
}

func (f *sharedFuture) write(inst *Instance, src copyBuffer, onCopyDone onCopyDoneFn) error {
	// assert(src.remain() == 1)
	if f.dropped { onCopyDone(copyDropped); return nil } // reader gave up
	// else: park, or same-instance trap + copy 1 + notify both — mirror of read.
}

func (f *sharedFuture) drop() {
	if !f.dropped {
		f.dropped = true
		if f.pendingBuffer != nil {
			// assert(pending is a WRITE buffer? No — reference asserts the
			// pending buffer is a WritableBuffer, i.e. only a parked READER
			// can be interrupted by a drop; a parked writer blocks the
			// dropper (WritableFutureEnd.drop traps unless DONE). See §1.5.
			f.resetAndNotifyPending(copyDropped)
		}
	}
}
```

`copyElements` — the one performance-sensitive line in the whole file:

```go
// copyElements moves n elements from src to dst.
//   Fast path: both ends numeric (none_or_number_type) AND both guestBuffers
//     => a single copy(dstMem[dp:dp+n*sz], srcMem[sp:sp+n*sz]) — numeric
//     layouts are byte-identical in every memory; observably equivalent to
//     the reference's per-element load/store (differential-tested).
//   General path: vs, err := src.read(n); dst.write(vs) — the reference's
//     exact sequence: lift n elements to host values, lower them into the
//     destination (through the destination's memory + realloc). This is what
//     makes guest↔guest copies across two memories correct: the host value
//     is the intermediary, and indirect payloads (strings, lists, records
//     with pointers) are re-allocated in the destination memory via its own
//     realloc — pointers are never carried across memories. See §4.2.
func copyElements(s *sharedStream, dst, src copyBuffer, n uint32) error
```

### 1.3 The ends — table entries with the 4-state copy machine

```go
// copyState is the reference CopyState.
type copyState uint8

const (
	copyIdle copyState = iota
	copyCopying
	copyCancelling // CANCELLING_COPY
	copyDone
)

// endSide discriminates readable vs writable (the reference uses distinct
// classes; one Go struct + side field keeps the table's type-switch flat).
type endSide uint8

const (
	sideReadable endSide = iota
	sideWritable
)

// streamEnd is ReadableStreamEnd / WritableStreamEnd: a waitable table entry.
type streamEnd struct {
	waitable
	side   endSide
	state  copyState
	shared *sharedStream
}

func (*streamEnd) entryKind() entryKind       { return entryStreamEnd }
func (e *streamEnd) waitablePtr() *waitable   { return &e.waitable }
func (e *streamEnd) copying() bool            { return e.state == copyCopying || e.state == copyCancelling }

// futureEnd is Readable/WritableFutureEnd — identical shape.
type futureEnd struct {
	waitable
	side   endSide
	state  copyState
	shared *sharedFuture
}

func (*futureEnd) entryKind() entryKind     { return entryFutureEnd }
func (e *futureEnd) waitablePtr() *waitable { return &e.waitable }
func (e *futureEnd) copying() bool          { return e.state == copyCopying || e.state == copyCancelling }
```

`resource.go` gains (comment slots already reserved):

```go
const (
	entryResource entryKind = iota
	entryWaitableSet
	entrySubtask
	entryStreamEnd    // *streamEnd  (Phase 2)
	entryFutureEnd    // *futureEnd  (Phase 2)
	entryErrorContext // *errorContext (Phase 2)
)
```

`waitable.go` gains one trivial helper (the reference's `in_waitable_set`):

```go
func (w *waitable) inWaitableSet() bool { return w.wset != nil }
```

**Critical transliteration point — state flips at DELIVERY, not completion.** The
reference's `stream_event`/`future_event` closures mutate `e.state`
(COPYING→IDLE/DONE) when the pending event is *evaluated* (`get_pending_event`), not
when the copy completes. This is load-bearing, not pedantry:

- `stream.drop-readable` on an end whose copy completed but whose event is
  undelivered must still see `state == COPYING` and hit `trap_if(e.copying())` —
  otherwise the pending event would outlive its end (tripping `waitable.dropWaitable`'s
  Phase 1 panic-assert, which stays a panic precisely because this trap fires first).
- `stream.cancel-read` racing a completion must pass its `trap_if(state != COPYING)`
  and then deliver the already-pending COMPLETED event (cancel that lost the race
  reports the completion, not CANCELLED).

So the Go closures mutate end state exactly as the reference does — inside the thunk.

### 1.4 The copy builtins — `stream.read` / `stream.write`

One shared implementation, mirroring `stream_copy(EndT, BufferT, event_code, ...)`.
Bind time (`computeCanonHostFunc`, kinds 0x0f/0x10): resolve `canon.TypeIdx` to
`binary.StreamDesc`, resolve `.Element` to a concrete `elemDesc` (+ precompute
`elemSz`/`align` via `abi.Size`/`abi.Alignment`, `numeric` via a
`noneOrNumberType(elemDesc)` helper), resolve the canon's memory/realloc opts via the
existing `canonMemoryAndRealloc`, and note `async_` (CanonOpt kind 0x06 present in
`canon.Opts`). **Bind-time fail-loud ceiling**: an element type containing
`own`/`borrow` refuses to bind ("stream element containing resource handles is not
supported by this milestone") — `contains_borrow` is a spec assert, and own-in-element
needs per-element table transfer this phase doesn't build (§4.1 item 7).

Core sig `(i: i32, ptr: i32, n: i32) -> i32`:

```go
// stream_builtins.go
func streamCopyHostFunc(in *Instance, side endSide, evCode eventCode,
	elemDesc binary.TypeDesc, elemSz, align uint32, numeric bool,
	async bool, memMod api.Module, reallocFn api.Function) hostFuncDef {

	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, name)                        // trap_if(!may_leave)
		i, ptr, n := u32(stack[0]), u32(stack[1]), u32(stack[2])

		e := requireStreamEnd(in, name, i, side)          // trap_if kind/side
		requireSameElem(e.shared.elem, elemDesc, name)    // trap_if(shared.t != stream_t.t): reflect.DeepEqual on resolved descs
		if e.state != copyIdle { trap("...: stream end is not idle") }
		if e.inWaitableSet() && !async { trap("...: sync copy on an end joined to a waitable set") }

		buf, err := newGuestBuffer(memModOr(mod), realloc, in.resolve,
			elemDesc, elemSz, align, numeric, ptr, n)     // constructor traps
		if err != nil { trap(err) }

		// -- the three closures, verbatim from stream_copy --
		streamEvent := func(result copyResult, reclaim func()) eventTuple {
			reclaim()
			// assert(e.copying())
			if result == copyDropped { e.state = copyDone } else { e.state = copyIdle }
			return eventTuple{evCode, i, packCopyResult(result, buf.progressed())}
		}
		onCopy := func(reclaim func()) { // partial progress: COMPLETED + live reclaim
			e.setPendingEvent(func() eventTuple { return streamEvent(copyCompleted, reclaim) })
		}
		onCopyDone := func(result copyResult) {
			e.setPendingEvent(func() eventTuple { return streamEvent(result, func() {}) })
		}

		e.state = copyCopying
		var cerr error
		if side == sideReadable { cerr = e.shared.read(in, buf, onCopy, onCopyDone)
		} else                  { cerr = e.shared.write(in, buf, onCopy, onCopyDone) }
		if cerr != nil { trap(cerr) }                     // same-instance / element traps

		if !e.hasPendingEvent() {
			if async { stack[0] = blockedSentinel; return } // [BLOCKED]
			// Sync copy: wait_for_pending_event => drive the scheduler until
			// THIS end has an event (counterpart arrives via a host stream op
			// or a queued thunk). Empty queue first => permanent deadlock.
			if derr := in.sched.drive(e.hasPendingEvent); derr != nil { trap(wrap(derr)) }
		}
		ev := e.getPendingEvent() // runs streamEvent: reclaims + flips state
		// assert(ev.code == evCode && ev.p1 == i && ev.p2 != blockedSentinel)
		stack[0] = uint64(ev.p2)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{i32, i32, i32}, results: []api.ValueType{i32}}
}

// packCopyResult: result | progress<<4. result < 2^4, progress <= 2^28-1
// (buffer constructor trap) => never ambiguous.
func packCopyResult(r copyResult, progress uint32) uint32 { return uint32(r) | progress<<4 }
```

`future.read`/`future.write` (kinds 0x16/0x17) are the same skeleton minus `n` (core
sig `(i32, i32) -> i32`, buffer length pinned to 1), minus `onCopy` (no partial
progress), and with the raw result as payload (`futureEvent` returns
`eventTuple{evCode, i, uint32(result)}` — **no** progress shift), plus the two
future-only bits from `future_copy`:

```go
futureEvent := func(result copyResult) eventTuple {
	// assert((buf.remain() == 0) == (result == copyCompleted))
	if result == copyDropped || result == copyCompleted { e.state = copyDone } else { e.state = copyIdle }
	return eventTuple{evCode, i, uint32(result)}
}
onCopyDone := func(result copyResult) {
	// assert(result != DROPPED || evCode == eventFutureWrite):
	// only a WRITER can observe DROPPED (reader gone); a reader never can —
	// the writable end can't be dropped before completing (§1.5).
	e.setPendingEvent(func() eventTuple { return futureEvent(result) })
}
```

Both copy builtins already have their event codes numbered in Phase 1's `waitable.go`
(`eventStreamRead=2 / eventStreamWrite=3 / eventFutureRead=4 / eventFutureWrite=5`);
delivery is entirely Phase 1 machinery — `setPendingEvent` + the WAIT driver /
`waitable-set.wait|poll`. Nothing in `async_lift.go` changes.

### 1.5 `stream.new` / `future.new` / drops / cancels

```go
// stream.new (0x0e), core sig () -> i64: pack both fresh handles.
// canon_stream_new: readable added FIRST (deterministic index order —
// the oracle asserts indices; free-list reuse is guest-observable).
func streamNewHostFunc(in *Instance, elemDesc binary.TypeDesc, elemSz, align uint32, numeric bool) hostFuncDef {
	// requireMayLeave; shared := &sharedStream{elem:..., ...}
	// ri := in.resources.addEntry(&streamEnd{side: sideReadable, state: copyIdle, shared: shared})
	// wi := in.resources.addEntry(&streamEnd{side: sideWritable, state: copyIdle, shared: shared})
	// stack[0] = uint64(ri) | uint64(wi)<<32
}
```

Drops (0x13/0x14/0x1a/0x1b), core sig `(i32) -> ()` — transliterating `drop` +
`CopyEnd.drop` + `WritableFutureEnd.drop`. Validate-then-remove, matching Phase 1's
`waitable-set.drop` ordering (kind mismatch leaves the table untouched):

```go
func streamDropHostFunc(in *Instance, side endSide, elemDesc binary.TypeDesc) hostFuncDef {
	// requireMayLeave
	// e := lookup + kind/side trap + elem-type trap (get, not remove, yet)
	// trap_if(e.copying())              // CopyEnd.drop — covers the undelivered-
	//                                   // event case too: state flips only at
	//                                   // delivery (§1.3), so a completed-but-
	//                                   // undelivered copy is still COPYING here.
	// e.shared.drop()                   // counterpart's parked copy => DROPPED event
	// e.dropWaitable()                  // panic-assert !hasPendingEvent (unreachable: above)
	// in.resources.removeEntry(i)
}

// futureDropHostFunc(side == sideWritable) adds WritableFutureEnd.drop's
// EXTRA gate BEFORE the CopyEnd logic:
//   trap_if(e.state != copyDone)
// A future writer may not walk away without either completing its write or
// having observed DROPPED — this is what makes SharedFutureImpl.read's
// assert(!dropped) and future_copy's reader-never-sees-DROPPED assert
// unreachable. The readable future end and both stream ends drop from any
// non-copying state.
```

Cancels (0x11/0x12/0x18/0x19), core sig `(i32) -> i32`, transliterating
`cancel_copy` (the `async_` flag is `canon.Async`, decoded in Phase 0):

```go
func cancelCopyHostFunc(in *Instance, /*stream|future*/ side endSide,
	evCode eventCode, elemDesc binary.TypeDesc, async bool) hostFuncDef {
	// requireMayLeave; e := lookup + kind/side/elem traps
	// trap_if(e.state != copyCopying)   // has_sync_waiter conjunct: omitted —
	//                                   // no sync waiter can exist while guest
	//                                   // code runs (single-threaded; the sync
	//                                   // copy path is inside sched.drive)
	// trap_if(e.inWaitableSet() && !async)
	// e.state = copyCancelling
	// if !e.hasPendingEvent() {         // no completion racing us
	//     e.shared.cancel()             // resetAndNotifyPending(CANCELLED) fires
	//                                   // OUR parked buffer's onCopyDone =>
	//                                   // installs OUR pending event
	//     if !e.hasPendingEvent() {     // host end deferred its cancel ack (§3):
	//         if async { return BLOCKED }
	//         drive(e.hasPendingEvent)  // wait_for_pending_event
	//     }
	// }
	// ev := e.getPendingEvent()         // flips state via the event thunk
	// // assert(!e.copying() && ev.code == evCode && ev.p1 == i)
	// stack[0] = uint64(ev.p2)          // CANCELLED|progress<<4, or the
	//                                   // COMPLETED/DROPPED result that won the race
}
```

Note the semantics that fall out (and must be tested): cancel after the counterpart
already rendezvoused returns COMPLETED with the real progress — cancellation *lost*;
cancel of a parked never-matched copy returns `CANCELLED | progress<<4` with whatever
partial progress an earlier `onCopy` had recorded; a guest-guest pair can never hit
the BLOCKED arm (`sharedStream.cancel` always notifies synchronously) — only a host
end that defers its cancel acknowledgment can (§3.4).

---

## 2. Stream/future VALUE lift/lower — handle transfer between instances

### 2.1 What the reference does

`lift_stream`/`lift_future` (~1513): **remove** the handle from the sender's table;
trap unless it names a *readable* end, element types match, `state == IDLE`, and the
end is not in a waitable set. The lifted host-level value is the *shared* object.
`lower_stream`/`lower_future` (~1817): wrap the shared object in a **fresh readable
end** added to the receiver's table. The writable end never moves. This is exactly the
shape of wazy's existing resource own-transfer (handle → identity → new handle in the
receiver's table), with `*sharedStream` playing the rep's role.

### 2.2 What changes in `abi` vs `instance`

**`abi`: nothing.** Phase 0 already made `StreamDesc`/`FutureDesc` flatten, load,
store, and plan-compile as opaque i32 handles alongside `OwnDesc`/`BorrowDesc`
(`plan.go:51/154`, `flat.go:103/692`, `memory.go:107/651`, `layout.go:59/100`); the
lifted Go value at the `abi` layer stays `uint32`. The table swap is an *instance*
concern, in the same four switches that already do it for resources:

| Seam (existing fn) | New cases |
|---|---|
| `host_import.go resolveHandleArg` — guest handle → host value, import args | `StreamDesc`: `takeReadableStreamEnd(resources, elemDesc, h)` → `*sharedStream`, wrapped as `*StreamReader` (§3). `FutureDesc` → `*FutureReader`. `ErrorContextDesc` → `*ErrorContextValue` (get, **not** remove — §2.3). |
| `host_import.go allocHandleResult` — host value → guest handle, import results | `StreamDesc`: `v.(*StreamReader)` (or a writer-created reader arg) → `resources.addEntry(&streamEnd{side: sideReadable, shared: ...})`. Same for future / error-context (`addEntry(&errorContext{...})`). |
| `composition.go providerHandleToRep` / `repToProviderHandle` — guest↔guest delegation | Same pair: the provider's returned readable-end handle reduces to `*sharedStream` (lift traps included), re-minted in the importer's table — the mirror of the `TakeOwn`/`NewOwn` lines at `composition.go:236/260`. |
| `instance.go resolveArgHandlesDepth` — `Call`/`CallAsync` args | `StreamDesc`/`FutureDesc` cases accepting the host API values (§3), minting the readable end in the callee's table. |

The lift-side guard, once, used by all of the above:

```go
// stream.go
// takeReadableStreamEnd transliterates lift_async_value: REMOVE i from t,
// trapping unless it is a readable stream end of elem type elemDesc, idle,
// and not joined to a waitable set. Returns the shared object.
func takeReadableStreamEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*sharedStream, error) {
	// getEntry; e.(*streamEnd); side == sideReadable; DeepEqual(elem);
	// trap_if(e.state != copyIdle); trap_if(e.inWaitableSet());
	// then removeEntry (validate-before-remove, Phase 1 convention).
}
// takeReadableFutureEnd: identical for *futureEnd.
```

Phase-2 ceiling, fail-loud at bind: stream/future/error-context **nested inside
composite types** (a `record` field, `list<stream<u8>>`, …) is rejected when a bound
export/import's param or result type contains one below top level. The abi layer
would lift them as bare uint32s with no table transfer — silently wrong — so the bind
check (a `typeContainsAsyncValue` walk, sibling of `typeContainsResource`) refuses
loudly. Top-level params/results cover wit-bindgen's actual output for 0.3-style
signatures (`func(body: stream<u8>) -> stream<u8>`).

### 2.3 Error-context values

```go
// errorcontext.go
type errorContext struct {
	debugMessage string
}

func (*errorContext) entryKind() entryKind { return entryErrorContext }
```

- `error-context.new` (0x1c), `(ptr, len) -> i32`: load the message string from the
  canon's memory opt (bounds + UTF-8 traps per `load_string_from_range`), then
  `addEntry(&errorContext{debugMessage: s})`. Deviation, pinned: the reference's
  DETERMINISTIC_PROFILE stores `''` and **skips the load entirely** (no traps); wazy
  always loads (traps live) and keeps the message — this is the spec's sanctioned
  "host-defined transformation" (= identity) on the non-deterministic branch. The
  oracle must monkeypatch that branch choice + identity transformation for parity
  (plan §6's determinism pins gain one entry).
- `error-context.debug-message` (0x1d), `(i, ptr) -> ()`: kind-trap lookup;
  `store_string(cx, msg, ptr)` — the guest passes a buffer via realloc-capable
  opts; reuse `abi.Store` with the canon's memory/realloc.
- `error-context.drop` (0x1e), `(i) -> ()`: kind-trap + `removeEntry`.
- Value lift/lower: `lift_error_context` is a **get** (the sender KEEPS its handle —
  unlike streams) and lower is an `addEntry` sharing the same `*errorContext`. Copy
  semantics, two live handles, each dropped independently.

---

## 3. The host-facing API — `StreamReader` / `StreamWriter` / future twins

This is what makes streams usable before WASI 0.3: host code is simply *the other
end* of a `sharedStream`, using `hostBuffer`s instead of `guestBuffer`s. No new
scheduler machinery — a host op either rendezvouses immediately (copies against the
parked guest buffer and installs the guest end's pending event) or parks its host
buffer (backpressure). The runq is NOT involved in the rendezvous itself; it remains
what Phase 1 made it: the place *deferred host work* lives. `setPendingEvent` on a
guest end IS the readiness signal the WAIT driver's predicate reads.

### 3.1 Types and constructors

```go
// stream_host.go  (exported via the component package facade, like AsyncCall)

// StreamWriter is the host-held writable end of a stream whose readable end
// lives (or will live) in a guest table. NOT a table entry, NOT a waitable:
// completion surfaces as Go callbacks, not events.
type StreamWriter struct {
	in     *Instance
	shared *sharedStream
	state  copyState // the same 4-state machine, for the host's own end
}

type StreamReader struct {
	in     *Instance
	shared *sharedStream
	state  copyState
}

type FutureWriter struct { in *Instance; shared *sharedFuture; state copyState }
type FutureReader struct { in *Instance; shared *sharedFuture; state copyState }

// NewStream creates a stream pair for passing data INTO or OUT OF the guest:
// the host keeps one end; the returned abi.Value is the READABLE end to hand
// to the guest (as a Call/CallAsync arg, or an async-import Resolve result —
// both routes lower it via §2.2's seams). elem follows WithImport's TypeDesc
// convention; nil elem = bare stream.
func (in *Instance) NewStream(elem binary.TypeDesc) (*StreamWriter, abi.Value, error)
func (in *Instance) NewFuture(elem binary.TypeDesc) (*FutureWriter, abi.Value, error)

// Guest-created streams arrive at the host pre-wrapped: an async (or sync)
// import whose param type is stream<T> receives args[i].(*StreamReader);
// a future<T> param arrives as *FutureReader (resolveHandleArg, §2.2).
```

(`NewStream` computes `elemSz`/`align`/`numeric` from `elem` exactly as the builtin
bind path does, and applies the same no-resource-in-element bind refusal.)

### 3.2 The single-threaded legality guard

Streams outlive individual calls, so the guard is wider than `AsyncCall.Resolve`'s
but the same structural idea — no goroutine-ID hacks:

```go
// requireSchedulable panics unless we are provably on the instance's driving
// goroutine: (a) no guest task is active (between Calls — host code path),
// or (b) a scheduler thunk is executing (sched.pumping), or (c) a host
// import invocation is on the stack (in.inHostCall, a counter the sync and
// async wrappers already bracket their fn calls with — one int on Instance).
// A live activeTask WITHOUT (b)/(c) means "called concurrently from another
// goroutine while the guest runs" — the one illegal shape.
func (in *Instance) requireSchedulable(op string) {
	if in.activeTask != nil && !in.sched.pumping && in.inHostCall == 0 {
		panic(fmt.Errorf("component/instance: %s called outside the instance scheduler while a task is active; external completion requires CallAsync (not yet implemented)", op))
	}
}
```

(This adds one `inHostCall++/--` pair to `buildHostWrapper`/`buildAsyncHostWrapper` —
the only Phase 1 file edit beyond additive cases, two lines each.)

### 3.3 Write / Read — signatures and control flow

Completion-callback shape, mirroring `OnCopyDone` — a blocking host `Write` is
impossible to give sound semantics inside a scheduler thunk (it would nest `drive`
under itself waiting on guest progress that requires the thunk to return):

```go
// Write offers vals to the stream. onDone fires EXACTLY ONCE with the
// result and the number of elements actually transferred:
//   - immediately (before Write returns), if a guest read is parked
//     (rendezvous) or the readable end was dropped (copyDropped, n=0);
//   - later, from inside a guest stream.read builtin, when the guest
//     finally reads (the host buffer was parked — backpressure).
// A second Write before onDone fires panics (state != idle): rendezvous
// streams have no host-side queue, BY DESIGN (plan §Phase-2).
func (w *StreamWriter) Write(vals []abi.Value, onDone func(res CopyResult, n uint32)) {
	w.in.requireSchedulable("StreamWriter.Write")
	if w.state != copyIdle { panic("Write while a previous Write is pending") }
	buf := &hostBuffer{vals: vals, length: uint32(len(vals))}
	w.state = copyCopying
	onCopy := func(reclaim func()) { reclaim() } // host needs no deferred reclaim:
	//   partial progress is visible in buf.progress; the guest side's event
	//   discipline doesn't apply to a callback-based host end.
	err := w.shared.write(nil /*host inst*/, buf, onCopy, func(res copyResult) {
		w.state = copyIdle
		if res == copyDropped { w.state = copyDone }
		onDone(CopyResult(res), buf.progress)
	})
	if err != nil { panic(err) } // element lower trap: host handed a bad value
}

// Close drops the writable end: a parked or future guest read completes with
// DROPPED; subsequent guest reads get DROPPED immediately. Idempotent.
func (w *StreamWriter) Close() {
	w.in.requireSchedulable("StreamWriter.Close")
	if w.copying() { panic("Close during a pending Write; Cancel it first") } // trap_if(e.copying())
	w.shared.drop()
	w.state = copyDone
}

// Cancel revokes a parked Write. onDone (from the pending Write) fires with
// CANCELLED and the partial progress. No-op if the Write already completed.
func (w *StreamWriter) Cancel() {
	w.in.requireSchedulable("StreamWriter.Cancel")
	if w.state != copyCopying { return }
	w.state = copyCancelling
	if w.shared.pendingBuffer != nil { w.shared.cancel() } // fires our onDone
}

// Read requests up to n elements. Same one-shot/park contract as Write.
func (r *StreamReader) Read(n uint32, onDone func(res CopyResult, vals []abi.Value))
func (r *StreamReader) Close() // drop readable end: parked guest write => DROPPED
func (r *StreamReader) Cancel()

// Futures: one element, one shot.
//   Set completes immediately if a guest read is parked; otherwise parks
//   until the guest reads (or reports DROPPED if the reader closed).
func (w *FutureWriter) Set(v abi.Value, onDone func(res CopyResult))
//   Close: ONLY legal after Set completed or reported DROPPED
//   (WritableFutureEnd.drop's trap_if(state != DONE), §1.5) — panic otherwise.
func (w *FutureWriter) Close()
func (r *FutureReader) Get(onDone func(res CopyResult, v abi.Value))
func (r *FutureReader) Close()

// CopyResult is the public mirror of copyResult.
type CopyResult uint32
const (
	CopyCompleted CopyResult = 0
	CopyDropped   CopyResult = 1
	CopyCancelled CopyResult = 2
)
```

### 3.4 End-to-end traces (the acceptance shapes)

**Host feeds a guest export a stream** (`export process: async func(in: stream<u8>) -> u32`):

1. `sw, readable, _ := inst.NewStream(u8)`; host calls
   `Call(ctx, "process", readable)`.
2. `resolveArgHandlesDepth` StreamDesc case mints the readable `streamEnd` in the
   guest table (index observable); lowers as i32.
3. Guest task starts, calls `stream.read(h, ptr, 4096)` (async opt) → no pending
   buffer → guest buffer parks → `[BLOCKED]`; guest joins end to its wset, returns
   `WAIT|wset<<4`.
4. Driver enters `drive(ws.hasPendingEvent)` → runq empty → **deadlock trap?** No:
   the host must have arranged progress *before* Call, or via an import. The
   supported MVP shape is (a) host pre-`Write`s before `Call` (values park, step 3
   rendezvouses immediately: `stream.read` returns `COMPLETED|n<<4` synchronously,
   no WAIT), or (b) the guest's read is answered from a host import the guest
   awaits, whose `AsyncCall.Defer` thunk calls `sw.Write(...)` — the thunk runs
   inside `drive`, the Write rendezvouses with the parked guest buffer, installs
   `(STREAM_READ, h, COMPLETED|n<<4)` on the guest end, pred flips, callback fires.
   Truly external production remains CallAsync's job (Phase 3) — same honest
   boundary as Phase 1, same deadlock trap text pointing at it.
5. Guest drains, host `sw.Close()` → next guest read gets `DROPPED` (state→DONE) →
   guest `stream.drop-readable` → `task.return` → EXIT.

**Guest hands the host a stream** (`import sink: async func(out: stream<u8>)`):
guest `stream.new` (i64 → two handles) → passes readable in the async-lowered import
→ wrapper's `resolveHandleArg` removes the readable end (lift traps enforced) and the
`AsyncHostFunc` receives `args[0].(*StreamReader)` → host `r.Read(n, onDone)` parks a
host buffer → guest `stream.write(wh, ptr, n)` rendezvouses: elements lift from guest
memory to host values into the host buffer, `onDone(COMPLETED, vals)` fires *inside
the guest's stream.write builtin*, and the guest's write completes synchronously with
`COMPLETED|n<<4`. Backpressure is visible to the guest as `BLOCKED` writes whenever
the host hasn't posted a Read.

---

## 4. Keep / trap / omit — Phase 2 specifics

### 4.1 Disposition table (Phase 2 items only; Phase 1 rows unchanged)

| Reference item | Disposition | Why |
|---|---|---|
| `SharedStreamImpl` single pending buffer (rendezvous, no elastic buffer) | **KEEP verbatim** | The whole backpressure model. Any host-side queue would change guest-observable BLOCKED/COMPLETED sequences and break oracle parity. |
| `CopyState` 4 states incl. CANCELLING_COPY | **KEEP** | CANCELLING is guest-observable: a cancel that races completion must deliver COMPLETED (state machine is how). |
| State flips **inside** the event thunk (stream_event/future_event) | **KEEP — the subtlest transliteration point** | Makes drop-with-undelivered-event trap via `trap_if(copying())` and cancel-vs-completion race resolve correctly (§1.3). Getting this "eager" would be silently wrong on 3 trap edges. |
| `pending_on_copy` / `ReclaimBuffer` (partial-progress re-arm) | **KEEP for guest ends; collapse for host ends** | Guest side: the parked reader's event must carry live progress and the reclaim must run before payload read (reference order). Host side: callbacks report progress directly; reclaim is called inline (§3.3) — pure simplification, no observable difference (host ends have no event thunks). |
| `Waitable.wait_for_pending_event` (sync copy park) | **COLLAPSE → `sched.drive(e.hasPendingEvent)`** | The Phase-1 translation applied to a single waitable. Deadlock ⇒ trap (Phase 1 deviation #2 extends to sync copies/cancels). |
| `has_sync_waiter` (+ its conjunct in cancel's trap and `waitable.join`'s trap) | **OMIT (already omitted in Phase 1)** | A sync waiter exists only *while* `drive` runs, during which no guest code executes single-threaded — the trap conjunct is identically false. Comment at both sites. |
| Zero-length handshake (write-side elif; read-side absence of it) | **KEEP verbatim, asymmetry included** | Spec's readiness-signaling idiom (0-length write probes a parked 0-length read without waking it). Named tests both directions. |
| `trap_if(inst is pending_inst and not none_or_number_type)` ("temporary") | **KEEP** | Guest-reachable trap; spec marks it temporary, so isolate in one predicate for easy removal. Host ends (`inst == nil`) exempt by construction, as in the reference. |
| Buffer ctor traps (align / bounds / 2^28) | **KEEP** (checked against *current* memory at call time) | Real trap edges; also what makes `progress<<4` packing sound. |
| `BufferGuestImpl` lift-to-host-values copy | **KEEP as the general path** + numeric memmove fast path | §4.2. |
| `WritableFutureEnd.drop` `trap_if(state != DONE)` | **KEEP** | Load-bearing: it is the only thing making `SharedFutureImpl.read`'s `assert(!dropped)` and the reader-never-sees-DROPPED assert unreachable. Those stay Go panic-asserts with the unreachability argument in comments. |
| `future_copy`'s completed/dropped ⇒ end state DONE | **KEEP** | Second read/write on a used future end must trap `state != IDLE`. |
| Host-deferred cancel acknowledgment (cancel's inner BLOCKED/wait arm) | **KEEP the arm; wazy host ends ack synchronously** | Guest-guest pairs can't reach it (sharedStream.cancel always notifies); wazy's §3 host ends also ack inline, so the arm is currently dead — kept because WASI 0.3 host streams (Phase 5) will defer, and the oracle exercises the branch via a scripted host. |
| `ErrorContext` deterministic-profile empty message | **DEVIATE — keep the real message** | Identity is a legal host transformation on the spec's own non-deterministic branch; enormously better diagnostics. Oracle pins the branch (§2.3). |
| Stream/future in composite types | **OMIT, fail-loud at bind** | No silent-wrong uint32 pass-through (§2.2). |
| own/borrow inside stream elements | **OMIT, fail-loud at bind** | borrow is a spec assert; own needs per-element table transfer — Phase 3+ if ever demanded. |

### 4.2 The element-copy-through-two-memories concern, resolved

For guest↔guest copies the source and destination are **different linear memories**
(and possibly different string encodings once more encodings land). The reference's
answer, which we transliterate, is that the host value is always the intermediary:
`ReadableBufferGuestImpl.read` *lifts* n elements out of the source memory
(`load_list_from_valid_range`) and `WritableBufferGuestImpl.write` *lowers* them into
the destination (`store_list_into_valid_range`) with the destination's cx — its
memory, its realloc. Consequences wazy inherits deliberately:

1. **Indirect payloads re-allocate in the destination.** A `stream<string>` element's
   bytes are copied into a fresh destination-realloc'd block; pointers never cross
   memories. This falls out of `abi.Store` + the destination `guestBuffer`'s realloc —
   zero new abi code.
2. **Lift traps are source-side, lower traps destination-side**, surfacing inside the
   copy builtin of whichever side *completed* the rendezvous (the second arriver) —
   matching the reference, where the trap happens inside the `read`/`write` call of
   the arriving side.
3. **The numeric fast path is exactly the spec's own equivalence class.**
   `none_or_number_type` types have identical, alignment-stable byte layouts in every
   memory, so `copy(dst[dp:], src[sp:sp+n*sz])` is bit-equivalent to per-element
   lift/lower. Gating the memmove on the *same predicate the spec uses* for the
   same-instance exemption means we never fast-path a type the spec considers
   layout-ambiguous. `stream<u8>` — the case that matters for 0.3 I/O — is one
   memmove. (bool/char are NOT in the class: bool must renormalize to 0/1 and char
   must validate scalar range, so they take the general path, where `abi.Load`
   already does both.)
4. **Memory growth between park and rendezvous** is handled the same way Phase 1's
   resolve-time lowering pinned: `guestBuffer` holds `api.Module`, never `[]byte`;
   bytes are fetched fresh inside `read`/`write`. The parked side's ptr/bounds were
   validated at park time against a memory that can only have grown since.

### 4.3 Forced changes to shipped code (honest list)

- `resource.go`: +3 `entryKind` constants. Zero behavior change.
- `waitable.go`: +`inWaitableSet()` (3 lines).
- `graph.go computeCanonHostFunc`: +17 additive cases (0x0e–0x1e); stream/future
  cases need `canon.TypeIdx`→StreamDesc/FutureDesc resolution + `canonMemoryAndRealloc`
  (both exist).
- `host_import.go` / `async_host_import.go`: +cases in `resolveHandleArg` /
  `allocHandleResult`; +`inHostCall` bracket (2 lines/wrapper) for §3.2's guard.
- `composition.go`: +cases in the two rep-transfer helpers.
- `instance.go resolveArgHandlesDepth`: +Stream/Future cases; +`typeContainsAsyncValue`
  bind walk.
- `abi` package: **no changes**. `binary` package: **no changes** (Phase 0 decoded
  everything Phase 2 executes, including `Canon.Async` for cancels and the i64
  return of `{stream,future}.new`).
- Phase 1 async driver (`async_lift.go`, `sched.go`, `task.go`): **untouched**.

### 4.4 Test hooks (the named-trap inventory this design implies)

Every `trap_if` row above gets a named test; the non-obvious ones: end-not-idle
double-read; sync copy while joined; drop-readable with undelivered completion
(must trap `copying`, not panic the waitable assert); cancel-vs-completion race
delivers COMPLETED; zero-length write handshake (parked read survives) and its
read-side asymmetry; intra-instance `stream<string>` trap vs cross-instance success
(two-memory string re-alloc verified byte-for-byte); `future.write` DROPPED after
reader Close; `FutureWriter.Close` before Set panics; guest handle-index reuse after
`drop-readable` (free-list observability); `BLOCKED` sentinel on async copy; deadlock
trap on sync read with empty runq; lift traps (transfer a writable end / a copying
readable end / elem mismatch); error-context bounds + UTF-8 traps; host `Write` twice
panics; host op from a raw goroutine while a task is active panics.

Oracle scenarios (plan §6 harness): scripted two-instance compositions driving
`stream.read/write` interleavings through definitions.py with the deterministic
monkeypatches + a scripted host end, diffing event tuples, packed payloads, and table
indices against wazy.
