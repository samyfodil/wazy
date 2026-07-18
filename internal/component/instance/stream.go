package instance

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file implements Phase 2's stream/future runtime: the shared rendezvous
// objects (sharedStream/sharedFuture), the per-instance "end" table entries
// (streamEnd/futureEnd), and the copyBuffer abstraction over a guest's linear
// memory (guestBuffer) or host Go values (hostBuffer). See
// docs/component-model-async-runtime-design.md §0-§1 for the design this
// transliterates from testdata/definitions.py's Buffer (~907), SharedStreamImpl
// (~986), CopyEnd (~1069), SharedFutureImpl (~1108), stream_copy (~2526),
// future_copy (~2580), cancel_copy (~2632), drop (~2666).

// maxBufferLen is the reference Buffer.MAX_LENGTH = 2^28 - 1. This also
// guarantees progress<<4 in packCopyResult cannot overflow 32 bits (progress
// is always <= maxBufferLen).
const maxBufferLen = 1<<28 - 1

// blockedSentinel is the reference BLOCKED = 0xffff_ffff: the i32 an
// async-opt copy/cancel builtin returns when the operation parked instead of
// completing.
const blockedSentinel = 0xffff_ffff

// copyResult is the reference CopyResult.
type copyResult uint32

const (
	copyCompleted copyResult = 0
	copyDropped   copyResult = 1
	copyCancelled copyResult = 2
)

// copyBuffer is the reference Buffer: one side of one read/write call.
// remain/progress are element counts, not bytes.
type copyBuffer interface {
	remain() uint32
	isZeroLength() bool // length == 0 (NOT remain == 0)
	progressed() uint32 // .progress -- for packCopyResult
	// read consumes n elements (n <= remain) as host values.
	read(n uint32) ([]abi.Value, error)
	// write appends host values (len(vs) <= remain).
	write(vs []abi.Value) error
}

// guestBuffer is the reference BufferGuestImpl (+Readable/Writable, which
// differ only in which of read/write is legal -- misuse here is a
// programming bug caught by the copy direction, not a guest-reachable
// state). memMod is fetched FRESH at every access (see read/write) since
// memory can grow between the parking call and the rendezvous.
type guestBuffer struct {
	memMod  api.Module      // the owning instance's module (for fresh memory bytes)
	realloc abi.Realloc     // dst-side realloc for indirect element payloads
	resolve abi.Resolver    // the owning instance's resolver
	elem    binary.TypeDesc // nil for a bare stream/future (no element)
	elemSz  uint32          // 0 when elem == nil
	numeric bool            // noneOrNumberType(elem) -- the memmove fast path

	ptr      uint32 // advances elemSz per element copied
	length   uint32
	progress uint32
}

// newGuestBuffer transliterates BufferGuestImpl.__init__'s traps:
//
//	trap_if(length > maxBufferLen)
//	if elem != nil && length > 0:
//	  trap_if(ptr != align_to(ptr, alignment(elem)))
//	  trap_if(ptr + length*elemSz > len(mem))   // checked against CURRENT mem
func newGuestBuffer(memMod api.Module, realloc abi.Realloc, resolve abi.Resolver, elem binary.TypeDesc, elemSz, align uint32, numeric bool, ptr, length uint32) (*guestBuffer, error) {
	if length > maxBufferLen {
		return nil, fmt.Errorf("stream/future buffer: length %d exceeds the maximum (%d)", length, maxBufferLen)
	}
	if elem != nil && length > 0 {
		mem, ok := memoryBytesOf(memMod)
		if !ok {
			return nil, fmt.Errorf("stream/future buffer: calling module has no memory")
		}
		if align == 0 {
			align = 1
		}
		if ptr != abi.Align(ptr, align) {
			return nil, fmt.Errorf("stream/future buffer: pointer %d not aligned to %d", ptr, align)
		}
		need := uint64(ptr) + uint64(length)*uint64(elemSz)
		if need > uint64(len(mem)) {
			return nil, fmt.Errorf("stream/future buffer: bounds: ptr=%d length=%d elemSz=%d mem_len=%d", ptr, length, elemSz, len(mem))
		}
	}
	return &guestBuffer{memMod: memMod, realloc: realloc, resolve: resolve, elem: elem, elemSz: elemSz, numeric: numeric, ptr: ptr, length: length}, nil
}

