package frontend

import (
	"fmt"
	"math"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
	"github.com/samyfodil/wazy/internal/engine/native/testcases"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestCompiler_LowerToSSA(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		// The expected SSA embeds field offsets of the runtime structs the
		// generated code loads from, and those move on a platform with
		// twelve-byte slice headers. The native compiler only runs on amd64
		// and arm64, so 64-bit is the only layout it ever emits code for.
		t.Skip("the native compiler does not run on a 32-bit platform")
	}
	// Most of the logic should look similar to Cranelift's Wasm frontend, so when you want to see
	// what output should look like, you can run:
	// `~/wasmtime/target/debug/clif-util wasm --target aarch64-apple-darwin testcase.wat -p -t`
	for _, tc := range []struct {
		name         string
		needListener bool
		// m is the *wasm.Module to be compiled in this test.
		m *wasm.Module
		// targetIndex is the index of a local function to be compiled in this test.
		targetIndex wasm.Index
		// exp is the *unoptimized* expected SSA IR for the function m.FunctionSection[targetIndex].
		exp string
		// expAfterPasses is not empty when we want to check the result after SSA passes.
		expAfterPasses string
		features       api.CoreFeatures
		// inline lets small callees be lowered in place of their calls, which the other cases keep
		// as calls so that they pin how a call is lowered.
		inline bool
	}{
		{
			name: "empty", m: testcases.Empty.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	Jump blk_ret
`,
		},
		{
			name: "unreachable", m: testcases.Unreachable.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	Exit v0, unreachable
`,
		},
		{
			name: testcases.OnlyReturn.Name, m: testcases.OnlyReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	Return
`,
		},
		{
			name: "params", m: testcases.Params.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:f32, v4:f64)
	Return
`,
		},
		{
			name: "add/sub params return", m: testcases.AddSubParamsReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	v4:i32 = Iadd v2, v3
	v5:i32 = Isub v4, v2
	Jump blk_ret, v5
`,
		},
		{
			name: "add/sub params return / listener", m: testcases.AddSubParamsReturn.Module,
			needListener: true,
			exp: `
signatures:
	sig1: i64i32i32i32_v
	sig2: i64i32i32_v

blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Store v1, v0, 0x8
	v4:i64 = Load v1, 0x8
	v5:i64 = Load v4, 0x0
	v6:i32 = Iconst_32 0x0
	CallIndirect v5:sig1, v0, v6, v2, v3
	v7:i32 = Iadd v2, v3
	v8:i32 = Isub v7, v2
	Store v1, v0, 0x8
	v9:i64 = Load v1, 0x10
	v10:i64 = Load v9, 0x0
	v11:i32 = Iconst_32 0x0
	CallIndirect v10:sig2, v0, v11, v8
	Jump blk_ret, v8
`,
		},
		{
			name: "locals", m: testcases.Locals.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	v3:i64 = Iconst_64 0x0
	v4:f32 = F32const 0.000000
	v5:f64 = F64const 0.000000
	Jump blk_ret
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	Jump blk_ret
`,
		},
		{
			name: "selects", m: testcases.Selects.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64, v5:i64, v6:f32, v7:f32, v8:f64, v9:f64)
	v10:i32 = Icmp eq, v4, v5
	v11:i32 = Select v10, v2, v3
	v12:i64 = Select v3, v4, v5
	v13:i32 = Fcmp gt, v8, v9
	v14:f32 = Select v13, v6, v7
	v15:i32 = Fcmp neq, v6, v7
	v16:f64 = Select v15, v8, v9
	Jump blk_ret, v11, v12, v14, v16
`,
		},
		{
			name: "local param tee return", m: testcases.LocalParamTeeReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	Jump blk_ret, v2, v2
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump blk_ret, v2, v2
`,
		},
		{
			name: "locals + params", m: testcases.LocalsParams.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i64, v3:f32, v4:f64)
	v5:i32 = Iconst_32 0x0
	v6:i64 = Iconst_64 0x0
	v7:f32 = F32const 0.000000
	v8:f64 = F64const 0.000000
	v9:i64 = Iadd v2, v2
	v10:i64 = Isub v9, v2
	v11:f32 = Fadd v3, v3
	v12:f32 = Fsub v11, v3
	v13:f32 = Fmul v12, v3
	v14:f32 = Fdiv v13, v3
	v15:f32 = Fmax v14, v3
	v16:f32 = Fmin v15, v3
	v17:f64 = Fadd v4, v4
	v18:f64 = Fsub v17, v4
	v19:f64 = Fmul v18, v4
	v20:f64 = Fdiv v19, v4
	v21:f64 = Fmax v20, v4
	v22:f64 = Fmin v21, v4
	Jump blk_ret, v10, v16, v22
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i64, v3:f32, v4:f64)
	v9:i64 = Iadd v2, v2
	v10:i64 = Isub v9, v2
	v11:f32 = Fadd v3, v3
	v12:f32 = Fsub v11, v3
	v13:f32 = Fmul v12, v3
	v14:f32 = Fdiv v13, v3
	v15:f32 = Fmax v14, v3
	v16:f32 = Fmin v15, v3
	v17:f64 = Fadd v4, v4
	v18:f64 = Fsub v17, v4
	v19:f64 = Fmul v18, v4
	v20:f64 = Fdiv v19, v4
	v21:f64 = Fmax v20, v4
	v22:f64 = Fmin v21, v4
	Jump blk_ret, v10, v16, v22
`,
		},
		{
			name: "locals + params + return", m: testcases.LocalParamReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	Jump blk_ret, v2, v3
`,
		},
		{
			name: "swap param and return", m: testcases.SwapParamAndReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Jump blk_ret, v3, v2
`,
		},
		{
			name: "swap params and return", m: testcases.SwapParamsAndReturn.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Jump blk_ret, v3, v2

blk1: ()
`,
		},
		{
			name: "block - br", m: testcases.BlockBr.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	v3:i64 = Iconst_64 0x0
	v4:f32 = F32const 0.000000
	v5:f64 = F64const 0.000000
	Jump blk1

blk1: () <-- (blk0)
	Jump blk_ret
`,
		},
		{
			name: "block - br_if", m: testcases.BlockBrIf.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brnz v2, blk1
	Jump blk2

blk1: () <-- (blk0)
	Jump blk_ret

blk2: () <-- (blk0)
	Exit v0, unreachable
`,
		},
		{
			name: "loop - counted", m: testcases.LoopCounted.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Jump blk1, v2

blk1: (v4:i32) <-- (blk0,blk4,blk3)
	v3:i32 = PreemptRequested v4, 0xfff
	Brnz v3, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk1, v4

blk4: () <-- (blk1)
	v5:i32 = Iconst_32 0x1
	v6:i32 = Iadd v4, v5
	v7:i32 = Iconst_32 0x1
	Brnz v7, blk1, v6
	Jump blk5

blk5: () <-- (blk4)
	Jump blk_ret
`,
		},
		{
			name: "loop - bounded", m: testcases.LoopBounded.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	v9:i32 = PreemptRequested
	Brnz v9, blk6
	Jump blk1, v2

blk1: (v4:i32) <-- (blk0,blk4,blk6)
	v3:i32 = PreemptRequested elided
	Brnz v3, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	v5:i32 = Iconst_32 0x1
	v6:i32 = Iadd v4, v5
	v7:i32 = Iconst_32 0x64
	v8:i32 = Icmp lt_u, v6, v7
	Brnz v8, blk1, v6
	Jump blk5

blk5: () <-- (blk4)
	Jump blk_ret

blk6: () <-- (blk0)
	Yield v0
	Jump blk1, v2
`,
		},
		{
			name: "loop - br", m: testcases.LoopBr.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	Jump blk1

blk1: () <-- (blk0,blk4)
	v2:i32 = PreemptRequested
	Brnz v2, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	Jump blk1
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	Jump fallthrough

blk1: () <-- (blk0,blk4)
	v2:i32 = PreemptRequested
	Brnz v2, blk3
	Jump fallthrough

blk5: () <-- (blk1)
	Jump fallthrough

blk4: () <-- (blk5,blk3)
	Jump blk1

blk3: () <-- (blk1)
	Yield v0
	Jump blk4
`,
		},
		{
			name: "loop - br_if", m: testcases.LoopBrIf.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	Jump blk1

blk1: () <-- (blk0,blk4)
	v2:i32 = PreemptRequested
	Brnz v2, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	v3:i32 = Iconst_32 0x1
	Brnz v3, blk1
	Jump blk5

blk5: () <-- (blk4)
	Return
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	Jump fallthrough

blk1: () <-- (blk0,blk7)
	v2:i32 = PreemptRequested
	Brnz v2, blk3
	Jump fallthrough

blk6: () <-- (blk1)
	Jump fallthrough

blk4: () <-- (blk6,blk3)
	v3:i32 = Iconst_32 0x1
	Brz v3, blk5
	Jump fallthrough

blk7: () <-- (blk4)
	Jump blk1

blk5: () <-- (blk4)
	Return

blk3: () <-- (blk1)
	Yield v0
	Jump blk4
`,
		},
		{
			name: "block - block - br", m: testcases.BlockBlockBr.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	v3:i64 = Iconst_64 0x0
	v4:f32 = F32const 0.000000
	v5:f64 = F64const 0.000000
	Jump blk1

blk1: () <-- (blk0)
	Jump blk_ret

blk2: ()
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	Jump fallthrough

blk1: () <-- (blk0)
	Jump blk_ret
`,
		},
		{
			name: "if without else", m: testcases.IfWithoutElse.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Jump blk3

blk2: () <-- (blk0)
	Jump blk3

blk3: () <-- (blk1,blk2)
	Jump blk_ret
`,
		},
		{
			name: "if-else", m: testcases.IfElse.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Jump blk3

blk2: () <-- (blk0)
	Jump blk_ret

blk3: () <-- (blk1)
	Jump blk_ret
`,
		},
		{
			// The hint says the if is rarely true, so the then arm and the block only it reaches go last, and
			// the branch is inverted for the else arm to fall through.
			name: "if-else hinted unlikely", m: branchHinted(testcases.IfElse.Module, wasm.BranchHint{Offset: 2, Likely: false}),
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Jump blk3

blk2: () <-- (blk0)
	Jump blk_ret

blk3: () <-- (blk1)
	Jump blk_ret
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brnz v2, blk1
	Jump fallthrough

blk2: () <-- (blk0)
	Jump blk_ret

blk1: () <-- (blk0)
	Jump fallthrough

blk3: () <-- (blk1)
	Jump blk_ret
`,
		},
		{
			// The hint says the if is usually true, so the else arm goes last and the then arm falls through
			// to the block after the if, which it cannot when the else arm sits between them.
			name: "if-else hinted likely", m: branchHinted(testcases.IfElse.Module, wasm.BranchHint{Offset: 2, Likely: true}),
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Jump blk3

blk2: () <-- (blk0)
	Jump blk_ret

blk3: () <-- (blk1)
	Jump blk_ret
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump fallthrough

blk1: () <-- (blk0)
	Jump fallthrough

blk3: () <-- (blk1)
	Jump blk_ret

blk2: () <-- (blk0)
	Jump blk_ret
`,
		},
		{
			name: "single predecessor local refs", m: testcases.SinglePredecessorLocalRefs.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Return v2

blk2: () <-- (blk0)
	Jump blk3

blk3: () <-- (blk2)
	Jump blk_ret, v2
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump fallthrough

blk1: () <-- (blk0)
	Return v2

blk2: () <-- (blk0)
	Jump fallthrough

blk3: () <-- (blk2)
	Jump blk_ret, v2
`,
		},
		{
			name: "multi predecessors local ref",
			m:    testcases.MultiPredecessorLocalRef.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	v4:i32 = Iconst_32 0x0
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	Jump blk3, v2

blk2: () <-- (blk0)
	Jump blk3, v3

blk3: (v5:i32) <-- (blk1,blk2)
	Jump blk_ret, v5
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Brz v2, blk2
	Jump fallthrough

blk1: () <-- (blk0)
	Jump blk3, v2

blk2: () <-- (blk0)
	Jump fallthrough, v3

blk3: (v5:i32) <-- (blk1,blk2)
	Jump blk_ret, v5
`,
		},
		{
			name: "reference value from unsealed block",
			m:    testcases.ReferenceValueFromUnsealedBlock.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	Jump blk1, v2

blk1: (v5:i32) <-- (blk0)
	v4:i32 = PreemptRequested elided
	Brnz v4, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	Return v5
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump fallthrough

blk1: () <-- (blk0)
	Jump fallthrough

blk4: () <-- (blk1)
	Return v2
`,
		},
		{
			name: "reference value from unsealed block - #2",
			m:    testcases.ReferenceValueFromUnsealedBlock2.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump blk1, v2

blk1: (v4:i32) <-- (blk0,blk4)
	v3:i32 = PreemptRequested
	Brnz v3, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	Brnz v4, blk1, v4
	Jump blk6

blk5: ()

blk6: () <-- (blk4)
	v5:i32 = Iconst_32 0x0
	Jump blk_ret, v5
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump fallthrough

blk1: () <-- (blk0,blk8)
	v3:i32 = PreemptRequested
	Brnz v3, blk3
	Jump fallthrough

blk7: () <-- (blk1)
	Jump fallthrough

blk4: () <-- (blk7,blk3)
	Brz v2, blk6
	Jump fallthrough

blk8: () <-- (blk4)
	Jump blk1

blk6: () <-- (blk4)
	v5:i32 = Iconst_32 0x0
	Jump blk_ret, v5

blk3: () <-- (blk1)
	Yield v0
	Jump blk4
`,
		},
		{
			name: "reference value from unsealed block - #3",
			m:    testcases.ReferenceValueFromUnsealedBlock3.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump blk1, v2

blk1: (v4:i32) <-- (blk0,blk6)
	v3:i32 = PreemptRequested
	Brnz v3, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	Brnz v4, blk_ret
	Jump blk6

blk5: ()

blk6: () <-- (blk4)
	v5:i32 = Iconst_32 0x1
	Jump blk1, v5
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32)
	Jump fallthrough, v2

blk1: (v4:i32) <-- (blk0,blk6)
	v3:i32 = PreemptRequested
	Brnz v3, blk3
	Jump fallthrough

blk7: () <-- (blk1)
	Jump fallthrough

blk4: () <-- (blk7,blk3)
	Brnz v4, blk8
	Jump blk6

blk8: () <-- (blk4)
	Jump blk_ret

blk6: () <-- (blk4)
	v5:i32 = Iconst_32 0x1
	Jump blk1, v5

blk3: () <-- (blk1)
	Yield v0
	Jump blk4
`,
		},
		{
			name: "call",
			m:    testcases.Call.Module,
			exp: `
signatures:
	sig1: i64i64_i32
	sig2: i64i64i32i32_i32
	sig3: i64i64i32_i32i32

blk0: (v0:i64, v1:i64)
	v2:i32 = Call f1:sig1, v0, v1
	v3:i32 = Iconst_32 0x5
	v4:i32 = Call f2:sig2, v0, v1, v2, v3
	v5:i32, v6:i32 = Call f3:sig3, v0, v1, v4
	Jump blk_ret, v5, v6
`,
		},
		{
			name: "call_inlined", m: testcases.Call.Module, inline: true,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x28
	v3:i32 = Iconst_32 0x5
	v4:i32 = Iadd v2, v3
	Jump blk_ret, v4, v4
`,
		},
		{
			name: "call_many_params",
			m:    testcases.CallManyParams.Module,
			exp: `
signatures:
	sig1: i64i64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64_v

blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64)
	Call f1:sig1, v0, v1, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5
	Jump blk_ret
`,
		},
		{
			name: "call_many_returns",
			m:    testcases.CallManyReturns.Module,
			exp: `
