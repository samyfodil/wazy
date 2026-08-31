package wasm

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasmruntime"
)

// stackRoots is a stand-in for an engine's wasm stack: a flat list of words the collector scans without
// knowing which of them are references.
type stackRoots struct {
	mu    sync.Mutex
	words []uint64
}

func (r *stackRoots) ScanGCRoots(visit func(uint64)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.words {
		visit(w)
	}
}

func (r *stackRoots) set(words ...uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.words = words
}

// gcCollectFixture is a store with one struct type and one execution registered, which is the smallest thing
// that can allocate and collect.
type gcCollectFixture struct {
	s     *Store
	m     *ModuleInstance
	roots *stackRoots
	exec  *GCExecution
	pause uint32
}

func newGCCollectFixture(t *testing.T) *gcCollectFixture {
	t.Helper()
	// type[0] is a struct of two anyref fields, so an object can name two others.
	types := []FunctionType{{
		CompositeKind: CompositeKindStruct,
		Fields:        []FieldType{{Type: ValueTypeAnyref, Mutable: true}, {Type: ValueTypeAnyref, Mutable: true}},
	}}
	types[0].CacheFieldSlots()
	m := gcTestModule(t, types, nil, nil)

	f := &gcCollectFixture{s: m.s, m: m, roots: &stackRoots{}}
	f.exec = m.RegisterGCExecution(f.roots, &f.pause)
	t.Cleanup(f.exec.Unregister)
	return f
}

// alloc makes one object and returns its handle.
func (f *gcCollectFixture) alloc() uint64 {
	return RunGC(f.m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
}

// collect runs one collection through the safepoint, which is the only way a collection ever happens.
func (f *gcCollectFixture) collect() {
	f.s.RequestGC()
	f.exec.Safepoint()
}

// endCollection clears the flag a test set by hand, waking whatever stood still for it.
func (f *gcCollectFixture) endCollection() {
	f.s.gc.mu.Lock()
	defer f.s.gc.mu.Unlock()
	f.s.gc.collecting.Store(false)
	f.s.gc.cond.Broadcast()
}

func TestCollector(t *testing.T) {
	t.Run("an object no root names is reclaimed", func(t *testing.T) {
		f := newGCCollectFixture(t)
		f.alloc()
		require.Equal(t, 1, f.s.GC.Live())
		f.collect()
		require.Equal(t, 0, f.s.GC.Live())
	})

	t.Run("an object a stack word names survives", func(t *testing.T) {
		f := newGCCollectFixture(t)
		kept, dead := f.alloc(), f.alloc()
		f.roots.set(kept)
		f.collect()
		require.Equal(t, 1, f.s.GC.Live())
		require.NotNil(t, f.s.GC.Deref(kept))
		// The reclaimed one's handle no longer names anything, and says so rather than aliasing.
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, requirePanic(t, func() { f.s.GC.Deref(dead) }))
	})

	t.Run("the scan follows fields, and cycles terminate", func(t *testing.T) {
		f := newGCCollectFixture(t)
		a, b, c := f.alloc(), f.alloc(), f.alloc()
		RunGC(f.m, GCStructSet, a, 0, b, 0, 0, nil)
		RunGC(f.m, GCStructSet, b, 0, c, 0, 0, nil)
		RunGC(f.m, GCStructSet, c, 0, a, 0, 0, nil) // a cycle back to the head
		f.alloc()                                   // and one object nothing names
		f.roots.set(a)
		f.collect()
		require.Equal(t, 3, f.s.GC.Live())
	})

	t.Run("a cycle nothing names is reclaimed whole", func(t *testing.T) {
		f := newGCCollectFixture(t)
		a, b := f.alloc(), f.alloc()
		RunGC(f.m, GCStructSet, a, 0, b, 0, 0, nil)
		RunGC(f.m, GCStructSet, b, 0, a, 0, 0, nil)
		f.collect()
		require.Equal(t, 0, f.s.GC.Live(), "reference counting would have kept this cycle alive")
	})

	t.Run("a global and a table are roots", func(t *testing.T) {
		f := newGCCollectFixture(t)
		inGlobal, inTable, dead := f.alloc(), f.alloc(), f.alloc()
		f.m.Globals = []*GlobalInstance{
			{Type: GlobalType{ValType: ValueTypeAnyref}, Val: inGlobal},
			{Type: GlobalType{ValType: ValueTypeI64}, Val: dead}, // not a reference type, so not a root
			nil,
		}
		f.m.Tables = []*TableInstance{{References: []Reference{0, Reference(inTable)}}, nil}
		f.s.mux.Lock()
		f.s.moduleList = f.m
		f.s.mux.Unlock()
		t.Cleanup(func() { require.NoError(t, f.s.deleteModule(f.m)) })

		f.collect()
		require.Equal(t, 2, f.s.GC.Live())
		require.NotNil(t, f.s.GC.Deref(inGlobal))
		require.NotNil(t, f.s.GC.Deref(inTable))
	})

	t.Run("a slot handed out again does not answer to the old handle", func(t *testing.T) {
		f := newGCCollectFixture(t)
		old := f.alloc()
		f.collect()
		fresh := f.alloc()
		require.NotEqual(t, old, fresh, "the generation has to change even though the slot is reused")
		require.Equal(t, old&gcRefIndexMask, fresh&gcRefIndexMask, "expected the slot itself to be reused")
		require.Equal(t, wasmruntime.ErrRuntimeCastFailure, requirePanic(t, func() { f.s.GC.Deref(old) }))
		require.NotNil(t, f.s.GC.Deref(fresh))
	})

	t.Run("a word that only looks like a handle retains nothing that was already free", func(t *testing.T) {
		f := newGCCollectFixture(t)
		kept := f.alloc()
		// A stale word from an earlier call, and an i31: neither names a live slot.
		f.roots.set(kept, kept+(1<<gcRefGenShift), EncodeI31(7), 0x7f0011223344, GCRefNull)
		f.collect()
		require.Equal(t, 1, f.s.GC.Live())
	})
}

