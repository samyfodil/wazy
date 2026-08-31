package wasm

import (
	"sync"
	"sync/atomic"
)

// This file reclaims the objects on the managed heap that guest code can no longer reach.
//
// # Where the roots are
//
// A reference lives in one of five places: a wasm value stack, a reference-typed global, a reference-typed
// table, a field of another object, or the parameter/result buffer of a call in flight. All but the first are
// Go data structures the collector can walk directly. A wasm value stack is a []uint64 (or, in the native
// engine, a []byte) with no type information attached at runtime, so it is scanned *conservatively*: every
// word that looks like a live handle keeps that object alive, whether or not it really is one. That is sound
// for a non-moving collector -- the worst it does is retain something dead until the next collection -- and it
// needs no stack maps, which is what keeps the cost off the hot path entirely.
//
// Slot reuse is what makes the conservative scan safe against stale words. A handle carries the generation of
// the slot it names (see gcSlot), so a word left behind by an earlier call names a generation that no longer
// exists and matches nothing.
//
// # When it is safe to look
//
// A stack may only be scanned while it is not being written to, so a collection stops the world first: it
// asks every wasm call in flight to park, waits for all of them, marks, sweeps, and lets them go. The parking
// points are the ones execution already passes through -- every loop header, and the start and end of every
// call -- so a guest cannot outrun a collection except by looping without a back edge, which no loop does.
//
// Nothing collects during an instruction. Allocation only *asks* for a collection (see GCHeap.Alloc); the
// execution that notices runs it at its next parking point, where its own stack is stable too.
//
// # Standing still without a lock
//
// Registering a call and dropping it happen once each per call into a GC-enabled guest, and EnterGo/LeaveGo
// once each per host call back out of one, so none of the four takes the collector's mutex unless a collection
// is actually under way (or, once per call that has ever overlapped another, a cell has to be added). Each
// instead publishes what it did -- took a cell, emptied it, parked, unparked --
// with an atomic store and *then* reads gc.collecting, while a collection stores gc.collecting and *then*
// reads the cells and the parked flags in them. sync/atomic is sequentially consistent, so of those two
// store-then-load pairs at least one side sees the other's store: there is no interleaving in which the
// collection misses the execution *and* the execution misses the collection. Whichever way round it falls is
// safe -- the collection waits for the execution to park, or the execution stands still until the collection
// is over -- and only missing each other would not be.
//
// A collection holds gc.mu from before it sets gc.collecting until after it clears it, so anything that does
// take the mutex is already excluded from a collection in progress and needs no handshake of its own.

// GCRoots is what an engine exposes so a collection can find the references one wasm call in flight holds.
type GCRoots interface {
	// ScanGCRoots calls visit with every word that could be a reference: the whole value stack, plus
	// whatever else the engine keeps live values in. It is called only while this execution is parked.
	ScanGCRoots(visit func(word uint64))
}

// GCExecution is one wasm call in flight, registered with a Store so that a collection can find its roots and
// stop it. An engine creates one per call engine and drops it when the call returns.
type GCExecution struct {
	s     *Store
	roots GCRoots
	// pause is what the engine polls at its parking points. The collector writes 1 into it; the engine
	// calls Safepoint when it sees a non-zero value. It is a bare *uint32 rather than an atomic.Uint32
	// because compiled code loads it by offset from the execution context; every access from Go goes
	// through sync/atomic, and the compiled-code load is a single aligned word. A stale read only delays
	// this execution to its next poll.
	pause *uint32
	// parked is true while this execution's stack is stable. A collection reads it holding only itself; see
	// the handshake above.
	parked atomic.Bool
	// cell is this execution's place among the calls in flight.
	cell *gcExecCell
}

// gcExecCell is one place in the list of calls in flight. A cell outlives the call that took it: Unregister
// empties it rather than unlinking it, so the list only ever grows -- to the most calls this store has had in
// flight at once -- and both walking it and joining it stay off the mutex.
type gcExecCell struct {
	exec atomic.Pointer[GCExecution]
	next *gcExecCell
}

// gcController is the Store-wide half of the stop-the-world handshake.
type gcController struct {
	mu   sync.Mutex
	cond *sync.Cond
	// execs is the head of the list of cells holding the calls in flight on this store.
	execs atomic.Pointer[gcExecCell]
	// collecting is true from the moment one execution decides to collect until it has swept. It is the word
	// every fast path reads after publishing its own; see the handshake above.
	collecting atomic.Bool
	// wanted is set by an allocation that crossed the threshold and cleared when a collection starts.
	wanted atomic.Bool
}

func (g *gcController) init() {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
	}
}

