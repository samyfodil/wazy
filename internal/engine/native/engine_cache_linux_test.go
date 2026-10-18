package native

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"unsafe"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// A file cache hit maps the code from the cache file rather than copying it
// into anonymous memory.
func TestDeserializeCompiledModule_mapsCodeFromFile(t *testing.T) {
	code := []byte{1, 2, 3, 4, 5}
	entry, err := io.ReadAll(serializeCompiledModule(testVersion, &compiledModule{
		executables:     &executables{executable: code},
		functionOffsets: []int{0},
		ehTables:        make([][]nativeapi.EhEntry, 1),
	}))
	require.NoError(t, err)
	f := cacheFile(t, entry)
	requireExecutableMapping(t, f)

	cm, staleCache, err := deserializeCompiledModule(testVersion, f)
	require.NoError(t, err)
	require.False(t, staleCache)
	require.Equal(t, code, cm.executable)
	defer func() { require.NoError(t, platform.MunmapCodeSegment(cm.executable)) }()

	require.Equal(t, f.Name(), mappingPath(t, uintptr(unsafe.Pointer(&cm.executable[0]))))
}

// The mapped code is released when the rest of the entry fails to decode, or
// turns out to be stale.
func TestDeserializeCompiledModule_unmapsCodeOnError(t *testing.T) {
	entry, err := io.ReadAll(serializeCompiledModule(testVersion, &compiledModule{
		executables:     &executables{executable: []byte{1, 2, 3, 4, 5}},
		functionOffsets: []int{0},
		ehTables:        make([][]nativeapi.EhEntry, 1),
	}))
	require.NoError(t, err)
	codeEnd := executablePadding(len(magic)+1+len(testVersion)+4+8+8) + len(magic) + 1 + len(testVersion) + 4 + 8 + 8 + 5 + 4

	t.Run("truncated", func(t *testing.T) {
		f := cacheFile(t, entry[:codeEnd])
		requireExecutableMapping(t, f)
		_, _, err := deserializeCompiledModule(testVersion, f)
		require.EqualError(t, err, "compilationcache: error reading source map presence: EOF")
		require.False(t, isMapped(t, f.Name()))
	})
	t.Run("stale", func(t *testing.T) {
		stale := append([]byte(nil), entry...)
		// Everything up to the entry preambles section decodes; a format
		// version it doesn't know makes the entry stale.
		stale[len(stale)-5] = entryPreambleFormatVersion + 1
		f := cacheFile(t, stale)
		requireExecutableMapping(t, f)
		cm, staleCache, err := deserializeCompiledModule(testVersion, f)
		require.NoError(t, err)
		require.True(t, staleCache)
		require.Nil(t, cm)
		require.False(t, isMapped(t, f.Name()))
	})
}

// requireExecutableMapping skips the test if f can't be mapped executable,
// for example because it is on a noexec mount, as /tmp is in some containers.
func requireExecutableMapping(t *testing.T, f *os.File) {
	code, err := platform.MapCodeSegmentFromFile(f, 0, 1)
	if err != nil {
		t.Skipf("cannot map %s as code: %v", f.Name(), err)
	}
	require.NoError(t, platform.MunmapCodeSegment(code))
}

func isMapped(t *testing.T, path string) bool {
	maps, err := os.ReadFile("/proc/self/maps")
	require.NoError(t, err)
	return strings.Contains(string(maps), path)
}

// mappingPath returns the file backing the mapping that contains addr, from
// /proc/self/maps.
func mappingPath(t *testing.T, addr uintptr) string {
	maps, err := os.ReadFile("/proc/self/maps")
	require.NoError(t, err)
	for _, line := range strings.Split(string(maps), "\n") {
		var start, end uintptr
		if _, err := fmt.Sscanf(line, "%x-%x", &start, &end); err != nil || addr < start || addr >= end {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return ""
		}
		return fields[5]
	}
	t.Fatalf("no mapping contains %#x", addr)
	return ""
}