func TestCollectorThreshold(t *testing.T) {
	f := newGCCollectFixture(t)
	// Allocation asks for a collection rather than running one, so the heap grows until a safepoint.
	for i := 0; i < gcInitialThreshold+10; i++ {
		f.alloc()
	}
	require.Equal(t, gcInitialThreshold+10, f.s.GC.Live())

	require.True(t, f.s.gc.wanted.Load(), "crossing the threshold should have asked for a collection")
	require.NotEqual(t, uint32(0), atomic.LoadUint32(&f.pause), "and should have asked this execution to park")

	f.exec.Safepoint()
	require.Equal(t, 0, f.s.GC.Live())
	require.Equal(t, uint32(0), atomic.LoadUint32(&f.pause), "the pause flag is cleared once the collection is over")
}

func TestCollectorStopsEveryExecution(t *testing.T) {
	f := newGCCollectFixture(t)

	// A second execution, standing in for another goroutine running wasm in the same store.
	otherRoots := &stackRoots{}
	var otherPause uint32
	other := f.m.RegisterGCExecution(otherRoots, &otherPause)
	defer other.Unregister()

	kept := f.alloc()
	otherRoots.set(kept)
	f.alloc() // and one nothing names

	// The collector must wait for the other execution, so run it on its own goroutine and have the other
	// park only once it has been asked to.
	parked := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.collect()
	}()
	go func() {
		for {
			asked := atomic.LoadUint32(&otherPause) != 0
			if asked {
				break
			}
		}
		close(parked)
		other.Safepoint()
	}()

	<-parked
	<-done
	require.Equal(t, 1, f.s.GC.Live(), "the other execution's root must have been seen")
	require.NotNil(t, f.s.GC.Deref(kept))
}

func TestCollectorEnterGo(t *testing.T) {
	f := newGCCollectFixture(t)

	// An execution blocked in a host call never reaches a safepoint, so EnterGo has to stand in for one.
	blockedRoots := &stackRoots{}
	var blockedPause uint32
	blocked := f.m.RegisterGCExecution(blockedRoots, &blockedPause)
	defer blocked.Unregister()

	kept := f.alloc()
	blockedRoots.set(kept)
	f.alloc()

	blocked.EnterGo()
	f.collect() // would block forever if EnterGo did not count as parked
	require.Equal(t, 1, f.s.GC.Live())
	blocked.LeaveGo()
	require.Equal(t, uint32(0), atomic.LoadUint32(&blockedPause))
}

