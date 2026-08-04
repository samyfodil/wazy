package instance

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
