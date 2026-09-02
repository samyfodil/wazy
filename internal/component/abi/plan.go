package abi

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file lowers the Canonical ABI's per-call lower tree-walk into a plan
// compiled once at bind time. LowerFlatInto, on every call, type-switches on
// the descriptor, calls Flatten (allocating a []string) to decide whether the
// value spills, and returns intermediate []CoreValue slices from each recursion
// level. For a fixed parameter type all of that is invariant across calls, so a
// LowerStep precomputes it: the common leaf kinds (primitive / string / handle
// -- the shapes real WASI signatures overwhelmingly use) execute a direct op
// straight into the destination buffer with no type-switch, no Flatten, and no
// intermediate slice; composites keep the exact tree-walk (lowerFlatImpl /
// spillValue) but with the spill decision precomputed. A LowerStep is immutable
// after CompileLower and safe to share/read concurrently across instances.

type lowerKind uint8

const (
	lowerKindPrimitive lowerKind = iota // non-string primitive -> one core value
	lowerKindString                     // string -> (ptr, len); 2 core values, never spills
	lowerKindHandle                     // own/borrow -> one i32 handle
	lowerKindComposite                  // record/variant/enum/flags/option/result/list/tuple
)

// LowerStep is a compiled plan to lower one top-level value of a fixed type.
type LowerStep struct {
	kind    lowerKind
	prim    string          // kind==primitive
	t       binary.TypeDesc // kind==composite: the resolved type
	resolve Resolver        // kind==composite: for the tree-walk body
	spills  bool            // kind==composite: precomputed len(Flatten(t)) > MaxFlatParams
	flat    []string        // kind==composite, non-spilling: Flatten(t), handed down the tree-walk
	store   StoreStep       // kind==composite, spilling: the spill's precomputed layout
}

// CompileLower builds the LowerStep for an already-resolved top-level parameter
// type t (a concrete TypeDesc, as instance.boundExport's paramTypes holds).
// resolve is used only to precompute a composite's spill decision and to drive
// its tree-walk body. A compile error surfaces the same message LowerFlatInto's
// own Flatten would.
func CompileLower(t binary.TypeDesc, resolve Resolver) (LowerStep, error) {
	switch d := t.(type) {
	case binary.PrimitiveDesc:
		if d.Prim == "string" {
			return LowerStep{kind: lowerKindString}, nil
		}
		return LowerStep{kind: lowerKindPrimitive, prim: d.Prim}, nil
	case binary.OwnDesc, binary.BorrowDesc, binary.StreamDesc, binary.FutureDesc:
		// stream/future are opaque i32 handles, same plan as own/borrow.
		return LowerStep{kind: lowerKindHandle}, nil
	default:
		flat, err := Flatten(t, resolve)
		if err != nil {
			return LowerStep{}, err
		}
		// The flat list computed for the spill decision is kept: Lower hands
		// it down the tree-walk so a variant/result at the top of the tree
		// doesn't re-Flatten itself on every call (see lowerFlatImpl's doc).
		step := LowerStep{kind: lowerKindComposite, t: t, resolve: resolve, spills: len(flat) > MaxFlatParams, flat: flat}
		if step.spills {
			if step.store, err = CompileStore(t, resolve); err != nil {
				return LowerStep{}, err
			}
		}
		return step, nil
	}
}

