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
	case binary.OwnDesc, binary.BorrowDesc:
		return LowerStep{kind: lowerKindHandle}, nil
	default:
		flat, err := Flatten(t, resolve)
		if err != nil {
			return LowerStep{}, err
		}
		return LowerStep{kind: lowerKindComposite, t: t, resolve: resolve, spills: len(flat) > MaxFlatParams}, nil
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
		ptr, byteLen, err := allocStoreString(mem, str, realloc)
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
			ptr, err := spillValue(v, s.t, mem, s.resolve, realloc)
			if err != nil {
				return nil, err
			}
			return append(dst, NewCoreValueI32(ptr)), nil
		}
		flat, err := lowerFlatImpl(v, s.t, s.resolve, realloc, mem)
		if err != nil {
			return nil, err
		}
		return append(dst, flat...), nil
	}
}
