package native

import (
	"bytes"
	"crypto/sha256"
	"hash/crc32"
	"io"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/testing/require"
	"github.com/samyfodil/wazy/internal/u32"
	"github.com/samyfodil/wazy/internal/u64"
	"github.com/samyfodil/wazy/internal/wasm"
)

var testVersion = "0.0.1"

func crcf(b []byte) []byte {
	c := crc32.Checksum(b, crc)
	return u32.LeBytes(c)
}

func TestSerializeCompiledModule(t *testing.T) {
	tests := []struct {
		in  *compiledModule
		exp []byte
	}{
		{
			in: &compiledModule{
				executables:        &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets:    []int{0},
				ehTables:           [][]nativeapi.EhEntry{nil},
				functionFrameSizes: []int64{0},
			},
			exp: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1),                         // number of functions.
				u64.LeBytes(0),                         // offset.
				u64.LeBytes(5),                         // length of code.
				[]byte{1, 2, 3, 4, 5},                  // code.
				crcf([]byte{1, 2, 3, 4, 5}),            // crc for the code.
				[]byte{0},                              // no source map.
				[]byte{tryTableInfoFormatVersion},      // try-table info format version.
				u32.LeBytes(0),                         // empty catch clause table.
				[]byte{ehTableFormatVersion},           // eh table format version.
				u32.LeBytes(1),                         // number of functions (eh tables).
				u32.LeBytes(0),                         // func[0]: 0 eh entries.
				u32.LeBytes(1),                         // number of function frame sizes.
				u64.LeBytes(0),                         // func[0] frame size.
				[]byte{interruptIntervalFormatVersion}, // interrupt-interval format version.
				u64.LeBytes(0),                         // interrupt-check interval.
				[]byte{entryPreambleFormatVersion},     // entry preamble format version.
				u32.LeBytes(0),                         // no entry preambles.
			),
		},
		{
			in: &compiledModule{
				executables:        &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets:    []int{0},
				ehTables:           [][]nativeapi.EhEntry{nil},
				functionFrameSizes: []int64{0},
			},
			exp: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1),                         // number of functions.
				u64.LeBytes(0),                         // offset.
				u64.LeBytes(5),                         // length of code.
				[]byte{1, 2, 3, 4, 5},                  // code.
				crcf([]byte{1, 2, 3, 4, 5}),            // crc for the code.
				[]byte{0},                              // no source map.
				[]byte{tryTableInfoFormatVersion},      // try-table info format version.
				u32.LeBytes(0),                         // empty catch clause table.
				[]byte{ehTableFormatVersion},           // eh table format version.
				u32.LeBytes(1),                         // number of functions (eh tables).
				u32.LeBytes(0),                         // func[0]: 0 eh entries.
				u32.LeBytes(1),                         // number of function frame sizes.
				u64.LeBytes(0),                         // func[0] frame size.
				[]byte{interruptIntervalFormatVersion}, // interrupt-interval format version.
				u64.LeBytes(0),                         // interrupt-check interval.
				[]byte{entryPreambleFormatVersion},     // entry preamble format version.
				u32.LeBytes(0),                         // no entry preambles.
			),
		},
		{
			in: &compiledModule{
				executables:        &executables{executable: []byte{1, 2, 3, 4, 5, 1, 2, 3}},
				functionOffsets:    []int{0, 5},
				ehTables:           [][]nativeapi.EhEntry{nil, nil},
				functionFrameSizes: []int64{0, 0},
			},
			exp: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(2), // number of functions.
				// Function index = 0.
				u64.LeBytes(0), // offset.
				// Function index = 1.
				u64.LeBytes(5), // offset.
				// Executable.
				u64.LeBytes(8),                         // length of code.
				[]byte{1, 2, 3, 4, 5, 1, 2, 3},         // code.
				crcf([]byte{1, 2, 3, 4, 5, 1, 2, 3}),   // crc for the code.
				[]byte{0},                              // no source map.
				[]byte{tryTableInfoFormatVersion},      // try-table info format version.
				u32.LeBytes(0),                         // empty catch clause table.
				[]byte{ehTableFormatVersion},           // eh table format version.
				u32.LeBytes(2),                         // number of functions (eh tables).
				u32.LeBytes(0),                         // func[0]: 0 eh entries.
				u32.LeBytes(0),                         // func[1]: 0 eh entries.
				u32.LeBytes(2),                         // number of function frame sizes.
				u64.LeBytes(0),                         // func[0] frame size.
				u64.LeBytes(0),                         // func[1] frame size.
				[]byte{interruptIntervalFormatVersion}, // interrupt-interval format version.
				u64.LeBytes(0),                         // interrupt-check interval.
				[]byte{entryPreambleFormatVersion},     // entry preamble format version.
				u32.LeBytes(0),                         // no entry preambles.
			),
		},
	}

	for i, tc := range tests {
		actual, err := io.ReadAll(serializeCompiledModule(testVersion, tc.in))
		require.NoError(t, err, i)
		require.Equal(t, tc.exp, actual, i)
	}
}

