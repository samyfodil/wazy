package abi

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

const (
	MaxFlatParams      = 16
	MaxFlatResults     = 1
	MaxFlatAsyncParams = 4
)

// Flatten converts a type into a sequence of core WebAssembly value types.
// This mirrors the canonical ABI flatten_type() function.
// Returns a slice of core type names: "i32", "i64", "f32", "f64".
func Flatten(t binary.TypeDesc, resolve Resolver) ([]string, error) {
	switch desc := t.(type) {
	// Primitives map directly
	case binary.PrimitiveDesc:
		return flattenPrimitive(desc.Prim)

	// Composite types
	case binary.ListDesc:
		return flattenList(desc, resolve)
	case binary.RecordDesc:
		return flattenRecord(desc, resolve)
	case binary.VariantDesc:
		return flattenVariant(desc, resolve)
	case binary.TupleDesc:
		return flattenTuple(desc, resolve)
	case binary.FlagsDesc:
		return flattenFlags(desc)
	case binary.EnumDesc:
		return flattenEnum(desc)

	// Special types
	case binary.OptionDesc:
		return flattenOption(desc, resolve)
	case binary.ResultDesc:
		return flattenResult(desc, resolve)

	// Handles
	case binary.OwnDesc, binary.BorrowDesc:
		return []string{"i32"}, nil

	// Unsupported
	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return nil, fmt.Errorf("cannot flatten unsupported type: %T", t)

	default:
		return nil, fmt.Errorf("unknown type descriptor: %T", t)
	}
}

// FlattenFunc flattens a function type's parameters and results,
// applying the MAX_FLAT_PARAMS and MAX_FLAT_RESULTS limits.
// Context must be either "lift" or "lower" and controls spill-to-memory behavior.
// This mirrors the canonical ABI flatten_functype() function.
func FlattenFunc(f binary.FuncDesc, resolve Resolver, context string) (params []string, results []string, err error) {
	// Flatten parameters
	for _, p := range f.Params {
		pt, err := resolveType(&p.Type, resolve)
		if err != nil {
			return nil, nil, err
		}
		pFlat, err := Flatten(pt, resolve)
		if err != nil {
			return nil, nil, err
		}
		params = append(params, pFlat...)
	}

	// Flatten results
	if f.Results.Unnamed != nil {
		rt, err := resolveType(f.Results.Unnamed, resolve)
		if err != nil {
			return nil, nil, err
		}
		rFlat, err := Flatten(rt, resolve)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, rFlat...)
	} else {
		for _, r := range f.Results.Named {
			rt, err := resolveType(&r.Type, resolve)
			if err != nil {
				return nil, nil, err
			}
			rFlat, err := Flatten(rt, resolve)
			if err != nil {
				return nil, nil, err
			}
			results = append(results, rFlat...)
		}
	}

	// Apply spill-to-memory rules (non-async)
	if len(params) > MaxFlatParams {
		params = []string{"i32"} // spill to memory via pointer
	}
	if len(results) > MaxFlatResults {
		if context == "lift" {
			results = []string{"i32"} // return pointer to result
		} else if context == "lower" {
			params = append(params, "i32") // output pointer passed in params
			results = []string{}
		} else {
			return nil, nil, fmt.Errorf("invalid context: %s (must be 'lift' or 'lower')", context)
		}
	}

	return params, results, nil
}

// ------- Primitive Flattening -------

func flattenPrimitive(prim string) ([]string, error) {
	switch prim {
	case "bool":
		return []string{"i32"}, nil
	case "u8", "u16", "u32", "s8", "s16", "s32":
		return []string{"i32"}, nil
	case "s64", "u64":
		return []string{"i64"}, nil
	case "f32":
		return []string{"f32"}, nil
	case "f64":
		return []string{"f64"}, nil
	case "char":
		return []string{"i32"}, nil
	case "string":
		// String = pointer + length (both as i32 pointers)
		return []string{"i32", "i32"}, nil
	default:
		return nil, fmt.Errorf("unknown primitive type: %s", prim)
	}
}

// ------- Composite Flattening -------

func flattenList(_ binary.ListDesc, _ Resolver) ([]string, error) {
	// Dynamic list: pointer + length (as i32 + i32)
	// Note: we don't use elemFlat for dynamic lists; fixed-length lists would use it.
	return []string{"i32", "i32"}, nil
}

func flattenRecord(desc binary.RecordDesc, resolve Resolver) ([]string, error) {
	var flat []string
	for _, f := range desc.Fields {
		ft, err := resolveType(&f.Type, resolve)
		if err != nil {
			return nil, err
		}
		fFlat, err := Flatten(ft, resolve)
		if err != nil {
			return nil, err
		}
		flat = append(flat, fFlat...)
	}
	return flat, nil
}