func (b *guestBuffer) remain() uint32     { return b.length - b.progress }
func (b *guestBuffer) isZeroLength() bool { return b.length == 0 }
func (b *guestBuffer) progressed() uint32 { return b.progress }

// read is ReadableBufferGuestImpl.read: elem==nil produces n empty tuples
// (represented as nil abi.Value each); else Load each element at
// b.ptr+i*elemSz, advancing ptr, progress += n.
func (b *guestBuffer) read(n uint32) ([]abi.Value, error) {
	if b.elem == nil {
		b.progress += n
		return make([]abi.Value, n), nil
	}
	mem, ok := memoryBytesOf(b.memMod)
	if !ok {
		return nil, fmt.Errorf("stream/future buffer read: calling module has no memory")
	}
	out := make([]abi.Value, n)
	for i := uint32(0); i < n; i++ {
		v, err := abi.Load(mem, b.ptr, b.elem, b.resolve)
		if err != nil {
			return nil, fmt.Errorf("stream/future buffer read: element %d: %w", i, err)
		}
		out[i] = v
		b.ptr += b.elemSz
	}
	b.progress += n
	return out, nil
}

// write is WritableBufferGuestImpl.write: Store each element (realloc for
// indirect payloads), advancing ptr, progress += len(vs).
func (b *guestBuffer) write(vs []abi.Value) error {
	if b.elem == nil {
		b.progress += uint32(len(vs))
		return nil
	}
	mem, ok := memoryBytesOf(b.memMod)
	if !ok {
		return fmt.Errorf("stream/future buffer write: calling module has no memory")
	}
	for i, v := range vs {
		if err := abi.Store(mem, b.ptr, b.elem, v, b.resolve, b.realloc); err != nil {
			return fmt.Errorf("stream/future buffer write: element %d: %w", i, err)
		}
		b.ptr += b.elemSz
	}
	b.progress += uint32(len(vs))
	return nil
}

// hostBuffer is the host-side buffer (stream_host.go): values live in Go, no
// memory involved.
type hostBuffer struct {
	vals     []abi.Value // read side: source; write side: filled up to length
	progress uint32
	length   uint32
}

// newHostReadBuffer wraps host values as the SOURCE side of a host Write.
func newHostReadBuffer(vals []abi.Value) *hostBuffer {
	return &hostBuffer{vals: vals, length: uint32(len(vals))}
}

// newHostWriteBuffer allocates the DESTINATION side of a host Read requesting
// up to n elements.
func newHostWriteBuffer(n uint32) *hostBuffer { return &hostBuffer{length: n} }

func (b *hostBuffer) remain() uint32     { return b.length - b.progress }
func (b *hostBuffer) isZeroLength() bool { return b.length == 0 }
func (b *hostBuffer) progressed() uint32 { return b.progress }

func (b *hostBuffer) read(n uint32) ([]abi.Value, error) {
	if b.progress+n > uint32(len(b.vals)) {
		return nil, fmt.Errorf("host stream buffer: read past the end (progress=%d n=%d have=%d)", b.progress, n, len(b.vals))
	}
	out := b.vals[b.progress : b.progress+n]
	b.progress += n
	return out, nil
}

func (b *hostBuffer) write(vs []abi.Value) error {
	if b.progress+uint32(len(vs)) > b.length {
		return fmt.Errorf("host stream buffer: write past capacity (progress=%d n=%d cap=%d)", b.progress, len(vs), b.length)
	}
	b.vals = append(b.vals, vs...)
	b.progress += uint32(len(vs))
	return nil
}

// onCopyFn / onCopyDoneFn are the reference OnCopy/OnCopyDone callback types.
// reclaim is the reference ReclaimBuffer: the shared object hands the parked
// side a thunk that un-parks its pending buffer; it MUST be called before the
// event payload is read (stream_event calls reclaim_buffer() first).
type (
	onCopyFn     func(reclaim func())
	onCopyDoneFn func(result copyResult)
)

