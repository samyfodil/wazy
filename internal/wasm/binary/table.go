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
		vt, offset, err = decodeRefType(buf, offset, b == wasm.RefPrefixNullable)
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

	var shared bool
	ret.Min, ret.Max, shared, offset, err = decodeLimitsType(buf, offset)
	if err != nil {
		return offset, fmt.Errorf("read limits: %v", err)
	}
	if ret.Min > wasm.MaximumFunctionIndex {
		return offset, fmt.Errorf("table min must be at most %d", wasm.MaximumFunctionIndex)
	}
	if ret.Max != nil {
		if *ret.Max < ret.Min {
			return offset, fmt.Errorf("table size minimum must not be greater than maximum")
		}
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
