package bench

import (
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/wasm"
	"github.com/samyfodil/wazy/internal/wasm/binary"
)

func BenchmarkCodec(b *testing.B) {
	b.Run("binary.DecodeModule", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := binary.DecodeModule(caseWasm, api.CoreFeaturesV2, wasm.MemoryLimitPages, false, 0, false, false); err != nil {
				b.Fatal(err)
			}
		}
	})
}
