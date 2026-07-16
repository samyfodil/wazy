package instance

import (
	"context"
	_ "embed"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// real_manyparams.component.wasm is a genuine cargo-component guest exporting
// sum20 -- a func with 20 u32 params (testdata/real_manyparams.wit). 20 params
// flatten to 20 core values, exceeding MAX_FLAT_PARAMS (16), so the Canonical
// ABI spills the WHOLE list to memory and the core func takes a single i32
// pointer. Also exercises the graph-path routing for a pure-compute (no-import)
// cargo-component guest whose only "alias" is its own core memory.
//
//go:embed testdata/real_manyparams.component.wasm
var realManyParamsWasm []byte

func TestRealManyParams(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, realManyParamsWasm, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	// sum of 1..20 = 210, and a second distinct input to rule out a constant.
	for _, tc := range []struct {
		base uint32
		want uint32
	}{
		{1, 210},    // 1+2+...+20
		{100, 2190}, // 100+101+...+119
	} {
		args := make([]abi.Value, 20)
		for i := range args {
			args[i] = tc.base + uint32(i)
		}
		res, err := inst.Call(ctx, "sum20", args...)
		if err != nil {
			t.Fatalf("sum20 (base %d): %v", tc.base, err)
		}
		if got := res[0].(uint32); got != tc.want {
			t.Errorf("sum20 (base %d) = %d, want %d", tc.base, got, tc.want)
		}
	}
}
