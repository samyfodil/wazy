package wazy

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// blockThenStoreModule blocks in a host function, then stores a sentinel into
// its own memory. That lets a test hold a call open across a close and a pool
// reuse, and see deterministically whose memory the store lands in.
//
//	(module
//	  (import "host" "block" (func $block))
//	  (memory (export "mem") 1)
//	  (func (export "run") (call $block) (i32.store (i32.const 0) (i32.const 1))))
var blockThenStoreModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	// import "host"."block"
	0x02, 0x0e, 0x01,
	0x04, 'h', 'o', 's', 't',
	0x05, 'b', 'l', 'o', 'c', 'k',
	0x00, 0x00,
	0x03, 0x02, 0x01, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export "mem" memory 0, "run" func 1
	0x07, 0x0d, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x03, 'r', 'u', 'n', 0x00, 0x01,
	// code
	0x0a, 0x0d, 0x01,
	0x0b, 0x00, // body size, 0 locals
	0x10, 0x00, // call 0
	0x41, 0x00, // i32.const 0
	0x41, 0x01, // i32.const 1
	0x36, 0x02, 0x00, // i32.store align=2 offset=0
	0x0b, // end
}

// The corruption go-pdfium's suite actually hit, made deterministic: a module
// is closed while its call is still in flight, its buffer goes back to the
// pool, the next instantiation gets that same array, and then the first call
// resumes and writes into it. This checks the survivors' memory directly
// rather than relying on the race detector to catch the overlap.
//
// Deliberately WITHOUT WithCloseOnContextDone: nothing then interrupts the
// blocked call, so it is certain to still be running at the close -- the state
// go-pdfium reached with a guest stuck in pdfium's own loop.
func TestClosedModuleBufferIsNotReusedWhileCallRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RuntimeConfig
	}{
		{"compiler", NewRuntimeConfig()},
		{"interpreter", NewRuntimeConfigInterpreter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := NewRuntimeWithConfig(ctx, tc.cfg)
			defer r.Close(ctx)

			entered, release := make(chan struct{}), make(chan struct{})
			_, err := r.NewHostModuleBuilder("host").
				NewFunctionBuilder().
				WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {
					close(entered)
					<-release
				}), nil, nil).
				Export("block").
				Instantiate(ctx)
			require.NoError(t, err)

			victim, err := r.InstantiateWithConfig(ctx, blockThenStoreModule,
				NewModuleConfig().WithName("victim"))
			require.NoError(t, err)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = victim.ExportedFunction("run").Call(ctx)
			}()

			<-entered                             // the call is now in flight
			require.NoError(t, victim.Close(ctx)) // ... and the module closes under it

			// Whoever would take the victim's buffer if it was wrongly pooled.
			const survivors = 8
			mems := make([]api.Memory, 0, survivors)
			for i := 0; i < survivors; i++ {
				m, err := r.InstantiateWithConfig(ctx, blockThenStoreModule,
					NewModuleConfig().WithName("").WithStartFunctions())
				require.NoError(t, err)
				defer m.Close(ctx)
				mem := m.Memory()
				require.NotNil(t, mem)
				require.True(t, mem.Write(0, []byte{0xAA, 0xAA, 0xAA, 0xAA}))
				mems = append(mems, mem)
			}

			close(release) // the victim resumes and stores 1 at offset 0
			wg.Wait()

			for i, mem := range mems {
				got, ok := mem.ReadUint32Le(0)
				require.True(t, ok)
				if got != 0xAAAAAAAA {
					t.Fatalf("survivor %d's memory was overwritten by a closed module's still-running call: got %#x, want 0xAAAAAAAA", i, got)
				}
			}
		})
	}
}
