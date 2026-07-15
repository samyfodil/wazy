package abi

// Value represents a component model value in Go.
// It is an alias for any, so callers can pass plain Go values directly
// (no wrapper constructors required). This mirrors the canonical ABI's
// value representation.
//
// Go type mappings for WIT types:
//   - bool -> bool
//   - s8/s16/s32 -> int32
//   - u8/u16/u32 -> uint32
//   - s64 -> int64
//   - u64 -> uint64
//   - f32 -> float32
//   - f64 -> float64
//   - char -> rune
//   - string -> string
//   - list<T> -> []Value
//   - record -> []Value (field order, not map)
//   - tuple -> []Value
//   - variant -> VariantValue
//   - enum -> uint32 (case index)
//   - flags -> uint32 (bitset, LSB=first label)
//   - option<T> -> nil (none) or the inner value (some)
//   - result<T, E> -> ResultValue
//   - own<R> / borrow<R> -> uint32 (handle)
type Value = any

// VariantValue represents a variant type value (discriminated union).
// Disc is the 0-based case index, Payload is the case value (or nil if no payload).
type VariantValue struct {
	Disc    uint32 `json:"disc"`    // discriminant: which case
	Payload Value  `json:"payload"` // case payload, or nil if case has no type
}

// ResultValue represents a result type (either ok or error).
// IsErr determines whether this is an error (true) or ok (false).
// Payload is the value (or nil if that arm has no type).
type ResultValue struct {
	IsErr   bool  `json:"isErr"`
	Payload Value `json:"payload"`
}
