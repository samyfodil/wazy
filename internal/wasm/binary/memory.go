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
	memorySizer func(minPages uint32, maxPages *uint32) (min, capacity, max uint32),
	memoryLimitPages uint32,
) (*wasm.Memory, int, error) {
	min, maxP, shared, offset, err := decodeLimitsType(buf, offset)
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

	min, capacity, max := memorySizer(min, maxP)
	mem := &wasm.Memory{Min: min, Cap: capacity, Max: max, IsMaxEncoded: maxP != nil, IsShared: shared}

	return mem, offset, mem.Validate(memoryLimitPages)
}
