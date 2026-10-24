package api

import (
	"context"

	"github.com/samyfodil/wazy/internal/expctxkeys"
)

// WithInterpreterLazyLowering makes the interpreter lower each function body on
// its first call instead of lowering the whole module in CompileModule. Set it
// on the context passed to CompileModule. The module is still fully validated
// at compile time, so an invalid function fails CompileModule as before.
//
// This saves compile time and memory when only part of a module runs, at the
// cost of a small delay on each function's first call. It has no effect on the
// compiler engine. A compiled module is cached by the engine, so the context
// that compiles a module first decides whether it is lowered lazily.
func WithInterpreterLazyLowering(ctx context.Context) context.Context {
	return context.WithValue(ctx, expctxkeys.InterpreterLazyLowering{}, true)
}

// GetInterpreterLazyLowering reports whether ctx enables lazy lowering.
func GetInterpreterLazyLowering(ctx context.Context) bool {
	v, _ := ctx.Value(expctxkeys.InterpreterLazyLowering{}).(bool)
	return v
}