signatures:
	sig0: i64i64i32i64f32f64_i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64i32i64f32f64

blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64)
	v6:i32, v7:i64, v8:f32, v9:f64, v10:i32, v11:i64, v12:f32, v13:f64, v14:i32, v15:i64, v16:f32, v17:f64, v18:i32, v19:i64, v20:f32, v21:f64, v22:i32, v23:i64, v24:f32, v25:f64, v26:i32, v27:i64, v28:f32, v29:f64, v30:i32, v31:i64, v32:f32, v33:f64, v34:i32, v35:i64, v36:f32, v37:f64, v38:i32, v39:i64, v40:f32, v41:f64, v42:i32, v43:i64, v44:f32, v45:f64 = Call f1:sig0, v0, v1, v2, v3, v4, v5
	Jump blk_ret, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17, v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30, v31, v32, v33, v34, v35, v36, v37, v38, v39, v40, v41, v42, v43, v44, v45
`,
		},
		{
			name: "integer comparisons", m: testcases.IntegerComparisons.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64, v5:i64)
	v6:i32 = Icmp eq, v2, v3
	v7:i32 = Icmp eq, v4, v5
	v8:i32 = Icmp neq, v2, v3
	v9:i32 = Icmp neq, v4, v5
	v10:i32 = Icmp lt_s, v2, v3
	v11:i32 = Icmp lt_s, v4, v5
	v12:i32 = Icmp lt_u, v2, v3
	v13:i32 = Icmp lt_u, v4, v5
	v14:i32 = Icmp gt_s, v2, v3
	v15:i32 = Icmp gt_s, v4, v5
	v16:i32 = Icmp gt_u, v2, v3
	v17:i32 = Icmp gt_u, v4, v5
	v18:i32 = Icmp le_s, v2, v3
	v19:i32 = Icmp le_s, v4, v5
	v20:i32 = Icmp le_u, v2, v3
	v21:i32 = Icmp le_u, v4, v5
	v22:i32 = Icmp ge_s, v2, v3
	v23:i32 = Icmp ge_s, v4, v5
	v24:i32 = Icmp ge_u, v2, v3
	v25:i32 = Icmp ge_u, v4, v5
	Jump blk_ret, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17, v18, v19, v20, v21, v22, v23, v24, v25
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64, v5:i64)
	v6:i32 = Icmp eq, v2, v3
	v7:i32 = Icmp eq, v4, v5
	v8:i32 = Icmp neq, v2, v3
	v9:i32 = Icmp neq, v4, v5
	v10:i32 = Icmp lt_s, v2, v3
	v11:i32 = Icmp lt_s, v4, v5
	v12:i32 = Icmp lt_u, v2, v3
	v13:i32 = Icmp lt_u, v4, v5
	v14:i32 = Icmp gt_s, v2, v3
	v15:i32 = Icmp gt_s, v4, v5
	v16:i32 = Icmp gt_u, v2, v3
	v17:i32 = Icmp gt_u, v4, v5
	v18:i32 = Icmp le_s, v2, v3
	v19:i32 = Icmp le_s, v4, v5
	v20:i32 = Icmp le_u, v2, v3
	v21:i32 = Icmp le_u, v4, v5
	v22:i32 = Icmp ge_s, v2, v3
	v23:i32 = Icmp ge_s, v4, v5
	v24:i32 = Icmp ge_u, v2, v3
	v25:i32 = Icmp ge_u, v4, v5
	Jump blk_ret, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17, v18, v19, v20, v21, v22, v23, v24, v25
`,
		},
		{
			name: "integer bitwise", m: testcases.IntegerBitwise.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64, v5:i64)
	v6:i32 = Band v2, v3
	v7:i32 = Bor v2, v3
	v8:i32 = Bxor v2, v3
	v9:i32 = Rotr v2, v3
	v10:i64 = Band v4, v5
	v11:i64 = Bor v4, v5
	v12:i64 = Bxor v4, v5
	v13:i64 = Iconst_64 0x8
	v14:i64 = Ishl v5, v13
	v15:i64 = Bxor v4, v14
	v16:i64 = Rotl v4, v5
	v17:i64 = Rotr v4, v5
	Jump blk_ret, v6, v7, v8, v9, v10, v11, v12, v15, v16, v17
`,
		},
		{
			name: "integer shift", m: testcases.IntegerShift.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64, v5:i64)
	v6:i32 = Ishl v2, v3
	v7:i32 = Iconst_32 0x1f
	v8:i32 = Ishl v2, v7
	v9:i64 = Ishl v4, v5
	v10:i64 = Iconst_64 0x20
	v11:i64 = Ishl v4, v10
	v12:i32 = Ushr v2, v3
	v13:i32 = Iconst_32 0x1f
	v14:i32 = Ushr v2, v13
	v15:i64 = Ushr v4, v5
	v16:i64 = Iconst_64 0x20
	v17:i64 = Ushr v4, v16
	v18:i32 = Sshr v2, v3
	v19:i32 = Iconst_32 0x1f
	v20:i32 = Sshr v2, v19
	v21:i64 = Sshr v4, v5
	v22:i64 = Iconst_64 0x20
	v23:i64 = Sshr v4, v22
	Jump blk_ret, v6, v8, v9, v11, v12, v14, v15, v17, v18, v20, v21, v23
`,
		},
		{
			name: "integer extension", m: testcases.IntegerExtensions.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64)
	v4:i64 = SExtend v2, 32->64
	v5:i64 = UExtend v2, 32->64
	v6:i64 = SExtend v3, 8->64
	v7:i64 = SExtend v3, 16->64
	v8:i64 = SExtend v3, 32->64
	v9:i32 = SExtend v2, 8->32
	v10:i32 = SExtend v2, 16->32
	Jump blk_ret, v4, v5, v6, v7, v8, v9, v10
`,
		},
		{
			name: "integer bit counts", m: testcases.IntegerBitCounts.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64)
	v4:i32 = Clz v2
	v5:i32 = Ctz v2
	v6:i32 = Popcnt v2
	v7:i64 = Clz v3
	v8:i64 = Ctz v3
	v9:i64 = Popcnt v3
	Jump blk_ret, v4, v5, v6, v7, v8, v9
`,
		},
		{
			name: "float comparisons", m: testcases.FloatComparisons.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:f32, v3:f32, v4:f64, v5:f64)
	v6:i32 = Fcmp eq, v2, v3
	v7:i32 = Fcmp neq, v2, v3
	v8:i32 = Fcmp lt, v2, v3
	v9:i32 = Fcmp gt, v2, v3
	v10:i32 = Fcmp le, v2, v3
	v11:i32 = Fcmp ge, v2, v3
	v12:i32 = Fcmp eq, v4, v5
	v13:i32 = Fcmp neq, v4, v5
	v14:i32 = Fcmp lt, v4, v5
	v15:i32 = Fcmp gt, v4, v5
	v16:i32 = Fcmp le, v4, v5
	v17:i32 = Fcmp ge, v4, v5
	Jump blk_ret, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17
`,
		},
		{
			name: "float conversions", m: testcases.FloatConversions.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:f64, v3:f32)
	v4:i64 = FcvtToSint v2
	v5:i64 = FcvtToSint v3
	v6:i32 = FcvtToSint v2
	v7:i32 = FcvtToSint v3
	v8:i64 = FcvtToUint v2
	v9:i64 = FcvtToUint v3
	v10:i32 = FcvtToUint v2
	v11:i32 = FcvtToUint v3
	v12:f32 = Fdemote v2
	v13:f64 = Fpromote v3
	Jump blk_ret, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13
`,
		},
		{
			name: "non-trapping float conversions", m: testcases.NonTrappingFloatConversions.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:f64, v3:f32)
	v4:i64 = FcvtToSintSat v2
	v5:i64 = FcvtToSintSat v3
	v6:i32 = FcvtToSintSat v2
	v7:i32 = FcvtToSintSat v3
	v8:i64 = FcvtToUintSat v2
	v9:i64 = FcvtToUintSat v3
	v10:i32 = FcvtToUintSat v2
	v11:i32 = FcvtToUintSat v3
	Jump blk_ret, v4, v5, v6, v7, v8, v9, v10, v11
`,
		},
		{
			name: "loop with param and results", m: testcases.LoopBrWithParamResults.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Jump blk1, v2, v3

blk1: (v4:i32,v5:i32) <-- (blk0,blk4)
	v7:i32 = PreemptRequested
	Brnz v7, blk3
	Jump blk4

blk2: (v6:i32)

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	v8:i32 = Iconst_32 0x1
	Brnz v8, blk1, v4, v5
	Jump blk5

blk5: () <-- (blk4)
	Jump blk_ret, v4
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Jump fallthrough

blk1: () <-- (blk0,blk7)
	v7:i32 = PreemptRequested
	Brnz v7, blk3
	Jump fallthrough

blk6: () <-- (blk1)
	Jump fallthrough

blk4: () <-- (blk6,blk3)
	v8:i32 = Iconst_32 0x1
	Brz v8, blk5
	Jump fallthrough

blk7: () <-- (blk4)
	Jump blk1

blk5: () <-- (blk4)
	Jump blk_ret, v2

blk3: () <-- (blk1)
	Yield v0
	Jump blk4
`,
		},
		{
			name:        "many_params_small_results",
			m:           testcases.ManyParamsSmallResults.Module,
			targetIndex: 0,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64, v6:i32, v7:i64, v8:f32, v9:f64, v10:i32, v11:i64, v12:f32, v13:f64, v14:i32, v15:i64, v16:f32, v17:f64, v18:i32, v19:i64, v20:f32, v21:f64, v22:i32, v23:i64, v24:f32, v25:f64, v26:i32, v27:i64, v28:f32, v29:f64, v30:i32, v31:i64, v32:f32, v33:f64, v34:i32, v35:i64, v36:f32, v37:f64, v38:i32, v39:i64, v40:f32, v41:f64)
	Jump blk_ret, v2, v11, v20, v29
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64, v6:i32, v7:i64, v8:f32, v9:f64, v10:i32, v11:i64, v12:f32, v13:f64, v14:i32, v15:i64, v16:f32, v17:f64, v18:i32, v19:i64, v20:f32, v21:f64, v22:i32, v23:i64, v24:f32, v25:f64, v26:i32, v27:i64, v28:f32, v29:f64, v30:i32, v31:i64, v32:f32, v33:f64, v34:i32, v35:i64, v36:f32, v37:f64, v38:i32, v39:i64, v40:f32, v41:f64)
	Jump blk_ret, v2, v11, v20, v29
`,
		},
		{
			name:        "small_params_many_results",
			m:           testcases.SmallParamsManyResults.Module,
			targetIndex: 0,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64)
	Jump blk_ret, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64)
	Jump blk_ret, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5, v2, v3, v4, v5
`,
		},
		{
			name:        "many_params_many_results",
			m:           testcases.ManyParamsManyResults.Module,
			targetIndex: 0,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64, v6:i32, v7:i64, v8:f32, v9:f64, v10:i32, v11:i64, v12:f32, v13:f64, v14:i32, v15:i64, v16:f32, v17:f64, v18:i32, v19:i64, v20:f32, v21:f64, v22:i32, v23:i64, v24:f32, v25:f64, v26:i32, v27:i64, v28:f32, v29:f64, v30:i32, v31:i64, v32:f32, v33:f64, v34:i32, v35:i64, v36:f32, v37:f64, v38:i32, v39:i64, v40:f32, v41:f64)
	Jump blk_ret, v41, v40, v39, v38, v37, v36, v35, v34, v33, v32, v31, v30, v29, v28, v27, v26, v25, v24, v23, v22, v21, v20, v19, v18, v17, v16, v15, v14, v13, v12, v11, v10, v9, v8, v7, v6, v5, v4, v3, v2
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:f32, v5:f64, v6:i32, v7:i64, v8:f32, v9:f64, v10:i32, v11:i64, v12:f32, v13:f64, v14:i32, v15:i64, v16:f32, v17:f64, v18:i32, v19:i64, v20:f32, v21:f64, v22:i32, v23:i64, v24:f32, v25:f64, v26:i32, v27:i64, v28:f32, v29:f64, v30:i32, v31:i64, v32:f32, v33:f64, v34:i32, v35:i64, v36:f32, v37:f64, v38:i32, v39:i64, v40:f32, v41:f64)
	Jump blk_ret, v41, v40, v39, v38, v37, v36, v35, v34, v33, v32, v31, v30, v29, v28, v27, v26, v25, v24, v23, v22, v21, v20, v19, v18, v17, v16, v15, v14, v13, v12, v11, v10, v9, v8, v7, v6, v5, v4, v3, v2
