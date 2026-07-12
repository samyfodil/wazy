package wasm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/samyfodil/wazy/internal/leb128"
)

type ConstantExpression struct {
	Data []byte
}

// evaluateConstExprFast attempts to evaluate a single-instruction constant expression -- the
// overwhelmingly common case: i32.const/i64.const/f32.const/f64.const/global.get/ref.func/ref.null/
// v128.const immediately followed by end -- without allocating the general evaluator's operand/type
// stacks below. ok is false whenever data doesn't match one of these single-instruction forms (a
// multi-instruction extended-const sequence, or any decode issue at all, however minor); the caller must
// then fall back to the general evaluator, which independently re-derives the exact same result or error
// (byte-for-byte identical messages) for that rarer case, so no behavior is ever duplicated or risked
// diverging here: this function only ever "commits" to a result once it has fully confirmed the match.
func evaluateConstExprFast(
	data []byte,
	globalResolver func(globalIndex Index) (ValueType, uint64, uint64, error),
	funcRefResolver func(funcIndex Index) (Reference, error),
) (lo, hi uint64, typ ValueType, ok bool, err error) {
	if len(data) < 3 {
		return 0, 0, 0, false, nil
	}
	switch data[0] {
	case OpcodeI32Const:
		v, n, derr := leb128.LoadInt32(data[1:])
		if derr != nil {
			return 0, 0, 0, false, nil
		}
		pc := 1 + n
		if pc >= uint64(len(data)) || data[pc] != OpcodeEnd || pc+1 != uint64(len(data)) {
			return 0, 0, 0, false, nil
		}
		return uint64(uint32(v)), 0, ValueTypeI32, true, nil
	case OpcodeI64Const:
		v, n, derr := leb128.LoadInt64(data[1:])
		if derr != nil {
			return 0, 0, 0, false, nil
		}
		pc := 1 + n
		if pc >= uint64(len(data)) || data[pc] != OpcodeEnd || pc+1 != uint64(len(data)) {
			return 0, 0, 0, false, nil
		}
		return uint64(v), 0, ValueTypeI64, true, nil
	case OpcodeF32Const:
		if len(data) != 6 || data[5] != OpcodeEnd { // 1 opcode + 4 bytes + end.
			return 0, 0, 0, false, nil
		}
		return uint64(binary.LittleEndian.Uint32(data[1:5])), 0, ValueTypeF32, true, nil
	case OpcodeF64Const:
		if len(data) != 10 || data[9] != OpcodeEnd { // 1 opcode + 8 bytes + end.
			return 0, 0, 0, false, nil
		}
		return binary.LittleEndian.Uint64(data[1:9]), 0, ValueTypeF64, true, nil
	case OpcodeGlobalGet:
		v, n, derr := leb128.LoadUint32(data[1:])
		if derr != nil {
			return 0, 0, 0, false, nil
		}
		pc := 1 + n
		if pc >= uint64(len(data)) || data[pc] != OpcodeEnd || pc+1 != uint64(len(data)) {
			return 0, 0, 0, false, nil
		}
		gTyp, gLo, gHi, gErr := globalResolver(Index(v))
		if gErr != nil {
			return 0, 0, 0, true, gErr
		}
		if gTyp != ValueTypeV128 {
			gHi = 0
		}
		return gLo, gHi, gTyp, true, nil
	case OpcodeRefNull:
		var valType ValueType
		switch data[1] {
		case RefTypeFuncref.Kind():
			valType = RefTypeFuncref
		case RefTypeExternref.Kind():
			valType = RefTypeExternref
		case ValueTypeExnref.Kind():
			valType = ValueTypeExnref
		default:
			// Concrete type index (typed refs): rare: let the general evaluator handle it.
			return 0, 0, 0, false, nil
		}
		if len(data) != 3 || data[2] != OpcodeEnd {
			return 0, 0, 0, false, nil
		}
		return 0, 0, valType, true, nil
	case OpcodeRefFunc:
		v, n, derr := leb128.LoadUint32(data[1:])
		if derr != nil {
			return 0, 0, 0, false, nil
		}
		pc := 1 + n
		if pc >= uint64(len(data)) || data[pc] != OpcodeEnd || pc+1 != uint64(len(data)) {
			return 0, 0, 0, false, nil
		}
		ref, rErr := funcRefResolver(Index(v))
		if rErr != nil {
			return 0, 0, 0, true, rErr
		}
		return uint64(ref), 0, ValueTypeFuncref, true, nil
	case OpcodeVecPrefix:
		if len(data) != 19 || data[1] != OpcodeVecV128Const || data[18] != OpcodeEnd { // 1 + 1 + 16 + 1.
			return 0, 0, 0, false, nil
		}
		return binary.LittleEndian.Uint64(data[2:10]), binary.LittleEndian.Uint64(data[10:18]), ValueTypeV128, true, nil
	default:
		return 0, 0, 0, false, nil
	}
}