// Lower appends v's flattened core values to dst, exactly as
// LowerFlatInto(dst, v, t, resolve, realloc, mem) would for the type t that
// CompileLower was built from -- byte-for-byte equivalent (differential-oracle
// verified via TestLowerStepMatchesLowerFlatInto), just without the per-call
// type-switch / Flatten / intermediate slices on the leaf paths.
func (s *LowerStep) Lower(dst []CoreValue, v Value, realloc Realloc, mem []byte) ([]CoreValue, error) {
	switch s.kind {
	case lowerKindPrimitive:
		cv, err := lowerPrimitiveCore(v, s.prim)
		if err != nil {
			return nil, err
		}
		return append(dst, cv), nil

	case lowerKindString:
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lowerFlat string: expected string, got %T", v)
		}
		// A string flattens to exactly (ptr, len) = 2 core values, always <=
		// MaxFlatParams (16), so it can never spill -- append both directly, no
		// Flatten re-check and no intermediate []CoreValue.
		// allocStoreString refreshes `mem` internally around its own grow, so
		// its bounds check and copy target the live array; the refreshed view
		// is dropped here because nothing in this frame -- or in Lower's
		// caller, instance.lowerParams -- writes through `mem` afterwards.
		// See lowerFlatImpl's doc for the invariant that makes that safe.
		ptr, byteLen, _, err := allocStoreString(mem, str, realloc)
		if err != nil {
			return nil, fmt.Errorf("lowerFlatString: %w", err)
		}
		return append(dst, NewCoreValueI32(ptr), NewCoreValueI32(byteLen)), nil

	case lowerKindHandle:
		h, ok := v.(uint32)
		if !ok {
			return nil, fmt.Errorf("lowerFlat: handle expected uint32, got %T", v)
		}
		return append(dst, NewCoreValueI32(h)), nil

	default: // lowerKindComposite
		if s.spills {
			ptr, err := s.store.Spill(v, mem, realloc)
			if err != nil {
				return nil, err
			}
			return append(dst, NewCoreValueI32(ptr)), nil
		}
		flat, err := lowerFlatImpl(v, s.t, s.flat, s.resolve, realloc, mem)
		if err != nil {
			return nil, err
		}
		return append(dst, flat...), nil
	}
}

// StoreStep is the memory-direction counterpart of LowerStep: a plan compiled
// once at bind time to store one value of a fixed type at a guest pointer.
// Store re-derives the type's alignment and size from the type graph on every
// call, and each of those two walks is itself super-linear in the type (a
// record's size asks every field for both its alignment and its size, then
// asks the record for its alignment again) -- on the wasi:http guest->host
// path that pair of walks was the largest single cost of a request. A type's
// layout is immutable, so a caller with a fixed type computes it once here.
type StoreStep struct {
	t       binary.TypeDesc
	resolve Resolver
	align   uint32
	size    uint32
}

// CompileStore resolves t's layout for repeated Store/Spill calls. Its errors
// are exactly the ones the package-level Store raises per call.
func CompileStore(t binary.TypeDesc, resolve Resolver) (StoreStep, error) {
	align, err := Alignment(t, resolve)
	if err != nil {
		return StoreStep{}, err
	}
	size, err := Size(t, resolve)
	if err != nil {
		return StoreStep{}, err
	}
	return StoreStep{t: t, resolve: resolve, align: align, size: size}, nil
}

// Store writes v at ptr, exactly as the package-level Store would for the type
// CompileStore was built from.
//
// mem must be a view that is current as of THIS call: the bounds check below
// runs against it, so a snapshot taken before some earlier allocation would
// reject a pointer into a region that genuinely exists. The []byte returned is
// the view valid after the store -- storing a string or list grows guest
// memory, which can replace the backing array (see Realloc.Mem) -- and a caller
// that stores repeatedly through one view MUST rebind from it. That is exactly
// what instance/stream.go's guestBuffer.write does per element: without it,
// element 0's grow leaves elements 1..n-1 bounds-checked against a stale length
// and written into an abandoned array.
func (s *StoreStep) Store(mem []byte, ptr uint32, v Value, realloc Realloc) ([]byte, error) {
	if ptr != Align(ptr, s.align) {
		return mem, fmt.Errorf("store: pointer %d not aligned to %d", ptr, s.align)
	}
	// ptr+s.size is wrap-guarded because ptr can come straight from the
	// guest's cabi_realloc (Spill, just below): a hostile or buggy allocator
	// returning a pointer near 2^32 would otherwise wrap past the check.
	if ptr+s.size < ptr || uint32(len(mem)) < ptr+s.size {
		return mem, fmt.Errorf("store: buffer overflow: ptr=%d size=%d mem_len=%d", ptr, s.size, len(mem))
	}
	return storeValue(mem, ptr, s.t, v, s.align, s.resolve, realloc)
}

// Spill allocates a region for v and stores it there, exactly as spillValue
// would for the type CompileStore was built from.
//
// This is the whole-parameter-list spill (instance.lowerParams' paramsSpill
// arm), i.e. the single largest allocation on the export path and the one most
// likely to force a page grow -- so the region it just allocated is, by
// construction, at or past the end of the caller's view. GrowMem hands back the
// post-growth view, which is what Store then bounds-checks and writes through;
// with the pre-grow one, Store's own check rejects the fresh pointer outright.
func (s *StoreStep) Spill(v Value, mem []byte, realloc Realloc) (uint32, error) {
	ptr, mem, err := realloc.GrowMem(mem, 0, 0, s.align, s.size)
	if err != nil {
		return 0, fmt.Errorf("spillValue: realloc failed: %w", err)
	}
	// The refreshed view Store returns is dropped: no caller of Spill writes
	// through mem afterwards (they only forward the pointer as a core value).
	if _, err := s.Store(mem, ptr, v, realloc); err != nil {
		return 0, fmt.Errorf("spillValue: store failed: %w", err)
	}
	return ptr, nil
}