`,
		},
		{
			name: "many_middle_values",
			m:    testcases.ManyMiddleValues.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:f32)
	v4:i32 = Iconst_32 0x1
	v5:i32 = Iconst_32 0x2
	v6:i32 = Iconst_32 0x1
	v7:i32 = Ishl v2, v6
	v8:i32 = Iconst_32 0x3
	v9:i32 = Imul v2, v8
	v10:i32 = Iconst_32 0x4
	v11:i32 = Iconst_32 0x2
	v12:i32 = Ishl v2, v11
	v13:i32 = Iconst_32 0x5
	v14:i32 = Imul v2, v13
	v15:i32 = Iconst_32 0x6
	v16:i32 = Imul v2, v15
	v17:i32 = Iconst_32 0x7
	v18:i32 = Imul v2, v17
	v19:i32 = Iconst_32 0x8
	v20:i32 = Iconst_32 0x3
	v21:i32 = Ishl v2, v20
	v22:i32 = Iconst_32 0x9
	v23:i32 = Imul v2, v22
	v24:i32 = Iconst_32 0xa
	v25:i32 = Imul v2, v24
	v26:i32 = Iconst_32 0xb
	v27:i32 = Imul v2, v26
	v28:i32 = Iconst_32 0xc
	v29:i32 = Imul v2, v28
	v30:i32 = Iconst_32 0xd
	v31:i32 = Imul v2, v30
	v32:i32 = Iconst_32 0xe
	v33:i32 = Imul v2, v32
	v34:i32 = Iconst_32 0xf
	v35:i32 = Imul v2, v34
	v36:i32 = Iconst_32 0x10
	v37:i32 = Iconst_32 0x4
	v38:i32 = Ishl v2, v37
	v39:i32 = Iconst_32 0x11
	v40:i32 = Imul v2, v39
	v41:i32 = Iconst_32 0x12
	v42:i32 = Imul v2, v41
	v43:i32 = Iconst_32 0x13
	v44:i32 = Imul v2, v43
	v45:i32 = Iconst_32 0x14
	v46:i32 = Imul v2, v45
	v47:i32 = Iadd v44, v46
	v48:i32 = Iadd v42, v47
	v49:i32 = Iadd v40, v48
	v50:i32 = Iadd v38, v49
	v51:i32 = Iadd v35, v50
	v52:i32 = Iadd v33, v51
	v53:i32 = Iadd v31, v52
	v54:i32 = Iadd v29, v53
	v55:i32 = Iadd v27, v54
	v56:i32 = Iadd v25, v55
	v57:i32 = Iadd v23, v56
	v58:i32 = Iadd v21, v57
	v59:i32 = Iadd v18, v58
	v60:i32 = Iadd v16, v59
	v61:i32 = Iadd v14, v60
	v62:i32 = Iadd v12, v61
	v63:i32 = Iadd v9, v62
	v64:i32 = Iadd v7, v63
	v65:i32 = Iadd v2, v64
	v66:f32 = F32const 1.000000
	v67:f32 = Fmul v3, v66
	v68:f32 = F32const 2.000000
	v69:f32 = Fmul v3, v68
	v70:f32 = F32const 3.000000
	v71:f32 = Fmul v3, v70
	v72:f32 = F32const 4.000000
	v73:f32 = Fmul v3, v72
	v74:f32 = F32const 5.000000
	v75:f32 = Fmul v3, v74
	v76:f32 = F32const 6.000000
	v77:f32 = Fmul v3, v76
	v78:f32 = F32const 7.000000
	v79:f32 = Fmul v3, v78
	v80:f32 = F32const 8.000000
	v81:f32 = Fmul v3, v80
	v82:f32 = F32const 9.000000
	v83:f32 = Fmul v3, v82
	v84:f32 = F32const 10.000000
	v85:f32 = Fmul v3, v84
	v86:f32 = F32const 11.000000
	v87:f32 = Fmul v3, v86
	v88:f32 = F32const 12.000000
	v89:f32 = Fmul v3, v88
	v90:f32 = F32const 13.000000
	v91:f32 = Fmul v3, v90
	v92:f32 = F32const 14.000000
	v93:f32 = Fmul v3, v92
	v94:f32 = F32const 15.000000
	v95:f32 = Fmul v3, v94
	v96:f32 = F32const 16.000000
	v97:f32 = Fmul v3, v96
	v98:f32 = F32const 17.000000
	v99:f32 = Fmul v3, v98
	v100:f32 = F32const 18.000000
	v101:f32 = Fmul v3, v100
	v102:f32 = F32const 19.000000
	v103:f32 = Fmul v3, v102
	v104:f32 = F32const 20.000000
	v105:f32 = Fmul v3, v104
	v106:f32 = Fadd v103, v105
	v107:f32 = Fadd v101, v106
	v108:f32 = Fadd v99, v107
	v109:f32 = Fadd v97, v108
	v110:f32 = Fadd v95, v109
	v111:f32 = Fadd v93, v110
	v112:f32 = Fadd v91, v111
	v113:f32 = Fadd v89, v112
	v114:f32 = Fadd v87, v113
	v115:f32 = Fadd v85, v114
	v116:f32 = Fadd v83, v115
	v117:f32 = Fadd v81, v116
	v118:f32 = Fadd v79, v117
	v119:f32 = Fadd v77, v118
	v120:f32 = Fadd v75, v119
	v121:f32 = Fadd v73, v120
	v122:f32 = Fadd v71, v121
	v123:f32 = Fadd v69, v122
	v124:f32 = Fadd v67, v123
	Jump blk_ret, v65, v124
`,
		},
		{
			name: "recursive_fibonacci", m: testcases.FibonacciRecursive.Module,
			exp: `
signatures:
	sig0: i64i64i32_i32

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = PreemptRequested
	Brnz v3, blk1
	Jump blk2

blk1: () <-- (blk0)
	Yield v0
	Jump blk2

blk2: () <-- (blk0,blk1)
	v4:i32 = Iconst_32 0x2
	v5:i32 = Icmp lt_s, v2, v4
	Brz v5, blk4
	Jump blk3

blk3: () <-- (blk2)
	Return v2

blk4: () <-- (blk2)
	Jump blk5

blk5: () <-- (blk4)
	v6:i32 = Iconst_32 0x1
	v7:i32 = Isub v2, v6
	v8:i32 = Call f0:sig0, v0, v1, v7
	v9:i32 = Iconst_32 0x2
	v10:i32 = Isub v2, v9
	v11:i32 = Call f0:sig0, v0, v1, v10
	v12:i32 = Iadd v8, v11
	Jump blk_ret, v12
`,
			expAfterPasses: `
signatures:
	sig0: i64i64i32_i32

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = PreemptRequested
	Brnz v3, blk1
	Jump fallthrough

blk6: () <-- (blk0)
	Jump fallthrough

blk2: () <-- (blk6,blk1)
	v4:i32 = Iconst_32 0x2
	v5:i32 = Icmp lt_s, v2, v4
	Brz v5, blk4
	Jump fallthrough

blk3: () <-- (blk2)
	Return v2

blk4: () <-- (blk2)
	Jump fallthrough

blk5: () <-- (blk4)
	v6:i32 = Iconst_32 0x1
	v7:i32 = Isub v2, v6
	v8:i32 = Call f0:sig0, v0, v1, v7
	v9:i32 = Iconst_32 0x2
	v10:i32 = Isub v2, v9
	v11:i32 = Call f0:sig0, v0, v1, v10
	v12:i32 = Iadd v8, v11
	Jump blk_ret, v12

blk1: () <-- (blk0)
	Yield v0
	Jump blk2
`,
		},
		{
			name: "memory_store_basic", m: testcases.MemoryStoreBasic.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	v4:i64 = Iconst_64 0x4
	v5:i64 = UExtend v2, 32->64
	v6:i64 = Load v1, 0x10
	v7:i64 = Isub v6, v4
	v8:i32 = Icmp gt_u, v5, v7
	ExitIfTrue v8, v0, memory_out_of_bounds
	v9:i64 = Load v1, 0x8
	v10:i64 = Iadd v9, v5
	Store v3, v10, 0x0
	Jump blk_ret, v3
`,
		},
		{
			name: "memory_load_basic", m: testcases.MemoryLoadBasic.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i64 = Iconst_64 0x4
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Load v1, 0x10
	v6:i64 = Isub v5, v3
	v7:i32 = Icmp gt_u, v4, v6
	ExitIfTrue v7, v0, memory_out_of_bounds
	v8:i64 = Load v1, 0x8
	v9:i64 = Iadd v8, v4
	v10:i32 = Load v9, 0x0
	Jump blk_ret, v10
`,
		},
		{
			name: "memory_load_urem", m: testcases.MemoryLoadURem.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0xfffd
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Iconst_64 0x100030009001c
	v6:i64 = Imul v4, v5
	v7:i64 = Iconst_64 0xfffd
	v8:i64 = Umulhi v6, v7
	v9:i32 = Ireduce v8
	v10:i32 = Iconst_32 0x100000
	v11:i32 = Iadd v9, v10
	v12:i64 = Load v1, 0x8
	v13:i64 = UExtend v11, 32->64
	v14:i64 = Iadd v12, v13
	v15:i32 = Load v14, 0x0
	Jump blk_ret, v15
`,
		},
		{
			name: "memory_load_basic2", m: testcases.MemoryLoadBasic2.Module,
			exp: `
signatures:
	sig1: i64i64_v

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	v4:i32 = Icmp eq, v2, v3
	Brz v4, blk2
	Jump blk1

blk1: () <-- (blk0)
	Call f1:sig1, v0, v1
	v5:i64 = Load v1, 0x8
	v6:i64 = Load v1, 0x10
	Jump blk3

blk2: () <-- (blk0)
	Jump blk3

blk3: () <-- (blk1,blk2)
	v8:i64 = Iconst_64 0x4
	v9:i64 = UExtend v2, 32->64
	v10:i64 = Load v1, 0x10
	v11:i64 = Isub v10, v8
	v12:i32 = Icmp gt_u, v9, v11
	ExitIfTrue v12, v0, memory_out_of_bounds
	v13:i64 = Load v1, 0x8
	v14:i64 = Iadd v13, v9
	v15:i32 = Load v14, 0x0
	Jump blk_ret, v15
`,
			expAfterPasses: `
signatures:
	sig1: i64i64_v

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	v4:i32 = Icmp eq, v2, v3
	Brz v4, blk2
	Jump fallthrough

blk1: () <-- (blk0)
	Call f1:sig1, v0, v1
	Jump blk3

blk2: () <-- (blk0)
	Jump fallthrough

blk3: () <-- (blk1,blk2)
	v8:i64 = Iconst_64 0x4
	v9:i64 = UExtend v2, 32->64
	v10:i64 = Load v1, 0x10
	v11:i64 = Isub v10, v8
	v12:i32 = Icmp gt_u, v9, v11
	ExitIfTrue v12, v0, memory_out_of_bounds
	v13:i64 = Load v1, 0x8
	v14:i64 = Iadd v13, v9
	v15:i32 = Load v14, 0x0
	Jump blk_ret, v15
`,
		},
		{
			name: "memory_load_after_pure_call", m: testcases.MemoryLoadAfterPureCall.Module,
			exp: `
signatures:
	sig1: i64i64_v

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = Iconst_32 0x0
	v4:i32 = Icmp eq, v2, v3
	Brz v4, blk2
	Jump blk1

blk1: () <-- (blk0)
	Call f1:sig1, v0, v1
	Jump blk3

blk2: () <-- (blk0)
	Jump blk3

blk3: () <-- (blk1,blk2)
	v6:i64 = Iconst_64 0x4
	v7:i64 = UExtend v2, 32->64
	v8:i64 = Load v1, 0x10
	v9:i64 = Isub v8, v6
	v10:i32 = Icmp gt_u, v7, v9
	ExitIfTrue v10, v0, memory_out_of_bounds
	v11:i64 = Load v1, 0x8
	v12:i64 = Iadd v11, v7
	v13:i32 = Load v12, 0x0
	Jump blk_ret, v13
`,
		},
		{
			name: "imported_function_call", m: testcases.ImportedFunctionCall.Module,
			exp: `
signatures:
	sig1: i64i64i32i32_i32

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = PreemptRequested
	Brnz v3, blk1
	Jump blk2

blk1: () <-- (blk0)
	Yield v0
	Jump blk2

blk2: () <-- (blk0,blk1)
	Store v1, v0, 0x8
	v4:i64 = Load v1, 0x8
	v5:i64 = Load v1, 0x10
	v6:i32 = CallIndirect v4:sig1, v0, v5, v2, v2
	Jump blk_ret, v6
`,
		},
		{
			name: "memory_loads", m: testcases.MemoryLoads.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i64 = Iconst_64 0x4
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Load v1, 0x10
	v6:i64 = Isub v5, v3
	v7:i32 = Icmp gt_u, v4, v6
	ExitIfTrue v7, v0, memory_out_of_bounds
	v8:i64 = Load v1, 0x8
	v9:i64 = Iadd v8, v4
	v10:i32 = Load v9, 0x0
	v11:i64 = Iconst_64 0x8
	v12:i64 = UExtend v2, 32->64
	v13:i64 = Isub v5, v11
	v14:i32 = Icmp gt_u, v12, v13
	ExitIfTrue v14, v0, memory_out_of_bounds
	v15:i64 = Load v9, 0x0
	v16:f32 = Load v9, 0x0
	v17:f64 = Load v9, 0x0
	v18:i64 = Iconst_64 0x13
	v19:i64 = UExtend v2, 32->64
	v20:i64 = Iadd v19, v18
	v21:i32 = Icmp lt_u, v5, v20
	ExitIfTrue v21, v0, memory_out_of_bounds
	v22:i32 = Load v9, 0xf
	v23:i64 = Iconst_64 0x17
	v24:i64 = UExtend v2, 32->64
	v25:i64 = Iadd v24, v23
	v26:i32 = Icmp lt_u, v5, v25
	ExitIfTrue v26, v0, memory_out_of_bounds
	v27:i64 = Load v9, 0xf
	v28:f32 = Load v9, 0xf
	v29:f64 = Load v9, 0xf
	v30:i32 = Sload8 v9, 0x0
	v31:i32 = Sload8 v9, 0xf
	v32:i32 = Uload8 v9, 0x0
	v33:i32 = Uload8 v9, 0xf
	v34:i32 = Sload16 v9, 0x0
	v35:i32 = Sload16 v9, 0xf
	v36:i32 = Uload16 v9, 0x0
	v37:i32 = Uload16 v9, 0xf
	v38:i64 = Sload8 v9, 0x0
	v39:i64 = Sload8 v9, 0xf
	v40:i64 = Uload8 v9, 0x0
	v41:i64 = Uload8 v9, 0xf
	v42:i64 = Sload16 v9, 0x0
	v43:i64 = Sload16 v9, 0xf
	v44:i64 = Uload16 v9, 0x0
	v45:i64 = Uload16 v9, 0xf
	v46:i64 = Sload32 v9, 0x0
	v47:i64 = Sload32 v9, 0xf
	v48:i64 = Uload32 v9, 0x0
	v49:i64 = Uload32 v9, 0xf
	Jump blk_ret, v10, v15, v16, v17, v22, v27, v28, v29, v30, v31, v32, v33, v34, v35, v36, v37, v38, v39, v40, v41, v42, v43, v44, v45, v46, v47, v48, v49
`,
		},
		{
			name: "globals_get",
			m:    testcases.GlobalsGet.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Load v1, 0x10
	v3:i64 = Load v1, 0x20
	v4:f32 = Load v1, 0x30
	v5:f64 = Load v1, 0x40
	v6:v128 = Load v1, 0x50
	Jump blk_ret, v2, v3, v4, v5, v6
`,
		},
		{
			name: "globals_set",
			m:    testcases.GlobalsSet.Module,
			exp: `
blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x1
	Store v2, v1, 0x10
	v3:i64 = Iconst_64 0x2
	Store v3, v1, 0x20
	v4:f32 = F32const 3.000000
	Store v4, v1, 0x30
	v5:f64 = F64const 4.000000
	Store v5, v1, 0x40
	v6:v128 = Vconst 000000000000000a 0000000000000014
	Store v6, v1, 0x50
	Jump blk_ret, v2, v3, v4, v5, v6
`,
		},
		{
			name: "globals_mutable",
			m:    testcases.GlobalsMutable.Module,
			exp: `
signatures:
	sig1: i64i64_v

blk0: (v0:i64, v1:i64)
	v2:i32 = Load v1, 0x10
	v3:i64 = Load v1, 0x20
	v4:f32 = Load v1, 0x30
	v5:f64 = Load v1, 0x40
	Call f1:sig1, v0, v1
	v6:i32 = Load v1, 0x10
	v7:i64 = Load v1, 0x20
	v8:f32 = Load v1, 0x30
	v9:f64 = Load v1, 0x40
	Jump blk_ret, v2, v3, v4, v5, v6, v7, v8, v9
`,
			expAfterPasses: `
signatures:
	sig1: i64i64_v

blk0: (v0:i64, v1:i64)
	v2:i32 = Load v1, 0x10
	v3:i64 = Load v1, 0x20
	v4:f32 = Load v1, 0x30
	v5:f64 = Load v1, 0x40
	Call f1:sig1, v0, v1
	v6:i32 = Load v1, 0x10
	v7:i64 = Load v1, 0x20
	v8:f32 = Load v1, 0x30
	v9:f64 = Load v1, 0x40
	Jump blk_ret, v2, v3, v4, v5, v6, v7, v8, v9
`,
		},
		{
			name: "imported_memory_grow",
			m:    testcases.ImportedMemoryGrow.Module,
			exp: `
signatures:
	sig0: i64i64_i32
	sig2: i64i32i64_i64

blk0: (v0:i64, v1:i64)
	v2:i32 = PreemptRequested
	Brnz v2, blk1
	Jump blk2

blk1: () <-- (blk0)
	Yield v0
	Jump blk2

blk2: () <-- (blk0,blk1)
	Store v1, v0, 0x8
	v3:i64 = Load v1, 0x20
	v4:i64 = Load v1, 0x28
	v5:i32 = CallIndirect v3:sig0, v0, v4
	v6:i64 = Load v1, 0x8
	v7:i64 = Load v6, 0x8
	v8:i64 = Load v1, 0x8
	v9:i64 = Load v8, 0x0
	v10:i64 = Iconst_64 0x10
	v11:i64 = Ushr v9, v10
	v12:i32 = Ireduce v11
	v13:i32 = Iconst_32 0xa
	v14:i64 = UExtend v13, 32->64
	Store v1, v0, 0x8
	v15:i64 = Load v0, 0x48
	v16:i32 = Iconst_32 0x0
	v17:i64 = CallIndirect v15:sig2, v0, v16, v14
	v18:i32 = Ireduce v17
	v19:i64 = Load v1, 0x8
	v20:i64 = Load v19, 0x8
	v21:i64 = Load v1, 0x8
	v22:i64 = Load v21, 0x0
	v23:i32 = PreemptRequested
	Brnz v23, blk3
	Jump blk4

blk3: () <-- (blk2)
	Yield v0
	Jump blk4

blk4: () <-- (blk2,blk3)
	Store v1, v0, 0x8
	v24:i64 = Load v1, 0x20
	v25:i64 = Load v1, 0x28
	v26:i32 = CallIndirect v24:sig0, v0, v25
	v27:i64 = Load v1, 0x8
	v28:i64 = Load v27, 0x8
	v29:i64 = Load v1, 0x8
	v30:i64 = Load v29, 0x0
	v31:i64 = Iconst_64 0x10
	v32:i64 = Ushr v30, v31
	v33:i32 = Ireduce v32
	Jump blk_ret, v5, v12, v26, v33
`,
			expAfterPasses: `
signatures:
	sig0: i64i64_i32
	sig2: i64i32i64_i64

blk0: (v0:i64, v1:i64)
	v2:i32 = PreemptRequested
	Brnz v2, blk1
	Jump fallthrough

blk5: () <-- (blk0)
	Jump fallthrough

blk2: () <-- (blk5,blk1)
	Store v1, v0, 0x8
	v3:i64 = Load v1, 0x20
	v4:i64 = Load v1, 0x28
	v5:i32 = CallIndirect v3:sig0, v0, v4
	v8:i64 = Load v1, 0x8
	v9:i64 = Load v8, 0x0
	v10:i64 = Iconst_64 0x10
	v11:i64 = Ushr v9, v10
	v12:i32 = Ireduce v11
	v13:i32 = Iconst_32 0xa
	v14:i64 = UExtend v13, 32->64
	Store v1, v0, 0x8
	v15:i64 = Load v0, 0x48
	v16:i32 = Iconst_32 0x0
	v17:i64 = CallIndirect v15:sig2, v0, v16, v14
	v23:i32 = PreemptRequested
	Brnz v23, blk3
	Jump fallthrough

blk6: () <-- (blk2)
	Jump fallthrough

blk4: () <-- (blk6,blk3)
	Store v1, v0, 0x8
	v24:i64 = Load v1, 0x20
	v25:i64 = Load v1, 0x28
	v26:i32 = CallIndirect v24:sig0, v0, v25
	v29:i64 = Load v1, 0x8
	v30:i64 = Load v29, 0x0
	v31:i64 = Iconst_64 0x10
	v32:i64 = Ushr v30, v31
	v33:i32 = Ireduce v32
	Jump blk_ret, v5, v12, v26, v33

blk3: () <-- (blk2)
	Yield v0
	Jump blk4

blk1: () <-- (blk0)
	Yield v0
	Jump blk2
`,
		},
		{
			name: "memory_size_grow",
			m:    testcases.MemorySizeGrow.Module,
			exp: `
signatures:
	sig1: i64i32i64_i64

blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x1
	v4:i64 = Load v1, 0x10
	v5:i64 = Iconst_64 0x10
	v6:i64 = Ushr v4, v5
	v7:i32 = Ireduce v6
	v8:i64 = UExtend v2, 32->64
	v9:i64 = Iadd v6, v8
	v10:i64 = Iconst_64 0x2
	v11:i32 = Icmp gt_u, v9, v10
	Brnz v11, blk4
	Jump blk1

blk1: () <-- (blk0)
	v12:i64 = Load v1, 0x18
	v13:i64 = Load v12, 0x40
	v14:i64 = Ishl v9, v5
	v15:i32 = Icmp ge_u, v13, v14
	Brnz v15, blk2
	Jump blk3

blk2: () <-- (blk1)
	Store v14, v1, 0x10
	Store v14, v12, 0x0
	Jump blk5, v7

blk3: () <-- (blk1)
	v16:i64 = UExtend v2, 32->64
	Store v1, v0, 0x8
	v17:i64 = Load v0, 0x48
	v18:i32 = Iconst_32 0x0
	v19:i64 = CallIndirect v17:sig1, v0, v18, v16
	v20:i32 = Ireduce v19
	Jump blk5, v20

blk4: () <-- (blk0)
	v21:i32 = Iconst_32 0xffffffff
	Jump blk5, v21

blk5: (v3:i32) <-- (blk2,blk3,blk4)
	v22:i64 = Load v1, 0x8
	v23:i64 = Load v1, 0x10
	v24:i64 = Iconst_64 0x10
	v25:i64 = Ushr v23, v24
	v26:i32 = Ireduce v25
	v27:i32 = Iconst_32 0x1
	v29:i64 = Iconst_64 0x10
	v30:i64 = Ushr v23, v29
	v31:i32 = Ireduce v30
	v32:i64 = UExtend v27, 32->64
	v33:i64 = Iadd v30, v32
	v34:i64 = Iconst_64 0x2
	v35:i32 = Icmp gt_u, v33, v34
	Brnz v35, blk9
	Jump blk6

blk6: () <-- (blk5)
	v36:i64 = Load v1, 0x18
	v37:i64 = Load v36, 0x40
	v38:i64 = Ishl v33, v29
	v39:i32 = Icmp ge_u, v37, v38
	Brnz v39, blk7
	Jump blk8

blk7: () <-- (blk6)
	Store v38, v1, 0x10
	Store v38, v36, 0x0
	Jump blk10, v31

blk8: () <-- (blk6)
	v40:i64 = UExtend v27, 32->64
	Store v1, v0, 0x8
	v41:i64 = Load v0, 0x48
	v42:i32 = Iconst_32 0x0
	v43:i64 = CallIndirect v41:sig1, v0, v42, v40
	v44:i32 = Ireduce v43
	Jump blk10, v44

blk9: () <-- (blk5)
	v45:i32 = Iconst_32 0xffffffff
	Jump blk10, v45

blk10: (v28:i32) <-- (blk7,blk8,blk9)
	v46:i64 = Load v1, 0x8
	v47:i64 = Load v1, 0x10
	Jump blk_ret, v3, v26, v28
`,
			expAfterPasses: `
signatures:
	sig1: i64i32i64_i64

blk0: (v0:i64, v1:i64)
	v2:i32 = Iconst_32 0x1
	v4:i64 = Load v1, 0x10
	v5:i64 = Iconst_64 0x10
	v6:i64 = Ushr v4, v5
	v7:i32 = Ireduce v6
	v8:i64 = UExtend v2, 32->64
	v9:i64 = Iadd v6, v8
	v10:i64 = Iconst_64 0x2
	v11:i32 = Icmp gt_u, v9, v10
	Brnz v11, blk4
	Jump fallthrough

blk1: () <-- (blk0)
	v12:i64 = Load v1, 0x18
	v13:i64 = Load v12, 0x40
	v14:i64 = Ishl v9, v5
	v15:i32 = Icmp ge_u, v13, v14
	Brz v15, blk3
	Jump fallthrough

blk2: () <-- (blk1)
	Store v14, v1, 0x10
	Store v14, v12, 0x0
	Jump blk5, v7

blk3: () <-- (blk1)
	v16:i64 = UExtend v2, 32->64
	Store v1, v0, 0x8
	v17:i64 = Load v0, 0x48
	v18:i32 = Iconst_32 0x0
	v19:i64 = CallIndirect v17:sig1, v0, v18, v16
	v20:i32 = Ireduce v19
	Jump blk5, v20

blk4: () <-- (blk0)
	v21:i32 = Iconst_32 0xffffffff
	Jump fallthrough, v21

blk5: (v3:i32) <-- (blk2,blk3,blk4)
	v23:i64 = Load v1, 0x10
	v24:i64 = Iconst_64 0x10
	v25:i64 = Ushr v23, v24
	v26:i32 = Ireduce v25
	v27:i32 = Iconst_32 0x1
	v29:i64 = Iconst_64 0x10
	v30:i64 = Ushr v23, v29
	v31:i32 = Ireduce v30
	v32:i64 = UExtend v27, 32->64
	v33:i64 = Iadd v30, v32
	v34:i64 = Iconst_64 0x2
	v35:i32 = Icmp gt_u, v33, v34
	Brnz v35, blk9
	Jump fallthrough

blk6: () <-- (blk5)
	v36:i64 = Load v1, 0x18
	v37:i64 = Load v36, 0x40
	v38:i64 = Ishl v33, v29
	v39:i32 = Icmp ge_u, v37, v38
	Brz v39, blk8
	Jump fallthrough

blk7: () <-- (blk6)
	Store v38, v1, 0x10
	Store v38, v36, 0x0
	Jump blk10, v31

blk8: () <-- (blk6)
	v40:i64 = UExtend v27, 32->64
	Store v1, v0, 0x8
	v41:i64 = Load v0, 0x48
	v42:i32 = Iconst_32 0x0
	v43:i64 = CallIndirect v41:sig1, v0, v42, v40
	v44:i32 = Ireduce v43
	Jump blk10, v44

blk9: () <-- (blk5)
	v45:i32 = Iconst_32 0xffffffff
	Jump fallthrough, v45

blk10: (v28:i32) <-- (blk7,blk8,blk9)
	Jump blk_ret, v3, v26, v28
`,
		},
		{
			name: "call_indirect", m: testcases.CallIndirect.Module,
			exp: `
signatures:
	sig2: i64i64_i32

blk0: (v0:i64, v1:i64, v2:i32)
	v3:i32 = PreemptRequested
	Brnz v3, blk1
	Jump blk2

blk1: () <-- (blk0)
	Yield v0
	Jump blk2

blk2: () <-- (blk0,blk1)
	v4:i64 = Load v1, 0x10
	v5:i32 = Load v4, 0x8
	v6:i32 = Icmp ge_u, v2, v5
	ExitIfTrue v6, v0, table_out_of_bounds
	v7:i64 = Load v4, 0x0
	v8:i64 = Iconst_64 0x3
	v9:i32 = Ishl v2, v8
	v10:i64 = Iadd v7, v9
	v11:i64 = Load v10, 0x0
	v12:i64 = Iconst_64 0x0
	v13:i32 = Icmp eq, v11, v12
	ExitIfTrue v13, v0, indirect_call_null_pointer
	v14:i32 = Load v11, 0x10
	v15:i64 = Load v1, 0x8
	v16:i32 = Load v15, 0x8
	v17:i32 = Icmp neq, v14, v16
	ExitIfTrue v17, v0, indirect_call_type_mismatch
	v18:i64 = Load v11, 0x0
	v19:i64 = Load v11, 0x8
	Store v1, v0, 0x8
	v20:i32 = CallIndirect v18:sig2, v0, v19
	Jump blk_ret, v20
`,
		},
		{
			name: "br_table", m: testcases.BrTable.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	BrTable v2, [blk7, blk8, blk9, blk10, blk11, blk12, blk13]

blk1: () <-- (blk12)
	v8:i32 = Iconst_32 0x10
	Return v8

blk2: () <-- (blk11)
	v7:i32 = Iconst_32 0xf
	Return v7

blk3: () <-- (blk10)
	v6:i32 = Iconst_32 0xe
	Return v6

blk4: () <-- (blk9)
	v5:i32 = Iconst_32 0xd
	Return v5

blk5: () <-- (blk8)
	v4:i32 = Iconst_32 0xc
	Return v4

blk6: () <-- (blk7,blk13)
	v3:i32 = Iconst_32 0xb
	Return v3

blk7: () <-- (blk0)
	Jump blk6

blk8: () <-- (blk0)
	Jump blk5

blk9: () <-- (blk0)
	Jump blk4

blk10: () <-- (blk0)
	Jump blk3

blk11: () <-- (blk0)
	Jump blk2

blk12: () <-- (blk0)
	Jump blk1

blk13: () <-- (blk0)
	Jump blk6
`,
		},
		{
			name: "br_table_with_arg", m: testcases.BrTableWithArg.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	BrTable v2, [blk7, blk8, blk9, blk10, blk11, blk12, blk13]

blk1: (v4:i32) <-- (blk12)
	v20:i32 = Iconst_32 0x10
	v21:i32 = Iadd v4, v20
	Return v21

blk2: (v5:i32) <-- (blk11)
	v18:i32 = Iconst_32 0xf
	v19:i32 = Iadd v5, v18
	Return v19

blk3: (v6:i32) <-- (blk10)
	v16:i32 = Iconst_32 0xe
	v17:i32 = Iadd v6, v16
	Return v17

blk4: (v7:i32) <-- (blk9)
	v14:i32 = Iconst_32 0xd
	v15:i32 = Iadd v7, v14
	Return v15

blk5: (v8:i32) <-- (blk8)
	v12:i32 = Iconst_32 0xc
	v13:i32 = Iadd v8, v12
	Return v13

blk6: (v9:i32) <-- (blk7,blk13)
	v10:i32 = Iconst_32 0xb
	v11:i32 = Iadd v9, v10
	Return v11

blk7: () <-- (blk0)
	Jump blk6, v3

blk8: () <-- (blk0)
	Jump blk5, v3

blk9: () <-- (blk0)
	Jump blk4, v3

blk10: () <-- (blk0)
	Jump blk3, v3

blk11: () <-- (blk0)
	Jump blk2, v3

blk12: () <-- (blk0)
	Jump blk1, v3

blk13: () <-- (blk0)
	Jump blk6, v3
`,

			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	BrTable v2, [blk7, blk8, blk9, blk10, blk11, blk12, blk13]

blk7: () <-- (blk0)
	Jump blk6

blk8: () <-- (blk0)
	Jump fallthrough

blk5: () <-- (blk8)
	v12:i32 = Iconst_32 0xc
	v13:i32 = Iadd v3, v12
	Return v13

blk9: () <-- (blk0)
	Jump fallthrough

blk4: () <-- (blk9)
	v14:i32 = Iconst_32 0xd
	v15:i32 = Iadd v3, v14
	Return v15

blk10: () <-- (blk0)
	Jump fallthrough

blk3: () <-- (blk10)
	v16:i32 = Iconst_32 0xe
	v17:i32 = Iadd v3, v16
	Return v17

blk11: () <-- (blk0)
	Jump fallthrough

blk2: () <-- (blk11)
	v18:i32 = Iconst_32 0xf
	v19:i32 = Iadd v3, v18
	Return v19

blk12: () <-- (blk0)
	Jump fallthrough

blk1: () <-- (blk12)
	v20:i32 = Iconst_32 0x10
	v21:i32 = Iadd v3, v20
	Return v21

blk13: () <-- (blk0)
	Jump fallthrough

blk6: () <-- (blk7,blk13)
	v10:i32 = Iconst_32 0xb
	v11:i32 = Iadd v3, v10
	Return v11
`,
		},
		{
			name: "if_then_end_nesting_unreachable_if_then_else_end", m: testcases.IfThenEndNestingUnreachableIfThenElseEnd.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:f64, v3:f64, v4:f64)
	v6:i64 = Load v1, 0x10
	v7:i64 = Iconst_64 0x10
	v8:i64 = Ushr v6, v7
	v9:i32 = Ireduce v8
	Brz v9, blk3
	Jump blk2

