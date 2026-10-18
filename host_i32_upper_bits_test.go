package wazy

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestHostI32Result_negative is the reproducer from
// https://github.com/tetratelabs/wazero/issues/2371: a host function echoes
// i32 -1 back, and the guest compares it with i32.const -1. In wazero's
// interpreter the host result kept its sign-extended upper 32 bits, so
// i32.ne compared 0xFFFFFFFF_FFFFFFFF with 0x00000000_FFFFFFFF and said they
// differed. Both host function forms are covered: the typed HostFunc1, and a
// raw api.GoFunction writing the sign-extended word itself, and the result
// is fed to every kind of i32 instruction that would see stray upper bits.
//
//	(module
//	  (import "env" "echo" (func $echo (param i32) (result i32)))
//	  (func (export "run") (result i32)
//	    i32.const -1 call $echo i32.const -1 i32.ne)
//	  (func (export "extend") (result i64)
//	    i32.const -1 call $echo i64.extend_i32_u)
//	  (func (export "shr") (result i32)
//	    i32.const -1 call $echo i32.const 16 i32.shr_u)
//	  (func (export "div") (result i32)
//	    i32.const -2 call $echo i32.const 2 i32.div_u)
//	  (func (export "rotr") (result i32)
//	    i32.const -2 call $echo i32.const 1 i32.rotr)
//	  (func (export "clz") (result i32)
//	    i32.const 1 call $echo i32.const 31 i32.shl call $echo i32.clz)
//	  (func (export "lt") (result i32)
//	    i32.const -1 call $echo i32.const 1 i32.gt_u)
//	  (func (export "local") (result i64) (local i32)
//	    i32.const -1 call $echo local.set 0 local.get 0 i64.extend_i32_u))
func TestHostI32Result_negative(t *testing.T) {
	bin, err := base64.StdEncoding.DecodeString("AGFzbQEAAAABDgNgAX8Bf2AAAX9gAAF+AgwBA2VudgRlY2hvAAADCQgBAgEBAQEBAgc2CANydW4AAQZleHRlbmQAAgNzaHIAAwNkaXYABARyb3RyAAUDY2x6AAYCbHQABwVsb2NhbAAIClYICQBBfxAAQX9HCwcAQX8QAK0LCQBBfxAAQRB2CwkAQX4QAEECbgsJAEF+EABBAXgLDABBARAAQR90EABnCwkAQX8QAEEBSwsNAQF/QX8QACEAIACtCw==")
	require.NoError(t, err)

	hosts := map[string]func(HostFunctionBuilder) HostFunctionBuilder{
		"typed": func(b HostFunctionBuilder) HostFunctionBuilder {
			return HostFunc1(b, func(_ context.Context, _ api.Module, v int32) int32 { return v })
		},
		"sign-extended": func(b HostFunctionBuilder) HostFunctionBuilder {
			return b.WithGoFunction(api.GoFunc(func(_ context.Context, stack []uint64) {
				stack[0] = uint64(int64(int32(uint32(stack[0]))))
			}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32})
		},
	}
	for _, engine := range []struct {
		name   string
		config func() RuntimeConfig
	}{
		{"compiler", NewRuntimeConfigCompiler},
		{"interpreter", NewRuntimeConfigInterpreter},
	} {
		for hostName, host := range hosts {
			t.Run(engine.name+"/"+hostName, func(t *testing.T) {
				if engine.name == "compiler" && !platform.CompilerSupported() {
					t.Skip("compiler not supported here, e.g. executable mmap is unavailable")
				}
				r := NewRuntimeWithConfig(testCtx, engine.config())
				defer r.Close(testCtx)
				_, err := host(r.NewHostModuleBuilder("env").NewFunctionBuilder()).Export("echo").Instantiate(testCtx)
				require.NoError(t, err)
				mod, err := r.Instantiate(testCtx, bin)
				require.NoError(t, err)
				for _, want := range []struct {
					fn  string
					i64 bool
					v   uint64
				}{
					{"run", false, 0},
					{"extend", true, 0xffff_ffff},
					{"shr", false, 0xffff},
					{"div", false, 0x7fff_ffff},
					{"rotr", false, 0x7fff_ffff},
					{"clz", false, 0},
					{"lt", false, 1},
					{"local", true, 0xffff_ffff},
				} {
					res, err := mod.ExportedFunction(want.fn).Call(testCtx)
					require.NoError(t, err)
					got := res[0]
					if !want.i64 {
						got = uint64(api.DecodeU32(got))
					}
					require.Equal(t, want.v, got, want.fn)
				}
			})
		}
	}
}