func concat(ins ...[]byte) (ret []byte) {
	for _, in := range ins {
		ret = append(ret, in...)
	}
	return
}

func TestDeserializeCompiledModule(t *testing.T) {
	tests := []struct {
		name                  string
		in                    []byte
		importedFunctionCount uint32
		expCompiledModule     *compiledModule
		expStaleCache         bool
		expErr                string
	}{
		{
			// With a buffered reader + io.ReadFull, a short input now surfaces as an
			// io.ErrUnexpectedEOF from the read itself rather than reaching the
			// (now practically unreachable) post-read length check.
			name:   "invalid header",
			in:     []byte{1},
			expErr: "compilationcache: error reading header: unexpected EOF",
		},
		{
			name: "invalid magic",
			in: concat(
				[]byte{'a', 'b', 'c', 'd', 'e', 'f'},
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
			),
			expErr: "compilationcache: invalid magic number: got NATIVE but want abcdef",
		},
		{
			name: "version mismatch",
			in: concat(
				magic,
				[]byte{byte(len("1233123.1.1"))},
				[]byte("1233123.1.1"),
				u32.LeBytes(1), // number of functions.
			),
			expStaleCache: true,
		},
		{
			name: "version mismatch",
			in: concat(
				magic,
				[]byte{byte(len("0.0.0"))},
				[]byte("0.0.0"),
				u32.LeBytes(1), // number of functions.
			),
			expStaleCache: true,
		},
		{
			name: "one function",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),                         // size.
				[]byte{1, 2, 3, 4, 5},                  // machine code.
				crcf([]byte{1, 2, 3, 4, 5}),            // machine code.
				[]byte{0},                              // no source map.
				[]byte{tryTableInfoFormatVersion},      // try-table info format version.
				u32.LeBytes(0),                         // empty catch clause table.
				[]byte{ehTableFormatVersion},           // eh table format version.
				u32.LeBytes(1),                         // number of functions (eh tables).
				u32.LeBytes(0),                         // func[0]: 0 eh entries.
				u32.LeBytes(1),                         // number of function frame sizes.
				u64.LeBytes(0),                         // func[0] frame size.
				[]byte{interruptIntervalFormatVersion}, // interrupt-interval format version.
				u64.LeBytes(64),                        // interrupt-check interval.
				[]byte{entryPreambleFormatVersion},     // entry preamble format version.
				u32.LeBytes(0),                         // no entry preambles.
			),
			expCompiledModule: &compiledModule{
				executables:            &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets:        []int{0},
				ehTables:               [][]nativeapi.EhEntry{nil},
				functionFrameSizes:     []int64{0},
				interruptCheckInterval: 64,
			},
			expStaleCache: false,
			expErr:        "",
		},
		{
			name: "two functions",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(2), // number of functions.
				// Function index = 0.
				u64.LeBytes(0), // offset.
				// Function index = 1.
				u64.LeBytes(7), // offset.
				// Executable.
				u64.LeBytes(10),                             // size.
				[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},       // machine code.
				crcf([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}), // crc for machine code.
				[]byte{0},                              // no source map.
				[]byte{tryTableInfoFormatVersion},      // try-table info format version.
				u32.LeBytes(0),                         // empty catch clause table.
				[]byte{ehTableFormatVersion},           // eh table format version.
				u32.LeBytes(2),                         // number of functions (eh tables).
				u32.LeBytes(0),                         // func[0]: 0 eh entries.
				u32.LeBytes(0),                         // func[1]: 0 eh entries.
				u32.LeBytes(2),                         // number of function frame sizes.
				u64.LeBytes(0),                         // func[0] frame size.
				u64.LeBytes(0),                         // func[1] frame size.
				[]byte{interruptIntervalFormatVersion}, // interrupt-interval format version.
				u64.LeBytes(64),                        // interrupt-check interval.
				[]byte{entryPreambleFormatVersion},     // entry preamble format version.
				u32.LeBytes(0),                         // no entry preambles.
			),
			importedFunctionCount: 1,
			expCompiledModule: &compiledModule{
				executables:            &executables{executable: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
				functionOffsets:        []int{0, 7},
				ehTables:               [][]nativeapi.EhEntry{nil, nil},
				functionFrameSizes:     []int64{0, 0},
				interruptCheckInterval: 64,
			},
			expStaleCache: false,
			expErr:        "",
		},
		{
			name: "old cache without catch clause table (stale)",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),              // size.
				[]byte{1, 2, 3, 4, 5},       // machine code.
				crcf([]byte{1, 2, 3, 4, 5}), // machine code.
				[]byte{0},                   // no source map.
				// no catch clause table — old format.
			),
			expStaleCache: true,
		},
		{
			// Simulates a cache entry written before the exception side
			// table (P0) was added: the (old-format) catch-clause table is
			// present, but there's nothing after it. This must be detected
			// as stale via the ehTableFormatVersion marker byte, not
			// misparsed (e.g. by reading unrelated trailing garbage as if
			// it were the marker and coincidentally matching, or by reading
			// past EOF and surfacing a plain error instead of staleness).
			name: "old cache without eh table (stale)",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),              // size.
				[]byte{1, 2, 3, 4, 5},       // machine code.
				crcf([]byte{1, 2, 3, 4, 5}), // machine code.
				[]byte{0},                   // no source map.
				u32.LeBytes(0),              // empty catch clause table.
				// no eh table section at all — pre-side-table format.
			),
			expStaleCache: true,
		},
		{
			// Same as above, but with a byte present at the marker position
			// that doesn't match the current ehTableFormatVersion (e.g. a
			// future/older layout version) -- must also be stale, not
			// misparsed.
			name: "eh table format version mismatch (stale)",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),                    // size.
				[]byte{1, 2, 3, 4, 5},             // machine code.
				crcf([]byte{1, 2, 3, 4, 5}),       // machine code.
				[]byte{0},                         // no source map.
				[]byte{tryTableInfoFormatVersion}, // try-table info format version.
				u32.LeBytes(0),                    // empty catch clause table.
				[]byte{ehTableFormatVersion + 1},  // wrong eh table format version.
				u32.LeBytes(1),
				u32.LeBytes(0),
				u32.LeBytes(1),
				u64.LeBytes(0),
			),
			expStaleCache: true,
		},
		{
			name: "reading executable offset",
			in: concat(
				[]byte(magic),
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(2), // number of functions.
				// Function index = 0.
				u64.LeBytes(5), // offset.
				// Function index = 1.
			),
			expErr: "compilationcache: error reading func[1] executable offset: EOF",
		},
		{
			name: "mmapping",
			in: concat(
				[]byte(magic),
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(2), // number of functions.
				// Function index = 0.
				u64.LeBytes(0), // offset.
				// Function index = 1.
				u64.LeBytes(5), // offset.
				// Executable.
				u64.LeBytes(5), // size of the executable.
				// Lack of machine code here.
			),
			expErr: "compilationcache: error reading executable (len=5): EOF",
		},
		{
			name: "bad crc",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),        // size.
				[]byte{1, 2, 3, 4, 5}, // machine code.
				[]byte{1, 2, 3, 4},    // crc for machine code.
			),
			expCompiledModule: &compiledModule{
				executables:     &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets: []int{0},
			},
			expStaleCache: false,
			expErr:        "compilationcache: checksum mismatch (expected 1397854123, got 67305985)",
		},
		{
			name: "missing crc",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),        // size.
				[]byte{1, 2, 3, 4, 5}, // machine code.
			),
			expCompiledModule: &compiledModule{
				executables:     &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets: []int{0},
			},
			expStaleCache: false,
			expErr:        "compilationcache: could not read checksum: EOF",
		},
		{
			name: "no source map presence",
			in: concat(
				magic,
				[]byte{byte(len(testVersion))},
				[]byte(testVersion),
				u32.LeBytes(1), // number of functions.
				u64.LeBytes(0), // offset.
				// Executable.
				u64.LeBytes(5),              // size.
				[]byte{1, 2, 3, 4, 5},       // machine code.
				crcf([]byte{1, 2, 3, 4, 5}), // crc for machine code.
			),
			expCompiledModule: &compiledModule{
				executables:     &executables{executable: []byte{1, 2, 3, 4, 5}},
				functionOffsets: []int{0},
			},
			expStaleCache: false,
			expErr:        "compilationcache: error reading source map presence: EOF",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cm, staleCache, err := deserializeCompiledModule(testVersion, io.NopCloser(bytes.NewReader(tc.in)))

			if tc.expErr != "" {
				require.EqualError(t, err, tc.expErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.expCompiledModule, cm)
			}

			require.Equal(t, tc.expStaleCache, staleCache)
		})
	}
}

