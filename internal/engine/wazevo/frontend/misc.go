package frontend

import (
	"github.com/samyfodil/wazy/internal/engine/wazevo/ssa"
	"github.com/samyfodil/wazy/internal/wasm"
)

func FunctionIndexToFuncRef(idx wasm.Index) ssa.FuncRef {
	return ssa.FuncRef(idx)
}
