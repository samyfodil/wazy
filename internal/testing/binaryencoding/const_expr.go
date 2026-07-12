package binaryencoding

import (
	"github.com/samyfodil/wazy/internal/wasm"
)

func encodeConstantExpression(expr wasm.ConstantExpression) (ret []byte) {
	ret = append(ret, expr.Data...)
	return
}