func TestCollectorUnregisterWakesTheCollector(t *testing.T) {
	f := newGCCollectFixture(t)
	otherRoots := &stackRoots{}
	var otherPause uint32
	other := f.m.RegisterGCExecution(otherRoots, &otherPause)

	f.alloc()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.collect()
	}()
	// A call that returns instead of parking must not leave the collector waiting.
	for {
		asked := atomic.LoadUint32(&otherPause) != 0
		if asked {
			break
		}
	}
	other.Unregister()
	<-done
	require.Equal(t, 0, f.s.GC.Live())
}

func TestCollectorRegisterDuringCollection(t *testing.T) {
	// A call that starts while a collection is in progress must not run until it is over: its cell is
	// visible to the scan from the moment it takes one, and the scan reads whatever stack the cell names.
	f := newGCCollectFixture(t)
	f.s.gc.collecting.Store(true)
	defer f.endCollection()

	var pause uint32
	registered := make(chan *GCExecution, 1)
	go func() { registered <- f.m.RegisterGCExecution(&stackRoots{}, &pause) }()
	select {
	case <-registered:
		t.Fatal("a call registered during a collection was let through")
	case <-time.After(20 * time.Millisecond):
	}

	f.endCollection()
	(<-registered).Unregister()
}

// TestCollectorLeaveGoWaitsOutACollection covers the half of the handshake that has no lock to fall back on: a
// host call that returns while a collection is scanning must not resume until the scan is over.
func TestCollectorLeaveGoWaitsOutACollection(t *testing.T) {
	f := newGCCollectFixture(t)
	var pause uint32
	e := f.m.RegisterGCExecution(&stackRoots{}, &pause)
	e.EnterGo()

	f.s.gc.collecting.Store(true)
	// However this ends, nothing may be left waiting on a collection that never finishes.
	defer e.Unregister()
	defer f.endCollection()

	resumed := make(chan struct{})
	go func() {
		e.LeaveGo()
		close(resumed)
	}()
	// The collection is still running, so LeaveGo owes the collector a wait rather than a return.
	select {
	case <-resumed:
		t.Fatal("LeaveGo resumed into a collection in progress")
	case <-time.After(20 * time.Millisecond):
	}
	f.endCollection()
	<-resumed
}

// TestCollectorLeaveGoKeepsAPendingRequest covers the pause flag across a host call. RequestGC will not ask a
// second time while a request is outstanding, so a LeaveGo that cleared the flag would drop the collection
// altogether and let the heap grow without bound.
func TestCollectorLeaveGoKeepsAPendingRequest(t *testing.T) {
	f := newGCCollectFixture(t)
	f.alloc()
	f.exec.EnterGo()
	f.s.RequestGC()
	f.exec.LeaveGo()
	require.NotEqual(t, uint32(0), atomic.LoadUint32(&f.pause),
		"the request has to still be waiting at the next parking point")
	f.exec.Safepoint()
	require.Equal(t, 0, f.s.GC.Live())
}

// TestCollectorCellsAreReused covers the one thing that keeps the grow-only list of calls in flight bounded: a
// cell an unregistered call left behind is taken by the next one rather than adding to the list.
func TestCollectorCellsAreReused(t *testing.T) {
	f := newGCCollectFixture(t)
	cells := func() int {
		n := 0
		for c := f.s.gc.execs.Load(); c != nil; c = c.next {
			n++
		}
		return n
	}
	before := cells()
	for round := 0; round < 3; round++ {
		var pause [4]uint32
		var execs [4]*GCExecution
		for i := range execs {
			execs[i] = f.m.RegisterGCExecution(&stackRoots{}, &pause[i])
		}
		for _, e := range execs {
			e.Unregister()
		}
	}
	require.Equal(t, before+4, cells(), "the list must grow to the peak in flight and stop there")
}

