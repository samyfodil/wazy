package wasm

import (
	"sync"
	"sync/atomic"
	"testing"

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
		t.Cleanup(func() { f.s.deleteModule(f.m) })

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

	f.s.gc.mu.Lock()
	wanted := f.s.gc.wanted
	f.s.gc.mu.Unlock()
	require.True(t, wanted, "crossing the threshold should have asked for a collection")
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

	resume := blocked.EnterGo()
	f.collect() // would block forever if EnterGo did not count as parked
	require.Equal(t, 1, f.s.GC.Live())
	resume()
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
	// A call that starts while a collection is in progress must not run until it is over.
	f := newGCCollectFixture(t)
	f.s.gc.mu.Lock()
	f.s.gc.collecting = true
	f.s.gc.mu.Unlock()

	var pause uint32
	e := f.m.RegisterGCExecution(&stackRoots{}, &pause)
	require.Equal(t, uint32(1), atomic.LoadUint32(&pause))

	f.s.gc.mu.Lock()
	f.s.gc.collecting = false
	f.s.gc.cond.Broadcast()
	f.s.gc.mu.Unlock()
	e.Unregister()
}

func TestGCExecutionNilIsInert(t *testing.T) {
	// The engines hold a nil *GCExecution whenever the GC proposal is off, and call through it regardless.
	var e *GCExecution
	e.Safepoint()
	e.Unregister()
	e.EnterGo()()
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
