package abi

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// These tests pin down the staleness contract between Realloc and the store /
// lower trees: a []byte taken before an allocation may be worthless after it,
// so every read, bounds check and write must go through the view the allocation
// left live (Realloc.Mem / Realloc.GrowMem).
//
// Every other test in this package builds its Realloc through ReallocFunc,
// which has no module behind it and hands out a FIXED Go slice that never
// relocates -- so the whole existing suite exercises only the nil-Mem path and
// would keep passing with the refresh removed. growingMem below is the missing
// half: an allocator whose memory genuinely moves.

const testPage = 64 * 1024

// growingMem stands in for wazy's MemoryInstance plus a guest's own allocator.
//
// The memory half models internal/wasm/memory.go's Grow64 under the DEFAULT
// runtime config, where the reserved capacity equals the declared minimum and
// so the very first memory.grow crosses it: growth allocates a brand-new
// backing array and copies the old contents into it, because Go has no in-place
// realloc. Everything holding the previous slice is left pointing at an array
// that is now detached -- writes to it vanish, reads from it return pre-grow
// bytes.
//
// The allocator half is a bump allocator that keeps `reserve` bytes of mapped
// headroom ahead of the bump pointer and grows a page at a time to maintain it
// -- the page-granular shape a real guest allocator (dlmalloc, wee_alloc, the
// hand-rolled bump allocator a no_std wasm32-unknown-unknown guest links) has.
// The reserve knob is what selects between the two ways the bug shows up:
//
//   - reserve == 0: an allocation is served from a pointer PAST the pre-grow
//     end. A bounds check against the pre-grow length then rejects a pointer
//     that is in fact perfectly valid -- the LOUD failure, and the one the
//     reported greet.wasm case hits ("allocated memory out of bounds:
//     ptr=1114120 size=4" against a 1114112-byte snapshot).
//
//   - reserve > 0: the allocation is served from a LOW pointer that still fits
//     inside the pre-grow length, while the refill that ran alongside it has
//     already replaced the backing array. Every bounds check passes and the
//     write lands in the abandoned array -- the SILENT failure, which no error
//     message ever reports.
type growingMem struct {
	buf     []byte
	bump    uint32
	reserve uint32
	grows   int
}

func newGrowingMem(bumpStart, reserve uint32) *growingMem {
	return &growingMem{buf: make([]byte, testPage), bump: bumpStart, reserve: reserve}
}

// live is the accessor an abi.Realloc carries as Mem: it always answers with
// the CURRENT backing array, which is the whole point.
func (m *growingMem) live() []byte { return m.buf }

func (m *growingMem) alloc(_, _, align, size uint32) (uint32, error) {
	if align == 0 {
		align = 1
	}
	ptr := Align(m.bump, align)
	end := ptr + size
	for uint32(len(m.buf)) < end+m.reserve {
		grown := make([]byte, len(m.buf)+testPage)
		copy(grown, m.buf)
		m.buf = grown
		m.grows++
	}
	m.bump = end
	return ptr, nil
}

// realloc builds the abi.Realloc for this memory, with the live accessor wired
// in exactly as instance.memAccessorOf does for a real module.
func (m *growingMem) realloc() Realloc {
	return Realloc{Call: func(_ context.Context, o, os, a, n uint32) (uint32, error) { return m.alloc(o, os, a, n) }, Mem: m.live}
}

func strRef() bintype.TypeRef { return bintype.TypeRef{Primitive: "string"} }
func u32Ref() bintype.TypeRef { return bintype.TypeRef{Primitive: "u32"} }
func idxRef(i uint32) bintype.TypeRef {
	return bintype.TypeRef{TypeIndex: &i}
}

// storeThenLoad stores v at ptr through the caller's PRE-GROW view (mem), then
// loads it back through the memory's LIVE view. That asymmetry is the whole
// test: a store that wrote into a detached array, or that refused a valid
// pointer, cannot survive it.
func storeThenLoad(t *testing.T, gm *growingMem, ptr uint32, desc bintype.TypeDesc, v Value, resolve Resolver) Value {
	t.Helper()
	stale := gm.buf // the snapshot a caller (instance.lowerParams, host_import, ...) holds
	if err := Store(stale, ptr, desc, v, resolve, gm.realloc()); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if gm.grows == 0 {
		t.Fatalf("test is not exercising anything: memory never relocated")
	}
	if len(gm.buf) == len(stale) {
		t.Fatalf("test is not exercising anything: backing array did not change length")
	}
	got, err := Load(gm.live(), ptr, desc, resolve)
	if err != nil {
		t.Fatalf("Load through the live view: %v", err)
	}
	return got
}

