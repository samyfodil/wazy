package binary

import (
	"runtime"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/testing/binaryencoding"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/wasm"
)

func TestTableSection(t *testing.T) {
	three := uint64(3)
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
	max := wasm.MemoryLimitPages
	max64 := uint64(max)

	three := uint64(3)
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
				{Min: 1, Cap: max64, Max: max64, IsMaxEncoded: false},
				{Min: 1, Cap: max64, Max: max64, IsMaxEncoded: false},
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
			expectedErr:      "total memory minimum across 2 memories (80000 pages) exceeds 65536 pages",
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

func TestImportSection_PerModuleRuns(t *testing.T) {
	imp := func(module, name string) []byte {
		b := []byte{byte(len(module))}
		b = append(b, module...)
		b = append(b, byte(len(name)))
		b = append(b, name...)
		return append(b, wasm.ExternTypeFunc, 0x00)
	}
	// "env" is interrupted by "other" and then resumed: the case the contiguous-run carving falls back to
	// appending for.
	in := []byte{0x04}
	in = append(in, imp("env", "one")...)
	in = append(in, imp("env", "two")...)
	in = append(in, imp("other", "three")...)
	in = append(in, imp("env", "four")...)

	names := func(imports []*wasm.Import) []string {
		var ret []string
		for _, i := range imports {
			ret = append(ret, i.Name)
		}
		return ret
	}

	_, perModule, funcCount, _, _, _, _, _, err := decodeImportSection(in, 0, &stringArena{},
		newMemorySizer(wasm.MemoryLimitPages, false, 0), wasm.MemoryLimitPages, api.CoreFeaturesV2)
	require.NoError(t, err)
	require.Equal(t, wasm.Index(4), funcCount)
	require.Equal(t, 2, len(perModule))
	require.Equal(t, []string{"one", "two", "four"}, names(perModule["env"]))
	require.Equal(t, []string{"three"}, names(perModule["other"]))
	// Each run is capacity-capped, so resuming "env" reallocated instead of overwriting "other"'s entry.
	require.Equal(t, len(perModule["other"]), cap(perModule["other"]))
}

func TestCodeSection_ArenaIsBoundedByTheInput(t *testing.T) {
	// A section header's declared size is only checked against the bytes actually decoded *after* the
	// section is read, so it is attacker controlled here: sizing the body arena from it alone would let
	// this 5-byte section ask for 256 MiB.
	const declared = uint32(256 << 20)
	in := []byte{
		0x01,       // 1 function body
		0x03,       // its size is 3 bytes
		0x00,       // no locals
		0x01, 0x0b, // nop, end
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	code, _, err := decodeCodeSection(in, 0, declared, api.CoreFeaturesV2)
	runtime.ReadMemStats(&after)

	require.NoError(t, err)
	require.Equal(t, []byte{0x01, 0x0b}, code[0].Body)
	require.True(t, after.TotalAlloc-before.TotalAlloc < 1<<20,
		"decoding a %d byte section allocated %d bytes", len(in), after.TotalAlloc-before.TotalAlloc)
}

func TestTypeSection_ResultIsBoundedByTheInput(t *testing.T) {
	// The vector count is untrusted too: sizing the result from it alone would turn this 6-byte section
	// into a 1,000,000 * sizeof(FunctionType) allocation.
	in := append(leb128.EncodeUint32(1_000_000), 0x60, 0x00, 0x00) // one (func), then nothing

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := decodeTypeSection(api.CoreFeaturesV2, in, 0, uint32(len(in)))
	runtime.ReadMemStats(&after)

	require.Error(t, err) // the count outruns the section
	require.True(t, after.TotalAlloc-before.TotalAlloc < 1<<20,
		"decoding a %d byte section allocated %d bytes", len(in), after.TotalAlloc-before.TotalAlloc)
}

func TestDataSection_SegmentsShareOneArena(t *testing.T) {
	in := []byte{
		0x02, // 2 segments
		0x00, wasm.OpcodeI32Const, 0x01, wasm.OpcodeEnd, 0x02, 0xaa, 0xbb,
		0x00, wasm.OpcodeI32Const, 0x08, wasm.OpcodeEnd, 0x03, 0x0c, 0x0d, 0x0e,
	}
	segments, _, err := decodeDataSection(in, 0, uint32(len(in)), api.CoreFeaturesV2)
	require.NoError(t, err)
	require.Equal(t, 2, len(segments))
	require.Equal(t, []byte{0xaa, 0xbb}, segments[0].Init)
	require.Equal(t, []byte{0x0c, 0x0d, 0x0e}, segments[1].Init)
	require.Equal(t, []byte{wasm.OpcodeI32Const, 0x01, wasm.OpcodeEnd}, segments[0].OffsetExpression.Data)
	require.Equal(t, []byte{wasm.OpcodeI32Const, 0x08, wasm.OpcodeEnd}, segments[1].OffsetExpression.Data)
	// Capacity-capped: appending to one segment's Init cannot overwrite the next segment's bytes.
	require.Equal(t, len(segments[0].Init), cap(segments[0].Init))
	segments[0].Init = append(segments[0].Init, 0xff)
	require.Equal(t, []byte{0x0c, 0x0d, 0x0e}, segments[1].Init)
}
