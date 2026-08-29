package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasm"
)

// decodeTable returns the offset after the wasm.Table decoded from buf[offset:] with the WebAssembly 1.0
// (20191205) Binary Format.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-table
func decodeTable(buf []byte, offset int, enabledFeatures api.CoreFeatures, ret *wasm.Table) (int, error) {
	b, offset, err := readByte(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read leading byte: %v", err)
	}

	hasInitExpr := false
	if b == 0x40 {
		// Table with initializer expression: 0x40 0x00 tabletype expr
		var reserved byte
		reserved, offset, err = readByte(buf, offset)
		if err != nil {
			return offset, fmt.Errorf("read reserved byte after 0x40: %v", err)
		}
		if reserved != 0x00 {
			return offset, fmt.Errorf("expected 0x00 after 0x40 table prefix, got 0x%02x", reserved)
		}
		hasInitExpr = true
		b, offset, err = readByte(buf, offset)
		if err != nil {
			return offset, fmt.Errorf("read table ref type: %v", err)
		}
	}

	switch b {
	case wasm.RefPrefixNullable, wasm.RefPrefixNonNullable:
		var vt wasm.ValueType
		vt, offset, err = decodeRefType(enabledFeatures, buf, offset, b == wasm.RefPrefixNullable)
		if err != nil {
			return offset, err
		}
		ret.Type = vt
	default:
		ret.Type = wasm.ValueType(b)
	}

	if ret.Type != wasm.RefTypeFuncref {
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureReferenceTypes); err != nil {
			return offset, fmt.Errorf("table type funcref is invalid: %w", err)
		}
	}

	var shared, index64 bool
	var min uint64
	var maxP *uint64
	min, maxP, shared, index64, offset, err = decodeLimitsType(buf, offset, enabledFeatures)
	if err != nil {
		return offset, fmt.Errorf("read limits: %v", err)
	}
	ret.IsTable64 = index64
	// The declared minimum is bounded only by the index type's own range: the
	// specification requires a module declaring more entries than any host could
	// allocate to still be *valid* (test/core/table.wast defines a table of
	// 2^32-1 entries without instantiating it). wazy's own MaximumFunctionIndex
	// ceiling is applied at instantiation instead, in buildTables, which is also
	// where the allocation would happen.
	ret.Min = min
	if maxP != nil {
		if *maxP < min {
			return offset, fmt.Errorf("table size minimum must not be greater than maximum")
		}
		ret.Max = maxP
	} else {
		ret.Max = nil
	}
	if shared {
		return offset, fmt.Errorf("tables cannot be marked as shared")
	}

	if hasInitExpr {
		var initExpr wasm.ConstantExpression
		offset, err = decodeConstantExpression(buf, offset, enabledFeatures, &initExpr)
		if err != nil {
			return offset, fmt.Errorf("read table init expr: %v", err)
		}
		ret.InitExpr = &initExpr
	}
	return offset, nil
}