// TestStoreThroughRelocatingGrow is the regression test for the stale-memory
// bug: every one of these shapes allocates at least once mid-store, and used to
// either reject the fresh pointer or write into the abandoned array.
func TestStoreThroughRelocatingGrow(t *testing.T) {
	// The value region the store targets lives at ptr 0..bumpStart; the bump
	// allocator starts past it so it never overwrites the value being written.
	const bumpStart = 512

	tests := []struct {
		name    string
		reserve uint32 // 0 => loud (pointer past the old end); >0 => silent (low pointer, dead array)
		desc    bintype.TypeDesc
		val     Value
		types   []bintype.TypeDesc // resolver table for nested refs
	}{
		{
			name: "string",
			desc: bintype.PrimitiveDesc{Prim: "string"},
			val:  strings.Repeat("a", testPage), // one allocation, big enough to force a page grow
		},
		{
			name:    "string, silent shape",
			reserve: testPage,
			desc:    bintype.PrimitiveDesc{Prim: "string"},
			val:     "hello",
		},
		{
			// The shape the reported greet.wasm case generalizes to: field 0's
			// allocation relocates memory, and fields 1..n then write their own
			// (ptr,len) pairs at LOW offsets that pass every bounds check.
			name:    "record of strings",
			reserve: testPage,
			desc: bintype.RecordDesc{Fields: []bintype.RecordField{
				{Name: "a", Type: strRef()},
				{Name: "b", Type: strRef()},
				{Name: "c", Type: strRef()},
			}},
			val: []Value{"first", "second", "third"},
		},
		{
			// A trailing scalar after a growing field: a u32 written through a
			// stale view is silently lost with no error anywhere.
			name:    "record with trailing scalar",
			reserve: testPage,
			desc: bintype.RecordDesc{Fields: []bintype.RecordField{
				{Name: "s", Type: strRef()},
				{Name: "n", Type: u32Ref()},
			}},
			val: []Value{"payload", uint32(0xDEADBEEF)},
		},
		{
			// Also the shape of a spilled parameter list (boundExport.paramTuple).
			name:    "tuple of strings",
			reserve: testPage,
			desc:    bintype.TupleDesc{Elements: []bintype.TypeRef{strRef(), strRef(), u32Ref()}},
			val:     []Value{"one", "two", uint32(7)},
		},
		{
			// allocStoreList's element loop: the outer allocation is only the
			// FIRST grow; each element allocates again, so refreshing once is
			// not enough.
			name:    "list of strings",
			reserve: testPage,
			desc:    bintype.ListDesc{Element: strRef()},
			val:     []Value{"alpha", "beta", "gamma", "delta"},
		},
		{
			name:    "list of records containing strings",
			reserve: testPage,
			desc:    bintype.ListDesc{Element: idxRef(0)},
			types: []bintype.TypeDesc{bintype.RecordDesc{Fields: []bintype.RecordField{
				{Name: "name", Type: strRef()},
				{Name: "tag", Type: strRef()},
			}}},
			val: []Value{
				[]Value{"n0", "t0"},
				[]Value{"n1", "t1"},
				[]Value{"n2", "t2"},
			},
		},
		{
			// encodeScalarList: its span used to be sliced off the pre-grow view.
			name:    "list of u32",
			reserve: testPage,
			desc:    bintype.ListDesc{Element: u32Ref()},
			val:     []uint32{1, 2, 3, 4, 5, 6, 7, 8},
		},
		{
			// The same list, big enough that its pointer lands past the
			// pre-grow end: the loud half of the same failure.
			name: "list of u32, loud shape",
			desc: bintype.ListDesc{Element: u32Ref()},
			val:  make([]uint32, testPage/2),
		},
		{
			name:    "list of u8",
			reserve: testPage,
			desc:    bintype.ListDesc{Element: bintype.TypeRef{Primitive: "u8"}},
			val:     []byte{9, 8, 7, 6},
		},
		{
			name:    "option of string",
			reserve: testPage,
			desc:    bintype.OptionDesc{Element: strRef()},
			val:     "some-payload",
		},
		{
			// result<string, u32> is the wasi:http / wasi:io workhorse shape.
			name:    "result ok of string",
			reserve: testPage,
			desc:    bintype.ResultDesc{Ok: refOf(strRef()), Err: refOf(u32Ref())},
			val:     ResultValue{IsErr: false, Payload: "ok-payload"},
		},
		{
			name:    "variant case carrying a string",
			reserve: testPage,
			desc: bintype.VariantDesc{Cases: []bintype.VariantCase{
				{Name: "none"},
				{Name: "some", Type: refOf(strRef())},
			}},
			val: VariantValue{Disc: 1, Payload: "variant-payload"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gm := newGrowingMem(bumpStart, tt.reserve)
			var resolve Resolver
			if tt.types != nil {
				types := tt.types
				resolve = func(i uint32) bintype.TypeDesc {
					if int(i) >= len(types) {
						return nil
					}
					return types[i]
				}
			}
			got := storeThenLoad(t, gm, 0, tt.desc, tt.val, resolve)
			if !reflect.DeepEqual(got, tt.val) {
				t.Errorf("round trip through a relocating grow:\n got  %#v\n want %#v", got, tt.val)
			}
		})
	}
}

