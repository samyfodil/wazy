package instance

import (
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/abi"
)

// BenchmarkCallAsyncFirstLight measures the callback-lift hot path with no
// suspension: run-async calls task.return then returns EXIT on its first
// callback return, so invokeAsyncCallback's loop runs once. Isolates the
// per-async-call overhead (task alloc, enter/exit, the callback loop) vs the
// synchronous BenchmarkCallAdd baseline.
func BenchmarkCallAsyncFirstLight(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	inst, err := Instantiate(ctx, r, firstLightAsyncWasm)
	if err != nil {
		b.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := inst.Call(ctx, "run-async")
		if err != nil {
			b.Fatalf("Call: %v", err)
		}
		if got, ok := res[0].(uint32); !ok || got != 42 {
			b.Fatalf("run-async = %v, want 42", res[0])
		}
	}
}

// BenchmarkCallAsyncAwaitImport measures the full WAIT path: run-async lowers
// an async host import that defers its Resolve, so the guest returns WAIT, the
// scheduler drives the deferred completion, delivers the SUBTASK event, the
// callback resumes and task.returns. Exercises sched.drive, the subtask + its
// table entry, waitable-set.new/join, and the deferred thunk.
func BenchmarkCallAsyncAwaitImport(b *testing.B) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	params, results := getImportParamsResults()
	opt := WithAsyncImport("get", "", func(_ context.Context, _ []abi.Value, call *AsyncCall) error {
		call.Defer(func() { call.Resolve([]abi.Value{uint32(99)}) })
		return nil
	}, params, results)

	inst, err := Instantiate(ctx, r, awaitImportAsyncWasm, opt)
	if err != nil {
		b.Fatalf("Instantiate: %v", err)
	}
	defer inst.Close(ctx)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := inst.Call(ctx, "run-async")
		if err != nil {
			b.Fatalf("Call: %v", err)
		}
		if got, ok := res[0].(uint32); !ok || got != 99 {
			b.Fatalf("run-async = %v, want 99", res[0])
		}
	}
}
