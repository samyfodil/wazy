package binary

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestTableSection(t *testing.T) {
	three := uint32(3)
	tests := []struct {
		name     string
		input    []byte
		expected []wasm.Table
	}{
		{
			name: "min and min with max",
			input: []byte{
				0x01,                                   // 1 table
				wasm.RefTypeFuncref.Kind(), 0x01, 2, 3, // (table 2 3)
			},
			expected: []wasm.Table{{Min: 2, Max: &three, Type: wasm.RefTypeFuncref}},
		},
		{
			name: "min and min with max - three tables",
			input: []byte{
				0x03,                                   // 3 table
				wasm.RefTypeFuncref.Kind(), 0x01, 2, 3, // (table 2 3)
				wasm.RefTypeExternref.Kind(), 0x01, 2, 3, // (table 2 3)
				wasm.RefTypeFuncref.Kind(), 0x01, 2, 3, // (table 2 3)
			},
			expected: []wasm.Table{
				{Min: 2, Max: &three, Type: wasm.RefTypeFuncref},
				{Min: 2, Max: &three, Type: wasm.RefTypeExternref},
				{Min: 2, Max: &three, Type: wasm.RefTypeFuncref},
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			tables, _, err := decodeTableSection(tc.input, 0, api.CoreFeatureReferenceTypes)
			require.NoError(t, err)
			require.Equal(t, tc.expected, tables)
		})
	}
}

