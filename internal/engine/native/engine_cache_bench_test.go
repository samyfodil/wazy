package native

import (
	"io"
	"testing"

	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
)

// BenchmarkSerializeCompiledModule isolates the A5 win: the serializer must not
// make a full copy of cm.executable (the native code, which dominates the
// artifact) into an intermediate bytes.Buffer. A realistically-sized executable
// makes the copy/realloc traffic visible in allocs/op and B/op.
func BenchmarkSerializeCompiledModule(b *testing.B) {
	const execLen = 512 << 10 // 512 KiB of "native code".
	const nFuncs = 256
	exec := make([]byte, execLen)
	offsets := make([]int, nFuncs)
	eh := make([][]nativeapi.EhEntry, nFuncs)
	frames := make([]int64, nFuncs)
	for i := range offsets {
		offsets[i] = i * (execLen / nFuncs)
	}
	cm := &compiledModule{
		executables:        &executables{executable: exec},
		functionOffsets:    offsets,
		ehTables:           eh,
		functionFrameSizes: frames,
	}

	b.ReportAllocs()
	b.SetBytes(execLen)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// io.Copy to Discard drains the reader exactly as fileCache.Add does,
		// without retaining the bytes.
		if _, err := io.Copy(io.Discard, serializeCompiledModule(testVersion, cm)); err != nil {
			b.Fatal(err)
		}
	}
}