blk1: (v5:i64)

blk2: () <-- (blk0)
	v10:i64 = Iconst_64 0x10
	v11:i64 = Ushr v6, v10
	v12:i32 = Ireduce v11
	Jump blk4

blk3: () <-- (blk0)
	Jump blk4

blk4: () <-- (blk2,blk3)
	v13:i64 = Iconst_64 0x0
	Jump blk_ret
`,
			expAfterPasses: `
blk0: (v0:i64, v1:i64, v2:f64, v3:f64, v4:f64)
	v6:i64 = Load v1, 0x10
	v7:i64 = Iconst_64 0x10
	v8:i64 = Ushr v6, v7
	v9:i32 = Ireduce v8
	Brz v9, blk3
	Jump fallthrough

blk2: () <-- (blk0)
	Jump blk4

blk3: () <-- (blk0)
	Jump fallthrough

blk4: () <-- (blk2,blk3)
	Jump blk_ret
`,
		},
		{
			name: "VecShuffle",
			m:    testcases.VecShuffle.Module,
			exp: `
blk0: (v0:i64, v1:i64, v2:v128, v3:v128)
	v4:v128 = Shuffle.[0 1 2 3 4 5 6 7 24 25 26 27 28 29 30 31] v2, v3
	Jump blk_ret, v4
