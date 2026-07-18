package instance

import (
	"context"
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/abi"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file implements canon error-context.new/debug-message/drop (CanonKind
// 0x1c/0x1d/0x1e), transliterating testdata/definitions.py's
// canon_error_context_* (~2770). See
// docs/component-model-async-runtime-design.md §2.3.

// errorContext is the reference ErrorContext: a table entry holding a debug
// message. Lift is a GET (the sender KEEPS its handle, unlike a stream's
// readable end); lower is an addEntry sharing the same *errorContext -- copy
// semantics, two live handles, each dropped independently.
type errorContext struct {
	debugMessage string
}

func (*errorContext) entryKind() entryKind { return entryErrorContext }

// errorContextNewHostFunc backs error-context.new (CanonKindErrorContextNew,
// 0x1c). Core sig (ptr:i32, len:i32) -> i32: load the message string from the
// canon's memory opt (bounds + UTF-8 traps per load_string_from_range), then
// addEntry(&errorContext{debugMessage: s}).
//
// Deviation, pinned by the design doc: the reference's DETERMINISTIC_PROFILE
// stores ” and skips the load entirely (no traps); wazy always loads (traps
// live) and keeps the real message -- the spec's sanctioned "host-defined
// transformation" (= identity) on the non-deterministic branch, and far
// better diagnostics.
func errorContextNewHostFunc(in *Instance, memMod api.Module) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, "error-context.new")
		m := memMod
		if m == nil {
			m = mod
		}
		ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
		mem, ok := memoryBytesOf(m)
		if !ok {
			panic(fmt.Errorf("component/instance: error-context.new: calling module has no memory"))
		}
		// A top-level `message: string` param flattens as two raw core
		// values (ptr, len) -- NOT the [ptr:4][len:4]-pair-in-memory layout
		// abi.Load expects for a NESTED string field -- so lift it via
		// LiftFlat directly from the two stack args, exactly as
		// task.return's own flat-result lift does (async_builtins.go).
		coreVals := []abi.CoreValue{{Kind: "i32", Bits: uint64(ptr)}, {Kind: "i32", Bits: uint64(length)}}
		s, err := abi.LiftFlat(coreVals, binary.PrimitiveDesc{Prim: "string"}, nil, mem)
		if err != nil {
			panic(fmt.Errorf("component/instance: error-context.new: load message: %w", err))
		}
		msg, _ := s.(string)
		stack[0] = uint64(in.resources.addEntry(&errorContext{debugMessage: msg}))
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: []api.ValueType{api.ValueTypeI32}}
}

// errorContextDebugMessageHostFunc backs error-context.debug-message
// (CanonKindErrorContextDebugMessage, 0x1d). Core sig (i:i32, ptr:i32) -> ():
// kind-trap lookup; store_string(cx, msg, ptr) via the canon's memory/realloc.
func errorContextDebugMessageHostFunc(in *Instance, memMod api.Module, reallocFn api.Function) hostFuncDef {
	fn := api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		requireMayLeave(in, "error-context.debug-message")
		m := memMod
		if m == nil {
			m = mod
		}
		h, ptr := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
		raw, ok := in.resources.getEntry(h)
		ec, isEC := raw.(*errorContext)
		if !ok || !isEC {
			panic(fmt.Errorf("component/instance: error-context.debug-message: handle %d is not an error-context", h))
		}
		mem, ok := memoryBytesOf(m)
		if !ok {
			panic(fmt.Errorf("component/instance: error-context.debug-message: calling module has no memory"))
		}
		realloc := reallocOf(ctx, m)
		if reallocFn != nil {
			realloc = reallocOfFunc(ctx, reallocFn)
		}
		if err := abi.Store(mem, ptr, binary.PrimitiveDesc{Prim: "string"}, ec.debugMessage, nil, realloc); err != nil {
			panic(fmt.Errorf("component/instance: error-context.debug-message: store: %w", err))
		}
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, results: nil}
}

// errorContextDropHostFunc backs error-context.drop (CanonKindErrorContextDrop,
// 0x1e). Core sig (i32) -> (): kind-trap + removeEntry.
func errorContextDropHostFunc(in *Instance) hostFuncDef {
	fn := api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
		requireMayLeave(in, "error-context.drop")
		h := api.DecodeU32(stack[0])
		raw, ok := in.resources.getEntry(h)
		_, isEC := raw.(*errorContext)
		if !ok || !isEC {
			panic(fmt.Errorf("component/instance: error-context.drop: handle %d is not an error-context", h))
		}
		in.resources.removeEntry(h)
	})
	return hostFuncDef{fn: fn, params: []api.ValueType{api.ValueTypeI32}, results: nil}
}
