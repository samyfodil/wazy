package wasm

import (
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// This file validates the instructions of the GC proposal, the ones prefixed by OpcodeGCPrefix plus ref.eq.
// br_on_cast and br_on_cast_fail are not here: they manipulate the control block stack, so they stay in the
// main validation loop with the other branch instructions.

// unpackedType is the value type a storage type appears as on the operand stack. The packed types are the
// only ones that differ: they are stored narrow and read as i32.
func unpackedType(st ValueType) ValueType {
	switch st {
	case ValueTypeI8, ValueTypeI16:
		return ValueTypeI32
	}
	return st
}

// isDefaultable reports whether a storage type has a value `struct.new_default` and `array.new_default` can
// fill it with. Every numeric type does, and a nullable reference defaults to null; a non-nullable one has no
// default at all.
func isDefaultable(st ValueType) bool {
	return !st.IsRef() || st.IsNullable()
}

// gcTypeAt resolves a type index immediate to a defined type of the expected composite kind.
func (m *Module) gcTypeAt(idx uint32, want CompositeKind, opName string) (*FunctionType, error) {
	if indexOutOfRange(idx, len(m.TypeSection)) {
		return nil, fmt.Errorf("%s: unknown type %d", opName, idx)
	}
	t := &m.TypeSection[idx]
	if t.CompositeKind != want {
		kinds := map[CompositeKind]string{CompositeKindFunc: "function", CompositeKindStruct: "struct", CompositeKindArray: "array"}
		return nil, fmt.Errorf("%s: type %d is a %s type, not a %s type", opName, idx, kinds[t.CompositeKind], kinds[want])
	}
	return t, nil
}

// fieldAt resolves a field index immediate against a struct type.
func fieldAt(t *FunctionType, idx uint32, typeIndex uint32, opName string) (FieldType, error) {
	if indexOutOfRange(idx, len(t.Fields)) {
		return FieldType{}, fmt.Errorf("%s: unknown field %d in type %d", opName, idx, typeIndex)
	}
	return t.Fields[idx], nil
}

// validateGCInstruction validates one non-branching instruction of the GC proposal, advancing over its
// immediates. body starts at the first immediate byte; the returned count is how many of them were consumed.
func (m *Module) validateGCInstruction(op OpcodeGC, body []byte, vs *valueTypeStack) (uint64, error) {
	name := GCInstructionName(op)
	var read uint64
	// u32 reads the next u32 immediate, panicking through err on a malformed one.
	var readErr error
	u32 := func() uint32 {
		if readErr != nil {
			return 0
		}
		v, n, err := leb128.LoadUint32(body[read:])
		if err != nil {
			readErr = fmt.Errorf("%s: read immediate: %w", name, err)
			return 0
		}
		read += uint64(n)
		return v
	}

	// popRef pops a reference operand and checks it is a subtype of want, letting the unknown type through
	// as every check in this validator does.
	popRef := func(want ValueType) error {
		have, err := vs.pop()
		if err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		if have == valueTypeUnknown {
			return nil
		}
		if !isRefSubtypeOf(have, want, m.TypeSection) {
			return fmt.Errorf("%s: expected %s but was %s", name, ValueTypeName(want), ValueTypeName(have))
		}
		return nil
	}
	popVal := func(want ValueType) error {
		if err := vs.popAndVerifyType(want); err != nil {
			return fmt.Errorf("%s: %v", name, err)
		}
		return nil
	}

	switch op {
	case OpcodeGCStructNew, OpcodeGCStructNewDefault:
		idx := u32()
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(idx, CompositeKindStruct, name)
		if err != nil {
			return read, err
		}
		if op == OpcodeGCStructNewDefault {
			for i, f := range t.Fields {
				if !isDefaultable(f.Type) {
					return read, fmt.Errorf("%s: field %d of type %d has no default value", name, i, idx)
				}
			}
		} else {
			// Fields were pushed in order, so pop them in reverse.
			for i := len(t.Fields) - 1; i >= 0; i-- {
				if err := popVal(unpackedType(t.Fields[i].Type)); err != nil {
					return read, err
				}
			}
		}
		vs.push(ValueTypeConcreteRef(idx, false))

	case OpcodeGCStructGet, OpcodeGCStructGetS, OpcodeGCStructGetU, OpcodeGCStructSet:
		typeIdx, fieldIdx := u32(), u32()
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(typeIdx, CompositeKindStruct, name)
		if err != nil {
			return read, err
		}
		f, err := fieldAt(t, fieldIdx, typeIdx, name)
		if err != nil {
			return read, err
		}
		if err := checkPackedAccess(op, f.Type, name); err != nil {
			return read, err
		}
		if op == OpcodeGCStructSet {
			if !f.Mutable {
				return read, fmt.Errorf("%s: field %d of type %d is immutable", name, fieldIdx, typeIdx)
			}
			if err := popVal(unpackedType(f.Type)); err != nil {
				return read, err
			}
		}
		if err := popRef(ValueTypeConcreteRef(typeIdx, true)); err != nil {
			return read, err
		}
		if op != OpcodeGCStructSet {
			vs.push(unpackedType(f.Type))
		}

	case OpcodeGCArrayNew, OpcodeGCArrayNewDefault, OpcodeGCArrayNewFixed:
		idx := u32()
		var count uint32
		if op == OpcodeGCArrayNewFixed {
			count = u32()
		}
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(idx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		elem := t.Fields[0].Type
		switch op {
		case OpcodeGCArrayNewDefault:
			if !isDefaultable(elem) {
				return read, fmt.Errorf("%s: the element of type %d has no default value", name, idx)
			}
			if err := popVal(ValueTypeI32); err != nil {
				return read, err
			}
		case OpcodeGCArrayNew:
			if err := popVal(ValueTypeI32); err != nil {
				return read, err
			}
			if err := popVal(unpackedType(elem)); err != nil {
				return read, err
			}
		case OpcodeGCArrayNewFixed:
			for i := uint32(0); i < count; i++ {
				if err := popVal(unpackedType(elem)); err != nil {
					return read, err
				}
			}
		}
		vs.push(ValueTypeConcreteRef(idx, false))

	case OpcodeGCArrayNewData, OpcodeGCArrayNewElem, OpcodeGCArrayInitData, OpcodeGCArrayInitElem:
		typeIdx, segIdx := u32(), u32()
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(typeIdx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		elem := t.Fields[0].Type
		fromData := op == OpcodeGCArrayNewData || op == OpcodeGCArrayInitData
		if fromData {
			if elem.IsRef() {
				return read, fmt.Errorf("%s: the element of type %d is a reference type", name, typeIdx)
			}
			if m.DataCountSection == nil {
				return read, fmt.Errorf("%s requires data count section", name)
			}
			if segIdx >= *m.DataCountSection {
				return read, fmt.Errorf("%s: unknown data segment %d", name, segIdx)
			}
		} else {
			if indexOutOfRange(segIdx, len(m.ElementSection)) {
				return read, fmt.Errorf("%s: unknown element segment %d", name, segIdx)
			}
			if !isRefSubtypeOf(m.ElementSection[segIdx].Type, elem, m.TypeSection) {
				return read, fmt.Errorf("%s: element segment %d holds %s, which is not a subtype of %s",
					name, segIdx, ValueTypeName(m.ElementSection[segIdx].Type), ValueTypeName(elem))
			}
		}
		isInit := op == OpcodeGCArrayInitData || op == OpcodeGCArrayInitElem
		if err := popVal(ValueTypeI32); err != nil { // length
			return read, err
		}
		if err := popVal(ValueTypeI32); err != nil { // segment offset
			return read, err
		}
		if isInit {
			if !t.Fields[0].Mutable {
				return read, fmt.Errorf("%s: the element of type %d is immutable", name, typeIdx)
			}
			if err := popVal(ValueTypeI32); err != nil { // destination index
				return read, err
			}
			if err := popRef(ValueTypeConcreteRef(typeIdx, true)); err != nil {
				return read, err
			}
		} else {
			vs.push(ValueTypeConcreteRef(typeIdx, false))
		}

	case OpcodeGCArrayGet, OpcodeGCArrayGetS, OpcodeGCArrayGetU, OpcodeGCArraySet:
		idx := u32()
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(idx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		elem := t.Fields[0]
		if err := checkPackedAccess(op, elem.Type, name); err != nil {
			return read, err
		}
		if op == OpcodeGCArraySet {
			if !elem.Mutable {
				return read, fmt.Errorf("%s: the element of type %d is immutable", name, idx)
			}
			if err := popVal(unpackedType(elem.Type)); err != nil {
				return read, err
			}
		}
		if err := popVal(ValueTypeI32); err != nil {
			return read, err
		}
		if err := popRef(ValueTypeConcreteRef(idx, true)); err != nil {
			return read, err
		}
		if op != OpcodeGCArraySet {
			vs.push(unpackedType(elem.Type))
		}

	case OpcodeGCArrayLen:
		if err := popRef(ValueTypeArrayref); err != nil {
			return read, err
		}
		vs.push(ValueTypeI32)

	case OpcodeGCArrayFill:
		idx := u32()
		if readErr != nil {
			return read, readErr
		}
		t, err := m.gcTypeAt(idx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		if !t.Fields[0].Mutable {
			return read, fmt.Errorf("%s: the element of type %d is immutable", name, idx)
		}
		if err := popVal(ValueTypeI32); err != nil { // length
			return read, err
		}
		if err := popVal(unpackedType(t.Fields[0].Type)); err != nil { // value
			return read, err
		}
		if err := popVal(ValueTypeI32); err != nil { // offset
			return read, err
		}
		if err := popRef(ValueTypeConcreteRef(idx, true)); err != nil {
			return read, err
		}

	case OpcodeGCArrayCopy:
		dstIdx, srcIdx := u32(), u32()
		if readErr != nil {
			return read, readErr
		}
		dst, err := m.gcTypeAt(dstIdx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		src, err := m.gcTypeAt(srcIdx, CompositeKindArray, name)
		if err != nil {
			return read, err
		}
		if !dst.Fields[0].Mutable {
			return read, fmt.Errorf("%s: the element of type %d is immutable", name, dstIdx)
		}
		if !valueTypeMatches(src.Fields[0].Type, dst.Fields[0].Type, m.TypeSection) {
			return read, fmt.Errorf("%s: %s is not a subtype of %s",
				name, ValueTypeName(src.Fields[0].Type), ValueTypeName(dst.Fields[0].Type))
		}
		if err := popVal(ValueTypeI32); err != nil { // length
			return read, err
		}
		if err := popVal(ValueTypeI32); err != nil { // source index
			return read, err
		}
		if err := popRef(ValueTypeConcreteRef(srcIdx, true)); err != nil {
			return read, err
		}
		if err := popVal(ValueTypeI32); err != nil { // destination index
			return read, err
		}
		if err := popRef(ValueTypeConcreteRef(dstIdx, true)); err != nil {
			return read, err
		}

	case OpcodeGCRefI31:
		if err := popVal(ValueTypeI32); err != nil {
			return read, err
		}
		vs.push(ValueTypeI31ref.AsNonNullable())

	case OpcodeGCI31GetS, OpcodeGCI31GetU:
		if err := popRef(ValueTypeI31ref); err != nil {
			return read, err
		}
		vs.push(ValueTypeI32)

	case OpcodeGCAnyConvertExtern, OpcodeGCExternConvertAny:
		from, to := ValueTypeExternref, ValueTypeAnyref
		if op == OpcodeGCExternConvertAny {
			from, to = ValueTypeAnyref, ValueTypeExternref
		}
		have, err := vs.pop()
		if err != nil {
			return read, fmt.Errorf("%s: %v", name, err)
		}
		if have != valueTypeUnknown {
			if !isRefSubtypeOf(have, from, m.TypeSection) {
				return read, fmt.Errorf("%s: expected %s but was %s", name, ValueTypeName(from), ValueTypeName(have))
			}
			// The conversion is bijective, so it carries nullability across rather than widening to it.
			if !have.IsNullable() {
				to = to.AsNonNullable()
			}
		}
		vs.push(to)

	default:
		return read, fmt.Errorf("invalid GC instruction: %#x", op)
	}
	return read, readErr
}

// checkPackedAccess enforces the spec's rule that a packed field is read with the signed or unsigned form and
// an unpacked one with the plain form, so that no access silently picks a widening.
func checkPackedAccess(op OpcodeGC, st ValueType, name string) error {
	packed := st == ValueTypeI8 || st == ValueTypeI16
	switch op {
	case OpcodeGCStructGet, OpcodeGCArrayGet:
		if packed {
			return fmt.Errorf("%s: the field is packed, so it needs the _s or _u form", name)
		}
	case OpcodeGCStructGetS, OpcodeGCStructGetU, OpcodeGCArrayGetS, OpcodeGCArrayGetU:
		if !packed {
			return fmt.Errorf("%s: the field is not packed", name)
		}
	}
	return nil
}

// validateBrOnCast validates br_on_cast and br_on_cast_fail, which branch on whether a reference is of a
// given type. Their immediates are a flags byte carrying the two nullabilities, a label index, and the two
// heap types: the type the operand is known to have and the type being cast to.
func (m *Module) validateBrOnCast(op OpcodeGC, body []byte, vs *valueTypeStack, cs *controlBlockStack) (uint64, error) {
	name := GCInstructionName(op)
	if len(body) == 0 {
		return 0, fmt.Errorf("%s: missing flags", name)
	}
	flags := body[0]
	if flags > 3 {
		return 1, fmt.Errorf("%s: invalid flags %#x", name, flags)
	}
	read := uint64(1)

	label, n, err := leb128.LoadUint32(body[read:])
	if err != nil {
		return read, fmt.Errorf("%s: read label: %w", name, err)
	}
	read += uint64(n)

	// rt1 is the type the operand already has, rt2 the type being cast to.
	rt1, n1, err := decodeHeapTypeImmediate(body[read:], flags&1 != 0, m.TypeSection)
	if err != nil {
		return read, fmt.Errorf("%s: %w", name, err)
	}
	read += n1
	rt2, n2, err := decodeHeapTypeImmediate(body[read:], flags&2 != 0, m.TypeSection)
	if err != nil {
		return read, fmt.Errorf("%s: %w", name, err)
	}
	read += n2

	if indexOutOfRange(label, len(cs.stack)) {
		return read, fmt.Errorf("%s: invalid label index %d", name, label)
	}
	if !isRefSubtypeOf(rt2, rt1, m.TypeSection) {
		return read, fmt.Errorf("%s: %s is not a subtype of %s", name, ValueTypeName(rt2), ValueTypeName(rt1))
	}

	// The difference type: what is left of rt1 once the values that are rt2 are taken out. Only nullability
	// narrows, since a failed cast on a nullable operand can still have been null.
	diff := rt1
	if rt2.IsNullable() {
		diff = rt1.AsNonNullable()
	}

	have, err := vs.pop()
	if err != nil {
		return read, fmt.Errorf("%s: %v", name, err)
	}
	if have != valueTypeUnknown && !isRefSubtypeOf(have, rt1, m.TypeSection) {
		return read, fmt.Errorf("%s: expected %s but was %s", name, ValueTypeName(rt1), ValueTypeName(have))
	}

	// br_on_cast branches with the cast type and falls through with the difference; br_on_cast_fail is the
	// other way round.
	branched, fallthrough_ := rt2, diff
	if op == OpcodeGCBrOnCastFail {
		branched, fallthrough_ = diff, rt2
	}

	target := &cs.stack[len(cs.stack)-int(label)-1]
	targetTypes := target.blockType.Results
	if target.op == OpcodeLoop {
		targetTypes = target.blockType.Params
	}
	if len(targetTypes) == 0 {
		return read, fmt.Errorf("%s: label %d has no results but needs a reference type", name, label)
	}
	last := targetTypes[len(targetTypes)-1]
	if !isRefSubtypeOf(branched, last, m.TypeSection) {
		return read, fmt.Errorf("%s: %s is not a subtype of label %d's last result %s",
			name, ValueTypeName(branched), label, ValueTypeName(last))
	}
	// The rest of the label's types have to be on the stack too, and stay there for the fall-through path.
	remaining := targetTypes[:len(targetTypes)-1]
	if err := vs.popResults(OpcodeGCPrefix, remaining, false); err != nil {
		return read, fmt.Errorf("%s: %v", name, err)
	}
	for _, t := range remaining {
		vs.push(t)
	}
	vs.push(fallthrough_)
	return read, nil
}