`,
		},
		{
			name:     "MemoryWait32",
			m:        testcases.MemoryWait32.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
signatures:
	sig5: i64i32i64i32i64_i32

blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i64)
	Store v1, v0, 0x8
	v5:i64 = UExtend v2, 32->64
	v6:i64 = Iconst_64 0x8
	v7:i64 = Iadd v5, v6
	v8:i64 = Iconst_64 0x3
	v9:i64 = Band v7, v8
	v10:i64 = Iconst_64 0x0
	v11:i32 = Icmp neq, v9, v10
	ExitIfTrue v11, v0, unaligned_atomic
	v12:i64 = Iconst_64 0xc
	v13:i64 = UExtend v2, 32->64
	v14:i64 = Iconst_64 0x10
	v15:i64 = Iadd v1, v14
	v16:i64 = AtomicLoad_64, v15
	v17:i64 = Iadd v13, v12
	v18:i32 = Icmp lt_u, v16, v17
	ExitIfTrue v18, v0, memory_out_of_bounds
	v19:i64 = Load v1, 0x8
	v20:i64 = Iadd v19, v13
	v21:i64 = Iconst_64 0x8
	v22:i64 = Iadd v20, v21
	v23:i64 = Load v0, 0x288
	v24:i32 = Iconst_32 0x0
	v25:i32 = CallIndirect v23:sig5, v0, v24, v4, v3, v22
	Jump blk_ret, v25
`,
		},
		{
			name:     "MemoryWait64",
			m:        testcases.MemoryWait64.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
signatures:
	sig6: i64i32i64i64i64_i32

blk0: (v0:i64, v1:i64, v2:i32, v3:i64, v4:i64)
	Store v1, v0, 0x8
	v5:i64 = UExtend v2, 32->64
	v6:i64 = Iconst_64 0x8
	v7:i64 = Iadd v5, v6
	v8:i64 = Iconst_64 0x7
	v9:i64 = Band v7, v8
	v10:i64 = Iconst_64 0x0
	v11:i32 = Icmp neq, v9, v10
	ExitIfTrue v11, v0, unaligned_atomic
	v12:i64 = Iconst_64 0x10
	v13:i64 = UExtend v2, 32->64
	v14:i64 = Iconst_64 0x10
	v15:i64 = Iadd v1, v14
	v16:i64 = AtomicLoad_64, v15
	v17:i64 = Iadd v13, v12
	v18:i32 = Icmp lt_u, v16, v17
	ExitIfTrue v18, v0, memory_out_of_bounds
	v19:i64 = Load v1, 0x8
	v20:i64 = Iadd v19, v13
	v21:i64 = Iconst_64 0x8
	v22:i64 = Iadd v20, v21
	v23:i64 = Load v0, 0x290
	v24:i32 = Iconst_32 0x0
	v25:i32 = CallIndirect v23:sig6, v0, v24, v4, v3, v22
	Jump blk_ret, v25
`,
		},
		{
			name:     "MemoryNotify",
			m:        testcases.MemoryNotify.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
signatures:
	sig7: i64i32i32i64_i32

blk0: (v0:i64, v1:i64, v2:i32, v3:i32)
	Store v1, v0, 0x8
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Iconst_64 0x8
	v6:i64 = Iadd v4, v5
	v7:i64 = Iconst_64 0x3
	v8:i64 = Band v6, v7
	v9:i64 = Iconst_64 0x0
	v10:i32 = Icmp neq, v8, v9
	ExitIfTrue v10, v0, unaligned_atomic
	v11:i64 = Iconst_64 0xc
	v12:i64 = UExtend v2, 32->64
	v13:i64 = Iconst_64 0x10
	v14:i64 = Iadd v1, v13
	v15:i64 = AtomicLoad_64, v14
	v16:i64 = Iadd v12, v11
	v17:i32 = Icmp lt_u, v15, v16
	ExitIfTrue v17, v0, memory_out_of_bounds
	v18:i64 = Load v1, 0x8
	v19:i64 = Iadd v18, v12
	v20:i64 = Iconst_64 0x8
	v21:i64 = Iadd v19, v20
	v22:i64 = Load v0, 0x298
	v23:i32 = Iconst_32 0x0
	v24:i32 = CallIndirect v22:sig7, v0, v23, v3, v21
	Jump blk_ret, v24
`,
		},
		{
			name:     "AtomicRMWAdd",
			m:        testcases.AtomicRmwAdd.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw add_8, v13, v2
	v15:i32 = Iconst_32 0x8
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x1
	v18:i64 = Band v16, v17
	v19:i64 = Iconst_64 0x0
	v20:i32 = Icmp neq, v18, v19
	ExitIfTrue v20, v0, unaligned_atomic
	v21:i64 = UExtend v15, 32->64
	v22:i64 = Iadd v11, v21
	v23:i32 = AtomicRmw add_16, v22, v3
	v24:i32 = Iconst_32 0x10
	v25:i64 = UExtend v24, 32->64
	v26:i64 = Iconst_64 0x3
	v27:i64 = Band v25, v26
	v28:i64 = Iconst_64 0x0
	v29:i32 = Icmp neq, v27, v28
	ExitIfTrue v29, v0, unaligned_atomic
	v30:i64 = UExtend v24, 32->64
	v31:i64 = Iadd v11, v30
	v32:i32 = AtomicRmw add_32, v31, v4
	v33:i32 = Iconst_32 0x18
	v34:i64 = UExtend v33, 32->64
	v35:i64 = UExtend v33, 32->64
	v36:i64 = Iadd v11, v35
	v37:i64 = AtomicRmw add_8, v36, v5
	v38:i32 = Iconst_32 0x20
	v39:i64 = UExtend v38, 32->64
	v40:i64 = Iconst_64 0x1
	v41:i64 = Band v39, v40
	v42:i64 = Iconst_64 0x0
	v43:i32 = Icmp neq, v41, v42
	ExitIfTrue v43, v0, unaligned_atomic
	v44:i64 = UExtend v38, 32->64
	v45:i64 = Iadd v11, v44
	v46:i64 = AtomicRmw add_16, v45, v6
	v47:i32 = Iconst_32 0x28
	v48:i64 = UExtend v47, 32->64
	v49:i64 = Iconst_64 0x3
	v50:i64 = Band v48, v49
	v51:i64 = Iconst_64 0x0
	v52:i32 = Icmp neq, v50, v51
	ExitIfTrue v52, v0, unaligned_atomic
	v53:i64 = UExtend v47, 32->64
	v54:i64 = Iadd v11, v53
	v55:i64 = AtomicRmw add_32, v54, v7
	v56:i32 = Iconst_32 0x30
	v57:i64 = UExtend v56, 32->64
	v58:i64 = Iconst_64 0x7
	v59:i64 = Band v57, v58
	v60:i64 = Iconst_64 0x0
	v61:i32 = Icmp neq, v59, v60
	ExitIfTrue v61, v0, unaligned_atomic
	v62:i64 = UExtend v56, 32->64
	v63:i64 = Iadd v11, v62
	v64:i64 = AtomicRmw add_64, v63, v8
	Jump blk_ret, v14, v23, v32, v37, v46, v55, v64
`,
		},
		{
			name:     "AtomicRMWSub",
			m:        testcases.AtomicRmwSub.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw sub_8, v13, v2
	v15:i32 = Iconst_32 0x8
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x1
	v18:i64 = Band v16, v17
	v19:i64 = Iconst_64 0x0
	v20:i32 = Icmp neq, v18, v19
	ExitIfTrue v20, v0, unaligned_atomic
	v21:i64 = UExtend v15, 32->64
	v22:i64 = Iadd v11, v21
	v23:i32 = AtomicRmw sub_16, v22, v3
	v24:i32 = Iconst_32 0x10
	v25:i64 = UExtend v24, 32->64
	v26:i64 = Iconst_64 0x3
	v27:i64 = Band v25, v26
	v28:i64 = Iconst_64 0x0
	v29:i32 = Icmp neq, v27, v28
	ExitIfTrue v29, v0, unaligned_atomic
	v30:i64 = UExtend v24, 32->64
	v31:i64 = Iadd v11, v30
	v32:i32 = AtomicRmw sub_32, v31, v4
	v33:i32 = Iconst_32 0x18
	v34:i64 = UExtend v33, 32->64
	v35:i64 = UExtend v33, 32->64
	v36:i64 = Iadd v11, v35
	v37:i64 = AtomicRmw sub_8, v36, v5
	v38:i32 = Iconst_32 0x20
	v39:i64 = UExtend v38, 32->64
	v40:i64 = Iconst_64 0x1
	v41:i64 = Band v39, v40
	v42:i64 = Iconst_64 0x0
	v43:i32 = Icmp neq, v41, v42
	ExitIfTrue v43, v0, unaligned_atomic
	v44:i64 = UExtend v38, 32->64
	v45:i64 = Iadd v11, v44
	v46:i64 = AtomicRmw sub_16, v45, v6
	v47:i32 = Iconst_32 0x28
	v48:i64 = UExtend v47, 32->64
	v49:i64 = Iconst_64 0x3
	v50:i64 = Band v48, v49
	v51:i64 = Iconst_64 0x0
	v52:i32 = Icmp neq, v50, v51
	ExitIfTrue v52, v0, unaligned_atomic
	v53:i64 = UExtend v47, 32->64
	v54:i64 = Iadd v11, v53
	v55:i64 = AtomicRmw sub_32, v54, v7
	v56:i32 = Iconst_32 0x30
	v57:i64 = UExtend v56, 32->64
	v58:i64 = Iconst_64 0x7
	v59:i64 = Band v57, v58
	v60:i64 = Iconst_64 0x0
	v61:i32 = Icmp neq, v59, v60
	ExitIfTrue v61, v0, unaligned_atomic
	v62:i64 = UExtend v56, 32->64
	v63:i64 = Iadd v11, v62
	v64:i64 = AtomicRmw sub_64, v63, v8
	Jump blk_ret, v14, v23, v32, v37, v46, v55, v64
`,
		},
		{
			name:     "AtomicRMWAnd",
			m:        testcases.AtomicRmwAnd.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw and_8, v13, v2
	v15:i32 = Iconst_32 0x0
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x8
	v18:i64 = Iadd v16, v17
	v19:i64 = Iconst_64 0x1
	v20:i64 = Band v18, v19
	v21:i64 = Iconst_64 0x0
	v22:i32 = Icmp neq, v20, v21
	ExitIfTrue v22, v0, unaligned_atomic
	v23:i64 = UExtend v15, 32->64
	v24:i64 = Iadd v11, v23
	v25:i64 = Iconst_64 0x8
	v26:i64 = Iadd v24, v25
	v27:i32 = AtomicRmw and_16, v26, v3
	v28:i32 = Iconst_32 0x0
	v29:i64 = UExtend v28, 32->64
	v30:i64 = Iconst_64 0x10
	v31:i64 = Iadd v29, v30
	v32:i64 = Iconst_64 0x3
	v33:i64 = Band v31, v32
	v34:i64 = Iconst_64 0x0
	v35:i32 = Icmp neq, v33, v34
	ExitIfTrue v35, v0, unaligned_atomic
	v36:i64 = UExtend v28, 32->64
	v37:i64 = Iadd v11, v36
	v38:i64 = Iconst_64 0x10
	v39:i64 = Iadd v37, v38
	v40:i32 = AtomicRmw and_32, v39, v4
	v41:i32 = Iconst_32 0x0
	v42:i64 = UExtend v41, 32->64
	v43:i64 = Iconst_64 0x18
	v44:i64 = Iadd v42, v43
	v45:i64 = UExtend v41, 32->64
	v46:i64 = Iadd v11, v45
	v47:i64 = Iconst_64 0x18
	v48:i64 = Iadd v46, v47
	v49:i64 = AtomicRmw and_8, v48, v5
	v50:i32 = Iconst_32 0x0
	v51:i64 = UExtend v50, 32->64
	v52:i64 = Iconst_64 0x20
	v53:i64 = Iadd v51, v52
	v54:i64 = Iconst_64 0x1
	v55:i64 = Band v53, v54
	v56:i64 = Iconst_64 0x0
	v57:i32 = Icmp neq, v55, v56
	ExitIfTrue v57, v0, unaligned_atomic
	v58:i64 = UExtend v50, 32->64
	v59:i64 = Iadd v11, v58
	v60:i64 = Iconst_64 0x20
	v61:i64 = Iadd v59, v60
	v62:i64 = AtomicRmw and_16, v61, v6
	v63:i32 = Iconst_32 0x0
	v64:i64 = UExtend v63, 32->64
	v65:i64 = Iconst_64 0x28
	v66:i64 = Iadd v64, v65
	v67:i64 = Iconst_64 0x3
	v68:i64 = Band v66, v67
	v69:i64 = Iconst_64 0x0
	v70:i32 = Icmp neq, v68, v69
	ExitIfTrue v70, v0, unaligned_atomic
	v71:i64 = UExtend v63, 32->64
	v72:i64 = Iadd v11, v71
	v73:i64 = Iconst_64 0x28
	v74:i64 = Iadd v72, v73
	v75:i64 = AtomicRmw and_32, v74, v7
	v76:i32 = Iconst_32 0x0
	v77:i64 = UExtend v76, 32->64
	v78:i64 = Iconst_64 0x30
	v79:i64 = Iadd v77, v78
	v80:i64 = Iconst_64 0x7
	v81:i64 = Band v79, v80
	v82:i64 = Iconst_64 0x0
	v83:i32 = Icmp neq, v81, v82
	ExitIfTrue v83, v0, unaligned_atomic
	v84:i64 = UExtend v76, 32->64
	v85:i64 = Iadd v11, v84
	v86:i64 = Iconst_64 0x30
	v87:i64 = Iadd v85, v86
	v88:i64 = AtomicRmw and_64, v87, v8
	Jump blk_ret, v14, v27, v40, v49, v62, v75, v88
`,
		},
		{
			name:     "AtomicRMWOr",
			m:        testcases.AtomicRmwOr.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw or_8, v13, v2
	v15:i32 = Iconst_32 0x0
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x8
	v18:i64 = Iadd v16, v17
	v19:i64 = Iconst_64 0x1
	v20:i64 = Band v18, v19
	v21:i64 = Iconst_64 0x0
	v22:i32 = Icmp neq, v20, v21
	ExitIfTrue v22, v0, unaligned_atomic
	v23:i64 = UExtend v15, 32->64
	v24:i64 = Iadd v11, v23
	v25:i64 = Iconst_64 0x8
	v26:i64 = Iadd v24, v25
	v27:i32 = AtomicRmw or_16, v26, v3
	v28:i32 = Iconst_32 0x0
	v29:i64 = UExtend v28, 32->64
	v30:i64 = Iconst_64 0x10
	v31:i64 = Iadd v29, v30
	v32:i64 = Iconst_64 0x3
	v33:i64 = Band v31, v32
	v34:i64 = Iconst_64 0x0
	v35:i32 = Icmp neq, v33, v34
	ExitIfTrue v35, v0, unaligned_atomic
	v36:i64 = UExtend v28, 32->64
	v37:i64 = Iadd v11, v36
	v38:i64 = Iconst_64 0x10
	v39:i64 = Iadd v37, v38
	v40:i32 = AtomicRmw or_32, v39, v4
	v41:i32 = Iconst_32 0x0
	v42:i64 = UExtend v41, 32->64
	v43:i64 = Iconst_64 0x18
	v44:i64 = Iadd v42, v43
	v45:i64 = UExtend v41, 32->64
	v46:i64 = Iadd v11, v45
	v47:i64 = Iconst_64 0x18
	v48:i64 = Iadd v46, v47
	v49:i64 = AtomicRmw or_8, v48, v5
	v50:i32 = Iconst_32 0x0
	v51:i64 = UExtend v50, 32->64
	v52:i64 = Iconst_64 0x20
	v53:i64 = Iadd v51, v52
	v54:i64 = Iconst_64 0x1
	v55:i64 = Band v53, v54
	v56:i64 = Iconst_64 0x0
	v57:i32 = Icmp neq, v55, v56
	ExitIfTrue v57, v0, unaligned_atomic
	v58:i64 = UExtend v50, 32->64
	v59:i64 = Iadd v11, v58
	v60:i64 = Iconst_64 0x20
	v61:i64 = Iadd v59, v60
	v62:i64 = AtomicRmw or_16, v61, v6
	v63:i32 = Iconst_32 0x0
	v64:i64 = UExtend v63, 32->64
	v65:i64 = Iconst_64 0x28
	v66:i64 = Iadd v64, v65
	v67:i64 = Iconst_64 0x3
	v68:i64 = Band v66, v67
	v69:i64 = Iconst_64 0x0
	v70:i32 = Icmp neq, v68, v69
	ExitIfTrue v70, v0, unaligned_atomic
	v71:i64 = UExtend v63, 32->64
	v72:i64 = Iadd v11, v71
	v73:i64 = Iconst_64 0x28
	v74:i64 = Iadd v72, v73
	v75:i64 = AtomicRmw or_32, v74, v7
	v76:i32 = Iconst_32 0x0
	v77:i64 = UExtend v76, 32->64
	v78:i64 = Iconst_64 0x30
	v79:i64 = Iadd v77, v78
	v80:i64 = Iconst_64 0x7
	v81:i64 = Band v79, v80
	v82:i64 = Iconst_64 0x0
	v83:i32 = Icmp neq, v81, v82
	ExitIfTrue v83, v0, unaligned_atomic
	v84:i64 = UExtend v76, 32->64
	v85:i64 = Iadd v11, v84
	v86:i64 = Iconst_64 0x30
	v87:i64 = Iadd v85, v86
	v88:i64 = AtomicRmw or_64, v87, v8
	Jump blk_ret, v14, v27, v40, v49, v62, v75, v88
`,
		},
		{
			name:     "AtomicRMWXor",
			m:        testcases.AtomicRmwXor.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw xor_8, v13, v2
	v15:i32 = Iconst_32 0x0
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x8
	v18:i64 = Iadd v16, v17
	v19:i64 = Iconst_64 0x1
	v20:i64 = Band v18, v19
	v21:i64 = Iconst_64 0x0
	v22:i32 = Icmp neq, v20, v21
	ExitIfTrue v22, v0, unaligned_atomic
	v23:i64 = UExtend v15, 32->64
	v24:i64 = Iadd v11, v23
	v25:i64 = Iconst_64 0x8
	v26:i64 = Iadd v24, v25
	v27:i32 = AtomicRmw xor_16, v26, v3
	v28:i32 = Iconst_32 0x0
	v29:i64 = UExtend v28, 32->64
	v30:i64 = Iconst_64 0x10
	v31:i64 = Iadd v29, v30
	v32:i64 = Iconst_64 0x3
	v33:i64 = Band v31, v32
	v34:i64 = Iconst_64 0x0
	v35:i32 = Icmp neq, v33, v34
	ExitIfTrue v35, v0, unaligned_atomic
	v36:i64 = UExtend v28, 32->64
	v37:i64 = Iadd v11, v36
	v38:i64 = Iconst_64 0x10
	v39:i64 = Iadd v37, v38
	v40:i32 = AtomicRmw xor_32, v39, v4
	v41:i32 = Iconst_32 0x0
	v42:i64 = UExtend v41, 32->64
	v43:i64 = Iconst_64 0x18
	v44:i64 = Iadd v42, v43
	v45:i64 = UExtend v41, 32->64
	v46:i64 = Iadd v11, v45
	v47:i64 = Iconst_64 0x18
	v48:i64 = Iadd v46, v47
	v49:i64 = AtomicRmw xor_8, v48, v5
	v50:i32 = Iconst_32 0x0
	v51:i64 = UExtend v50, 32->64
	v52:i64 = Iconst_64 0x20
	v53:i64 = Iadd v51, v52
	v54:i64 = Iconst_64 0x1
	v55:i64 = Band v53, v54
	v56:i64 = Iconst_64 0x0
	v57:i32 = Icmp neq, v55, v56
	ExitIfTrue v57, v0, unaligned_atomic
	v58:i64 = UExtend v50, 32->64
	v59:i64 = Iadd v11, v58
	v60:i64 = Iconst_64 0x20
	v61:i64 = Iadd v59, v60
	v62:i64 = AtomicRmw xor_16, v61, v6
	v63:i32 = Iconst_32 0x0
	v64:i64 = UExtend v63, 32->64
	v65:i64 = Iconst_64 0x28
	v66:i64 = Iadd v64, v65
	v67:i64 = Iconst_64 0x3
	v68:i64 = Band v66, v67
	v69:i64 = Iconst_64 0x0
	v70:i32 = Icmp neq, v68, v69
	ExitIfTrue v70, v0, unaligned_atomic
	v71:i64 = UExtend v63, 32->64
	v72:i64 = Iadd v11, v71
	v73:i64 = Iconst_64 0x28
	v74:i64 = Iadd v72, v73
	v75:i64 = AtomicRmw xor_32, v74, v7
	v76:i32 = Iconst_32 0x0
	v77:i64 = UExtend v76, 32->64
	v78:i64 = Iconst_64 0x30
	v79:i64 = Iadd v77, v78
	v80:i64 = Iconst_64 0x7
	v81:i64 = Band v79, v80
	v82:i64 = Iconst_64 0x0
	v83:i32 = Icmp neq, v81, v82
	ExitIfTrue v83, v0, unaligned_atomic
	v84:i64 = UExtend v76, 32->64
	v85:i64 = Iadd v11, v84
	v86:i64 = Iconst_64 0x30
	v87:i64 = Iadd v85, v86
	v88:i64 = AtomicRmw xor_64, v87, v8
	Jump blk_ret, v14, v27, v40, v49, v62, v75, v88
`,
		},
		{
			name:     "AtomicRMWXchg",
			m:        testcases.AtomicRmwXchg.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	v14:i32 = AtomicRmw xchg_8, v13, v2
	v15:i32 = Iconst_32 0x0
	v16:i64 = UExtend v15, 32->64
	v17:i64 = Iconst_64 0x8
	v18:i64 = Iadd v16, v17
	v19:i64 = Iconst_64 0x1
	v20:i64 = Band v18, v19
	v21:i64 = Iconst_64 0x0
	v22:i32 = Icmp neq, v20, v21
	ExitIfTrue v22, v0, unaligned_atomic
	v23:i64 = UExtend v15, 32->64
	v24:i64 = Iadd v11, v23
	v25:i64 = Iconst_64 0x8
	v26:i64 = Iadd v24, v25
	v27:i32 = AtomicRmw xchg_16, v26, v3
	v28:i32 = Iconst_32 0x0
	v29:i64 = UExtend v28, 32->64
	v30:i64 = Iconst_64 0x10
	v31:i64 = Iadd v29, v30
	v32:i64 = Iconst_64 0x3
	v33:i64 = Band v31, v32
	v34:i64 = Iconst_64 0x0
	v35:i32 = Icmp neq, v33, v34
	ExitIfTrue v35, v0, unaligned_atomic
	v36:i64 = UExtend v28, 32->64
	v37:i64 = Iadd v11, v36
	v38:i64 = Iconst_64 0x10
	v39:i64 = Iadd v37, v38
	v40:i32 = AtomicRmw xchg_32, v39, v4
	v41:i32 = Iconst_32 0x0
	v42:i64 = UExtend v41, 32->64
	v43:i64 = Iconst_64 0x18
	v44:i64 = Iadd v42, v43
	v45:i64 = UExtend v41, 32->64
	v46:i64 = Iadd v11, v45
	v47:i64 = Iconst_64 0x18
	v48:i64 = Iadd v46, v47
	v49:i64 = AtomicRmw xchg_8, v48, v5
	v50:i32 = Iconst_32 0x0
	v51:i64 = UExtend v50, 32->64
	v52:i64 = Iconst_64 0x20
	v53:i64 = Iadd v51, v52
	v54:i64 = Iconst_64 0x1
	v55:i64 = Band v53, v54
	v56:i64 = Iconst_64 0x0
	v57:i32 = Icmp neq, v55, v56
	ExitIfTrue v57, v0, unaligned_atomic
	v58:i64 = UExtend v50, 32->64
	v59:i64 = Iadd v11, v58
	v60:i64 = Iconst_64 0x20
	v61:i64 = Iadd v59, v60
	v62:i64 = AtomicRmw xchg_16, v61, v6
	v63:i32 = Iconst_32 0x0
	v64:i64 = UExtend v63, 32->64
	v65:i64 = Iconst_64 0x28
	v66:i64 = Iadd v64, v65
	v67:i64 = Iconst_64 0x3
	v68:i64 = Band v66, v67
	v69:i64 = Iconst_64 0x0
	v70:i32 = Icmp neq, v68, v69
	ExitIfTrue v70, v0, unaligned_atomic
	v71:i64 = UExtend v63, 32->64
	v72:i64 = Iadd v11, v71
	v73:i64 = Iconst_64 0x28
	v74:i64 = Iadd v72, v73
	v75:i64 = AtomicRmw xchg_32, v74, v7
	v76:i32 = Iconst_32 0x0
	v77:i64 = UExtend v76, 32->64
	v78:i64 = Iconst_64 0x30
	v79:i64 = Iadd v77, v78
	v80:i64 = Iconst_64 0x7
	v81:i64 = Band v79, v80
	v82:i64 = Iconst_64 0x0
	v83:i32 = Icmp neq, v81, v82
	ExitIfTrue v83, v0, unaligned_atomic
	v84:i64 = UExtend v76, 32->64
	v85:i64 = Iadd v11, v84
	v86:i64 = Iconst_64 0x30
	v87:i64 = Iadd v85, v86
	v88:i64 = AtomicRmw xchg_64, v87, v8
	Jump blk_ret, v14, v27, v40, v49, v62, v75, v88
`,
		},
		{
			name:     "AtomicStoreLoad",
			m:        testcases.AtomicStoreLoad.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i64, v6:i64, v7:i64, v8:i64)
	v9:i32 = Iconst_32 0x0
	v10:i64 = UExtend v9, 32->64
	v11:i64 = Load v1, 0x8
	v12:i64 = UExtend v9, 32->64
	v13:i64 = Iadd v11, v12
	AtomicStore_8, v13, v2
	v14:i32 = Iconst_32 0x0
	v15:i64 = UExtend v14, 32->64
	v16:i64 = UExtend v14, 32->64
	v17:i64 = Iadd v11, v16
	v18:i32 = AtomicLoad_8, v17
	v19:i32 = Iconst_32 0x0
	v20:i64 = UExtend v19, 32->64
	v21:i64 = Iconst_64 0x8
	v22:i64 = Iadd v20, v21
	v23:i64 = Iconst_64 0x1
	v24:i64 = Band v22, v23
	v25:i64 = Iconst_64 0x0
	v26:i32 = Icmp neq, v24, v25
	ExitIfTrue v26, v0, unaligned_atomic
	v27:i64 = UExtend v19, 32->64
	v28:i64 = Iadd v11, v27
	v29:i64 = Iconst_64 0x8
	v30:i64 = Iadd v28, v29
	AtomicStore_16, v30, v3
	v31:i32 = Iconst_32 0x0
	v32:i64 = UExtend v31, 32->64
	v33:i64 = Iconst_64 0x8
	v34:i64 = Iadd v32, v33
	v35:i64 = Iconst_64 0x1
	v36:i64 = Band v34, v35
	v37:i64 = Iconst_64 0x0
	v38:i32 = Icmp neq, v36, v37
	ExitIfTrue v38, v0, unaligned_atomic
	v39:i64 = UExtend v31, 32->64
	v40:i64 = Iadd v11, v39
	v41:i64 = Iconst_64 0x8
	v42:i64 = Iadd v40, v41
	v43:i32 = AtomicLoad_16, v42
	v44:i32 = Iconst_32 0x0
	v45:i64 = UExtend v44, 32->64
	v46:i64 = Iconst_64 0x10
	v47:i64 = Iadd v45, v46
	v48:i64 = Iconst_64 0x3
	v49:i64 = Band v47, v48
	v50:i64 = Iconst_64 0x0
	v51:i32 = Icmp neq, v49, v50
	ExitIfTrue v51, v0, unaligned_atomic
	v52:i64 = UExtend v44, 32->64
	v53:i64 = Iadd v11, v52
	v54:i64 = Iconst_64 0x10
	v55:i64 = Iadd v53, v54
	AtomicStore_32, v55, v4
	v56:i32 = Iconst_32 0x0
	v57:i64 = UExtend v56, 32->64
	v58:i64 = Iconst_64 0x10
	v59:i64 = Iadd v57, v58
	v60:i64 = Iconst_64 0x3
	v61:i64 = Band v59, v60
	v62:i64 = Iconst_64 0x0
	v63:i32 = Icmp neq, v61, v62
	ExitIfTrue v63, v0, unaligned_atomic
	v64:i64 = UExtend v56, 32->64
	v65:i64 = Iadd v11, v64
	v66:i64 = Iconst_64 0x10
	v67:i64 = Iadd v65, v66
	v68:i32 = AtomicLoad_32, v67
	v69:i32 = Iconst_32 0x0
	v70:i64 = UExtend v69, 32->64
	v71:i64 = Iconst_64 0x18
	v72:i64 = Iadd v70, v71
	v73:i64 = UExtend v69, 32->64
	v74:i64 = Iadd v11, v73
	v75:i64 = Iconst_64 0x18
	v76:i64 = Iadd v74, v75
	AtomicStore_8, v76, v5
	v77:i32 = Iconst_32 0x0
	v78:i64 = UExtend v77, 32->64
	v79:i64 = Iconst_64 0x18
	v80:i64 = Iadd v78, v79
	v81:i64 = UExtend v77, 32->64
	v82:i64 = Iadd v11, v81
	v83:i64 = Iconst_64 0x18
	v84:i64 = Iadd v82, v83
	v85:i64 = AtomicLoad_8, v84
	v86:i32 = Iconst_32 0x0
	v87:i64 = UExtend v86, 32->64
	v88:i64 = Iconst_64 0x20
	v89:i64 = Iadd v87, v88
	v90:i64 = Iconst_64 0x1
	v91:i64 = Band v89, v90
	v92:i64 = Iconst_64 0x0
	v93:i32 = Icmp neq, v91, v92
	ExitIfTrue v93, v0, unaligned_atomic
	v94:i64 = UExtend v86, 32->64
	v95:i64 = Iadd v11, v94
	v96:i64 = Iconst_64 0x20
	v97:i64 = Iadd v95, v96
	AtomicStore_16, v97, v6
	v98:i32 = Iconst_32 0x0
	v99:i64 = UExtend v98, 32->64
	v100:i64 = Iconst_64 0x20
	v101:i64 = Iadd v99, v100
	v102:i64 = Iconst_64 0x1
	v103:i64 = Band v101, v102
	v104:i64 = Iconst_64 0x0
	v105:i32 = Icmp neq, v103, v104
	ExitIfTrue v105, v0, unaligned_atomic
	v106:i64 = UExtend v98, 32->64
	v107:i64 = Iadd v11, v106
	v108:i64 = Iconst_64 0x20
	v109:i64 = Iadd v107, v108
	v110:i64 = AtomicLoad_16, v109
	v111:i32 = Iconst_32 0x0
	v112:i64 = UExtend v111, 32->64
	v113:i64 = Iconst_64 0x28
	v114:i64 = Iadd v112, v113
	v115:i64 = Iconst_64 0x3
	v116:i64 = Band v114, v115
	v117:i64 = Iconst_64 0x0
	v118:i32 = Icmp neq, v116, v117
	ExitIfTrue v118, v0, unaligned_atomic
	v119:i64 = UExtend v111, 32->64
	v120:i64 = Iadd v11, v119
	v121:i64 = Iconst_64 0x28
	v122:i64 = Iadd v120, v121
	AtomicStore_32, v122, v7
	v123:i32 = Iconst_32 0x0
	v124:i64 = UExtend v123, 32->64
	v125:i64 = Iconst_64 0x28
	v126:i64 = Iadd v124, v125
	v127:i64 = Iconst_64 0x3
	v128:i64 = Band v126, v127
	v129:i64 = Iconst_64 0x0
	v130:i32 = Icmp neq, v128, v129
	ExitIfTrue v130, v0, unaligned_atomic
	v131:i64 = UExtend v123, 32->64
	v132:i64 = Iadd v11, v131
	v133:i64 = Iconst_64 0x28
	v134:i64 = Iadd v132, v133
	v135:i64 = AtomicLoad_32, v134
	v136:i32 = Iconst_32 0x0
	v137:i64 = UExtend v136, 32->64
	v138:i64 = Iconst_64 0x30
	v139:i64 = Iadd v137, v138
	v140:i64 = Iconst_64 0x7
	v141:i64 = Band v139, v140
	v142:i64 = Iconst_64 0x0
	v143:i32 = Icmp neq, v141, v142
	ExitIfTrue v143, v0, unaligned_atomic
	v144:i64 = UExtend v136, 32->64
	v145:i64 = Iadd v11, v144
	v146:i64 = Iconst_64 0x30
	v147:i64 = Iadd v145, v146
	AtomicStore_64, v147, v8
	v148:i32 = Iconst_32 0x0
	v149:i64 = UExtend v148, 32->64
	v150:i64 = Iconst_64 0x30
	v151:i64 = Iadd v149, v150
	v152:i64 = Iconst_64 0x7
	v153:i64 = Band v151, v152
	v154:i64 = Iconst_64 0x0
	v155:i32 = Icmp neq, v153, v154
	ExitIfTrue v155, v0, unaligned_atomic
	v156:i64 = UExtend v148, 32->64
	v157:i64 = Iadd v11, v156
	v158:i64 = Iconst_64 0x30
	v159:i64 = Iadd v157, v158
	v160:i64 = AtomicLoad_64, v159
	Jump blk_ret, v18, v43, v68, v85, v110, v135, v160