// LiftStep is the result-side counterpart of LowerStep: a plan compiled once at
// bind time to lift one top-level result value of a fixed type from the core
// values the guest returned. LiftFlat, on every call, re-Flattens the descriptor
// (to decide the result spill) and type-switches through liftFlatImpl even for a
// scalar result. A LiftStep precomputes the spill decision and dispatches
// directly: a scalar primitive is lifted from its one core value with no
// iterator and no Flatten; own/borrow is a bare i32 handle; a result that
// flattens past MaxFlatResults (a string, list, or multi-field aggregate) is
// loaded from the pointer the guest returned; only a genuinely flat aggregate
// (enum/flags/option-of-scalar that still fits one core value) keeps the
// tree-walk, now without the redundant Flatten. Immutable after CompileLift and
// safe to share across instances.
type LiftStep struct {
	kind    liftKind
	prim    string          // kind==liftPrimitive
	t       binary.TypeDesc // kind==liftSpilled/liftFlatAgg
	resolve Resolver        // kind==liftSpilled/liftFlatAgg
	flat    []string        // kind==liftFlatAgg: Flatten(t), handed down the tree-walk
}

type liftKind uint8

const (
	liftPrimitive liftKind = iota // scalar primitive -> one core value
	liftHandle                    // own/borrow -> one i32 handle
	liftSpilled                   // flattens past MaxFlatResults -> Load(t) from a returned pointer
	liftFlatAgg                   // aggregate that still fits within MaxFlatResults core values
)

// CompileLift builds the LiftStep for an already-resolved top-level result type
// t. The result spill threshold is MaxFlatResults (unlike CompileLower's
// MaxFlatParams), so a string result -- which flattens to (ptr,len) -- spills to
// a single returned pointer and is lifted via Load, matching liftResult's own
// spill path. A compile error surfaces the same message LiftFlat's Flatten would.
func CompileLift(t binary.TypeDesc, resolve Resolver) (LiftStep, error) {
	switch d := t.(type) {
	case binary.PrimitiveDesc:
		if d.Prim == "string" {
			return LiftStep{kind: liftSpilled, t: t, resolve: resolve}, nil
		}
		return LiftStep{kind: liftPrimitive, prim: d.Prim}, nil
	case binary.OwnDesc, binary.BorrowDesc, binary.StreamDesc, binary.FutureDesc:
		// stream/future are opaque i32 handles, same plan as own/borrow.
		return LiftStep{kind: liftHandle}, nil
	default:
		flat, err := Flatten(t, resolve)
		if err != nil {
			return LiftStep{}, err
		}
		if len(flat) > MaxFlatResults {
			return LiftStep{kind: liftSpilled, t: t, resolve: resolve}, nil
		}
		// Keep the flat list computed for the spill decision: Lift hands it
		// down the tree-walk so the aggregate doesn't re-Flatten itself on
		// every call (see liftFlatImpl's doc).
		return LiftStep{kind: liftFlatAgg, t: t, resolve: resolve, flat: flat}, nil
	}
}

// Lift produces the Go result value from the guest's returned core values,
// exactly as liftResult's LiftFlat/Load did -- byte-for-byte equivalent
// (differential-oracle verified), without the per-call Flatten / type-switch on
// the scalar and spilled paths. coreResults holds the guest's flat results
// (already kind-tagged by liftResult); mem is the guest linear memory.
func (s *LiftStep) Lift(coreResults []CoreValue, mem []byte) (Value, error) {
	switch s.kind {
	case liftPrimitive:
		return liftScalarPrimitive(coreResults[0], s.prim)
	case liftHandle:
		return coreResults[0].AsI32(), nil
	case liftSpilled:
		return Load(mem, coreResults[0].AsI32(), s.t, s.resolve)
	default: // liftFlatAgg
		vi := getCoreValueIter(coreResults)
		val, err := liftFlatImpl(vi, s.t, s.flat, s.resolve, mem)
		putCoreValueIter(vi)
		return val, err
	}
}
