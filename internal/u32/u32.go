package u32

// LeBytes returns a byte slice corresponding to the 4 bytes in the uint32 in little-endian byte order.
func LeBytes(v uint32) []byte {
	return []byte{
		byte(v),
		byte(v >> 8),
		byte(v >> 16),
		byte(v >> 24),
	}
}

// OutOfRange reports whether index, read out of a module or a component, falls outside a slice of the given
// length.
//
// Use it in preference to "int(index) >= len(s)". An int is 32 bits wide on GOARCH=386, arm and wasm, where
// int(index) of anything above math.MaxInt32 is negative -- so the naive form accepts the index and the very next
// line panics. Indices come straight out of the binary, or out of guest memory, so malformed input reaches it.
// Widening both sides to uint64 is exact on every platform, and inlines away.
func OutOfRange(index uint32, length int) bool {
	return uint64(index) >= uint64(length)
}