`,
		},
		{
			name:     "AtomicCas",
			m:        testcases.AtomicCas.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32, v3:i32, v4:i32, v5:i32, v6:i32, v7:i32, v8:i64, v9:i64, v10:i64, v11:i64, v12:i64, v13:i64, v14:i64, v15:i64)
	v16:i32 = Iconst_32 0x0
	v17:i64 = UExtend v16, 32->64
	v18:i64 = Load v1, 0x8
	v19:i64 = UExtend v16, 32->64
	v20:i64 = Iadd v18, v19
	v21:i32 = AtomicCas_8, v20, v2, v3
	v22:i32 = Iconst_32 0x0
	v23:i64 = UExtend v22, 32->64
	v24:i64 = Iconst_64 0x8
	v25:i64 = Iadd v23, v24
	v26:i64 = Iconst_64 0x1
	v27:i64 = Band v25, v26
	v28:i64 = Iconst_64 0x0
	v29:i32 = Icmp neq, v27, v28
	ExitIfTrue v29, v0, unaligned_atomic
	v30:i64 = UExtend v22, 32->64
	v31:i64 = Iadd v18, v30
	v32:i64 = Iconst_64 0x8
	v33:i64 = Iadd v31, v32
	v34:i32 = AtomicCas_16, v33, v4, v5
	v35:i32 = Iconst_32 0x0
	v36:i64 = UExtend v35, 32->64
	v37:i64 = Iconst_64 0x10
	v38:i64 = Iadd v36, v37
	v39:i64 = Iconst_64 0x3
	v40:i64 = Band v38, v39
	v41:i64 = Iconst_64 0x0
	v42:i32 = Icmp neq, v40, v41
	ExitIfTrue v42, v0, unaligned_atomic
	v43:i64 = UExtend v35, 32->64
	v44:i64 = Iadd v18, v43
	v45:i64 = Iconst_64 0x10
	v46:i64 = Iadd v44, v45
	v47:i32 = AtomicCas_32, v46, v6, v7
	v48:i32 = Iconst_32 0x0
	v49:i64 = UExtend v48, 32->64
	v50:i64 = Iconst_64 0x18
	v51:i64 = Iadd v49, v50
	v52:i64 = UExtend v48, 32->64
	v53:i64 = Iadd v18, v52
	v54:i64 = Iconst_64 0x18
	v55:i64 = Iadd v53, v54
	v56:i64 = AtomicCas_8, v55, v8, v9
	v57:i32 = Iconst_32 0x0
	v58:i64 = UExtend v57, 32->64
	v59:i64 = Iconst_64 0x20
	v60:i64 = Iadd v58, v59
	v61:i64 = Iconst_64 0x1
	v62:i64 = Band v60, v61
	v63:i64 = Iconst_64 0x0
	v64:i32 = Icmp neq, v62, v63
	ExitIfTrue v64, v0, unaligned_atomic
	v65:i64 = UExtend v57, 32->64
	v66:i64 = Iadd v18, v65
	v67:i64 = Iconst_64 0x20
	v68:i64 = Iadd v66, v67
	v69:i64 = AtomicCas_16, v68, v10, v11
	v70:i32 = Iconst_32 0x0
	v71:i64 = UExtend v70, 32->64
	v72:i64 = Iconst_64 0x28
	v73:i64 = Iadd v71, v72
	v74:i64 = Iconst_64 0x3
	v75:i64 = Band v73, v74
	v76:i64 = Iconst_64 0x0
	v77:i32 = Icmp neq, v75, v76
	ExitIfTrue v77, v0, unaligned_atomic
	v78:i64 = UExtend v70, 32->64
	v79:i64 = Iadd v18, v78
	v80:i64 = Iconst_64 0x28
	v81:i64 = Iadd v79, v80
	v82:i64 = AtomicCas_32, v81, v12, v13
	v83:i32 = Iconst_32 0x0
	v84:i64 = UExtend v83, 32->64
	v85:i64 = Iconst_64 0x30
	v86:i64 = Iadd v84, v85
	v87:i64 = Iconst_64 0x7
	v88:i64 = Band v86, v87
	v89:i64 = Iconst_64 0x0
	v90:i32 = Icmp neq, v88, v89
	ExitIfTrue v90, v0, unaligned_atomic
	v91:i64 = UExtend v83, 32->64
	v92:i64 = Iadd v18, v91
	v93:i64 = Iconst_64 0x30
	v94:i64 = Iadd v92, v93
	v95:i64 = AtomicCas_64, v94, v14, v15
	Jump blk_ret, v21, v34, v47, v56, v69, v82, v95
`,
		},
		{
			name:     "AtomicFence",
			m:        testcases.AtomicFence.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64)
	Fence 0
	Jump blk_ret