func evaluateConstExpr(e *ConstantExpression, globalResolver func(globalIndex Index) (ValueType, uint64, uint64, error), funcRefResolver func(funcIndex Index) (Reference, error)) ([]uint64, ValueType, error) {
	if lo, hi, typ, ok, err := evaluateConstExprFast(e.Data, globalResolver, funcRefResolver); ok {
		if err != nil {
			return nil, 0, err
		}
		if typ == ValueTypeV128 {
			return []uint64{lo, hi}, typ, nil
		}
		return []uint64{lo}, typ, nil
	}

	var stack []uint64
	var typeStack []ValueType
	var pc uint64
	data := e.Data
	for {
		if pc >= uint64(len(data)) {
			return nil, 0, io.ErrUnexpectedEOF
		}
		opCode := data[pc]
		pc++
		switch opCode {
		case OpcodeI32Const:
			v, n, err := leb128.LoadInt32(data[pc:])
			if err != nil {
				return nil, 0, fmt.Errorf("read i32: %w", err)
			}
			pc += n
			stack = append(stack, uint64(uint32(v)))
			typeStack = append(typeStack, ValueTypeI32)
		case OpcodeI64Const:
			v, n, err := leb128.LoadInt64(data[pc:])
			if err != nil {
				return nil, 0, fmt.Errorf("read i64: %w", err)
			}
			pc += n
			stack = append(stack, uint64(v))
			typeStack = append(typeStack, ValueTypeI64)
		case OpcodeF32Const:
			if len(data[pc:]) < 4 {
				return nil, 0, io.ErrUnexpectedEOF
			}
			v := binary.LittleEndian.Uint32(data[pc:])
			pc += 4
			stack = append(stack, uint64(v))
			typeStack = append(typeStack, ValueTypeF32)
		case OpcodeF64Const:
			if len(data[pc:]) < 8 {
				return nil, 0, io.ErrUnexpectedEOF
			}
			v := binary.LittleEndian.Uint64(data[pc:])
			pc += 8
			stack = append(stack, uint64(v))
			typeStack = append(typeStack, ValueTypeF64)
		case OpcodeGlobalGet:
			v, n, err := leb128.LoadUint32(data[pc:])
			if err != nil {
				return nil, 0, fmt.Errorf("read index of global: %w", err)
			}
			pc += n
			typ, lo, hi, err := globalResolver(Index(v))
			if err != nil {
				return nil, 0, err
			}
			switch typ {
			case ValueTypeV128:
				stack = append(stack, lo, hi)
			default:
				stack = append(stack, lo)
			}
			typeStack = append(typeStack, typ)
		case OpcodeRefNull:
			// Reference types are opaque 64bit pointer at runtime.
			if pc >= uint64(len(data)) {
				return nil, 0, fmt.Errorf("read reference type for ref.null: %w", io.ErrShortBuffer)
			}
			b := data[pc]
			var valType ValueType
			switch b {
			case RefTypeFuncref.Kind():
				valType = RefTypeFuncref
				pc++
			case RefTypeExternref.Kind():
				valType = RefTypeExternref
				pc++
			case ValueTypeExnref.Kind():
				valType = ValueTypeExnref
				pc++
			default:
				// Concrete type index encoded as LEB128.
				typeIdx, n, err := leb128.LoadUint32(data[pc:])
				if err != nil {
					return nil, 0, fmt.Errorf("invalid type for ref.null: 0x%x", b)
				}
				pc += n
				valType = ValueTypeConcreteRef(typeIdx, true)
			}
			stack = append(stack, 0)
			typeStack = append(typeStack, valType)
		case OpcodeRefFunc:
			v, n, err := leb128.LoadUint32(data[pc:])
			if err != nil {
				return nil, 0, fmt.Errorf("read i32: %w", err)
			}
			pc += n
			ref, err := funcRefResolver(Index(v))
			if err != nil {
				return nil, 0, err
			}
			stack = append(stack, uint64(ref))
			typeStack = append(typeStack, ValueTypeFuncref)
		case OpcodeVecPrefix:
			if data[pc] != OpcodeVecV128Const {
				return nil, 0, fmt.Errorf("invalid vector opcode for const expression: %#x", data[pc-1])
			}
			pc++
			if len(data[pc:]) < 16 {
				return nil, 0, fmt.Errorf("%s needs 16 bytes but was %d bytes", OpcodeVecV128ConstName, len(data[pc:]))
			}
			lo := binary.LittleEndian.Uint64(data[pc:])
			pc += 8
			hi := binary.LittleEndian.Uint64(data[pc:])
			pc += 8
			stack = append(stack, lo, hi)
			typeStack = append(typeStack, ValueTypeV128)
		case OpcodeI32Add:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i32.add")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI32 || v2 != ValueTypeI32 {
				return nil, 0, fmt.Errorf("type mismatch on i32.add: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, uint64(uint32(a)+uint32(b)))
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI32)
		case OpcodeI32Sub:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i32.sub")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI32 || v2 != ValueTypeI32 {
				return nil, 0, fmt.Errorf("type mismatch on i32.sub: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, uint64(uint32(a)-uint32(b)))
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI32)
		case OpcodeI32Mul:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i32.mul")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI32 || v2 != ValueTypeI32 {
				return nil, 0, fmt.Errorf("type mismatch on i32.mul: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, uint64(uint32(a)*uint32(b)))
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI32)
		case OpcodeI64Add:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i64.add")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI64 || v2 != ValueTypeI64 {
				return nil, 0, fmt.Errorf("type mismatch on i64.add: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a+b)
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI64)
		case OpcodeI64Sub:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i64.sub")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI64 || v2 != ValueTypeI64 {
				return nil, 0, fmt.Errorf("type mismatch on i64.sub: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a-b)
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI64)
		case OpcodeI64Mul:
			if len(typeStack) < 2 {
				return nil, 0, errors.New("stack underflow on i64.mul")
			}
			v1 := typeStack[len(typeStack)-1]
			v2 := typeStack[len(typeStack)-2]
			if v1 != ValueTypeI64 || v2 != ValueTypeI64 {
				return nil, 0, fmt.Errorf("type mismatch on i64.mul: %s, %s", ValueTypeName(v2), ValueTypeName(v1))
			}
			b, a := stack[len(stack)-1], stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a*b)
			typeStack = typeStack[:len(typeStack)-2]
			typeStack = append(typeStack, ValueTypeI64)
		case OpcodeEnd:
			if len(typeStack) != 1 {
				return nil, 0, errors.New("stack has more than one value at end of constant expression")
			}
			return stack, typeStack[0], nil
		default:
			return nil, 0, fmt.Errorf("invalid opcode for const expression: 0x%x", opCode)
		}
	}
}