// TestSpillThroughRelocatingGrow covers StoreStep.Spill, the whole-parameter-
// list path (instance.lowerParams' paramsSpill arm via abi.SpillValue). The
// spill's own allocation is by construction at or past the end of the caller's
// view, so Store's bounds check used to reject it outright.
func TestSpillThroughRelocatingGrow(t *testing.T) {
	desc := bintype.TupleDesc{Elements: []bintype.TypeRef{strRef(), strRef(), u32Ref()}}
	val := []Value{"spilled-a", "spilled-b", uint32(1234)}

	for _, tc := range []struct {
		reserve   uint32
		bumpStart uint32
	}{
		// reserve == 0: the bump pointer starts near the end of the memory, so
		// the spilled region is allocated PAST the caller's view -- the loud
		// half. reserve == testPage: the region is low and in range, but the
		// refill has already replaced the array -- the silent half.
		{reserve: 0, bumpStart: testPage - 16},
		{reserve: testPage, bumpStart: 512},
	} {
		reserve := tc.reserve
		gm := newGrowingMem(tc.bumpStart, reserve)
		stale := gm.buf
		ptr, err := SpillValue(val, desc, stale, nil, gm.realloc())
		if err != nil {
			t.Fatalf("reserve=%d: SpillValue: %v", reserve, err)
		}
		if gm.grows == 0 {
			t.Fatalf("reserve=%d: memory never relocated", reserve)
		}
		got, err := Load(gm.live(), ptr, desc, nil)
		if err != nil {
			t.Fatalf("reserve=%d: Load through the live view: %v", reserve, err)
		}
		if !reflect.DeepEqual(got, val) {
			t.Errorf("reserve=%d: spill round trip:\n got  %#v\n want %#v", reserve, got, val)
		}
	}
}

// TestLowerFlatThroughRelocatingGrow covers the flat lowering tree, which does
// NOT thread a refreshed view: it relies on every allocating leaf refreshing
// internally (see lowerFlatImpl's invariant). A record of strings is the case
// that would break if a lowerFlat* frame ever started writing through its own
// `mem` -- each field's (ptr,len) must still name bytes that are readable in
// the live memory.
func TestLowerFlatThroughRelocatingGrow(t *testing.T) {
	desc := bintype.RecordDesc{Fields: []bintype.RecordField{
		{Name: "a", Type: strRef()},
		{Name: "b", Type: strRef()},
	}}
	want := []Value{"flat-a", "flat-b"}

	gm := newGrowingMem(512, testPage)
	stale := gm.buf
	flat, err := LowerFlat(want, desc, nil, gm.realloc(), stale)
	if err != nil {
		t.Fatalf("LowerFlat: %v", err)
	}
	if gm.grows == 0 {
		t.Fatal("memory never relocated")
	}
	if len(flat) != 4 {
		t.Fatalf("LowerFlat produced %d core values, want 4 (two (ptr,len) pairs)", len(flat))
	}
	for i, w := range []string{"flat-a", "flat-b"} {
		ptr, n := flat[i*2].AsI32(), flat[i*2+1].AsI32()
		got, err := loadStringFromRange(gm.live(), ptr, n)
		if err != nil {
			t.Fatalf("field %d: read back through the live view: %v", i, err)
		}
		if got != w {
			t.Errorf("field %d = %q, want %q", i, got, w)
		}
	}
}