// TestCollectorHandshakeChurn runs the whole handshake -- registering, host calls, safepoints and dropping the
// call -- from several goroutines at once while collections happen underneath, which is what the -race
// detector needs to see to catch a missing edge between an execution and a collection.
func TestCollectorHandshakeChurn(t *testing.T) {
	types := []FunctionType{{
		CompositeKind: CompositeKindStruct,
		Fields:        []FieldType{{Type: ValueTypeAnyref, Mutable: true}, {Type: ValueTypeAnyref, Mutable: true}},
	}}
	types[0].CacheFieldSlots()
	m := gcTestModule(t, types, nil, nil)

	const callers, calls = 6, 400
	var wg sync.WaitGroup
	fail := make(chan string, callers)
	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < calls; i++ {
				roots := &stackRoots{}
				var pause uint32
				e := m.RegisterGCExecution(roots, &pause)
				ref := RunGC(m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
				// Nothing collects until every execution parks, and this one parks only below, so
				// rooting the object here is soon enough.
				roots.set(ref)
				if atomic.LoadUint32(&pause) != 0 {
					e.Safepoint()
				}
				// A host call: the stack stands still, so a collection may run right through it.
				e.EnterGo()
				m.s.RequestGC()
				e.LeaveGo()
				if atomic.LoadUint32(&pause) != 0 {
					e.Safepoint()
				}
				// A collection that had not seen this call would have reclaimed what its stack names.
				if _, ok := m.s.GC.TypeIDOf(ref); !ok {
					fail <- "a collection reclaimed an object this call's stack still named"
					return
				}
				e.Unregister()
			}
		}()
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Fatal(msg)
	}
	// Every call has returned, so nothing roots anything: one last collection must reclaim the lot.
	roots := &stackRoots{}
	var pause uint32
	e := m.RegisterGCExecution(roots, &pause)
	m.s.RequestGC()
	e.Safepoint()
	e.Unregister()
	require.Equal(t, 0, m.s.GC.Live())
}

// TestCollectorUnregisterEmptiesOnlyItsOwnCell covers the list the executions are threaded into: dropping one
// from the middle, and then from the head, must leave every other execution's roots reachable.
func TestCollectorUnregisterEmptiesOnlyItsOwnCell(t *testing.T) {
	f := newGCCollectFixture(t)
	var pause [3]uint32
	var roots [3]*stackRoots
	var execs [3]*GCExecution
	for i := range execs {
		roots[i] = &stackRoots{}
		execs[i] = f.m.RegisterGCExecution(roots[i], &pause[i])
		// Stand still, as a host call would, so a collection has something to wait on.
		execs[i].EnterGo()
	}
	// Every cell is taken as this goes, so execs[2] is the head, execs[1] the middle and execs[0] the tail.
	for i := range roots {
		roots[i].set(f.alloc())
	}
	f.alloc() // and one nothing names

	execs[1].Unregister()
	f.collect()
	require.Equal(t, 2, f.s.GC.Live(), "the executions on either side of the dropped one are still roots")

	execs[2].Unregister() // the head this time
	f.collect()
	require.Equal(t, 1, f.s.GC.Live())

	execs[1].Unregister() // dropping one twice must not empty the cell out from under anyone
	execs[0].Unregister()
	f.collect()
	require.Equal(t, 0, f.s.GC.Live())
}

