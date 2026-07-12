package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func ensureElementKindFuncRef(buf []byte, offset int) (int, error) {
	elemKind, offset, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read element prefix: %w", err)
	}
	if elemKind != 0x0 { // ElemKind is fixed to 0x0 now: https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#element-section
		return offset, fmt.Errorf("element kind must be zero but was 0x%x", elemKind)
	}
	return offset, nil
}

// decodeElementInitValueVector decodes a vec(funcidx) as used by element section prefixes 0-3: every
// entry is a bare function index (never ref.null/global.get), so this stores the index directly into
// ElementSegment.Init without allocating a ConstantExpression (nor the LEB128 round-trip that would
// otherwise be needed to re-encode it into one) per entry -- the hot path for large all-funcref tables.
func decodeElementInitValueVector(buf []byte, offset int) ([]wasm.Index, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, offset, fmt.Errorf("get size of vector: %w", err)
	}
	offset += int(n)

	vec := make([]wasm.Index, vs)
	for i := range vec {
		u32, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("read function index: %w", err)
		}
		offset += int(n)

		if u32 >= wasm.MaximumFunctionIndex {
			return nil, offset, fmt.Errorf("too large function index in Element init: %d", u32)
		}
		vec[i] = wasm.Index(u32)
	}
	return vec, offset, nil
}

// decodeElementConstExprVector decodes a vec(expr) as used by element section prefixes 4-7: each entry
// is a full constant expression (ref.func, ref.null, global.get, or -- with extended-const/GC features --
// something else entirely). Each is compacted via wasm.CompactElementInit into the Init/Exprs
// representation described on ElementSegment.Init; only entries that don't fit a compact form (the rare
// path) end up allocating/retaining a ConstantExpression.
func decodeElementConstExprVector(buf []byte, offset int, elemType wasm.RefType, enabledFeatures api.CoreFeatures) ([]wasm.Index, []wasm.ConstantExpression, int, error) {
	vs, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return nil, nil, offset, fmt.Errorf("failed to get the size of constexpr vector: %w", err)
	}
	offset += int(n)

	init := make([]wasm.Index, vs)
	var exprs []wasm.ConstantExpression
	for i := range init {
		var expr wasm.ConstantExpression
		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &expr)
		if err != nil {
			return nil, nil, offset, err
		}
		// Expression will be validated later since we don't yet have globals to resolve the types yet.
		init[i], exprs = wasm.CompactElementInit(expr, elemType, exprs)
	}
	return init, exprs, offset, nil
}

func decodeElementRefType(buf []byte, offset int) (wasm.RefType, int, error) {
	b, offset, err := readByte(buf, offset)
	if err != nil {
		return 0, offset, fmt.Errorf("read element ref type: %w", err)
	}
	switch b {
	case wasm.RefPrefixNullable, wasm.RefPrefixNonNullable:
		return decodeRefType(buf, offset, b == wasm.RefPrefixNullable)
	default:
		ret := wasm.ValueType(b)
		if ret != wasm.RefTypeFuncref && ret != wasm.RefTypeExternref {
			return 0, offset, fmt.Errorf("invalid ref type for element: 0x%x", b)
		}
		return ret, offset, nil
	}
}

const (
	// The prefix is explained at https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#element-section

	// elementSegmentPrefixLegacy is the legacy prefix and is only valid one before CoreFeatureBulkMemoryOperations.
	elementSegmentPrefixLegacy = iota
	// elementSegmentPrefixPassiveFuncrefValueVector is the passive element whose indexes are encoded as vec(varint), and reftype is fixed to funcref.
	elementSegmentPrefixPassiveFuncrefValueVector
	// elementSegmentPrefixActiveFuncrefValueVectorWithTableIndex is the same as elementSegmentPrefixPassiveFuncrefValueVector but active and table index is encoded.
	elementSegmentPrefixActiveFuncrefValueVectorWithTableIndex
	// elementSegmentPrefixDeclarativeFuncrefValueVector is the same as elementSegmentPrefixPassiveFuncrefValueVector but declarative.
	elementSegmentPrefixDeclarativeFuncrefValueVector
	// elementSegmentPrefixActiveFuncrefConstExprVector is active whoce reftype is fixed to funcref and indexes are encoded as vec(const_expr).
	elementSegmentPrefixActiveFuncrefConstExprVector
	// elementSegmentPrefixPassiveConstExprVector is passive whoce indexes are encoded as vec(const_expr), and reftype is encoded.
	elementSegmentPrefixPassiveConstExprVector
	// elementSegmentPrefixPassiveConstExprVector is active whoce indexes are encoded as vec(const_expr), and reftype and table index are encoded.
	elementSegmentPrefixActiveConstExprVector
	// elementSegmentPrefixDeclarativeConstExprVector is declarative whoce indexes are encoded as vec(const_expr), and reftype is encoded.
	elementSegmentPrefixDeclarativeConstExprVector
)

