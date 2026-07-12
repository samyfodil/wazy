package binary

import (
	"fmt"
	"io"
	"math"

	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

// localsGroup is one decoded (type, count) run of locals. It holds only value types (no pointers), so the
// backing scratch is safe to reuse across every function in a code section.
type localsGroup struct {
	vt  wasm.ValueType
	num uint32
}

// decodeCode decodes one function body into arena[arenaOff:] and returns the new input offset and the new arena
// offset. groups is a caller-owned scratch, reused across the section, into which the single locals pass records
// its (type, count) runs before LocalTypes is materialized once.
func decodeCode(buf []byte, offset, codeSectionStart int, arena []byte, arenaOff int, localTypesArena *valueTypeArena, groups *[]localsGroup, ret *wasm.Code) (newOffset, newArenaOff int, err error) {
	ss, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, arenaOff, fmt.Errorf("get the size of code: %w", err)
	}
	offset += int(n)
	remaining := int64(ss)

	// Parse #locals.
	ls, bytesRead, err := leb128.LoadUint32(buf[offset:])
	offset += int(bytesRead)
	remaining -= int64(bytesRead)
	if err != nil {
		return offset, arenaOff, fmt.Errorf("get the size locals: %v", err)
	} else if remaining < 0 {
		return offset, arenaOff, io.EOF
	}

	// Single pass over the locals. The previous code walked them twice (a probe that summed the counts and
	// validated types, then a rewind that re-decoded every LEB128 to expand LocalTypes). Here each (count, type)
	// run is decoded exactly once, recorded in the reusable groups scratch, and the total accumulated so
	// LocalTypes can be sized and filled in one shot below. The remaining-bytes accounting and error strings are
	// kept identical to the old second pass; type validation (formerly only in the first pass) is folded in via
	// the switch's default arm.
	g := (*groups)[:0]
	var sum uint64
	for i := uint32(0); i < ls; i++ {
		num, n, err := leb128.LoadUint32(buf[offset:])
		offset += int(n)
		remaining -= int64(n) + 1 // +1 for the subsequent type byte
		if err != nil {
			return offset, arenaOff, fmt.Errorf("read n of locals: %v", err)
		} else if remaining < 0 {
			return offset, arenaOff, io.EOF
		}

		b, o, err := readByte(buf, offset)
		if err != nil {
			return offset, arenaOff, fmt.Errorf("read type of local: %v", err)
		}
		offset = o

		var vt wasm.ValueType
		switch vtb := wasm.ValueType(b); vtb {
		case wasm.ValueTypeI32, wasm.ValueTypeF32, wasm.ValueTypeI64, wasm.ValueTypeF64,
			wasm.ValueTypeFuncref, wasm.ValueTypeExternref, wasm.ValueTypeV128,
			wasm.ValueTypeExnref:
			vt = vtb
		default:
			switch b {
			case wasm.RefPrefixNullable, wasm.RefPrefixNonNullable:
				before := offset
				vt, offset, err = decodeRefType(buf, offset, b == wasm.RefPrefixNullable)
				if err != nil {
					return offset, arenaOff, err
				}
				remaining -= int64(offset - before)
			default:
				return offset, arenaOff, fmt.Errorf("invalid local type: 0x%x", b)
			}
		}

		sum += uint64(num)
		g = append(g, localsGroup{vt: vt, num: num})
	}
	*groups = g // keep the (possibly grown) backing array for the next function in this section.

	if sum > math.MaxUint32 {
		return offset, arenaOff, fmt.Errorf("too many locals: %d", sum)
	}

	// Materialize LocalTypes once, into the section-wide arena when there are any locals. For the (common)
	// no-locals case keep the previous non-nil, zero-length, heap-free slice so downstream equality/len behavior
	// is unchanged.
	var localTypes []wasm.ValueType
	if sum > 0 {
		localTypes = localTypesArena.alloc(int(sum))
		idx := 0
		for _, grp := range g {
			for j := uint32(0); j < grp.num; j++ {
				localTypes[idx] = grp.vt
				idx++
			}
		}
	} else {
		localTypes = make([]wasm.ValueType, 0)
	}

	bodyOffsetInCodeSection := uint64(offset - codeSectionStart)
	bodyLen := int(remaining)
	bodySrc, o, err := readBytes(buf, offset, bodyLen)
	if err != nil {
		return offset, arenaOff, fmt.Errorf("read body: %w", err)
	}
	offset = o

	// Copy the body into the section arena. Well-formed modules always fit (bodies are a subset of the section);
	// the fallback only triggers on malformed input whose declared body size overruns the arena, where it keeps
	// behavior identical to the old per-body allocation instead of panicking on the arena slice bounds.
	var body []byte
	if arenaOff+bodyLen <= len(arena) {
		body = arena[arenaOff : arenaOff+bodyLen : arenaOff+bodyLen]
		arenaOff += bodyLen
	} else {
		body = make([]byte, bodyLen)
	}
	copy(body, bodySrc)

	if endIndex := len(body) - 1; endIndex < 0 || body[endIndex] != wasm.OpcodeEnd {
		return offset, arenaOff, fmt.Errorf("expr not end with OpcodeEnd")
	}

	ret.BodyOffsetInCodeSection = bodyOffsetInCodeSection
	ret.LocalTypes = localTypes
	ret.Body = body
	return offset, arenaOff, nil
}
