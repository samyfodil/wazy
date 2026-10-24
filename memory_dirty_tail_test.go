package wazy

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// (module
//
//	(import "host" "block" (func $block))
//	(memory (export "mem") 1 2)
//	(func (export "run")
//	  (call $block)
//	  (drop (memory.grow (i32.const 1)))
//	  (i32.store (i32.const 65536) (i32.const 1))))
var growAfterBlockModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	0x02, 0x0e, 0x01,
	0x04, 'h', 'o', 's', 't',
	0x05, 'b', 'l', 'o', 'c', 'k',
	0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x04, 0x01, 0x01, 0x01, 0x02, // memory min 1 max 2
	0x07, 0x0d, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x03, 'r', 'u', 'n', 0x00, 0x01,
	0x0a, 0x14, 0x01,
	0x12, 0x00, // body size, 0 locals
	0x10, 0x00, // call $block
	0x41, 0x01, // i32.const 1
	0x40, 0x00, // memory.grow 0
	0x1a,                   // drop
	0x41, 0x80, 0x80, 0x04, // i32.const 65536
	0x41, 0x01, // i32.const 1
	0x36, 0x02, 0x00, // i32.store
	0x0b,
}

// A close captures what to recycle while a call is still running. That call
// can then GROW within reserved capacity and write ABOVE the length the close
// saw -- and the pool clears only the prefix it is handed, so those bytes used
// to reach the next instance. The prefix has to be taken once the call has
// drained, not when the close ran.
//
// Deterministic: the survivor reads the previous tenant's byte, no race
// detector needed.
func TestClosedModuleGrowthDoesNotLeakIntoNextInstance(t *testing.T) {
	ctx := context.Background()
	r := NewRuntimeWithConfig(ctx, NewRuntimeConfig().WithMemoryCapacityFromMax(true))
	defer r.Close(ctx)

	entered, release := make(chan struct{}), make(chan struct{})
	_, err := r.NewHostModuleBuilder("host").NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
			close(entered)
			<-release
		}), nil, nil).
		Export("block").Instantiate(ctx)
	require.NoError(t, err)

	victim, err := r.InstantiateWithConfig(ctx, growAfterBlockModule,
		NewModuleConfig().WithName("victim"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = victim.ExportedFunction("run").Call(ctx)
	}()

	<-entered
	require.NoError(t, victim.Close(ctx)) // sees a 1-page memory

	close(release) // the call grows to 2 pages and stores at 65536
	wg.Wait()

	survivor, err := r.InstantiateWithConfig(ctx, growAfterBlockModule,
		NewModuleConfig().WithName("survivor").WithStartFunctions())
	require.NoError(t, err)
	mem := survivor.Memory()
	require.NotNil(t, mem)
	if _, ok := mem.Grow(1); !ok {
		t.Fatal("could not grow the survivor to reach the tail")
	}
	got, ok := mem.ReadUint32Le(65536)
	require.True(t, ok)
	if got != 0 {
		t.Fatalf("a fresh instance sees the previous tenant's post-close write at offset 65536: %#x, want 0", got)
	}
}
