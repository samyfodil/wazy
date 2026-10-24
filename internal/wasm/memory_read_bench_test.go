package wasm

import "testing"

// What the released check costs on the api.Memory path, which is where it
// lives. The guest never arrives here: both engines index mem.Buffer directly.
func BenchmarkMemoryRead(b *testing.B) {
	m := &MemoryInstance{Buffer: make([]byte, 65536), Min: 1, Cap: 1, Max: 1}
	m.sizeBytes = uint64(len(m.Buffer))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m.Read(0, 8); !ok {
			b.Fatal("read failed")
		}
	}
}

func BenchmarkMemoryReadUint32Le(b *testing.B) {
	m := &MemoryInstance{Buffer: make([]byte, 65536), Min: 1, Cap: 1, Max: 1}
	m.sizeBytes = uint64(len(m.Buffer))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := m.ReadUint32Le(0); !ok {
			b.Fatal("read failed")
		}
	}
}
