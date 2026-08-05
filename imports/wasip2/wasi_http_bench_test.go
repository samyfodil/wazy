package wasip2

import (
	"context"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/component/abi"
)

// Benchmarks for the response-header read path. fields.entries is the one
// call whose cost scales with the whole header block (see its doc), so its
// number is worth being able to reproduce.
func benchFields(n, valueLen int) *httpFields {
	f := &httpFields{}
	for i := 0; i < n; i++ {
		v := make([]byte, valueLen)
		for j := range v {
			v[j] = 'a'
		}
		f.names = append(f.names, fmt.Sprintf("X-Header-%d", i))
		f.values = append(f.values, v)
	}
	return f
}

func BenchmarkFieldsEntries(b *testing.B) {
	for _, tc := range []struct{ n, vlen int }{
		{8, 32},    // a typical response
		{20, 64},   // a chatty one
		{40, 4096}, // pathological: big values (cookies, CSP)
	} {
		b.Run(fmt.Sprintf("n%d_len%d", tc.n, tc.vlen), func(b *testing.B) {
			h := newTestHTTP()
			rep := h.newFieldsRep(benchFields(tc.n, tc.vlen))
			args := []abi.Value{rep}
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := h.fieldsEntries(ctx, args); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkIncomingResponseHeaders(b *testing.B) {
	h := newTestHTTP()
	rep := h.newInResponseRep(&httpIncomingResponse{status: 200, headers: benchFields(8, 32)})
	args := []abi.Value{rep}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.incomingResponseHeaders(ctx, args); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInputStreamRead measures the biggest consumer of the list<u8>
// lowering: a file/body read, which may carry up to wasiMaxStreamRead bytes
// in a single call.
func BenchmarkInputStreamRead(b *testing.B) {
	for _, size := range []int{4096, 65536, 1 << 20} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			payload := make([]byte, size)
			t := &testing.T{}
			h, resources, _ := wasiFSConfigDir(t, map[string][]byte{"/f": payload})
			rootRep := rootHandleRep(t, resources, rootDescriptorHandle(t, h))
			fileRep := openRep(t, h, resources, rootRep, "f", 0, 0)
			read := wasiFSFn(t, h, wasiIfaceStreams, "[method]input-stream.read")
			ctx := context.Background()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rv := callFS(t, h, "[method]descriptor.read-via-stream", fileRep, uint64(0))
				sh := rv.Payload.(uint32)
				sRep, _ := resources.Rep(wasiInputStreamResType, sh)
				if _, err := read(ctx, []abi.Value{sRep, uint64(size)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