// RegisterGCExecution registers one wasm call in flight. pause is the flag the engine polls at its parking
// points, which the collector writes into; it must stay valid until Unregister.
func (s *Store) RegisterGCExecution(roots GCRoots, pause *uint32) *GCExecution {
	g := &s.gc
	e := &GCExecution{s: s, roots: roots, pause: pause}
	e.cell = g.take(e)
	// The cell became visible to a collection the moment take filled it, so a collection already scanning
	// may have this call in hand before it has run an instruction: it must stand still rather than start.
	// A collection that instead published itself first is one this load sees; only one of the two can miss
	// the other, and the one that saw asks the pair to stop.
	if g.collecting.Load() {
		e.waitOut()
	}
	return e
}

// take puts e in an empty cell, adding one when every cell is taken. Adding is the only part that needs the
// mutex, and happens once per call that has ever been in flight beside another.
func (g *gcController) take(e *GCExecution) *gcExecCell {
	for c := g.execs.Load(); c != nil; c = c.next {
		// The load is what keeps this cheap when several calls start at once: a taken cell is read, not
		// written, so only the one call that finds a cell empty pays for the exclusive line.
		if c.exec.Load() == nil && c.exec.CompareAndSwap(nil, e) {
			return c
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	c := &gcExecCell{next: g.execs.Load()}
	c.exec.Store(e)
	g.execs.Store(c)
	return c
}

// Unregister drops this execution, waking a collector that is waiting for it to stop.
func (e *GCExecution) Unregister() {
	if e == nil {
		return
	}
	// The compare is what keeps a second Unregister from evicting whichever call took the cell after this
	// one. Emptying the cell is all it takes to leave the collector's sight; the wait is for the collection
	// that may already have this execution in hand, whose scan has to finish before the caller is free to
	// hand its stack back to a pool.
	if e.cell.exec.CompareAndSwap(e, nil) && e.s.gc.collecting.Load() {
		e.waitOut()
	}
}

// Safepoint is called by an engine when it sees its pause flag set, at a point where its wasm stack is stable.
// Whichever execution gets here first while a collection is wanted runs it; the rest park until it is done.
func (e *GCExecution) Safepoint() {
	if e == nil {
		return
	}
	g := &e.s.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	e.parkLocked()
}

// parkLocked is Safepoint's body. g.mu must be held.
func (e *GCExecution) parkLocked() {
	g := &e.s.gc
	for {
		if g.collecting.Load() {
			// Someone else is collecting: stay still until they are done.
			e.parked.Store(true)
			g.cond.Broadcast()
			for g.collecting.Load() {
				g.cond.Wait()
			}
			e.parked.Store(false)
			atomic.StoreUint32(e.pause, 0)
			continue
		}
		if !g.wanted.Load() {
			atomic.StoreUint32(e.pause, 0)
			return
		}
		// Nobody is collecting and one is wanted, so this execution runs it. Publishing that before reading
		// any cell is what leaves a call joining the list, or resuming from Go, no way to miss it.
		g.wanted.Store(false)
		g.collecting.Store(true)
		e.parked.Store(true)
		for c := g.execs.Load(); c != nil; c = c.next {
			if other := c.exec.Load(); other != nil && other != e {
				atomic.StoreUint32(other.pause, 1)
			}
		}
		for !g.othersParkedLocked(e) {
			g.cond.Wait()
		}
		e.s.collectGarbageLocked()
		g.collecting.Store(false)
		e.parked.Store(false)
		for c := g.execs.Load(); c != nil; c = c.next {
			if other := c.exec.Load(); other != nil {
				atomic.StoreUint32(other.pause, 0)
			}
		}
		g.cond.Broadcast()
		return
	}
}

// othersParkedLocked reports whether every call in flight but self is standing still. A cell that fills after
// this has looked at it holds an execution that saw the collection and parks at its first parking point, and
// one that empties holds an execution that will not touch its stack again; see the handshake above.
func (g *gcController) othersParkedLocked(self *GCExecution) bool {
	for c := g.execs.Load(); c != nil; c = c.next {
		if e := c.exec.Load(); e != nil && e != self && !e.parked.Load() {
			return false
		}
	}
	return true
}

// EnterGo marks this execution stable for the duration of a Go call made from wasm -- a host function, or
// anything else that can block -- so a collection is never left waiting on a stack that has stopped moving
// anyway. LeaveGo resumes.
//
// These are a pair of nil-checked methods rather than one call returning a closure because a host call is a
// hot path: with the GC proposal off the whole thing has to fold down to a branch.
func (e *GCExecution) EnterGo() {
	if e == nil {
		return
	}
	e.enterGo()
}

// enterGo is EnterGo's body, split out so EnterGo itself stays under the inliner's budget. Every host call
// runs EnterGo, and for a guest without the GC proposal e is nil -- that case has to stay a compare and a
// branch at the call site, not a call.
func (e *GCExecution) enterGo() {
	e.parked.Store(true)
	if e.s.gc.collecting.Load() {
		e.wake()
	}
}

// wake tells a collection already under way that this execution has stopped moving. It is the mutex, not the
// flag above, that makes the wakeup impossible to lose: the collector only ever waits holding it.
func (e *GCExecution) wake() {
	g := &e.s.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cond.Broadcast()
}

// LeaveGo resumes an execution that EnterGo parked.
func (e *GCExecution) LeaveGo() {
	if e == nil {
		return
	}
	e.leaveGo()
}

// leaveGo is LeaveGo's body; see enterGo for why it is split.
func (e *GCExecution) leaveGo() {
	e.parked.Store(false)
	// Do not resume into a collection in progress: that would be a stack moving under the scan. The pause
	// flag is left alone -- if a collection was asked for while this call was in Go, its next parking point
	// is where that belongs.
	if e.s.gc.collecting.Load() {
		e.waitOut()
	}
}

// waitOut stands this execution still for the rest of a collection already under way. It is the slow half the
// fast paths here share: whichever one saw gc.collecting has to wait, because the collector may already be
// scanning -- or waiting for -- this execution.
func (e *GCExecution) waitOut() {
	g := &e.s.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	e.parked.Store(true)
	g.cond.Broadcast()
	for g.collecting.Load() {
		g.cond.Wait()
	}
	e.parked.Store(false)
}

// RegisterGCExecution is Store.RegisterGCExecution, reachable from the engines through a module instance.
func (m *ModuleInstance) RegisterGCExecution(roots GCRoots, pause *uint32) *GCExecution {
	return m.s.RegisterGCExecution(roots, pause)
}

// GCLiveObjects is how many objects the store's heap holds. It exists for tests, which have no other way to
// see a collection happen: nothing about reclamation is observable through the public API.
func (m *ModuleInstance) GCLiveObjects() int { return m.s.GC.Live() }

// RequestGC asks every execution on this store to park at its next safepoint, because the heap has grown past
// its threshold. It is called from allocation, which cannot collect on the spot.
func (s *Store) RequestGC() {
	g := &s.gc
	// The threshold stays crossed until a collection actually happens, so every allocation after the one
	// that crossed it lands here too; answer those without taking anything. The execution that asks is
	// always one the walk below can see -- it registered before it could allocate -- so a request is never
	// left with nobody to notice it.
	if g.wanted.Load() || g.collecting.Load() {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	if g.wanted.Load() || g.collecting.Load() {
		return
	}
	g.wanted.Store(true)
	for c := g.execs.Load(); c != nil; c = c.next {
		if e := c.exec.Load(); e != nil {
			atomic.StoreUint32(e.pause, 1)
		}
	}
}

// collectGarbageLocked marks every object reachable from the roots and frees the rest. Store.gc.mu is held and
// every execution but the caller is parked, so no wasm stack can move while this runs.
func (s *Store) collectGarbageLocked() {
	h := &s.GC
	h.mu.Lock()
	defer h.mu.Unlock()

	// Every execution that could be reading a slot is parked, and each of them reads gc.collecting -- or
	// takes gc.mu -- before it runs another instruction, so the plain writes below are ordered before
	// anything sees them.
	slots := h.table()[:h.used]
	for i := range slots {
		slots[i].marked = false
	}

	// pending is the mark stack: an object is pushed when first marked and popped to scan its fields, so a
	// deep or cyclic object graph costs heap rather than Go stack.
	var pending []*GCObject
	mark := func(ref uint64) {
		slot, ok := h.slotOf(ref)
		if !ok || slot.marked {
			return
		}
		slot.marked = true
		pending = append(pending, slot.obj)
	}

	for c := s.gc.execs.Load(); c != nil; c = c.next {
		if e := c.exec.Load(); e != nil {
			e.roots.ScanGCRoots(mark)
		}
	}
	s.scanModuleRootsLocked(mark)

	for len(pending) > 0 {
		o := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		// A field that is not a reference cannot name a live slot except by coincidence, and retaining
		// something dead is the price of not tracking which fields are which.
		for _, w := range o.Fields {
			mark(w)
		}
	}

	for i := range slots {
		slot := &slots[i]
		if slot.obj == nil || slot.marked {
			continue
		}
		slot.obj = nil
		slot.gen++
		h.free = append(h.free, uint32(i))
	}

	h.allocs = 0
	// Collect again once the heap has roughly doubled, so a program with a large live set does not spend
	// all its time marking.
	live := h.used - len(h.free)
	h.nextAt = live + gcInitialThreshold
}

// scanModuleRootsLocked visits every reference held outside a wasm stack: the globals and tables of every
// module instance on the store.
func (s *Store) scanModuleRootsLocked(mark func(uint64)) {
	s.mux.RLock()
	defer s.mux.RUnlock()
	for m := s.moduleList; m != nil; m = m.next {
		for _, g := range m.Globals {
			if g == nil || !g.Type.ValType.IsRef() {
				continue
			}
			// Value(), not Val: the native engine keeps a module's globals in its own opaque context, so
			// the GlobalInstance field is not where the live value is.
			lo, _ := g.Value()
			mark(lo)
		}
		for _, t := range m.Tables {
			if t == nil {
				continue
			}
			for _, r := range t.References {
				mark(uint64(r))
			}
		}
	}
}
