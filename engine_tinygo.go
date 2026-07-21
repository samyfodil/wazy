//go:build tinygo

package wazy

import "github.com/samyfodil/wazy/internal/engine/interpreter"

// TinyGo cannot use the native JIT engine; fall back to the interpreter.
var nativeEngine = interpreter.NewEngine
