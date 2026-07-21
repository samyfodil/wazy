//go:build !tinygo

package wazy

import "github.com/samyfodil/wazy/internal/engine/native"

var nativeEngine = native.NewEngine