// sharedStream is the reference SharedStreamImpl: the rendezvous point. At
// most ONE pending buffer at a time -- whichever side calls read/write first
// parks; the second side copies directly between the two buffers. No elastic
// host buffer exists, so backpressure is structural.
type sharedStream struct {
	elem    binary.TypeDesc // resolved element type (nil = bare stream)
	elemSz  uint32
	align   uint32
	numeric bool // noneOrNumberType(elem)

	dropped bool

	// The pending side. pendingInst identifies the guest instance whose
	// buffer is parked (nil for a host buffer) -- consumed ONLY by the
	// "temporary" same-instance trap below.
	pendingInst       *Instance
	pendingBuffer     copyBuffer
	pendingOnCopy     onCopyFn
	pendingOnCopyDone onCopyDoneFn
}

func (s *sharedStream) resetPending() { s.setPending(nil, nil, nil, nil) }

func (s *sharedStream) setPending(inst *Instance, b copyBuffer, oc onCopyFn, ocd onCopyDoneFn) {
	s.pendingInst, s.pendingBuffer, s.pendingOnCopy, s.pendingOnCopyDone = inst, b, oc, ocd
}

// resetAndNotifyPending clears pending BEFORE invoking the callback
// (reference order -- the callback may immediately re-park).
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

// write is a writable-end (or host-writer) call with src as the source
// buffer. Branch-for-branch transliteration of stream_copy's write side.
func (s *sharedStream) write(inst *Instance, src copyBuffer, onCopy onCopyFn, onCopyDone onCopyDoneFn) error {
	switch {
	case s.dropped:
		onCopyDone(copyDropped)

	case s.pendingBuffer == nil:
		s.setPending(inst, src, onCopy, onCopyDone) // park; rendezvous later

	default:
		// trap_if(inst is pending_inst and not none_or_number_type(t)): the
		// spec's TEMPORARY same-instance restriction -- an intra-instance
		// copy of a non-numeric element would alias the source and
		// destination memory mid-lift. inst==nil (host side) never trips it.
		if inst != nil && inst == s.pendingInst && !s.numeric {
			return errors.New("cannot read from and write to intra-component stream (element type requires cross-memory lift/lower, spec restriction)")
		}
		if s.pendingBuffer.remain() > 0 {
			if src.remain() > 0 {
				n := min(src.remain(), s.pendingBuffer.remain())
				if err := copyElements(s.numeric, s.pendingBuffer, src, n); err != nil {
					return err // element lift/lower trap
				}
				// Notify the parked reader mid-copy: it re-arms or reclaims.
				s.pendingOnCopy(s.resetPending)
			}
			onCopyDone(copyCompleted) // the WRITER completes immediately
		} else if src.isZeroLength() && s.pendingBuffer.isZeroLength() {
			// Zero-length handshake, writer side: a 0-length write against a
			// parked 0-length read COMPLETEs the write WITHOUT waking the
			// reader -- the read stays parked as the "ready to receive"
			// signal.
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

// read is the mirror image of write: dst receives, s.pendingBuffer is the
// parked WRITER's source. NOTE: read has NO zero-length elif -- a 0-length
// read against a parked 0-length write takes the remain()==0 else-branch
// (completes the parked writer, parks the reader). Reference asymmetry, kept
// verbatim.
func (s *sharedStream) read(inst *Instance, dst copyBuffer, onCopy onCopyFn, onCopyDone onCopyDoneFn) error {
	switch {
	case s.dropped:
		onCopyDone(copyDropped)

	case s.pendingBuffer == nil:
		s.setPending(inst, dst, onCopy, onCopyDone)

	default:
		if inst != nil && inst == s.pendingInst && !s.numeric {
			return errors.New("cannot read from and write to intra-component stream (element type requires cross-memory lift/lower, spec restriction)")
		}
		if s.pendingBuffer.remain() > 0 {
			if dst.remain() > 0 {
				n := min(dst.remain(), s.pendingBuffer.remain())
				if err := copyElements(s.numeric, dst, s.pendingBuffer, n); err != nil {
					return err
				}
				s.pendingOnCopy(s.resetPending)
			}
			onCopyDone(copyCompleted)
		} else {
			s.resetAndNotifyPending(copyCompleted)
			s.setPending(inst, dst, onCopy, onCopyDone)
		}
	}
	return nil
}

// copyElements moves n elements from src to dst.
//   - Fast path: numeric element AND both ends guestBuffers => a single
//     copy(dstMem[dp:dp+n*sz], srcMem[sp:sp+n*sz]) -- numeric layouts are
//     byte-identical in every memory; observably equivalent to the
//     reference's per-element load/store.
//   - General path: vs, err := src.read(n); dst.write(vs) -- lift n elements
//     to host values, lower them into the destination (through the
//     destination's memory + realloc). This is what makes guest<->guest
//     copies across two memories correct: the host value is the
//     intermediary, and indirect payloads (strings, lists, records with
//     pointers) are re-allocated in the destination memory via its own
//     realloc -- pointers are never carried across memories.
func copyElements(numeric bool, dst, src copyBuffer, n uint32) error {
	if numeric {
		if dstG, ok := dst.(*guestBuffer); ok {
			if srcG, ok2 := src.(*guestBuffer); ok2 {
				return copyElementsMemmove(dstG, srcG, n)
			}
		}
	}
	vs, err := src.read(n)
	if err != nil {
		return err
	}
	return dst.write(vs)
}

// copyElementsMemmove is copyElements' numeric fast path between two guest
// buffers (possibly in different linear memories).
func copyElementsMemmove(dst, src *guestBuffer, n uint32) error {
	if n == 0 {
		return nil
	}
	nb := n * dst.elemSz
	// A bare stream/future (elem == nil, elemSz == 0) carries no payload
	// bytes at all -- nb == 0 regardless of n or the pointer values, which
	// per the reference (newGuestBuffer's "elem != nil && length > 0" guard,
	// mirrored here) are never actually dereferenced and so must not be
	// bounds-checked (or even have their module's memory looked up: a
	// canon future.read/write for an elementless future declares no memory
	// option at all, since none is ever needed -- e.g.
	// drop-cross-task-borrow.wast's `(type $FT (future))` -- so memMod can
	// be nil here, and looking it up unconditionally would spuriously trap
	// with "destination/source module has no memory" on a copy that never
	// touches memory in the first place). Suites like empty-wait/
	// wait-during-callback deliberately pass a garbage pointer (0xdeadbeef)
	// to future.read/future.write on an elementless `future`; skipping
	// straight past the memory lookup, and the copy itself (which would
	// otherwise slice out of range), matches that. copyElements' n==0
	// short-circuit above doesn't cover this: n can be > 0 while elemSz
	// (and so nb) is 0.
	if nb > 0 {
		dstMem, ok := memoryBytesOf(dst.memMod)
		if !ok {
			return fmt.Errorf("stream copy: destination module has no memory")
		}
		srcMem, ok := memoryBytesOf(src.memMod)
		if !ok {
			return fmt.Errorf("stream copy: source module has no memory")
		}
		dp, sp := dst.ptr, src.ptr
		if uint64(dp)+uint64(nb) > uint64(len(dstMem)) {
			return fmt.Errorf("stream copy: destination bounds: ptr=%d n=%d elemSz=%d mem_len=%d", dp, n, dst.elemSz, len(dstMem))
		}
		if uint64(sp)+uint64(nb) > uint64(len(srcMem)) {
			return fmt.Errorf("stream copy: source bounds: ptr=%d n=%d elemSz=%d mem_len=%d", sp, n, src.elemSz, len(srcMem))
		}
		copy(dstMem[dp:dp+nb], srcMem[sp:sp+nb])
	}
	dst.ptr += nb
	src.ptr += nb
	dst.progress += n
	src.progress += n
	return nil
}

// packCopyResult: result | progress<<4. result < 2^4, progress <= 2^28-1
// (buffer constructor trap) => never ambiguous.
func packCopyResult(r copyResult, progress uint32) uint32 { return uint32(r) | progress<<4 }

// noneOrNumberType is the reference none_or_number_type: true for a bare
// stream/future (elem == nil) or a numeric primitive element (fixed-width
// integer or float) -- the equivalence class whose byte layout is identical
// in every linear memory. bool/char are deliberately NOT included: bool must
// renormalize to 0/1 and char must validate scalar range, so they take the
// general (lift/lower) path.
func noneOrNumberType(t binary.TypeDesc) bool {
	if t == nil {
		return true
	}
	p, ok := t.(binary.PrimitiveDesc)
	if !ok {
		return false
	}
	switch p.Prim {
	case "u8", "u16", "u32", "u64", "s8", "s16", "s32", "s64", "f32", "f64":
		return true
	default:
		return false
	}
}

// copyState is the reference CopyState.
type copyState uint8

const (
	copyIdle copyState = iota
	copyCopying
	copyCancelling // CANCELLING_COPY
	copyDone
)

// copyNotIdleTrapText is the spec's trap_if(state != IDLE) wording for a
// stream/future READ/WRITE builtin call (stream_copy/future_copy's own entry
// guard, stream_builtins.go's streamCopyHostFunc/futureCopyHostFunc), which
// depends on WHY state isn't IDLE:
//
//   - COPYING/CANCELLING: a prior copy on this same end is still in flight or
//     its result undelivered -- the spec calls this "concurrent operations"
//     regardless of stream-vs-future or readable-vs-writable (verified
//     against big-interleaving-test.wast's "cannot have concurrent
//     operations active on a future/stream").
//   - DONE: a prior copy already delivered its TERMINAL result -- this end's
//     own successful completion (future only; a stream's successful copies
//     return to IDLE, never DONE -- see streamCopyHostFunc's streamEvent) or
//     the counterpart's DROPPED notification -- and the wording is scenario-
//     specific (trap-if-done.wast), by (future|stream) x (readable|writable).
func copyNotIdleTrapText(isFuture bool, side endSide, state copyState) string {
	if state != copyDone { // COPYING or CANCELLING
		return "cannot have concurrent operations active on a future/stream"
	}
	switch {
	case isFuture && side == sideWritable:
		// A future's writable end reaches DONE either by completing its own
		// write or by observing the reader dropped before it ever wrote --
		// the spec's assert_trap text for both scenarios is this same
		// (longer) string, of which the shorter "cannot write to future
		// after previous write succeeded" is a substring, so one wording
		// covers both.
		return "cannot write to future after previous write succeeded or readable end dropped"
	case isFuture:
		return "cannot read from future after previous read succeeded"
	case side == sideWritable:
		// A stream's writable end only ever reaches DONE via the reader
		// dropping (a successful write returns to IDLE, not DONE -- streams
		// are multi-shot).
		return "cannot write to stream after being notified that the readable end dropped"
	default:
		return "cannot read from stream after being notified that the writable end dropped"
	}
}

// liftNotIdleTrapText is copyNotIdleTrapText's twin for the LIFT boundary
// (lift_async_value's identical trap_if(state != IDLE), reached whenever a
// readable stream/future end crosses a canonical-ABI boundary as a value --
// an argument being lifted into a callee, or a top-level export's RESULT
// being lifted back out to the host -- see takeReadableStreamEnd/
// takeReadableFutureEnd (arg/result-crossing callers) and instance.go's
// liftResult (top-level result)). Only the readable side is ever lifted (a
// component-level stream<T>/future<T> type always denotes the readable
// handle), so there is no writable-side case here; the DONE wording matches
// trap-if-done.wast's "lift" scenarios specifically.
func liftNotIdleTrapText(isFuture bool, state copyState) string {
	if state != copyDone {
		return "cannot have concurrent operations active on a future/stream"
	}
	if isFuture {
		return "cannot lift future after previous read succeeded"
	}
	return "cannot lift stream after being notified that the writable end dropped"
}

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

	// livePending is true while this end's pendingEvent is a LIVE, still-
	// reclaimable notification -- installed by streamCopyHostFunc's onCopy
	// closure when a peer's rendezvous left OUR buffer with unfilled
	// capacity (buf.remain() > 0 immediately after the copy: sharedStream.
	// write/read's `pendingBuffer.remain() > 0` and zero-length-handshake
	// branches never reset their own pending bookkeeping -- only ours does,
	// via set_pending_event -- see stream.go's write/read doc). A cancel
	// arriving before we retrieve that notification must still be allowed to
	// supersede it (component-model's cancel_copy: a copy that only made
	// partial progress -- didn't fill our request -- is still cancellable).
	// Contrast a rendezvous that happens to fill our buffer EXACTLY (buf.
	// remain() == 0 right after the same onCopy call): that is just as FINAL
	// as one installed by onCopyDone (our own immediate completion, or a
	// buffer-exhausted rendezvous that already reset the shared object's own
	// bookkeeping for us) even though it arrives through the identical
	// pre-copy `remain() > 0` branch -- cancelCopyHostFunc must leave a
	// FINAL notification alone: the shared object may already consider a
	// DIFFERENT party pending by then, and blindly cancelling would hit that
	// party's copy instead of ours. Reset false at the top of every fresh
	// copy (state -> COPYING) so no stale flag survives across calls;
	// futureEnd has no equivalent field since future.{read,write} have no
	// partial-progress notion (a future carries exactly one element, copied
	// atomically -- always fully "fills", so onCopy is never even called for
	// a future; only onCopyDone is).
	livePending bool
}

func (*streamEnd) entryKind() entryKind     { return entryStreamEnd }
func (e *streamEnd) waitablePtr() *waitable { return &e.waitable }
func (e *streamEnd) copying() bool          { return e.state == copyCopying || e.state == copyCancelling }

// futureEnd is Readable/WritableFutureEnd -- identical shape.
type futureEnd struct {
	waitable
	side   endSide
	state  copyState
	shared *sharedFuture
}

func (*futureEnd) entryKind() entryKind     { return entryFutureEnd }
func (e *futureEnd) waitablePtr() *waitable { return &e.waitable }
func (e *futureEnd) copying() bool          { return e.state == copyCopying || e.state == copyCancelling }

// sharedFuture is SharedFutureImpl: a stream of exactly one element with a
// simpler protocol -- no onCopy (no partial progress), buffers always
// length 1.
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

func (f *sharedFuture) resetPending() { f.setPending(nil, nil, nil) }

func (f *sharedFuture) setPending(inst *Instance, b copyBuffer, ocd onCopyDoneFn) {
	f.pendingInst, f.pendingBuffer, f.pendingOnCopyDone = inst, b, ocd
}

func (f *sharedFuture) resetAndNotifyPending(result copyResult) {
	ocd := f.pendingOnCopyDone
	f.resetPending()
	ocd(result)
}

func (f *sharedFuture) cancel() { f.resetAndNotifyPending(copyCancelled) }

// read mirrors future_copy's read side: assert(!dropped && dst.remain()==1)
// is a reference assert, unreachable through the builtins (a future end
// checks state==DONE-unreachable / never re-reads after DONE) -- kept as a
// Go panic-assert, not a trap.
func (f *sharedFuture) read(inst *Instance, dst copyBuffer, onCopyDone onCopyDoneFn) error {
	if f.dropped {
		panic("BUG: sharedFuture.read on a dropped future (unreachable: WritableFutureEnd.drop's trap_if(state != DONE) makes this impossible)")
	}
	if f.pendingBuffer == nil {
		f.setPending(inst, dst, onCopyDone)
		return nil
	}
	if inst != nil && inst == f.pendingInst && !f.numeric {
		return errors.New("cannot read from and write to intra-component future (element type requires cross-memory lift/lower, spec restriction)")
	}
	// pendingBuffer is a parked WRITER's source; copy it into dst then
	// complete both sides.
	if err := copyElements(f.numeric, dst, f.pendingBuffer, 1); err != nil {
		return err
	}
	f.resetAndNotifyPending(copyCompleted) // completes the parked WRITER
	onCopyDone(copyCompleted)              // completes the reader
	return nil
}

// write mirrors future_copy's write side.
func (f *sharedFuture) write(inst *Instance, src copyBuffer, onCopyDone onCopyDoneFn) error {
	if f.dropped {
		onCopyDone(copyDropped) // reader gave up
		return nil
	}
	if f.pendingBuffer == nil {
		f.setPending(inst, src, onCopyDone)
		return nil
	}
	if inst != nil && inst == f.pendingInst && !f.numeric {
		return errors.New("cannot read from and write to intra-component future (element type requires cross-memory lift/lower, spec restriction)")
	}
	// pendingBuffer is a parked READER's destination.
	if err := copyElements(f.numeric, f.pendingBuffer, src, 1); err != nil {
		return err
	}
	f.resetAndNotifyPending(copyCompleted)
	onCopyDone(copyCompleted)
	return nil
}

func (f *sharedFuture) drop() {
	if !f.dropped {
		f.dropped = true
		if f.pendingBuffer != nil {
			// Only a parked READER can be interrupted by a drop; a parked
			// writer blocks the dropper (WritableFutureEnd.drop traps unless
			// DONE -- see stream_builtins.go's futureDropHostFunc).
			f.resetAndNotifyPending(copyDropped)
		}
	}
}

// peekReadableStreamEnd validates handle i the way lift_async_value does for
// a stream (readable end, matching elem type, IDLE, not joined to a waitable
// set) WITHOUT removing it from t -- the split takeReadableStreamEnd needs
// (it DOES remove, for the arg/delegated-result crossings where ownership of
// the table slot genuinely transfers to the other side) from what
// instance.go's liftResult needs for a top-level export RESULT: per wazy's
// existing convention for own<T>/borrow<T> top-level results (liftResult
// never removes those either), a stream/future result handle stays valid in
// the guest's OWN table for the host to manage explicitly afterward, so only
// the validation -- not the removal -- applies there.
func peekReadableStreamEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*streamEnd, error) {
	raw, ok := t.getEntry(i)
	if !ok {
		return nil, fmt.Errorf("unknown stream handle %d", i)
	}
	se, ok := raw.(*streamEnd)
	if !ok {
		return nil, fmt.Errorf("handle %d is not a stream end", i)
	}
	if se.side != sideReadable {
		return nil, fmt.Errorf("handle %d is not a READABLE stream end", i)
	}
	if !reflect.DeepEqual(se.shared.elem, elemDesc) {
		return nil, fmt.Errorf("handle %d: stream element type mismatch", i)
	}
	if se.state != copyIdle {
		return nil, fmt.Errorf("handle %d: %s", i, liftNotIdleTrapText(false, se.state))
	}
	if se.inWaitableSet() {
		return nil, fmt.Errorf("handle %d: cannot lift stream while it's in a waitable set", i)
	}
	return se, nil
}

// takeReadableStreamEnd transliterates lift_async_value for a stream: REMOVE
// i from t, trapping unless it is a readable stream end of elem type
// elemDesc, idle, and not joined to a waitable set. Returns the shared
// object -- the lifted host-level value.
func takeReadableStreamEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*sharedStream, error) {
	se, err := peekReadableStreamEnd(t, elemDesc, i)
	if err != nil {
		return nil, err
	}
	t.removeEntry(i)
	return se.shared, nil
}