`,
		},
		{
			name:     "AtomicFenceNoMemory",
			m:        testcases.AtomicFenceNoMemory.Module,
			features: api.CoreFeaturesV2 | experimental.CoreFeaturesThreads,
			exp: `
blk0: (v0:i64, v1:i64)
	Jump blk_ret
`,
		},
		{
			name: "bounds check in if-else",
			m: &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}}},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{{
					Body: []byte{
						wasm.OpcodeLocalGet, 0,
						wasm.OpcodeI32Load, 0x2, 0x20, // alignment=2 (natural alignment) staticOffset=0x20
						wasm.OpcodeDrop,

						wasm.OpcodeLocalGet, 0,
						wasm.OpcodeIf, 0x40, // blockSignature:vv.
						/* */ wasm.OpcodeLocalGet, 0,
						/* */ // This bound check should be removed since it's known to be in bounds.
						/* */ wasm.OpcodeI32Load, 0x2, 0x10, // alignment=2 (natural alignment) staticOffset=0x20
						/* */ wasm.OpcodeDrop,
						wasm.OpcodeElse,
						/* */ wasm.OpcodeLocalGet, 0,
						/* */ // This bound check should be removed since it's known to be in bounds.
						/* */ wasm.OpcodeI32Load, 0x2, 0x10, // alignment=2 (natural alignment) staticOffset=0x20
						/* */ wasm.OpcodeDrop,
						/* */ // This shouldn't be removed since it's not known to be in bounds.
						/* */ wasm.OpcodeLocalGet, 0,
						/* */ wasm.OpcodeI32Load, 0x2, 0x30, // alignment=2 (natural alignment) staticOffset=0x20
						/* */ wasm.OpcodeDrop,
						/* */ // But this is known to be in bounds.
						/* */ wasm.OpcodeLocalGet, 0,
						/* */ wasm.OpcodeI32Load, 0x2, 0x25, // alignment=2 (natural alignment) staticOffset=0x20
						/* */ wasm.OpcodeDrop,
						wasm.OpcodeEnd,

						// At this point, the known bound 0x20 should be retained.
						wasm.OpcodeLocalGet, 0,
						wasm.OpcodeI32Load, 0x2, 0x15, // alignment=2 (natural alignment) staticOffset=0x20
						wasm.OpcodeDrop,

						wasm.OpcodeEnd,
					},
				}},
				MemorySection: []wasm.Memory{{Min: 1}},
			},
			features: api.CoreFeaturesV2,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i64 = Iconst_64 0x24
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Load v1, 0x10
	v6:i64 = Iadd v4, v3
	v7:i32 = Icmp lt_u, v5, v6
	ExitIfTrue v7, v0, memory_out_of_bounds
	v8:i64 = Load v1, 0x8
	v9:i64 = Iadd v8, v4
	v10:i32 = Load v9, 0x20
	Brz v2, blk2
	Jump blk1

blk1: () <-- (blk0)
	v11:i32 = Load v9, 0x10
	Jump blk3

blk2: () <-- (blk0)
	v12:i32 = Load v9, 0x10
	v13:i64 = Iconst_64 0x34
	v14:i64 = UExtend v2, 32->64
	v15:i64 = Iadd v14, v13
	v16:i32 = Icmp lt_u, v5, v15
	ExitIfTrue v16, v0, memory_out_of_bounds
	v17:i32 = Load v9, 0x30
	v18:i32 = Load v9, 0x25
	Jump blk3

blk3: () <-- (blk1,blk2)
	v20:i64 = Load v1, 0x8
	v21:i64 = UExtend v2, 32->64
	v22:i64 = Iadd v20, v21
	v23:i32 = Load v22, 0x15
	Jump blk_ret
`,
		},
		{
			name: "bounds check in loop with if-else",
			m: &wasm.Module{
				TypeSection:     []wasm.FunctionType{{Params: []wasm.ValueType{wasm.ValueTypeI32}}},
				FunctionSection: []wasm.Index{0},
				CodeSection: []wasm.Code{{
					Body: []byte{
						wasm.OpcodeLocalGet, 0,
						wasm.OpcodeI32Load, 0x2, 0x10, // staticOffset=0x20
						wasm.OpcodeDrop,

						wasm.OpcodeLoop, 0x40, // blockSignature:vv.

						/* */ wasm.OpcodeLocalGet, 0,
						// This address must be always recomputed:
						// v9 in `Load v9, 0x20` won't propagate from blk0 to blk1,
						/* */ wasm.OpcodeI32Load, 0x2, 0x20, // staticOffset=0x30

						/* */ wasm.OpcodeIf, 0x40, // blockSignature:vv.
						/* */ /* */ wasm.OpcodeLoop, 0x40, // blockSignature:vv.
						/* */ /* */ /* */ wasm.OpcodeLocalGet, 0,
						/* */ /* */ /* */ wasm.OpcodeI32Load, 0x2, 0x50, // staticOffset=0x50
						/* */ /* */ /* */ wasm.OpcodeBrIf, 2, // quit to the inner loop
						/* */ /* */ wasm.OpcodeEnd,
						/* */ wasm.OpcodeElse,
						/* */ /* */ wasm.OpcodeLocalGet, 0,
						/* */ /* */ wasm.OpcodeBrIf, 2, // quit to the outer loop
						/* */ wasm.OpcodeEnd,
						/* */ wasm.OpcodeBr, 1, // quit to the loop

						wasm.OpcodeEnd, // end loop

						wasm.OpcodeEnd,
					},
				}},
				MemorySection: []wasm.Memory{{Min: 1}},
			},
			features: api.CoreFeaturesV2,
			exp: `
blk0: (v0:i64, v1:i64, v2:i32)
	v3:i64 = Iconst_64 0x14
	v4:i64 = UExtend v2, 32->64
	v5:i64 = Load v1, 0x10
	v6:i64 = Iadd v4, v3
	v7:i32 = Icmp lt_u, v5, v6
	ExitIfTrue v7, v0, memory_out_of_bounds
	v8:i64 = Load v1, 0x8
	v9:i64 = Iadd v8, v4
	v10:i32 = Load v9, 0x10
	Jump blk1, v2

blk1: (v12:i32) <-- (blk0,blk11)
	v11:i32 = PreemptRequested
	Brnz v11, blk3
	Jump blk4

blk2: ()

blk3: () <-- (blk1)
	Yield v0
	Jump blk4

blk4: () <-- (blk1,blk3)
	v13:i64 = Iconst_64 0x24
	v14:i64 = UExtend v12, 32->64
	v15:i64 = Load v1, 0x10
	v16:i64 = Iadd v14, v13
	v17:i32 = Icmp lt_u, v15, v16
	ExitIfTrue v17, v0, memory_out_of_bounds
	v18:i64 = Load v1, 0x8
	v19:i64 = Iadd v18, v14
	v20:i32 = Load v19, 0x20
	Brz v20, blk6
	Jump blk5

blk5: () <-- (blk4)
	Jump blk8, v12

blk6: () <-- (blk4)
	Brnz v12, blk_ret
	Jump blk13

blk7: () <-- (blk12,blk13)
	Jump blk_ret

blk8: (v22:i32) <-- (blk5)
	v21:i32 = PreemptRequested elided
	Brnz v21, blk10
	Jump blk11

blk9: ()

blk10: () <-- (blk8)
	Yield v0
	Jump blk11

blk11: () <-- (blk8,blk10)
	v23:i64 = Iconst_64 0x54
	v24:i64 = UExtend v22, 32->64
	v25:i64 = Load v1, 0x10
	v26:i64 = Iadd v24, v23
	v27:i32 = Icmp lt_u, v25, v26
	ExitIfTrue v27, v0, memory_out_of_bounds
	v28:i64 = Load v1, 0x8
	v29:i64 = Iadd v28, v24
	v30:i32 = Load v29, 0x50
	Brnz v30, blk1, v22
	Jump blk12

blk12: () <-- (blk11)
	Jump blk7

blk13: () <-- (blk6)
	Jump blk7
`,
		},
	} {

		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Just in case let's check the test module is valid.
			features := tc.features
			if features == 0 {
				features = api.CoreFeaturesV2
			}
			err := tc.m.Validate(features)
			require.NoError(t, err, "invalid test case module!")

			b := ssa.NewBuilder()

			offset := nativeapi.NewModuleContextOffsetData(tc.m, tc.needListener)
			fc := NewFrontendCompiler(tc.m, b, &offset, tc.needListener, false)
			fc.SetInlineLeaves(tc.inline)
			fc.SetVecMap(false)
			typeIndex := tc.m.FunctionSection[tc.targetIndex]
			code := &tc.m.CodeSection[tc.targetIndex]
			fc.Init(tc.targetIndex, typeIndex, &tc.m.TypeSection[typeIndex], code.LocalTypes, code.Body, tc.needListener, 0)

			fc.LowerToSSA()

			actual := fc.formatBuilder()
			require.Equal(t, tc.exp, actual)

			b.RunPasses()
			if expAfterOpt := tc.expAfterPasses; expAfterOpt != "" {
				actualAfterOpt := fc.formatBuilder()
				require.Equal(t, expAfterOpt, actualAfterOpt)
			}
		})
	}
}

