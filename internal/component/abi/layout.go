// Package abi implements the Canonical ABI for the WebAssembly Component Model.
// This package is responsible for computing memory layout, flattening types, and
// managing the lift/lower operations for type marshalling across component boundaries.
//
// The implementation follows the reference specification in:
// https://github.com/WebAssembly/component-model/blob/main/design/mvp/canonical-abi/definitions.py
package abi

import (
	"fmt"
	"math"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// Resolver is a function that resolves a type index to its descriptor.
// Used to follow TypeRef indices during layout computation.
type Resolver func(idx uint32) binary.TypeDesc

// Align rounds offset up to the nearest multiple of alignment.
func Align(offset, alignment uint32) uint32 {
	if alignment == 0 {
		return offset
	}
	return ((offset + alignment - 1) / alignment) * alignment
}

// Alignment computes the byte alignment requirement for a type.
// This mirrors the canonical ABI alignment() function.
func Alignment(t binary.TypeDesc, resolve Resolver) (uint32, error) {
	switch desc := t.(type) {
	// Primitives
	case binary.PrimitiveDesc:
		return alignmentPrimitive(desc.Prim)

	// Composite types
	case binary.ListDesc:
		return alignmentList(desc, resolve)
	case binary.RecordDesc:
		return alignmentRecord(desc, resolve)
	case binary.VariantDesc:
		return alignmentVariant(desc, resolve)
	case binary.TupleDesc:
		return alignmentTuple(desc, resolve)
	case binary.FlagsDesc:
		return alignmentFlags(desc)
	case binary.EnumDesc:
		return alignmentEnum(desc)

	// Special types
	case binary.OptionDesc:
		return alignmentOption(desc, resolve)
	case binary.ResultDesc:
		return alignmentResult(desc, resolve)

	// Handles
	case binary.OwnDesc, binary.BorrowDesc:
		return 4, nil

	// Unsupported types
	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return 0, fmt.Errorf("unsupported type for layout: %T", t)

	default:
		return 0, fmt.Errorf("unknown type descriptor: %T", t)
	}
}

// Size computes the number of bytes occupied by a value of the given type.
// This mirrors the canonical ABI elem_size() function.
func Size(t binary.TypeDesc, resolve Resolver) (uint32, error) {
	switch desc := t.(type) {
	// Primitives
	case binary.PrimitiveDesc:
		return sizePrimitive(desc.Prim)

	// Composite types
	case binary.ListDesc:
		return sizeList(desc, resolve)
	case binary.RecordDesc:
		return sizeRecord(desc, resolve)
	case binary.VariantDesc:
		return sizeVariant(desc, resolve)
	case binary.TupleDesc:
		return sizeTuple(desc, resolve)
	case binary.FlagsDesc:
		return sizeFlags(desc)
	case binary.EnumDesc:
		return sizeEnum(desc)

	// Special types
	case binary.OptionDesc:
		return sizeOption(desc, resolve)
	case binary.ResultDesc:
		return sizeResult(desc, resolve)

	// Handles
	case binary.OwnDesc, binary.BorrowDesc:
		return 4, nil

	// Unsupported types
	case binary.FuncDesc, binary.InstanceDesc, binary.ComponentDesc, binary.ResourceDesc:
		return 0, fmt.Errorf("unsupported type for layout: %T", t)

	default:
		return 0, fmt.Errorf("unknown type descriptor: %T", t)
	}
}

// DiscriminantType returns the core type used to encode a variant discriminant.
// This mirrors the canonical ABI discriminant_type() function.
func DiscriminantType(numCases int) string {
	if numCases <= 0 || numCases > math.MaxUint32 {
		return "" // invalid
	}
	// Compute ceil(log2(numCases) / 8)
	logCases := math.Ceil(math.Log2(float64(numCases)) / 8)
	switch int(logCases) {
	case 0, 1:
		return "u8"
	case 2:
		return "u16"
	default:
		return "u32"
	}
}

// MaxCaseAlignment computes the maximum alignment of all case payloads in a variant.
func MaxCaseAlignment(cases []binary.VariantCase, resolve Resolver) (uint32, error) {
	maxAlign := uint32(1)
	for _, c := range cases {
		if c.Type != nil {
			t, err := resolveType(c.Type, resolve)
			if err != nil {
				return 0, err
			}
			align, err := Alignment(t, resolve)
			if err != nil {
				return 0, err
			}
			if align > maxAlign {
				maxAlign = align
			}
		}
	}
	return maxAlign, nil
}

// ------- Primitive Type Sizes and Alignments -------

func alignmentPrimitive(prim string) (uint32, error) {
	switch prim {
	case "bool", "s8", "u8":
		return 1, nil
	case "s16", "u16":
		return 2, nil
	case "s32", "u32", "f32", "char":
		return 4, nil
	case "s64", "u64", "f64":
		return 8, nil
	case "string":
		// String is pointer + length; alignment depends on pointer size
		// For now assume 4-byte pointers (32-bit). Caller should pass resolver for config.
		return 4, nil
	default:
		return 0, fmt.Errorf("unknown primitive type: %s", prim)
	}
}

func sizePrimitive(prim string) (uint32, error) {
	switch prim {
	case "bool", "s8", "u8":
		return 1, nil
	case "s16", "u16":
		return 2, nil
	case "s32", "u32", "f32", "char":
		return 4, nil
	case "s64", "u64", "f64":
		return 8, nil
	case "string":
		// String is pointer + length (two words)
		return 8, nil
	default:
		return 0, fmt.Errorf("unknown primitive type: %s", prim)
	}
}

// ------- Composite Types -------

func alignmentList(_ binary.ListDesc, _ Resolver) (uint32, error) {
	// A dynamic list is represented as pointer + length; its alignment is the
	// pointer alignment (4 bytes), independent of the element type.
	// (spec: alignment(list) = alignment(u32) = 4.)
	return 4, nil
}

func alignmentRecord(desc binary.RecordDesc, resolve Resolver) (uint32, error) {
	maxAlign := uint32(1)
	for _, f := range desc.Fields {
		ft, err := resolveType(&f.Type, resolve)
		if err != nil {
			return 0, err
		}
		align, err := Alignment(ft, resolve)
		if err != nil {
			return 0, err
		}
		if align > maxAlign {
			maxAlign = align
		}
	}
	return maxAlign, nil
}

func alignmentVariant(desc binary.VariantDesc, resolve Resolver) (uint32, error) {
	discType := DiscriminantType(len(desc.Cases))
	discAlign, err := alignmentPrimitive(discType)
	if err != nil {
		return 0, err
	}
	maxCaseAlign, err := MaxCaseAlignment(desc.Cases, resolve)
	if err != nil {
		return 0, err
	}
	if maxCaseAlign > discAlign {
		return maxCaseAlign, nil
	}
	return discAlign, nil
}

func alignmentTuple(desc binary.TupleDesc, resolve Resolver) (uint32, error) {
	maxAlign := uint32(1)
	for _, elem := range desc.Elements {
		et, err := resolveType(&elem, resolve)
		if err != nil {
			return 0, err
		}
		align, err := Alignment(et, resolve)
		if err != nil {
			return 0, err
		}
		if align > maxAlign {
			maxAlign = align
		}
	}
	return maxAlign, nil
}

func alignmentFlags(desc binary.FlagsDesc) (uint32, error) {
	return alignmentFlagsNumLabels(len(desc.Names))
}

func alignmentFlagsNumLabels(numLabels int) (uint32, error) {
	if numLabels <= 0 || numLabels > 32 {
		return 0, fmt.Errorf("invalid flags: %d labels", numLabels)
	}
	switch {
	case numLabels <= 8:
		return 1, nil
	case numLabels <= 16:
		return 2, nil
	default:
		return 4, nil
	}
}

func alignmentEnum(desc binary.EnumDesc) (uint32, error) {
	return alignmentFlagsNumLabels(len(desc.Cases))
}

func alignmentOption(desc binary.OptionDesc, resolve Resolver) (uint32, error) {
	// Option is a variant with "none" and "some" cases
	elemT, err := resolveType(&desc.Element, resolve)
	if err != nil {
		return 0, err
	}
	elemAlign, err := Alignment(elemT, resolve)
	if err != nil {
		return 0, err
	}
	discAlign, _ := alignmentPrimitive("u8")
	if elemAlign > discAlign {
		return elemAlign, nil
	}
	return discAlign, nil
}

func alignmentResult(desc binary.ResultDesc, resolve Resolver) (uint32, error) {
	// Result is a variant with "ok" and "error" cases
	maxAlign := uint32(1)
	if desc.Ok != nil {
		okT, err := resolveType(desc.Ok, resolve)
		if err != nil {
			return 0, err
		}
		okAlign, err := Alignment(okT, resolve)
		if err != nil {
			return 0, err
		}
		if okAlign > maxAlign {
			maxAlign = okAlign
		}
	}
	if desc.Err != nil {
		errT, err := resolveType(desc.Err, resolve)
		if err != nil {
			return 0, err
		}
		errAlign, err := Alignment(errT, resolve)
		if err != nil {
			return 0, err
		}
		if errAlign > maxAlign {
			maxAlign = errAlign
		}
	}
	discAlign, _ := alignmentPrimitive("u8")
	if maxAlign > discAlign {
		return maxAlign, nil
	}
	return discAlign, nil
}

// ------- Size Functions -------

func sizeList(_ binary.ListDesc, _ Resolver) (uint32, error) {
	// A dynamic list is always represented as pointer + length = 8 bytes,
	// regardless of the element type/size.
	// (spec: size(list) = 8.)
	return 8, nil
}

func sizeRecord(desc binary.RecordDesc, resolve Resolver) (uint32, error) {
	offset := uint32(0)
	for _, f := range desc.Fields {
		ft, err := resolveType(&f.Type, resolve)
		if err != nil {
			return 0, err
		}
		align, err := Alignment(ft, resolve)
		if err != nil {
			return 0, err
		}
		offset = Align(offset, align)
		size, err := Size(ft, resolve)
		if err != nil {
			return 0, err
		}
		offset += size
	}
	// Align final size to record alignment
	recordAlign, err := alignmentRecord(desc, resolve)
	if err != nil {
		return 0, err
	}
	return Align(offset, recordAlign), nil
}

func sizeVariant(desc binary.VariantDesc, resolve Resolver) (uint32, error) {
	discType := DiscriminantType(len(desc.Cases))
	discSize, err := sizePrimitive(discType)
	if err != nil {
		return 0, err
	}

	offset := discSize
	maxCaseAlign, err := MaxCaseAlignment(desc.Cases, resolve)
	if err != nil {
		return 0, err
	}
	offset = Align(offset, maxCaseAlign)

	maxCaseSize := uint32(0)
	for _, c := range desc.Cases {
		if c.Type != nil {
			ct, err := resolveType(c.Type, resolve)
			if err != nil {
				return 0, err
			}
			cSize, err := Size(ct, resolve)
			if err != nil {
				return 0, err
			}
			if cSize > maxCaseSize {
				maxCaseSize = cSize
			}
		}
	}
	offset += maxCaseSize

	// Align final size to variant alignment
	variantAlign, err := alignmentVariant(desc, resolve)
	if err != nil {
		return 0, err
	}
	return Align(offset, variantAlign), nil
}

func sizeTuple(desc binary.TupleDesc, resolve Resolver) (uint32, error) {
	offset := uint32(0)
	for _, elem := range desc.Elements {
		et, err := resolveType(&elem, resolve)
		if err != nil {
			return 0, err
		}
		align, err := Alignment(et, resolve)
		if err != nil {
			return 0, err
		}
		offset = Align(offset, align)
		size, err := Size(et, resolve)
		if err != nil {
			return 0, err
		}
		offset += size
	}
	// Align final size to tuple alignment
	tupleAlign, err := alignmentTuple(desc, resolve)
	if err != nil {
		return 0, err
	}
	return Align(offset, tupleAlign), nil
}

func sizeFlags(desc binary.FlagsDesc) (uint32, error) {
	return sizeFlagsNumLabels(len(desc.Names))
}

func sizeFlagsNumLabels(numLabels int) (uint32, error) {
	if numLabels <= 0 || numLabels > 32 {
		return 0, fmt.Errorf("invalid flags: %d labels", numLabels)
	}
	switch {
	case numLabels <= 8:
		return 1, nil
	case numLabels <= 16:
		return 2, nil
	default:
		return 4, nil
	}
}

func sizeEnum(desc binary.EnumDesc) (uint32, error) {
	return sizeFlagsNumLabels(len(desc.Cases))
}

func sizeOption(desc binary.OptionDesc, resolve Resolver) (uint32, error) {
	// Option is a variant with "none" and "some" cases
	elemT, err := resolveType(&desc.Element, resolve)
	if err != nil {
		return 0, err
	}
	elemSize, err := Size(elemT, resolve)
	if err != nil {
		return 0, err
	}
	elemAlign, err := Alignment(elemT, resolve)
	if err != nil {
		return 0, err
	}
	discAlign, _ := alignmentPrimitive("u8")

	// Size = discriminant + padding + max(element)
	offset := uint32(1)
	offset = Align(offset, elemAlign)
	offset += elemSize

	// Align to variant alignment
	maxAlign := max(discAlign, elemAlign)
	return Align(offset, maxAlign), nil
}

func sizeResult(desc binary.ResultDesc, resolve Resolver) (uint32, error) {
	// Result is a variant with "ok" and "error" cases
	offset := uint32(1) // discriminant byte
	maxPayloadSize := uint32(0)
	maxPayloadAlign := uint32(1)

	if desc.Ok != nil {
		okT, err := resolveType(desc.Ok, resolve)
		if err != nil {
			return 0, err
		}
		okSize, err := Size(okT, resolve)
		if err != nil {
			return 0, err
		}
		okAlign, err := Alignment(okT, resolve)
		if err != nil {
			return 0, err
		}
		if okSize > maxPayloadSize {
			maxPayloadSize = okSize
		}
		if okAlign > maxPayloadAlign {
			maxPayloadAlign = okAlign
		}
	}
	if desc.Err != nil {
		errT, err := resolveType(desc.Err, resolve)
		if err != nil {
			return 0, err
		}
		errSize, err := Size(errT, resolve)
		if err != nil {
			return 0, err
		}
		errAlign, err := Alignment(errT, resolve)
		if err != nil {
			return 0, err
		}
		if errSize > maxPayloadSize {
			maxPayloadSize = errSize
		}
		if errAlign > maxPayloadAlign {
			maxPayloadAlign = errAlign
		}
	}

	offset = Align(offset, maxPayloadAlign)
	offset += maxPayloadSize

	// Align to result alignment
	discAlign, _ := alignmentPrimitive("u8")
	maxAlign := max(discAlign, maxPayloadAlign)
	return Align(offset, maxAlign), nil
}

// ------- Helper: resolveType -------

func resolveType(ref *binary.TypeRef, resolve Resolver) (binary.TypeDesc, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil type reference")
	}
	if ref.Primitive != "" {
		return binary.PrimitiveDesc{Prim: ref.Primitive}, nil
	}
	if ref.TypeIndex != nil {
		if resolve == nil {
			return nil, fmt.Errorf("type index %d requires resolver", *ref.TypeIndex)
		}
		return resolve(*ref.TypeIndex), nil
	}
	return nil, fmt.Errorf("type reference has neither primitive nor index")
}
