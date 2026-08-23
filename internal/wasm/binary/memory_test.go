package binary

import (
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/experimental"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func Test_newMemorySizer(t *testing.T) {
	zero := uint64(0)
	ten := uint64(10)
	defaultLimit := wasm.MemoryLimitPages
	defaultLimit64 := uint64(defaultLimit)
	specCeiling64 := wasm.Memory64LimitPages
	overSpecCeiling64 := wasm.Memory64LimitPages + 1

	tests := []struct {
		name                                       string
		memoryCapacityFromMax                      bool
		memoryCapacityReservePages                 uint32
		index64                                    bool
		limit                                      uint32
		min                                        uint64
		max                                        *uint64
		expectedMin, expectedCapacity, expectedMax uint64
	}{
		{
			name:             "min 0",
			limit:            defaultLimit,
			min:              zero,
			max:              &defaultLimit64,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      defaultLimit64,
		},
		{
			name:             "min 0 defaults max to defaultLimit",
			limit:            defaultLimit,
			min:              zero,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      defaultLimit64,
		},
		{
			name:             "min 0, max 0",
			limit:            defaultLimit,
			min:              zero,
			max:              &zero,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      zero,
		},
		{
			name:             "min 0, max 10",
			limit:            defaultLimit,
			min:              zero,
			max:              &ten,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      ten,
		},
		{
			name:                       "min 0, max 10 memoryCapacityFromMax",
			limit:                      defaultLimit,
			memoryCapacityFromMax:      true,
			memoryCapacityReservePages: 2,
			min:                        zero,
			max:                        &ten,
			expectedMin:                zero,
			expectedCapacity:           ten,
			expectedMax:                ten,
		},
		{
			name:             "min 10, no max",
			limit:            200,
			min:              10,
			expectedMin:      10,
			expectedCapacity: 10,
			expectedMax:      200,
		},
		{
			name:                       "min 10, reserve 20, no max",
			memoryCapacityReservePages: 20,
			limit:                      200,
			min:                        10,
			expectedMin:                10,
			expectedCapacity:           30,
			expectedMax:                200,
		},
		{
			name:                       "reserve capped at encoded max",
			memoryCapacityReservePages: 20,
			limit:                      defaultLimit,
			min:                        0,
			max:                        &ten,
			expectedMin:                0,
			expectedCapacity:           10,
			expectedMax:                10,
		},
		{
			name:                       "reserve capped at runtime limit",
			memoryCapacityReservePages: 20,
			limit:                      5,
			min:                        0,
			max:                        &ten,
			expectedMin:                0,
			expectedCapacity:           5,
			expectedMax:                5,
		},
		{
			name:                  "min 10, no max memoryCapacityFromMax",
			memoryCapacityFromMax: true,
			limit:                 200,
			min:                   10,
			expectedMin:           10,
			expectedCapacity:      200,
			expectedMax:           200,
		},
		{
			name:             "min=max",
			limit:            defaultLimit,
			min:              ten,
			max:              &ten,
			expectedMin:      ten,
			expectedCapacity: ten,
			expectedMax:      ten,
		},
		{
			name:             "max > memoryLimitPages",
			limit:            5,
			min:              0,
			max:              &ten,
			expectedMin:      0,
			expectedCapacity: 0,
			expectedMax:      5,
		},
		{
			// A 64-bit memory's declared limits are bounded by the
			// specification's ceiling, not the embedder's, which is applied at
			// instantiation instead. See newMemorySizer.
			name:             "i64 min 0, no max defaults to the spec ceiling",
			index64:          true,
			limit:            5,
			min:              zero,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      wasm.Memory64LimitPages,
		},
		{
			name:             "i64 max over the embedder limit is kept",
			index64:          true,
			limit:            5,
			min:              zero,
			max:              &specCeiling64,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      wasm.Memory64LimitPages,
		},
		{
			name:             "i64 min over the embedder limit is kept",
			index64:          true,
			limit:            5,
			min:              wasm.Memory64LimitPages,
			expectedMin:      wasm.Memory64LimitPages,
			expectedCapacity: wasm.Memory64LimitPages,
			expectedMax:      wasm.Memory64LimitPages,
		},
		{
			name:             "i64 max over the spec ceiling propagates for Validate to reject",
			index64:          true,
			limit:            5,
			min:              zero,
			max:              &overSpecCeiling64,
			expectedMin:      zero,
			expectedCapacity: zero,
			expectedMax:      wasm.Memory64LimitPages + 1,
		},
		{
			name:                  "i64 memoryCapacityFromMax caps capacity at the spec ceiling",
			index64:               true,
			memoryCapacityFromMax: true,
			limit:                 5,
			min:                   zero,
			expectedMin:           zero,
			expectedCapacity:      wasm.Memory64LimitPages,
			expectedMax:           wasm.Memory64LimitPages,
		},
	}

	for _, tt := range tests {
		tc := tt
		t.Run(tc.name, func(t *testing.T) {
			sizer := newMemorySizer(tc.limit, tc.memoryCapacityFromMax, tc.memoryCapacityReservePages)
			min, capacity, max := sizer(tc.min, tc.max, tc.index64)
			require.Equal(t, tc.expectedMin, min)
			// The sizer fills in a maximum no larger than a slice can address,
			// so both expectations clamp with it where an int is 32 bits wide.
			require.Equal(t, clampToAllocatable(tc.expectedCapacity), capacity)
			require.Equal(t, clampToAllocatable(tc.expectedMax), max)
		})
	}
}