// TestCollectorConcurrentReaders drives the heap's lock-free read path from several executions at once, with
// collections happening in between, which is where a missing publication edge shows up under -race.
func TestCollectorConcurrentReaders(t *testing.T) {
	types := []FunctionType{{
		CompositeKind: CompositeKindStruct,
		Fields:        []FieldType{{Type: ValueTypeAnyref, Mutable: true}, {Type: ValueTypeAnyref, Mutable: true}},
	}}
	types[0].CacheFieldSlots()
	m := gcTestModule(t, types, nil, nil)

	// Enough allocations across the readers to cross the collection threshold several times over.
	const readers, iterations = 4, gcInitialThreshold

	var wg sync.WaitGroup
	fail := make(chan string, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			roots := &stackRoots{}
			var pause uint32
			e := m.RegisterGCExecution(roots, &pause)
			defer e.Unregister()
			for i := 0; i < iterations; i++ {
				ref := RunGC(m, GCStructNewDefault, 0, 0, 0, 0, 0, nil)
				// Nothing collects until every execution parks, and this one only parks below, so
				// rooting the object here is soon enough.
				roots.set(ref)
				want := EncodeI31(uint32(i))
				RunGC(m, GCStructSet, ref, 0, want, 0, 0, nil)
				if got := RunGC(m, GCStructGet, ref, 0, 0, 0, 0, nil); got != want {
					fail <- fmt.Sprintf("struct.get read %#x, want %#x", got, want)
					return
				}
				if id, ok := m.s.GC.TypeIDOf(ref); !ok || id != m.TypeIDs[0] {
					fail <- fmt.Sprintf("TypeIDOf gave (%d, %v), want (%d, true)", id, ok, m.TypeIDs[0])
					return
				}
				if atomic.LoadUint32(&pause) != 0 {
					e.Safepoint()
				}
			}
		}()
	}
	wg.Wait()
	close(fail)
	for msg := range fail {
		t.Fatal(msg)
	}
}

func TestGCExecutionNilIsInert(t *testing.T) {
	// The engines hold a nil *GCExecution whenever the GC proposal is off, and call through it regardless.
	var e *GCExecution
	e.Safepoint()
	e.Unregister()
	e.EnterGo()
	e.LeaveGo()
}

func TestStructSlotsWithoutLayout(t *testing.T) {
	// A hand-built type has no slot layout, and every storage type but a vector is one word anyway.
	def := &FunctionType{CompositeKind: CompositeKindStruct, Fields: []FieldType{
		{Type: ValueTypeI32}, {Type: ValueTypeI64},
	}}
	require.Equal(t, 2, structSlots(def))
	def.CacheFieldSlots()
	require.Equal(t, 2, structSlots(def))

	withVector := &FunctionType{CompositeKind: CompositeKindStruct, Fields: []FieldType{
		{Type: ValueTypeI32}, {Type: ValueTypeV128}, {Type: ValueTypeI32},
	}}
	withVector.CacheFieldSlots()
	require.Equal(t, []uint32{0, 1, 3, 4}, withVector.FieldSlots)
	require.Equal(t, 4, structSlots(withVector))

	// CacheFieldSlots is a no-op the second time, and leaves a function type alone.
	fn := &FunctionType{}
	fn.CacheFieldSlots()
	require.Nil(t, fn.FieldSlots)
}

func TestArraySlotBounds(t *testing.T) {
	at := &FunctionType{CompositeKind: CompositeKindArray, Fields: []FieldType{{Type: ValueTypeV128}}}
	o := &GCObject{Type: at, Fields: make([]uint64, 4)} // two elements of two words each
	require.Equal(t, uint32(2), o.ElemSlots())
	require.Equal(t, 2, o.Len())
	require.Equal(t, 0, arraySlot(o, 0, 0))
	require.Equal(t, 1, arraySlot(o, 0, 2))
	require.Equal(t, 2, arraySlot(o, 1, 0))
	require.Equal(t, 3, arraySlot(o, 1, 2))
	require.Equal(t, wasmruntime.ErrRuntimeOutOfBoundsArrayAccess,
		requirePanic(t, func() { arraySlot(o, 2, 0) }))
}

var _ = api.CoreFeaturesV2

// BenchmarkGCHandshake is what one call into a GC-enabled guest that makes one host call pays the collector
// even when no collection ever happens: registering the call, parking for the host call, resuming, and
// dropping it. Run it with -cpu 1,8 -- the point of the handshake is that it does not serialise.
func BenchmarkGCHandshake(b *testing.B) {
	s := NewStore(api.CoreFeaturesV2, nil)
	m := &ModuleInstance{s: s}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		roots := &stackRoots{}
		var pause uint32
		for pb.Next() {
			e := m.RegisterGCExecution(roots, &pause)
			e.EnterGo()
			e.LeaveGo()
			e.Unregister()
		}
	})
}