func evaluateConstExprInModuleInstance(e *ConstantExpression, m *ModuleInstance) []uint64 {
	v, _, _ := evaluateConstExpr(
		e,
		func(globalIndex Index) (ValueType, uint64, uint64, error) {
			g := m.Globals[globalIndex]
			return g.Type.ValType, g.Val, g.ValHi, nil
		},
		func(funcIndex Index) (Reference, error) {
			return m.Engine.FunctionInstanceReference(funcIndex), nil
		},
	)
	return v
}

// evaluateElementInit evaluates ElementSegment.Init[i] (see that field's doc for the encoding),
// resolving global/function references via the given callbacks exactly as evaluateConstExpr would for
// the equivalent full constant expression -- but without needing to allocate or re-parse one for the
// overwhelmingly common plain-function-index, ref.null, and global.get cases.
func evaluateElementInit(
	e *ElementSegment, i int,
	globalResolver func(globalIndex Index) (ValueType, uint64, uint64, error),
	funcRefResolver func(funcIndex Index) (Reference, error),
) (Reference, ValueType, error) {
	v := e.Init[i]
	switch {
	case v == ElementInitNullReference:
		return 0, e.Type, nil
	case v&elementInitImportedGlobalReference != 0:
		typ, lo, _, err := globalResolver(v &^ elementInitImportedGlobalReference)
		if err != nil {
			return 0, 0, err
		}
		return Reference(lo), typ, nil
	case v&elementInitExprReference != 0:
		vals, typ, err := evaluateConstExpr(&e.Exprs[v&^elementInitExprReference], globalResolver, funcRefResolver)
		if err != nil {
			return 0, 0, err
		}
		return Reference(vals[0]), typ, nil
	default:
		ref, err := funcRefResolver(v)
		if err != nil {
			return 0, 0, err
		}
		return ref, ValueTypeFuncref, nil
	}
}

// evaluateElementInitInModuleInstance evaluates ElementSegment.Init[i] against a live ModuleInstance,
// mirroring evaluateConstExprInModuleInstance's resolver wiring and error-ignoring semantics (errors are
// not expected here: Init entries are validated up front by validateTable before instantiation ever
// reaches this point).
func evaluateElementInitInModuleInstance(e *ElementSegment, i int, m *ModuleInstance) Reference {
	ref, _, _ := evaluateElementInit(
		e, i,
		func(globalIndex Index) (ValueType, uint64, uint64, error) {
			g := m.Globals[globalIndex]
			return g.Type.ValType, g.Val, g.ValHi, nil
		},
		func(funcIndex Index) (Reference, error) {
			return m.Engine.FunctionInstanceReference(funcIndex), nil
		},
	)
	return ref
}

func NewConstantExpressionFromOpcode(
	opcode byte, opData []byte,
) ConstantExpression {
	data := make([]byte, 0, 3+len(opData)) // 2 for opcode and optional vec prefix, 1 for end
	if opcode == OpcodeVecV128Const {
		data = append(data, OpcodeVecPrefix)
	}
	data = append(data, opcode)
	data = append(data, opData...)
	data = append(data, OpcodeEnd)
	return ConstantExpression{Data: data}
}

func NewConstantExpressionFromI32(val int32) ConstantExpression {
	return NewConstantExpressionFromOpcode(OpcodeI32Const, leb128.EncodeInt32(val))
}

func NewConstantExpressionFromI64(val int64) ConstantExpression {
	return NewConstantExpressionFromOpcode(OpcodeI64Const, leb128.EncodeInt64(val))
}
