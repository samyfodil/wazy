package wazy

import (
	"context"
	"sync"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// (module (memory (export "mem") 1))
var closeRaceModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	0x05, 0x03, 0x01, 0x00, 0x01,
	0x07, 0x07, 0x01, 0x03, 'm', 'e', 'm', 0x02, 0x00,
}

// Issue #69's reproducer, verbatim in shape: read a module's memory through an
// api.Memory obtained before the close, from a goroutine, while another closes
// the module. The embedder that reported it cannot order the two -- the close
// is a kill for a call stuck in the guest (WithCloseOnContextDone) and the
// reader is that call still unwinding through host code, so waiting for it is
// the one thing the kill must not do.
//
// A read that observes the close must fail; it must not be a data race, and it
// must not read a buffer the pool has handed to some other module. Only
// meaningful under -race, which CI runs.
func TestModuleCloseDuringMemoryReadIsRaceFree(t *testing.T) {
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, closeRaceModule)
	require.NoError(t, err)

	mem := mod.Memory()
	require.NotNil(t, mem)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100000; i++ {
			if _, ok := mem.Read(0, 8); !ok {
				return // saw the close: the documented outcome
			}
		}
	}()
	require.NoError(t, mod.Close(ctx))
	wg.Wait()

	// After the close the memory reads as empty rather than as whatever the
	// pool hands that array to next, and reports a size consistent with that.
	_, ok := mem.Read(0, 1)
	require.False(t, ok, "reading a closed module's memory must fail")
	require.Equal(t, uint32(0), mem.Size())

	// Writes too -- they take the same path and must not land in a buffer that
	// now belongs to a different instance.
	require.False(t, mem.Write(0, []byte{1}), "writing a closed module's memory must fail")
	require.False(t, mem.WriteUint32Le(0, 1))
	_, ok = mem.ReadUint32Le(0)
	require.False(t, ok)
}

// The same, with many readers, since an embedder's host functions run
// concurrently on one module.
func TestModuleCloseDuringConcurrentMemoryReadsIsRaceFree(t *testing.T) {
	ctx := context.Background()
	r := NewRuntime(ctx)
	defer r.Close(ctx)

	mod, err := r.Instantiate(ctx, closeRaceModule)
	require.NoError(t, err)
	mem := mod.Memory()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20000; j++ {
				if _, ok := mem.Read(0, 8); !ok {
					return
				}
			}
		}()
	}
	require.NoError(t, mod.Close(ctx))
	wg.Wait()
}