func flattenVariant(desc binary.VariantDesc, resolve Resolver) ([]string, error) {
	// Flatten discriminant first
	discType := DiscriminantType(len(desc.Cases))
	discFlat, err := flattenPrimitive(discType)
	if err != nil {
		return nil, err
	}

	// Collect all case payloads and join their types
	var caseFlats [][]string
	for _, c := range desc.Cases {
		if c.Type != nil {
			ct, err := resolveType(c.Type, resolve)
			if err != nil {
				return nil, err
			}
			cFlat, err := Flatten(ct, resolve)
			if err != nil {
				return nil, err
			}
			caseFlats = append(caseFlats, cFlat)
		}
	}

	// Join all case flats together by position
	var joined []string
	maxLen := 0
	for _, cFlat := range caseFlats {
		if len(cFlat) > maxLen {
			maxLen = len(cFlat)
		}
	}

	for i := 0; i < maxLen; i++ {
		var candidates []string
		for _, cFlat := range caseFlats {
			if i < len(cFlat) {
				candidates = append(candidates, cFlat[i])
			}
		}
		if len(candidates) > 0 {
			joined = append(joined, joinCoreTypes(candidates))
		}
	}

	// Prepend discriminant
	return append(discFlat, joined...), nil
}

func flattenTuple(desc binary.TupleDesc, resolve Resolver) ([]string, error) {
	var flat []string
	for _, elem := range desc.Elements {
		et, err := resolveType(&elem, resolve)
		if err != nil {
			return nil, err
		}
		eFlat, err := Flatten(et, resolve)
		if err != nil {
			return nil, err
		}
		flat = append(flat, eFlat...)
	}
	return flat, nil
}

func flattenFlags(desc binary.FlagsDesc) ([]string, error) {
	return flattenFlagsNumLabels(len(desc.Names))
}

func flattenFlagsNumLabels(numLabels int) ([]string, error) {
	if numLabels <= 0 || numLabels > 32 {
		return nil, fmt.Errorf("invalid flags: %d labels", numLabels)
	}
	// Flags always flatten to a single i32
	return []string{"i32"}, nil
}

func flattenEnum(desc binary.EnumDesc) ([]string, error) {
	return flattenFlagsNumLabels(len(desc.Cases))
}

func flattenOption(desc binary.OptionDesc, resolve Resolver) ([]string, error) {
	// Option is a variant: discriminant (u8) + element type
	elemT, err := resolveType(&desc.Element, resolve)
	if err != nil {
		return nil, err
	}
	elemFlat, err := Flatten(elemT, resolve)
	if err != nil {
		return nil, err
	}
	// Discriminant (u8) + max(element)
	// u8 flattens to i32, join with element
	var candidates []string
	candidates = append(candidates, "i32") // discriminant
	candidates = append(candidates, elemFlat...)
	return candidates, nil
}

func flattenResult(desc binary.ResultDesc, resolve Resolver) ([]string, error) {
	// Result is a variant: discriminant (u8) + max(ok, error)
	var okFlat, errFlat []string
	var err error

	if desc.Ok != nil {
		okT, e := resolveType(desc.Ok, resolve)
		if e != nil {
			return nil, e
		}
		okFlat, err = Flatten(okT, resolve)
		if err != nil {
			return nil, err
		}
	}

	if desc.Err != nil {
		errT, e := resolveType(desc.Err, resolve)
		if e != nil {
			return nil, e
		}
		errFlat, err = Flatten(errT, resolve)
		if err != nil {
			return nil, err
		}
	}

	// Join ok and error flats by position
	var joined []string
	maxLen := max(len(errFlat), max(len(okFlat), 0))

	for i := range maxLen {
		var candidates []string
		if i < len(okFlat) {
			candidates = append(candidates, okFlat[i])
		}
		if i < len(errFlat) {
			candidates = append(candidates, errFlat[i])
		}
		if len(candidates) > 0 {
			joined = append(joined, joinCoreTypes(candidates))
		}
	}

	// Prepend discriminant (u8, flattens to i32)
	return append([]string{"i32"}, joined...), nil
}

// ------- Helper: Join core types -------

// joinCoreTypes merges multiple core types at the same position in the flat sequence.
// This mirrors the canonical ABI join() function applied pairwise across a set of
// candidates: if all types are identical, return that type; otherwise, if every
// candidate is in {i32, f32}, the join is i32; otherwise (any i64 or f64 present,
// or a mix that isn't purely i32/f32) the join is i64.
func joinCoreTypes(types []string) string {
	if len(types) == 0 {
		return "i32" // shouldn't happen
	}

	// Check if all are the same
	all := types[0]
	allSame := true
	allI32OrF32 := true
	for _, t := range types {
		if t != all {
			allSame = false
		}
		if t != "i32" && t != "f32" {
			allI32OrF32 = false
		}
	}
	if allSame {
		return all
	}
	if allI32OrF32 {
		return "i32"
	}
	return "i64"
}