// peekReadableFutureEnd is peekReadableStreamEnd's future twin.
func peekReadableFutureEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*futureEnd, error) {
	raw, ok := t.getEntry(i)
	if !ok {
		return nil, fmt.Errorf("unknown future handle %d", i)
	}
	fe, ok := raw.(*futureEnd)
	if !ok {
		return nil, fmt.Errorf("handle %d is not a future end", i)
	}
	if fe.side != sideReadable {
		return nil, fmt.Errorf("handle %d is not a READABLE future end", i)
	}
	if !reflect.DeepEqual(fe.shared.elem, elemDesc) {
		return nil, fmt.Errorf("handle %d: future element type mismatch", i)
	}
	if fe.state != copyIdle {
		return nil, fmt.Errorf("handle %d: %s", i, liftNotIdleTrapText(true, fe.state))
	}
	if fe.inWaitableSet() {
		return nil, fmt.Errorf("handle %d: cannot lift future while it's in a waitable set", i)
	}
	return fe, nil
}

// takeReadableFutureEnd is takeReadableStreamEnd's future twin.
func takeReadableFutureEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*sharedFuture, error) {
	fe, err := peekReadableFutureEnd(t, elemDesc, i)
	if err != nil {
		return nil, err
	}
	t.removeEntry(i)
	return fe.shared, nil
}
