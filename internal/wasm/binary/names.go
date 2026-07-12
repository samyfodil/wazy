package binary

import (
	"fmt"
	"io"

	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

const (
	// subsectionIDModuleName contains only the module name.
	subsectionIDModuleName = uint8(0)
	// subsectionIDFunctionNames is a map of indices to function names, in ascending order by function index
	subsectionIDFunctionNames = uint8(1)
	// subsectionIDLocalNames contain a map of function indices to a map of local indices to their names, in ascending
	// order by function and local index
	subsectionIDLocalNames = uint8(2)
)

// decodeNameSection deserializes the data associated with the "name" key in SectionIDCustom according to the
// standard:
//
// * ModuleName decode from subsection 0
// * FunctionNames decode from subsection 1
// * LocalNames decode from subsection 2
//
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#binary-namesec
func decodeNameSection(buf []byte, offset int, arena *stringArena, limit uint64) (result *wasm.NameSection, newOffset int, err error) {
	result = &wasm.NameSection{}

	// subsectionID is decoded if known, and skipped if not
	var subsectionID uint8
	// subsectionSize is the length to skip when the subsectionID is unknown
	var subsectionSize uint32
	var bytesRead uint64
	for limit > 0 {
		if subsectionID, offset, err = readByte(buf, offset); err != nil {
			if err == io.EOF {
				return result, offset, nil
			}
			// TODO: untestable as this can't fail for a reason beside EOF reading a byte from a buffer
			return nil, offset, fmt.Errorf("failed to read a subsection ID: %w", err)
		}
		limit--

		if subsectionSize, bytesRead, err = leb128.LoadUint32(buf[offset:]); err != nil {
			return nil, offset, fmt.Errorf("failed to read the size of subsection[%d]: %w", subsectionID, err)
		}
		offset += int(bytesRead)
		limit -= bytesRead

		switch subsectionID {
		case subsectionIDModuleName:
			if result.ModuleName, offset, err = decodeUTF8(buf, offset, arena, "module name"); err != nil {
				return nil, offset, err
			}
		case subsectionIDFunctionNames:
			if result.FunctionNames, offset, err = decodeFunctionNames(buf, offset, arena); err != nil {
				return nil, offset, err
			}
		case subsectionIDLocalNames:
			if result.LocalNames, offset, err = decodeLocalNames(buf, offset, arena); err != nil {
				return nil, offset, err
			}
		default: // Skip other subsections.
			// Note: this mirrors io.CopyN's behavior (used here prior to the slice-indexed rewrite), which
			// reports io.EOF whenever fewer than subsectionSize bytes remain - unlike io.ReadFull, it never
			// reports io.ErrUnexpectedEOF for a partial remainder.
			if len(buf)-offset < int(subsectionSize) {
				return nil, offset, fmt.Errorf("failed to skip subsection[%d]: %w", subsectionID, io.EOF)
			}
			offset += int(subsectionSize)
		}
		limit -= uint64(subsectionSize)
	}
	return result, offset, nil
}

func decodeFunctionNames(buf []byte, offset int, arena *stringArena) (wasm.NameMap, int, error) {
	functionCount, offset, err := decodeFunctionCount(buf, offset, subsectionIDFunctionNames)
	if err != nil {
		return nil, offset, err
	}

	result := make(wasm.NameMap, functionCount)
	for i := uint32(0); i < functionCount; i++ {
		functionIndex, o, err := decodeFunctionIndex(buf, offset, subsectionIDFunctionNames)
		if err != nil {
			return nil, offset, err
		}
		offset = o

		name, o, err := decodeUTF8(buf, offset, arena, "function[%d] name", functionIndex)
		if err != nil {
			return nil, offset, err
		}
		offset = o
		result[i] = wasm.NameAssoc{Index: functionIndex, Name: name}
	}
	return result, offset, nil
}

func decodeLocalNames(buf []byte, offset int, arena *stringArena) (wasm.IndirectNameMap, int, error) {
	functionCount, offset, err := decodeFunctionCount(buf, offset, subsectionIDLocalNames)
	if err != nil {
		return nil, offset, err
	}

	result := make(wasm.IndirectNameMap, functionCount)
	for i := uint32(0); i < functionCount; i++ {
		functionIndex, o, err := decodeFunctionIndex(buf, offset, subsectionIDLocalNames)
		if err != nil {
			return nil, offset, err
		}
		offset = o

		localCount, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return nil, offset, fmt.Errorf("failed to read the local count for function[%d]: %w", functionIndex, err)
		}
		offset += int(n)

		locals := make(wasm.NameMap, localCount)
		for j := uint32(0); j < localCount; j++ {
			localIndex, n, err := leb128.LoadUint32(buf[offset:])
			if err != nil {
				return nil, offset, fmt.Errorf("failed to read a local index of function[%d]: %w", functionIndex, err)
			}
			offset += int(n)

			name, o, err := decodeUTF8(buf, offset, arena, "function[%d] local[%d] name", functionIndex, localIndex)
			if err != nil {
				return nil, offset, err
			}
			offset = o
			locals[j] = wasm.NameAssoc{Index: localIndex, Name: name}
		}
		result[i] = wasm.NameMapAssoc{Index: functionIndex, NameMap: locals}
	}
	return result, offset, nil
}

func decodeFunctionIndex(buf []byte, offset int, subsectionID uint8) (uint32, int, error) {
	functionIndex, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return 0, offset, fmt.Errorf("failed to read a function index in subsection[%d]: %w", subsectionID, err)
	}
	return functionIndex, offset + int(n), nil
}

func decodeFunctionCount(buf []byte, offset int, subsectionID uint8) (uint32, int, error) {
	functionCount, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return 0, offset, fmt.Errorf("failed to read the function count of subsection[%d]: %w", subsectionID, err)
	}
	return functionCount, offset + int(n), nil
}
