package abi

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// maxFlatWidth bounds a flattened width.
//
// Widths are summed over a type graph the resolver hands back, and nothing
// stops a malformed or hostile component from describing one that expands
// exponentially -- a record of records, nested deeply enough. Unbounded
// addition would wrap to a negative width and take the spill decision with
// it, which is a corrupted lift rather than a rejected type. The bound is far
// above any real signature (Flatten of such a type would be building a
// multi-megabyte slice by then), so reaching it means the type is malformed.
const maxFlatWidth = 1 << 20

// addFlatWidth sums two widths, failing rather than wrapping.
func addFlatWidth(a, b int) (int, error) {
	if a > maxFlatWidth-b {
		return 0, fmt.Errorf("flattened width exceeds %d core values", maxFlatWidth)
	}
	return a + b, nil
}

// FlatWidth returns how many core values t flattens to, without building the
// flattened type list.
//
// It answers exactly len(Flatten(t, resolve)) -- flatwidth_test.go asserts that
// on every shape, including the errors -- and exists because most callers only
// ever wanted the count. Flatten allocates a []string per node of the type
// graph, so asking it for a number means allocating a tree and reading its
// length: on the wasi:http path that was the single largest source of garbage,
// since every option::none rebuilt its element's whole flattened layout just to
// learn how many core slots to skip.
//
// Callers that need the flattened types themselves (variant and result lifting
// coerce each slot against the joined types) still use Flatten.
func FlatWidth(t binary.TypeDesc, resolve Resolver) (int, error) {
	switch desc := t.(type) {
	case binary.PrimitiveDesc:
		return flatWidthPrimitive(desc.Prim)

	case binary.ListDesc:
		// A dynamic list is a pointer and a length.
		return 2, nil

	case binary.RecordDesc:
		total := 0
		for i := range desc.Fields {
			w, err := flatWidthRef(&desc.Fields[i].Type, resolve)
			if err != nil {
				return 0, err
			}
			if total, err = addFlatWidth(total, w); err != nil {
				return 0, err
			}
		}
		return total, nil

	case binary.TupleDesc:
		total := 0
		for i := range desc.Elements {
			w, err := flatWidthRef(&desc.Elements[i], resolve)
			if err != nil {
				return 0, err
			}
			if total, err = addFlatWidth(total, w); err != nil {
				return 0, err
			}
		}
		return total, nil

	case binary.VariantDesc:
		// A discriminant, then the join of the case payloads -- and a join is
		// as wide as its widest member.
		widest := 0
		for _, c := range desc.Cases {
			if c.Type == nil {
				continue
			}
			w, err := flatWidthRef(c.Type, resolve)
			if err != nil {
				return 0, err
			}
			if w > widest {
				widest = w
			}
		}
		disc, err := flatWidthPrimitive(DiscriminantType(len(desc.Cases)))
		if err != nil {
			return 0, err
		}
		return addFlatWidth(disc, widest)

	case binary.OptionDesc:
		w, err := flatWidthRef(&desc.Element, resolve)
		if err != nil {
			return 0, err
		}
		return addFlatWidth(1, w)

	case binary.ResultDesc:
		var okW, errW int
		var err error
		if desc.Ok != nil {
			if okW, err = flatWidthRef(desc.Ok, resolve); err != nil {
				return 0, err
			}
		}
		if desc.Err != nil {
			if errW, err = flatWidthRef(desc.Err, resolve); err != nil {
				return 0, err
			}
		}
		if errW > okW {
			okW = errW
		}
		return addFlatWidth(1, okW)

	case binary.FlagsDesc:
		// Reuses flattenFlagsNumLabels rather than repeating its label cap,
		// so an invalid flags set fails identically either way.
		f, err := flattenFlagsNumLabels(len(desc.Names))
		if err != nil {
			return 0, err
		}
		return len(f), nil

	case binary.EnumDesc:
		f, err := flattenEnum(desc)
		if err != nil {
			return 0, err
		}
		return len(f), nil

	case binary.OwnDesc, binary.BorrowDesc, binary.StreamDesc, binary.FutureDesc:
		return 1, nil

	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return 0, fmt.Errorf("cannot flatten unsupported type: %T", t)

	default:
		return 0, fmt.Errorf("unknown type descriptor: %T", t)
	}
}

// flatWidthRef resolves ref and returns its flat width.
func flatWidthRef(ref *binary.TypeRef, resolve Resolver) (int, error) {
	t, err := resolveType(ref, resolve)
	if err != nil {
		return 0, err
	}
	return FlatWidth(t, resolve)
}

// flatWidthPrimitive is flattenPrimitive's width, sharing its unknown-primitive
// error so the two cannot disagree about what is valid.
func flatWidthPrimitive(prim string) (int, error) {
	f, err := flattenPrimitive(prim)
	if err != nil {
		return 0, err
	}
	return len(f), nil
}