// TestSerializeDeserializeEntryPreambles exercises the numPreambles>0 path:
// a round-trip must preserve the preamble pointers and blob bytes, and a
// corrupted blob must be rejected by the CRC32 checksum.
func TestSerializeDeserializeEntryPreambles(t *testing.T) {
	// Distinct from the executable bytes below so bytes.Index locates the blob
	// unambiguously.
	blob := make([]byte, 32)
	for i := range blob {
		blob[i] = byte(i + 100)
	}
	cm := &compiledModule{
		executables:        &executables{executable: []byte{1, 2, 3, 4, 5}},
		functionOffsets:    []int{0},
		ehTables:           [][]nativeapi.EhEntry{nil},
		functionFrameSizes: []int64{0},
	}
	cm.entryPreambles = mmapExecutable(blob)
	cm.entryPreamblesPtrs = []*byte{&cm.entryPreambles[0], &cm.entryPreambles[16]}

	serialized, err := io.ReadAll(serializeCompiledModule(testVersion, cm))
	require.NoError(t, err)

	// Positive: round-trip preserves the two preambles and the blob bytes.
	got, stale, err := deserializeCompiledModule(testVersion, io.NopCloser(bytes.NewReader(serialized)))
	require.NoError(t, err)
	require.False(t, stale)
	require.Equal(t, 2, len(got.entryPreamblesPtrs))
	require.Equal(t, blob, got.entryPreambles)
	// The second pointer must land exactly at offset 16 of the loaded blob.
	require.True(t, got.entryPreamblesPtrs[1] == &got.entryPreambles[16])

	// Negative: corrupting a single blob byte must fail the checksum.
	idx := bytes.Index(serialized, blob)
	require.True(t, idx >= 0)
	corrupt := make([]byte, len(serialized))
	copy(corrupt, serialized)
	corrupt[idx] ^= 0xFF
	_, _, err = deserializeCompiledModule(testVersion, io.NopCloser(bytes.NewReader(corrupt)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "entry preamble checksum mismatch")
}

func Test_fileCacheKey(t *testing.T) {
	s := sha256.New()
	s.Write([]byte("hello world"))
	m := &wasm.Module{}
	s.Sum(m.ID[:0])
	original := m.ID
	result := fileCacheKey(m)
	require.Equal(t, original, m.ID)
	require.NotEqual(t, original, result)
}
