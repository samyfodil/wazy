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
	// parked is true while this execution's stack is stable. Guarded by Store.gc.mu.
	parked bool
}

// gcController is the Store-wide half of the stop-the-world handshake.
type gcController struct {
	mu   sync.Mutex
	cond *sync.Cond
	// execs is every wasm call in flight on this store.
	execs map[*GCExecution]struct{}
	// collecting is true from the moment one execution decides to collect until it has swept.
	collecting bool
	// wanted is set by an allocation that crossed the threshold and cleared when a collection starts.
	wanted bool
}

func (g *gcController) init() {
	if g.cond == nil {
		g.cond = sync.NewCond(&g.mu)
		g.execs = map[*GCExecution]struct{}{}
	}
}

// RegisterGCExecution registers one wasm call in flight. pause is the flag the engine polls at its parking
// points, which the collector writes into; it must stay valid until Unregister.
func (s *Store) RegisterGCExecution(roots GCRoots, pause *uint32) *GCExecution {
	e := &GCExecution{s: s, roots: roots, pause: pause}
	s.gc.mu.Lock()
	defer s.gc.mu.Unlock()
	s.gc.init()
	s.gc.execs[e] = struct{}{}
	// A collection may already be waiting for everyone to stop, in which case this new execution must not
	// start running until it is over.
	if s.gc.collecting {
		atomic.StoreUint32(pause, 1)
	}
	return e
}

// Unregister drops this execution, waking a collector that is waiting for it to stop.
func (e *GCExecution) Unregister() {
	if e == nil {
		return
	}
	e.s.gc.mu.Lock()
	defer e.s.gc.mu.Unlock()
	delete(e.s.gc.execs, e)
	e.s.gc.cond.Broadcast()
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

// parkLocked is Safepoint's body, and is also how an execution about to block outside wasm makes itself
// scannable. g.mu must be held.
func (e *GCExecution) parkLocked() {
	g := &e.s.gc
	for {
		if g.collecting {
			// Someone else is collecting: stay still until they are done.
			e.parked = true
			g.cond.Broadcast()
			for g.collecting {
				g.cond.Wait()
			}
			e.parked = false
			atomic.StoreUint32(e.pause, 0)
			continue
		}
		if !g.wanted {
			atomic.StoreUint32(e.pause, 0)
			return
		}
		// Nobody is collecting and one is wanted, so this execution runs it.
		g.wanted = false
		g.collecting = true
		e.parked = true
		for other := range g.execs {
			if other != e {
				atomic.StoreUint32(other.pause, 1)
			}
		}
		for !g.othersParkedLocked(e) {
			g.cond.Wait()
		}
		e.s.collectGarbageLocked()
		g.collecting = false
		e.parked = false
		for other := range g.execs {
			atomic.StoreUint32(other.pause, 0)
		}
		g.cond.Broadcast()
		return
	}
}

func (g *gcController) othersParkedLocked(self *GCExecution) bool {
	for e := range g.execs {
		if e != self && !e.parked {
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
	e.enterGoSlow()
}

func (e *GCExecution) enterGoSlow() {
	g := &e.s.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	e.parked = true
	g.cond.Broadcast()
}

// LeaveGo resumes an execution that EnterGo parked.
func (e *GCExecution) LeaveGo() {
	if e == nil {
		return
	}
	e.leaveGoSlow()
}

func (e *GCExecution) leaveGoSlow() {
	g := &e.s.gc
	g.mu.Lock()
	defer g.mu.Unlock()
	// Do not resume into a collection in progress: that would be a stack moving under the scan.
	for g.collecting {
		g.cond.Wait()
	}
	e.parked = false
	atomic.StoreUint32(e.pause, 0)
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
	s.gc.mu.Lock()
	defer s.gc.mu.Unlock()
	s.gc.init()
	if s.gc.wanted || s.gc.collecting {
		return
	}
	s.gc.wanted = true
	for e := range s.gc.execs {
		atomic.StoreUint32(e.pause, 1)
	}
}

// collectGarbageLocked marks every object reachable from the roots and frees the rest. Store.gc.mu is held and
// every execution but the caller is parked, so no wasm stack can move while this runs.
func (s *Store) collectGarbageLocked() {
	h := &s.GC
	h.mu.Lock()
	defer h.mu.Unlock()

	for i := range h.slots {
		h.slots[i].marked = false
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

	for e := range s.gc.execs {
		e.roots.ScanGCRoots(mark)
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

	for i := range h.slots {
		slot := &h.slots[i]
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
	live := len(h.slots) - len(h.free)
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
