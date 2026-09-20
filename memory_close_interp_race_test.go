package wazy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// A module whose exported "spin" reads linear memory in an unbounded loop, so
// the engine is inside a memory access when the close lands. Hand-assembled:
//
//	(module
//	  (memory (export "mem") 1)
//	  (func (export "spin") (local i32)
//	    (loop $l
//	      (drop (i32.load (i32.const 0)))
//	      (br $l))))
var spinModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: one func type () -> ()
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	// function section: one function of type 0
	0x03, 0x02, 0x01, 0x00,
	// memory section: one memory, min 1
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export section: "mem" memory 0, "spin" func 0
	0x07, 0x0e, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x04, 's', 'p', 'i', 'n', 0x00, 0x00,
	// code section
	0x0a, 0x0f, 0x01,
	0x0d, 0x00, // body size, 0 locals
	0x03, 0x40, // loop (void)
	0x41, 0x00, // i32.const 0
	0x28, 0x02, 0x00, // i32.load align=2 offset=0
	0x1a,       // drop
	0x0c, 0x00, // br 0
	0x0b, // end loop
	0x0b, // end func
}

// spinHostModule is spinModule with the load replaced by a call to an imported
// host function, so the close lands while a HOST function is reading guest
// memory -- go-pdfium's actual shape.
//
//	(module
//	  (import "host" "peek" (func $peek))
//	  (memory (export "mem") 1)
//	  (func (export "spin") (loop $l (call $peek) (br $l))))
var spinHostModule = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
	// type section: one func type () -> ()
	0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
	// import section: "host" "peek" func type 0
	0x02, 0x0d, 0x01,
	0x04, 'h', 'o', 's', 't',
	0x04, 'p', 'e', 'e', 'k',
	0x00, 0x00,
	// function section: one function of type 0 (func index 1, after the import)
	0x03, 0x02, 0x01, 0x00,
	// memory section: one memory, min 1
	0x05, 0x03, 0x01, 0x00, 0x01,
	// export section: "mem" memory 0, "spin" func 1
	0x07, 0x0e, 0x02,
	0x03, 'm', 'e', 'm', 0x02, 0x00,
	0x04, 's', 'p', 'i', 'n', 0x00, 0x01,
	// code section
	0x0a, 0x0b, 0x01,
	0x09, 0x00, // body size, 0 locals
	0x03, 0x40, // loop (void)
	0x10, 0x00, // call 0 (the import)
	0x0c, 0x00, // br 0
	0x0b, // end loop
	0x0b, // end func
}

// closeWhileGuestReadsMemory runs "spin" -- which is inside a memory access
// essentially all of the time -- and closes the module out from under it, the
// way WithCloseOnContextDone does from its watchdog goroutine. The engine reads
// mem.Buffer directly on that path, so anything the close writes to the memory
// races it. Only meaningful under -race.
func closeWhileGuestReadsMemory(t *testing.T, cfg RuntimeConfig) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := NewRuntimeWithConfig(ctx, cfg)
	defer r.Close(context.Background())

	mod, err := r.Instantiate(ctx, spinModule)
	require.NoError(t, err)

	spin := mod.ExportedFunction("spin")
	require.NotNil(t, spin)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Traps or returns an exit error once the module closes; either is
		// fine, the point is that it is still executing when that happens.
		_, _ = spin.Call(ctx)
	}()

	// Let it get into the loop, then pull the memory out from under it.
	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()
}

func TestGuestMemoryReadDuringCloseIsRaceFree_Interpreter(t *testing.T) {
	closeWhileGuestReadsMemory(t, NewRuntimeConfigInterpreter().WithCloseOnContextDone(true))
}

func TestGuestMemoryReadDuringCloseIsRaceFree_Compiler(t *testing.T) {
	cfg := NewRuntimeConfig().WithCloseOnContextDone(true)
	closeWhileGuestReadsMemory(t, cfg)
}

// The shape go-pdfium's Kill() actually produces: the interrupted call is
// unwinding through a HOST function that reads guest memory when the close
// lands, on a different goroutine from the close by construction. Both engines,
// since each reaches guest memory its own way.
func TestHostMemoryReadDuringCloseIsRaceFree(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  RuntimeConfig
	}{
		{"compiler", NewRuntimeConfig().WithCloseOnContextDone(true)},
		{"interpreter", NewRuntimeConfigInterpreter().WithCloseOnContextDone(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			r := NewRuntimeWithConfig(ctx, tc.cfg)
			defer r.Close(context.Background())

			// A host function that reads the caller's memory on every call,
			// the way go-pdfium's host side does while a render unwinds.
			var reads sync.WaitGroup
			_, err := r.NewHostModuleBuilder("host").
				NewFunctionBuilder().
				WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
					if mem := mod.Memory(); mem != nil {
						_, _ = mem.Read(0, 8)
					}
				}), nil, nil).
				Export("peek").
				Instantiate(ctx)
			require.NoError(t, err)

			mod, err := r.Instantiate(ctx, spinHostModule)
			require.NoError(t, err)

			spin := mod.ExportedFunction("spin")
			require.NotNil(t, spin)

			reads.Add(1)
			go func() {
				defer reads.Done()
				_, _ = spin.Call(ctx)
			}()

			time.Sleep(20 * time.Millisecond)
			cancel()
			reads.Wait()
		})
	}
}