func TestSignatureForListener(t *testing.T) {
	for _, tc := range []struct {
		name          string
		sig           *wasm.FunctionType
		before, after *ssa.Signature
	}{
		{
			name:   "empty",
			sig:    &wasm.FunctionType{},
			before: &ssa.Signature{Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			after:  &ssa.Signature{Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
		},
		{
			name: "multi",
			sig: &wasm.FunctionType{
				Params: []wasm.ValueType{wasm.ValueTypeF64, wasm.ValueTypeI32},
				Results: []wasm.ValueType{
					wasm.ValueTypeI64, wasm.ValueTypeI32, wasm.ValueTypeF32, wasm.ValueTypeF64, wasm.ValueTypeV128,
				},
			},
			before: &ssa.Signature{
				Params: []ssa.Type{
					ssa.TypeI64, ssa.TypeI32,
					ssa.TypeF64, ssa.TypeI32,
				},
			},
			after: &ssa.Signature{
				Params: []ssa.Type{
					ssa.TypeI64, ssa.TypeI32,
					ssa.TypeI64, ssa.TypeI32, ssa.TypeF32, ssa.TypeF64, ssa.TypeV128,
				},
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			before, after := SignatureForListener(tc.sig)
			require.Equal(t, tc.before, before)
			require.Equal(t, tc.after, after)
		})
	}
}

func TestCompiler_declareSignatures(t *testing.T) {
	m := &wasm.Module{
		TypeSection: []wasm.FunctionType{
			{},
			{Params: []wasm.ValueType{wasm.ValueTypeI64, wasm.ValueTypeI32}},
			{Params: []wasm.ValueType{wasm.ValueTypeF64, wasm.ValueTypeI32}},
			{Results: []wasm.ValueType{wasm.ValueTypeI64, wasm.ValueTypeI32}},
		},
	}

	t.Run("listener=false", func(t *testing.T) {
		builder := ssa.NewBuilder()
		c := &Compiler{m: m, ssaBuilder: builder}
		c.declareSignatures(false)

		declaredSigs := builder.Signatures()
		expected := []*ssa.Signature{
			{ID: 0, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
			{ID: 1, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI32}},
			{ID: 2, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeF64, ssa.TypeI32}},
			{ID: 3, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			{ID: 4, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 5, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 6, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 7, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeI64}},
			{ID: 8, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			{ID: 9, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			{ID: 10, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			// EH signatures.
			{ID: 11, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 12, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
			// GC runtime. The safepoint shares it rather than declaring one of its own.
			{ID: 13, Params: []ssa.Type{
				ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64,
			}, Results: []ssa.Type{ssa.TypeI64}},
			// memclr signature.
			{ID: 14, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
		}

		require.Equal(t, len(expected), len(declaredSigs))
		for i := 0; i < len(expected); i++ {
			require.Equal(t, expected[i].String(), declaredSigs[i].String(), i)
		}
	})

	t.Run("listener=false", func(t *testing.T) {
		builder := ssa.NewBuilder()
		c := &Compiler{m: m, ssaBuilder: builder}
		c.declareSignatures(true)

		declaredSigs := builder.Signatures()

		expected := []*ssa.Signature{
			{ID: 0, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
			{ID: 1, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI32}},
			{ID: 2, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeF64, ssa.TypeI32}},
			{ID: 3, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			// Before.
			{ID: 4, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			{ID: 5, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI32}},
			{ID: 6, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeF64, ssa.TypeI32}},
			{ID: 7, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			// After.
			{ID: 8, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			{ID: 9, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			{ID: 10, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}},
			{ID: 11, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI32}},
			// Misc.
			{ID: 12, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 13, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 14, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 15, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64, ssa.TypeI64}},
			{ID: 16, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			{ID: 17, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			{ID: 18, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI32, ssa.TypeI32, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI32}},
			// EH signatures.
			{ID: 19, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}, Results: []ssa.Type{ssa.TypeI64}},
			{ID: 20, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
			// GC runtime. The safepoint shares it rather than declaring one of its own.
			{ID: 21, Params: []ssa.Type{
				ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64, ssa.TypeI64,
			}, Results: []ssa.Type{ssa.TypeI64}},
			// memclr signature.
			{ID: 22, Params: []ssa.Type{ssa.TypeI64, ssa.TypeI64}},
		}
		require.Equal(t, len(expected), len(declaredSigs))
		for i := 0; i < len(declaredSigs); i++ {
			require.Equal(t, expected[i].String(), declaredSigs[i].String(), i)
		}
	})
}

func TestCompiler_recordKnownSafeBound(t *testing.T) {
	c := &Compiler{}
	c.recordKnownSafeBound(1, 99, 9999)
	require.Equal(t, 1, len(c.knownSafeBoundsSet))
	require.True(t, c.getKnownSafeBound(1).valid())
	require.Equal(t, uint64(99), c.getKnownSafeBound(1).bound)
	require.Equal(t, ssa.Value(9999), c.getKnownSafeBound(1).absoluteAddr)

	c.recordKnownSafeBound(1, 150, 9999)
	require.Equal(t, 1, len(c.knownSafeBoundsSet))
	require.Equal(t, uint64(150), c.getKnownSafeBound(1).bound)

	c.recordKnownSafeBound(5, 666, 54321)
	require.Equal(t, 2, len(c.knownSafeBoundsSet))
	require.Equal(t, uint64(666), c.getKnownSafeBound(5).bound)
	require.Equal(t, ssa.Value(54321), c.getKnownSafeBound(5).absoluteAddr)
}

func TestCompiler_getKnownSafeBound(t *testing.T) {
	c := &Compiler{
		knownSafeBounds: []knownSafeBound{
			{}, {bound: 2134},
		},
	}
	require.Nil(t, c.getKnownSafeBound(5))
	require.Nil(t, c.getKnownSafeBound(12345))
	require.False(t, c.getKnownSafeBound(0).valid())
	require.True(t, c.getKnownSafeBound(1).valid())
}

func TestCompiler_clearSafeBounds(t *testing.T) {
	c := &Compiler{}
	c.knownSafeBounds = []knownSafeBound{{bound: 1}, {}, {bound: 2}, {}, {}, {bound: 3}}
	c.knownSafeBoundsSet = []ssa.ValueID{0, 2, 5}
	c.clearSafeBounds()
	require.Equal(t, 0, len(c.knownSafeBoundsSet))
	require.Equal(t, []knownSafeBound{{absoluteAddr: ssa.ValueInvalid}, {}, {absoluteAddr: ssa.ValueInvalid}, {}, {}, {absoluteAddr: ssa.ValueInvalid}}, c.knownSafeBounds)
}

func TestCompiler_resetAbsoluteAddressInSafeBounds(t *testing.T) {
	c := &Compiler{}
	c.knownSafeBounds = []knownSafeBound{
		{bound: 1, absoluteAddr: ssa.Value(1)},
		{},
		{bound: 2, absoluteAddr: ssa.Value(2)},
		{},
		{},
		{bound: 3, absoluteAddr: ssa.Value(3)},
	}
	c.knownSafeBoundsSet = []ssa.ValueID{0, 2, 5}
	c.resetAbsoluteAddressInSafeBounds()
	require.Equal(t, 3, len(c.knownSafeBoundsSet))
	require.Equal(t, []knownSafeBound{
		{bound: 1, absoluteAddr: ssa.ValueInvalid},
		{},
		{bound: 2, absoluteAddr: ssa.ValueInvalid},
		{},
		{},
		{bound: 3, absoluteAddr: ssa.ValueInvalid},
	}, c.knownSafeBounds)
}

func TestKnownSafeBound_valid(t *testing.T) {
	k := &knownSafeBound{bound: 10, absoluteAddr: 12345}
	require.True(t, k.valid())
	k.bound = 0
	require.False(t, k.valid())
}

func TestCompiler_finalizeKnownSafeBoundsAtTheEndOoBlock(t *testing.T) {
	c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
	blk := c.ssaBuilder.AllocateBasicBlock()
	require.True(t, len(c.getKnownSafeBoundsAtTheEndOfBlocks(blk.ID()).View()) == 0)
	c.ssaBuilder.SetCurrentBlock(blk)
	c.knownSafeBoundsSet = []ssa.ValueID{0, 2, 5}
	c.knownSafeBounds = []knownSafeBound{
		{bound: 1, absoluteAddr: ssa.Value(1)},
		{},
		{bound: 2, absoluteAddr: ssa.Value(2)},
		{},
		{},
		{bound: 3, absoluteAddr: ssa.Value(3)},
	}
	c.finalizeKnownSafeBoundsAtTheEndOfBlock(blk.ID())
	require.True(t, len(c.knownSafeBoundsSet) == 0)
	finalized := c.getKnownSafeBoundsAtTheEndOfBlocks(blk.ID())
	require.Equal(t, 3, len(finalized.View()))
}

func TestCompiler_initializeCurrentBlockKnownBounds(t *testing.T) {
	t.Run("single (sealed)", func(t *testing.T) {
		c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
		builder := c.ssaBuilder
		child := builder.AllocateBasicBlock()
		{
			parent := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(parent)
			c.recordKnownSafeBound(1, 99, 9999)
			c.recordKnownSafeBound(2, 150, 9999)
			c.recordKnownSafeBound(5, 666, 54321)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(parent.ID())
		}

		// Seal the child: the absolute addresses will be propagated.
		builder.Seal(child)
		builder.SetCurrentBlock(child)
		c.initializeCurrentBlockKnownBounds()
		kb := c.getKnownSafeBound(1)
		require.True(t, kb.valid())
		require.Equal(t, uint64(99), kb.bound)
		require.Equal(t, ssa.Value(9999), kb.absoluteAddr)
		kb = c.getKnownSafeBound(2)
		require.True(t, kb.valid())
		require.Equal(t, uint64(150), kb.bound)
		require.Equal(t, ssa.Value(9999), kb.absoluteAddr)
		kb = c.getKnownSafeBound(5)
		require.True(t, kb.valid())
		require.Equal(t, uint64(666), kb.bound)
		require.Equal(t, ssa.Value(54321), kb.absoluteAddr)
	})
	t.Run("single (unsealed)", func(t *testing.T) {
		c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
		builder := c.ssaBuilder
		child := builder.AllocateBasicBlock()
		{
			parent := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(parent)
			c.recordKnownSafeBound(1, 99, 9999)
			c.recordKnownSafeBound(2, 150, 9999)
			c.recordKnownSafeBound(5, 666, 54321)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(parent.ID())
		}

		// Do not seal the child: the absolute addresses will be recomputed.
		builder.SetCurrentBlock(child)
		c.initializeCurrentBlockKnownBounds()
		kb := c.getKnownSafeBound(1)
		require.True(t, kb.valid())
		require.Equal(t, uint64(99), kb.bound)
		require.NotEqual(t, ssa.Value(9999), kb.absoluteAddr)
		kb = c.getKnownSafeBound(2)
		require.True(t, kb.valid())
		require.Equal(t, uint64(150), kb.bound)
		require.NotEqual(t, ssa.Value(9999), kb.absoluteAddr)
		kb = c.getKnownSafeBound(5)
		require.True(t, kb.valid())
		require.Equal(t, uint64(666), kb.bound)
		require.NotEqual(t, ssa.Value(54321), kb.absoluteAddr)
	})
	t.Run("one predecessor reaching twice", func(t *testing.T) {
		// A br_table can name the same target twice; that is still a single predecessor,
		// including when the epoch that deduplicates them wraps around.
		for _, epoch := range []uint32{0, math.MaxUint32} {
			c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
			c.predSeenEpoch = epoch
			builder := c.ssaBuilder
			child := builder.AllocateBasicBlock()
			parent := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(parent)
			c.recordKnownSafeBound(1, 99, 9999)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(parent.ID())

			builder.Seal(child)
			builder.SetCurrentBlock(child)
			c.initializeCurrentBlockKnownBounds()
			require.Equal(t, []ssa.BasicBlockID{parent.ID()}, c.preds)
			kb := c.getKnownSafeBound(1)
			require.True(t, kb.valid())
			require.Equal(t, ssa.Value(9999), kb.absoluteAddr)
		}
	})
	t.Run("multiple predecessors", func(t *testing.T) {
		c := NewFrontendCompiler(&wasm.Module{}, ssa.NewBuilder(), nil, false, false)
		builder := c.ssaBuilder
		child := builder.AllocateBasicBlock()
		{
			p1 := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(p1)
			c.recordKnownSafeBound(1, 99, 9999)
			c.recordKnownSafeBound(2, 150, 9999)
			c.recordKnownSafeBound(5, 666, 54321)
			c.recordKnownSafeBound(592131, 666, 54321)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(p1.ID())
		}
		{
			p2 := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(p2)
			c.recordKnownSafeBound(1, 100, 999419)
			c.recordKnownSafeBound(2, 4, 9991239)
			c.recordKnownSafeBound(5, 555, 54341221)
			c.recordKnownSafeBound(6, 666, 54321)
			c.recordKnownSafeBound(7, 666, 54321)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(p2.ID())
		}
		{
			p3 := builder.AllocateBasicBlock()
			builder.SetCurrentBlock(p3)
			c.recordKnownSafeBound(1, 1, 999419)
			c.recordKnownSafeBound(2, 11111, 9991239)
			c.recordKnownSafeBound(5, 5551231, 54341221)
			c.recordKnownSafeBound(7, 666, 54321)
			c.recordKnownSafeBound(60, 666, 54321)
			builder.AllocateInstruction().AsJump(ssa.ValuesNil, child).Insert(builder)
			c.finalizeKnownSafeBoundsAtTheEndOfBlock(p3.ID())
		}

		builder.SetCurrentBlock(child)
		c.initializeCurrentBlockKnownBounds()
		for _, tc := range []struct {
			id    ssa.ValueID
			valid bool
			bound uint64
		}{
			{id: 0, valid: false},
			{id: 1, valid: true, bound: 1},
			{id: 2, valid: true, bound: 4},
			{id: 3, valid: false},
			{id: 4, valid: false},
			{id: 5, valid: true, bound: 555},
			{id: 6, valid: false},
			{id: 7, valid: false},
			{id: 60, valid: false},
			{id: 592131, valid: false},
		} {
			t.Run(fmt.Sprintf("id=%d", tc.id), func(t *testing.T) {
				kb := c.getKnownSafeBound(tc.id)
				require.Equal(t, tc.valid, kb.valid())
				if kb.valid() {
					require.Equal(t, tc.bound, kb.bound)
					require.Equal(t, ssa.ValueInvalid, kb.absoluteAddr)
				}
			})
		}
	})
}

// branchHinted returns single-function module m with that function carrying hints, as a branch hint section would
// give them.
func branchHinted(m *wasm.Module, hints ...wasm.BranchHint) *wasm.Module {
	code := m.CodeSection[0]
	code.BranchHints = hints
	return &wasm.Module{
		TypeSection: m.TypeSection, FunctionSection: m.FunctionSection,
		CodeSection: []wasm.Code{code}, ExportSection: m.ExportSection,
	}
}