func decodeElementSegment(buf []byte, offset int, enabledFeatures api.CoreFeatures, ret *wasm.ElementSegment) (int, error) {
	prefix, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, fmt.Errorf("read element prefix: %w", err)
	}
	offset += int(n)

	if prefix != elementSegmentPrefixLegacy {
		if err := enabledFeatures.RequireEnabled(api.CoreFeatureBulkMemoryOperations); err != nil {
			return offset, fmt.Errorf("non-zero prefix for element segment is invalid as %w", err)
		}
	}

	// Encoding depends on the prefix and described at https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/binary/modules.html#element-section
	switch prefix {
	case elementSegmentPrefixLegacy:
		// Legacy prefix which is WebAssembly 1.0 compatible.
		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &ret.OffsetExpr)
		if err != nil {
			return offset, fmt.Errorf("read expr for offset: %w", err)
		}

		ret.Init, offset, err = decodeElementInitValueVector(buf, offset)
		if err != nil {
			return offset, err
		}

		ret.Mode = wasm.ElementModeActive
		ret.Type = wasm.RefTypeFuncref.AsNonNullable()
		return offset, nil
	case elementSegmentPrefixPassiveFuncrefValueVector:
		// Prefix 1 requires funcref.
		if offset, err = ensureElementKindFuncRef(buf, offset); err != nil {
			return offset, err
		}

		ret.Init, offset, err = decodeElementInitValueVector(buf, offset)
		if err != nil {
			return offset, err
		}
		ret.Mode = wasm.ElementModePassive
		ret.Type = wasm.RefTypeFuncref.AsNonNullable()
		return offset, nil
	case elementSegmentPrefixActiveFuncrefValueVectorWithTableIndex:
		var tn uint64
		ret.TableIndex, tn, err = leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("get size of vector: %w", err)
		}
		offset += int(tn)

		if ret.TableIndex != 0 {
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureReferenceTypes); err != nil {
				return offset, fmt.Errorf("table index must be zero but was %d: %w", ret.TableIndex, err)
			}
		}

		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &ret.OffsetExpr)
		if err != nil {
			return offset, fmt.Errorf("read expr for offset: %w", err)
		}

		// Prefix 2 requires funcref.
		if offset, err = ensureElementKindFuncRef(buf, offset); err != nil {
			return offset, err
		}

		ret.Init, offset, err = decodeElementInitValueVector(buf, offset)
		if err != nil {
			return offset, err
		}

		ret.Mode = wasm.ElementModeActive
		ret.Type = wasm.RefTypeFuncref.AsNonNullable()
		return offset, nil
	case elementSegmentPrefixDeclarativeFuncrefValueVector:
		// Prefix 3 requires funcref.
		if offset, err = ensureElementKindFuncRef(buf, offset); err != nil {
			return offset, err
		}
		ret.Init, offset, err = decodeElementInitValueVector(buf, offset)
		if err != nil {
			return offset, err
		}
		ret.Type = wasm.RefTypeFuncref.AsNonNullable()
		ret.Mode = wasm.ElementModeDeclarative
		return offset, nil
	case elementSegmentPrefixActiveFuncrefConstExprVector:
		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &ret.OffsetExpr)
		if err != nil {
			return offset, fmt.Errorf("read expr for offset: %w", err)
		}

		ret.Init, ret.Exprs, offset, err = decodeElementConstExprVector(buf, offset, wasm.RefTypeFuncref, enabledFeatures)
		if err != nil {
			return offset, err
		}
		ret.Mode = wasm.ElementModeActive
		ret.Type = wasm.RefTypeFuncref
		return offset, nil
	case elementSegmentPrefixPassiveConstExprVector:
		ret.Type, offset, err = decodeElementRefType(buf, offset)
		if err != nil {
			return offset, err
		}
		ret.Init, ret.Exprs, offset, err = decodeElementConstExprVector(buf, offset, ret.Type, enabledFeatures)
		if err != nil {
			return offset, err
		}
		ret.Mode = wasm.ElementModePassive
		return offset, nil
	case elementSegmentPrefixActiveConstExprVector:
		var tn uint64
		ret.TableIndex, tn, err = leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("get size of vector: %w", err)
		}
		offset += int(tn)

		if ret.TableIndex != 0 {
			if err := enabledFeatures.RequireEnabled(api.CoreFeatureReferenceTypes); err != nil {
				return offset, fmt.Errorf("table index must be zero but was %d: %w", ret.TableIndex, err)
			}
		}
		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &ret.OffsetExpr)
		if err != nil {
			return offset, fmt.Errorf("read expr for offset: %w", err)
		}

		ret.Type, offset, err = decodeElementRefType(buf, offset)
		if err != nil {
			return offset, err
		}

		ret.Init, ret.Exprs, offset, err = decodeElementConstExprVector(buf, offset, ret.Type, enabledFeatures)
		if err != nil {
			return offset, err
		}

		ret.Mode = wasm.ElementModeActive
		return offset, nil
	case elementSegmentPrefixDeclarativeConstExprVector:
		ret.Type, offset, err = decodeElementRefType(buf, offset)
		if err != nil {
			return offset, err
		}
		ret.Init, ret.Exprs, offset, err = decodeElementConstExprVector(buf, offset, ret.Type, enabledFeatures)
		if err != nil {
			return offset, err
		}

		ret.Mode = wasm.ElementModeDeclarative
		return offset, nil
	default:
		return offset, fmt.Errorf("invalid element segment prefix: 0x%x", prefix)
	}
}