// TestStoreStepStoreReturnsLiveView is the contract instance/stream.go's
// guestBuffer.write depends on: storing element after element through ONE view
// only works if each store hands back the view the next one must use.
func TestStoreStepStoreReturnsLiveView(t *testing.T) {
	elem := bintype.PrimitiveDesc{Prim: "string"}
	st, err := CompileStore(elem, nil)
	if err != nil {
		t.Fatalf("CompileStore: %v", err)
	}
	gm := newGrowingMem(512, testPage)
	mem := gm.buf
	want := []string{"e0", "e1", "e2", "e3"}
	for i, s := range want {
		if mem, err = st.Store(mem, uint32(i)*8, s, gm.realloc()); err != nil {
			t.Fatalf("element %d: %v", i, err)
		}
	}
	if gm.grows == 0 {
		t.Fatal("memory never relocated")
	}
	for i, w := range want {
		got, err := Load(gm.live(), uint32(i)*8, elem, nil)
		if err != nil {
			t.Fatalf("element %d: Load: %v", i, err)
		}
		if got != Value(w) {
			t.Errorf("element %d = %v, want %q", i, got, w)
		}
	}
}

// ---------- fail-loud branches ----------

// fixedMem is an allocator over a memory that never grows: whatever pointer it
// returns has to be checked against the memory that actually exists. This is
// how the nil-Mem path (ReallocFunc, and every existing test in this package)
// behaves, and it must keep failing loud.
func fixedMem(ptr uint32) Realloc {
	return ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return ptr, nil })
}

func TestAllocationOutOfBoundsStillFailsLoud(t *testing.T) {
	mem := make([]byte, 64)
	tests := []struct {
		name    string
		run     func(Realloc) error
		realloc Realloc
	}{
		{
			name:    "string past the end",
			realloc: fixedMem(60),
			run: func(r Realloc) error {
				_, _, _, err := allocStoreString(mem, "hello world", r)
				return err
			},
		},
		{
			name:    "list<u8> past the end",
			realloc: fixedMem(60),
			run: func(r Realloc) error {
				_, _, _, err := allocStoreBytes(mem, make([]byte, 16), r)
				return err
			},
		},
		{
			name:    "list<Value> past the end",
			realloc: fixedMem(60),
			run: func(r Realloc) error {
				_, _, _, err := allocStoreList(mem, []Value{uint32(1), uint32(2)}, bintype.PrimitiveDesc{Prim: "u32"}, nil, r)
				return err
			},
		},
		{
			name:    "list<u32> past the end",
			realloc: fixedMem(60),
			run: func(r Realloc) error {
				_, _, _, _, err := storeScalarList(mem, []uint32{1, 2, 3}, "u32", r)
				return err
			},
		},
		{
			// The wrap guard: ptr+size overflows uint32 and, without the check,
			// compares as a small number and sails through.
			name:    "string at a wrapping pointer",
			realloc: fixedMem(0xFFFFFFF0),
			run: func(r Realloc) error {
				_, _, _, err := allocStoreString(mem, "wraparound", r)
				return err
			},
		},
		{
			name:    "list<u8> at a wrapping pointer",
			realloc: fixedMem(0xFFFFFFF0),
			run: func(r Realloc) error {
				_, _, _, err := allocStoreBytes(mem, make([]byte, 32), r)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(tt.realloc)
			if err == nil {
				t.Fatal("expected an out-of-bounds error, got nil")
			}
			if !strings.Contains(err.Error(), "allocated memory out of bounds") {
				t.Errorf("error = %v, want it to name the out-of-bounds allocation", err)
			}
		})
	}
}

// TestOutOfBoundsIsReportedAgainstTheRefreshedView pins the one behavior change
// this fix makes on purpose: a pointer past the PRE-grow end is now accepted
// (it is valid), while a pointer past the POST-grow end is still rejected -- and
// the message reports the length that was actually checked.
func TestOutOfBoundsIsReportedAgainstTheRefreshedView(t *testing.T) {
	gm := newGrowingMem(512, 0)
	r := gm.realloc()
	// Make the allocator hand back a pointer far past even the grown memory.
	r.Call = func(_ context.Context, _, _, _, size uint32) (uint32, error) {
		if _, err := gm.alloc(0, 0, 1, size); err != nil {
			return 0, err
		}
		return uint32(len(gm.buf)) + 1, nil
	}
	_, _, _, err := allocStoreString(gm.buf, "x", r)
	if err == nil {
		t.Fatal("expected an out-of-bounds error for a pointer past the grown memory")
	}
	if !strings.Contains(err.Error(), "allocated memory out of bounds") {
		t.Fatalf("error = %v, want it to name the out-of-bounds allocation", err)
	}
	// The reported mem_len must be the REFRESHED length, not the caller's.
	if !strings.Contains(err.Error(), "mem_len=") {
		t.Errorf("error = %v, want it to report the length it checked against", err)
	}
}