// overAllocatableLimit is the tail of the error naming the effective page
// ceiling, which is lower than MemoryLimitPages where an int is 32 bits wide.
func overAllocatableLimit() string {
	return fmt.Sprintf("over limit of %d pages (%s)",
		wasm.MaxAllocatablePages, wasm.PagesToUnitOfBytes(wasm.MaxAllocatablePages))
}

// pick returns wide on a platform that can address a full 65536-page memory and
// narrow otherwise, for the cases where the clamp changes which check fires.
func pick(wide, narrow string) string {
	if wasm.MaxAllocatablePages == wasm.MemoryLimitPages {
		return wide
	}
	return narrow
}

// clampToAllocatable mirrors what newMemorySizer applies, so the expectations
// stay honest on a platform whose int is 32 bits wide. See
// wasm.MaxAllocatablePages.
func clampToAllocatable(pages uint32) uint32 {
	return min(pages, wasm.MaxAllocatablePages)
}

func TestMemoryType(t *testing.T) {
	zero := uint64(0)
	max := uint64(wasm.MemoryLimitPages)

	tests := []struct {
		name             string
		input            *wasm.Memory
		memoryLimitPages uint32
		expected         []byte
	}{
		{
			name:     "min 0",
			input:    &wasm.Memory{Max: max, IsMaxEncoded: true},
			expected: []byte{0x1, 0, 0x80, 0x80, 0x4},
		},
		{
			name:     "min 0 default max",
			input:    &wasm.Memory{Max: max},
			expected: []byte{0x0, 0},
		},
		{
			name:     "min 0, max 0",
			input:    &wasm.Memory{Max: zero, IsMaxEncoded: true},
			expected: []byte{0x1, 0, 0},
		},
		{
			name:     "min=max",
			input:    &wasm.Memory{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true},
			expected: []byte{0x1, 1, 1},
		},
		{
			name:     "min 0, max largest",
			input:    &wasm.Memory{Max: max, IsMaxEncoded: true},
			expected: []byte{0x1, 0, 0x80, 0x80, 0x4},
		},
		{
			name:     "min largest max largest",
			input:    &wasm.Memory{Min: max, Cap: max, Max: max, IsMaxEncoded: true},
			expected: []byte{0x1, 0x80, 0x80, 0x4, 0x80, 0x80, 0x4},
		},
		{
			name:             "min 0, max largest, wazy limit",
			input:            &wasm.Memory{Max: max, IsMaxEncoded: true},
			memoryLimitPages: 512,
			expected:         []byte{0x1, 0, 0x80, 0x80, 0x4},
		},
		{
			name:     "min 0, max 1, shared",
			input:    &wasm.Memory{Max: 1, IsMaxEncoded: true, IsShared: true},
			expected: []byte{0x3, 0, 1},
		},
	}

	for _, tt := range tests {
		tc := tt

		b := binaryencoding.EncodeMemory(tc.input)
		t.Run(fmt.Sprintf("encode %s", tc.name), func(t *testing.T) {
			require.Equal(t, tc.expected, b)
		})

		t.Run(fmt.Sprintf("decode %s", tc.name), func(t *testing.T) {
			tmax := max
			expectedDecoded := tc.input
			if tc.memoryLimitPages != 0 {
				// If a memory limit exists, then the expected module Max reflects that limit.
				tmax = uint64(tc.memoryLimitPages)
				expectedDecoded.Max = tmax
			}
			// ...clamped to what a slice on this platform can address, which
			// only bites where an int is 32 bits wide.
			if expectedDecoded.Max > wasm.MaxAllocatablePages {
				expectedDecoded.Max = wasm.MaxAllocatablePages
			}

			if tc.input.Min > wasm.MaxAllocatablePages {
				// A memory this large cannot exist where an int is 32 bits
				// wide, and decodeMemory rightly says so. What that rejection
				// looks like is covered by TestMemory_MaxAllocatablePages.
				t.Skip("declared minimum is past what a slice can address here")
			}

			features := api.CoreFeaturesV2 | api.CoreFeatureMemory64
			if tc.input.IsShared {
				features = features.SetEnabled(experimental.CoreFeaturesThreads, true)
			}
			decoded, _, err := decodeMemory(b, 0, features, newMemorySizer(uint32(tmax), false, 0), uint32(tmax))
			require.NoError(t, err)
			require.Equal(t, decoded, expectedDecoded)
		})
	}
}

