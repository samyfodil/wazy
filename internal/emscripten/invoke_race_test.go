package emscripten

import (
	"sync"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

// TestNewInvokeFunc_typeKeyIsCached is a regression test for a data race of the
// shape of https://github.com/tetratelabs/wazero/issues/2520.
//
// NewInvokeFunc builds a FunctionType in Go, so it never passes through the
// decoder's eager key caching. That type is held by the host module and shared
// by every instance of it, while (*InvokeFunc).Call looks its ID up per call,
// from guest execution -- so a lazily-cached key would be written concurrently.
//
// Run under -race: two goroutines resolving the shared type at once is exactly
// what a pool of instances doing emscripten invoke_* calls does.
func TestNewInvokeFunc_typeKeyIsCached(t *testing.T) {
	hf := NewInvokeFunc("invoke_iii",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32})

	shared := hf.Code.GoFunc.(*InvokeFunc).FunctionType

	// Nothing may touch shared before the goroutines: reading its key here
	// would populate any lazy cache and hide the very race under test.
	//
	// Two stores resolving the same shared type concurrently, as two module
	// instances calling invoke_* would.
	s1, s2 := wasm.NewStore(api.CoreFeaturesV2, nil), wasm.NewStore(api.CoreFeaturesV2, nil)
	var wg sync.WaitGroup
	ids := make([]wasm.FunctionTypeID, 2)
	for i, s := range []*wasm.Store{s1, s2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.GetFunctionTypeID(shared)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	require.Equal(t, ids[0], ids[1])
	// The key is cached up front, so no call site had to write it.
	require.Equal(t, "i32i32_i32", shared.String())
}
