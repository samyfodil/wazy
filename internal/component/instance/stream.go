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
			return errors.New("stream: intra-instance copy of a non-numeric element type (spec restriction)")
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
			return errors.New("stream: intra-instance copy of a non-numeric element type (spec restriction)")
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
	dstMem, ok := memoryBytesOf(dst.memMod)
	if !ok {
		return fmt.Errorf("stream copy: destination module has no memory")
	}
	srcMem, ok := memoryBytesOf(src.memMod)
	if !ok {
		return fmt.Errorf("stream copy: source module has no memory")
	}
	nb := n * dst.elemSz
	dp, sp := dst.ptr, src.ptr
	if uint64(dp)+uint64(nb) > uint64(len(dstMem)) {
		return fmt.Errorf("stream copy: destination bounds: ptr=%d n=%d elemSz=%d mem_len=%d", dp, n, dst.elemSz, len(dstMem))
	}
	if uint64(sp)+uint64(nb) > uint64(len(srcMem)) {
		return fmt.Errorf("stream copy: source bounds: ptr=%d n=%d elemSz=%d mem_len=%d", sp, n, src.elemSz, len(srcMem))
	}
	copy(dstMem[dp:dp+nb], srcMem[sp:sp+nb])
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
		return errors.New("future: intra-instance copy of a non-numeric element type (spec restriction)")
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
		return errors.New("future: intra-instance copy of a non-numeric element type (spec restriction)")
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

// takeReadableStreamEnd transliterates lift_async_value for a stream: REMOVE
// i from t, trapping unless it is a readable stream end of elem type
// elemDesc, idle, and not joined to a waitable set. Returns the shared
// object -- the lifted host-level value.
func takeReadableStreamEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*sharedStream, error) {
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
		return nil, fmt.Errorf("handle %d: stream end is not idle (in-flight copy)", i)
	}
	if se.inWaitableSet() {
		return nil, fmt.Errorf("handle %d: stream end is joined to a waitable set", i)
	}
	t.removeEntry(i)
	return se.shared, nil
}

// takeReadableFutureEnd is takeReadableStreamEnd's future twin.
func takeReadableFutureEnd(t *handleTable, elemDesc binary.TypeDesc, i uint32) (*sharedFuture, error) {
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
		return nil, fmt.Errorf("handle %d: future end is not idle (in-flight copy)", i)
	}
	if fe.inWaitableSet() {
		return nil, fmt.Errorf("handle %d: future end is joined to a waitable set", i)
	}
	t.removeEntry(i)
	return fe.shared, nil
}