func TestDecodeMemoryType_Errors(t *testing.T) {
	max := wasm.MemoryLimitPages

	tests := []struct {
		name           string
		input          []byte
		threadsEnabled bool
		expectedErr    string
	}{
		{
			name:  "max < min",
			input: []byte{0x1, 0x80, 0x80, 0x4, 0},
			// A 65536-page minimum is itself past what a slice can address
			// where an int is 32 bits wide, and that check comes first.
			expectedErr: pick(
				"min 65536 pages (4 Gi) > max 0 pages (0 Ki)",
				"min 65536 pages (4 Gi) "+overAllocatableLimit()),
		},
		{
			name:        "min > limit",
			input:       []byte{0x0, 0xff, 0xff, 0xff, 0xff, 0xf},
			expectedErr: "min 4294967295 pages (255 Ti) over limit of 65536 pages (4 Gi)",
		},
		{
			name:        "max > limit",
			input:       []byte{0x1, 0, 0xff, 0xff, 0xff, 0xff, 0xf},
			expectedErr: "max 4294967295 pages (255 Ti) over limit of 65536 pages (4 Gi)",
		},
		{
			name:        "shared but no threads",
			input:       []byte{0x2, 0, 0x80, 0x80, 0x4},
			expectedErr: "shared memory requested but threads feature not enabled",
		},
		{
			name:           "shared but no max",
			input:          []byte{0x2, 0, 0x80, 0x80, 0x4},
			threadsEnabled: true,
			expectedErr:    "shared memory requires a maximum size to be specified",
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			features := api.CoreFeaturesV2
			if tc.threadsEnabled {
				features = features.SetEnabled(experimental.CoreFeaturesThreads, true)
			} else {
				// Allow test to work if threads is ever added to default features by explicitly removing threads features
				features = features.SetEnabled(experimental.CoreFeaturesThreads, false)
			}
			_, _, err := decodeMemory(tc.input, 0, features, newMemorySizer(max, false, 0), max)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}