// ---------- the nil-Mem contract ----------

// TestZeroReallocKeepsWorking is the compatibility constraint that forbids
// making Mem mandatory: instance/async_builtins.go's storeEvent passes a
// literal abi.Realloc{} (nil Call AND nil Mem) to store two u32s, which never
// reach realloc.Grow.
func TestZeroReallocKeepsWorking(t *testing.T) {
	mem := make([]byte, 32)
	u32 := bintype.PrimitiveDesc{Prim: "u32"}
	if err := Store(mem, 0, u32, uint32(0x11223344), nil, Realloc{}); err != nil {
		t.Fatalf("Store u32 with a zero Realloc: %v", err)
	}
	if err := Store(mem, 4, u32, uint32(0x55667788), nil, Realloc{}); err != nil {
		t.Fatalf("second Store u32 with a zero Realloc: %v", err)
	}
	for i, want := range []uint32{0x11223344, 0x55667788} {
		got, err := Load(mem, uint32(i)*4, u32, nil)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got != Value(want) {
			t.Errorf("slot %d = %v, want %#x", i, got, want)
		}
	}
	// A zero Realloc must still fail loud the moment something DOES allocate.
	err := Store(mem, 0, bintype.PrimitiveDesc{Prim: "string"}, "grow me", nil, Realloc{})
	if err == nil {
		t.Fatal("expected a zero Realloc to fail loud when a store needs to allocate")
	}
	if !strings.Contains(err.Error(), "cabi_realloc") {
		t.Errorf("error = %v, want it to name the missing cabi_realloc export", err)
	}
}

// TestGrowMemWithoutAccessor covers the nil-Mem fallback itself: GrowMem must
// hand the caller's own slice straight back rather than nil-dereferencing or
// turning "cannot refresh" into an error. Every existing test in this package,
// and every ReallocFunc-based host adapter, depends on it.
func TestGrowMemWithoutAccessor(t *testing.T) {
	mem := make([]byte, 16)

	t.Run("nil Mem", func(t *testing.T) {
		r := ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 4, nil })
		ptr, fresh, err := r.GrowMem(mem, 0, 0, 1, 4)
		if err != nil {
			t.Fatalf("GrowMem: %v", err)
		}
		if ptr != 4 {
			t.Errorf("ptr = %d, want 4", ptr)
		}
		if &fresh[0] != &mem[0] || len(fresh) != len(mem) {
			t.Error("nil Mem must return the caller's own slice unchanged")
		}
	})

	t.Run("Mem answering nil", func(t *testing.T) {
		// A module that lost its memory answers nil; that is not an improvement
		// on what the caller holds, so the caller's slice is kept and the bounds
		// check that follows reports the failure in its own terms.
		r := ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 4, nil })
		r.Mem = func() []byte { return nil }
		_, fresh, err := r.GrowMem(mem, 0, 0, 1, 4)
		if err != nil {
			t.Fatalf("GrowMem: %v", err)
		}
		if &fresh[0] != &mem[0] {
			t.Error("a nil refresh must leave the caller's slice in place")
		}
	})

	t.Run("failed grow", func(t *testing.T) {
		// Mem must not even be consulted when the allocation failed.
		consulted := false
		r := ReallocFunc(func(_, _, _, _ uint32) (uint32, error) { return 0, errAllocatorRefused })
		r.Mem = func() []byte { consulted = true; return nil }
		_, fresh, err := r.GrowMem(mem, 0, 0, 1, 4)
		if err == nil {
			t.Fatal("expected the allocator's error")
		}
		if consulted {
			t.Error("Mem was consulted on a failed grow")
		}
		if &fresh[0] != &mem[0] {
			t.Error("a failed grow must return the caller's slice unchanged")
		}
	})
}

var errAllocatorRefused = errors.New("allocator refused")