func TestTableSection_Errors(t *testing.T) {
	tests := []struct {
		name        string
		input       []byte
		expectedErr string
		features    api.CoreFeatures
	}{
		{
			name: "min and min with max",
			input: []byte{
				0x02,                                   // 2 tables
				wasm.RefTypeFuncref.Kind(), 0x00, 0x01, // (table 1)
				wasm.RefTypeFuncref.Kind(), 0x01, 0x02, 0x03, // (table 2 3)
			},
			expectedErr: "at most one table allowed in module as feature \"reference-types\" is disabled",
			features:    api.CoreFeaturesV1,
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeTableSection(tc.input, 0, tc.features)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

func TestMemorySection(t *testing.T) {
	// What newMemorySizer fills in for a memory with no declared maximum: the
	// configured limit clamped to what a slice can address, which is lower
	// where an int is 32 bits wide. See wasm.MaxAllocatablePages.
	max := min(wasm.MemoryLimitPages, wasm.MaxAllocatablePages)

	three := uint32(3)
	tests := []struct {
		name                  string
		input                 []byte
		features              api.CoreFeatures
		memoryCapacityFromMax bool
		expected              []wasm.Memory
	}{
		{
			name: "min and min with max",
			input: []byte{
				0x01,             // 1 memory
				0x01, 0x02, 0x03, // (memory 2 3)
			},
			features: api.CoreFeaturesV2,
			expected: []wasm.Memory{{Min: 2, Cap: 2, Max: three, IsMaxEncoded: true}},
		},
		{
			// Regression test: with memoryCapacityFromMax, a max-less memory's
			// Cap is inflated to the full memoryLimitPages (see newMemorySizer),
			// even though its Min (what's actually declared/eagerly touched) is
			// tiny. The aggregate memory-budget check in decodeMemorySection
			// sums Min, not Cap, precisely so this ordinary, spec-valid
			// two-memory module isn't rejected outright under this config.
			name: "aggregate minimum ignores capacity-from-max inflation",
			input: []byte{
				0x02,       // 2 memories
				0x00, 0x01, // (memory min=1, no max)
				0x00, 0x01, // (memory min=1, no max)
			},
			features:              api.CoreFeaturesV2 | api.CoreFeatureMultiMemory,
			memoryCapacityFromMax: true,
			expected: []wasm.Memory{
				{Min: 1, Cap: max, Max: max, IsMaxEncoded: false},
				{Min: 1, Cap: max, Max: max, IsMaxEncoded: false},
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			memories, _, err := decodeMemorySection(tc.input, 0, tc.features, newMemorySizer(max, tc.memoryCapacityFromMax, 0), max)
			require.NoError(t, err)
			require.Equal(t, tc.expected, memories)
		})
	}
}

func TestMemorySection_Errors(t *testing.T) {
	max := wasm.MemoryLimitPages

	tests := []struct {
		name             string
		input            []byte
		features         api.CoreFeatures
		memoryLimitPages uint32
		expectedErr      string
	}{
		{
			name: "min and min with max",
			input: []byte{
				0x02,       // 2 memories
				0x01,       // (memory 1)
				0x02, 0x03, // (memory 2 3)
			},
			features:         api.CoreFeaturesV2,
			memoryLimitPages: max,
			expectedErr:      `at most one memory allowed in module as feature "multi-memory" is disabled`,
		},
		{
			name: "size exceeds remaining bytes",
			input: []byte{
				0xff, 0xff, 0xff, 0xff, 0x0f, // vs = 0xffffffff (max u32)
				0x01, 0x02, 0x03, // a single, real memory entry's worth of bytes
			},
			features:         api.CoreFeaturesV2 | api.CoreFeatureMultiMemory,
			memoryLimitPages: max,
			expectedErr:      "memory section size 4294967295 exceeds remaining module bytes (3)",
		},
		{
			name: "size exceeds remaining bytes, accounting for the 2-byte minimum per entry",
			input: []byte{
				0x02,             // vs = 2, but...
				0x01, 0x02, 0x03, // ...only 3 bytes remain, and each entry needs at least 2
			},
			features:         api.CoreFeaturesV2 | api.CoreFeatureMultiMemory,
			memoryLimitPages: max,
			expectedErr:      "memory section size 2 exceeds remaining module bytes (3)",
		},
		{
			name: "aggregate minimum across memories exceeds MemoryLimitPages",
			input: []byte{
				0x02,                   // 2 memories
				0x00, 0xc0, 0xb8, 0x02, // (memory min=40000)
				0x00, 0xc0, 0xb8, 0x02, // (memory min=40000)
			},
			features:         api.CoreFeaturesV2 | api.CoreFeatureMultiMemory,
			memoryLimitPages: max,
			// Where an int is 32 bits wide a single 40000-page memory is
			// already past what a slice can address, so that check fires
			// first and the aggregate is never reached.
			expectedErr: pick(
				"total memory minimum across 2 memories (80000 pages) exceeds 65536 pages",
				"min 40000 pages (2 Gi) "+overAllocatableLimit()),
		},
		{
			name: "aggregate minimum respects an embedder-configured limit below MemoryLimitPages",
			input: []byte{
				0x02,       // 2 memories
				0x00, 0x3c, // (memory min=60)
				0x00, 0x3c, // (memory min=60)
			},
			features:         api.CoreFeaturesV2 | api.CoreFeatureMultiMemory,
			memoryLimitPages: 100, // each memory (60 pages) is individually within the limit, but the sum (120) is not
			expectedErr:      "total memory minimum across 2 memories (120 pages) exceeds 100 pages",
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decodeMemorySection(tc.input, 0, tc.features, newMemorySizer(tc.memoryLimitPages, false, 0), tc.memoryLimitPages)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

func TestDecodeExportSection(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected []wasm.Export
	}{
		{
			name: "empty and non-empty name",
			input: []byte{
				0x02,                      // 2 exports
				0x00,                      // Size of empty name
				wasm.ExternTypeFunc, 0x02, // func[2]
				0x01, 'a', // Size of name, name
				wasm.ExternTypeFunc, 0x01, // func[1]
			},
			expected: []wasm.Export{
				{Name: "", Type: wasm.ExternTypeFunc, Index: wasm.Index(2)},
				{Name: "a", Type: wasm.ExternTypeFunc, Index: wasm.Index(1)},
			},
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			actual, actualExpMap, _, err := decodeExportSection(tc.input, 0, &stringArena{})
			require.NoError(t, err)
			require.Equal(t, tc.expected, actual)

			expMap := make(map[string]*wasm.Export, len(tc.expected))
			for i := range tc.expected {
				exp := &tc.expected[i]
				expMap[exp.Name] = exp
			}
			require.Equal(t, expMap, actualExpMap)
		})
	}
}

func TestDecodeExportSection_Errors(t *testing.T) {
	tests := []struct {
		name        string
		input       []byte
		expectedErr string
	}{
		{
			name: "duplicates empty name",
			input: []byte{
				0x02,                      // 2 exports
				0x00,                      // Size of empty name
				wasm.ExternTypeFunc, 0x00, // func[0]
				0x00,                      // Size of empty name
				wasm.ExternTypeFunc, 0x00, // func[0]
			},
			expectedErr: "export[1] duplicates name \"\"",
		},
		{
			name: "duplicates name",
			input: []byte{
				0x02,      // 2 exports
				0x01, 'a', // Size of name, name
				wasm.ExternTypeFunc, 0x00, // func[0]
				0x01, 'a', // Size of name, name
				wasm.ExternTypeFunc, 0x00, // func[0]
			},
			expectedErr: "export[1] duplicates name \"a\"",
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := decodeExportSection(tc.input, 0, &stringArena{})
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

func TestEncodeFunctionSection(t *testing.T) {
	require.Equal(t, []byte{wasm.SectionIDFunction, 0x2, 0x01, 0x05}, binaryencoding.EncodeFunctionSection([]wasm.Index{5}))
}

// TestEncodeStartSection uses the same index as TestEncodeFunctionSection to highlight the encoding is different.
func TestEncodeStartSection(t *testing.T) {
	require.Equal(t, []byte{wasm.SectionIDStart, 0x01, 0x05}, binaryencoding.EncodeStartSection(5))
}

func TestDecodeDataCountSection(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		v, _, err := decodeDataCountSection([]byte{0x1}, 0)
		require.NoError(t, err)
		require.Equal(t, uint32(1), *v)
	})
	t.Run("eof", func(t *testing.T) {
		// EOF is fine as the datacount is optional.
		_, _, err := decodeDataCountSection([]byte{}, 0)
		require.NoError(t, err)
	})
}
