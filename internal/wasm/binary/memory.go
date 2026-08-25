package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/wasm"
)

// decodeMemory returns the api.Memory decoded from buf[offset:] with the WebAssembly 1.0 (20191205) Binary
// Format, and the offset after it.
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-memory
func decodeMemory(
	buf []byte,
	offset int,
	enabledFeatures api.CoreFeatures,
	memorySizer memorySizer,
	memoryLimitPages uint32,
) (*wasm.Memory, int, error) {
	min, maxP, shared, index64, offset, err := decodeLimitsType(buf, offset, enabledFeatures)
	if err != nil {
		return nil, offset, err
	}

	if shared {
		if !enabledFeatures.IsEnabled(experimental.CoreFeaturesThreads) {
			return nil, offset, fmt.Errorf("shared memory requested but threads feature not enabled")
		}

		// This restriction may be lifted in the future.
		// https://webassembly.github.io/threads/core/binary/types.html#memory-types
		if maxP == nil {
			return nil, offset, fmt.Errorf("shared memory requires a maximum size to be specified")
		}
	}

	// The declared limits of a 64-bit memory are bounded by the specification's
	// own ceiling rather than by the embedder's -- see newMemorySizer.
	limitPages := uint64(memoryLimitPages)
	if index64 {
		limitPages = wasm.Memory64LimitPages
	}

	min, capacity, max := memorySizer(min, maxP, index64)
	mem := &wasm.Memory{
		Min: min, Cap: capacity, Max: max,
		IsMaxEncoded: maxP != nil, IsShared: shared, IsMemory64: index64,
	}

	return mem, offset, mem.Validate(limitPages)
}
