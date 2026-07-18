package binary

import (
	"fmt"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/leb128"
	"github.com/samyfodil/wazy/internal/wasm"
)

func decodeImport(
	buf []byte,
	offset int,
	idx uint32,
	memorySizer memorySizer,
	memoryLimitPages uint32,
	enabledFeatures api.CoreFeatures,
	arena *stringArena,
	prevModule string,
	ret *wasm.Import,
) (newOffset int, err error) {
	// Intern the module namespace: imports are grouped by module in practice ("env",
	// "wasi_snapshot_preview1", ...), so the module string of one import almost always equals the previous
	// import's. Reuse it via a single comparison instead of copying identical bytes into the arena again.
	// string(rawModule) in an == comparison does not allocate (compiler special case).
	var rawModule []byte
	if rawModule, offset, err = decodeUTF8Raw(buf, offset, "import module"); err != nil {
		err = fmt.Errorf("import[%d] error decoding module: %w", idx, err)
		return offset, err
	}
	if prevModule == string(rawModule) {
		ret.Module = prevModule
	} else {
		ret.Module = arena.string(rawModule)
	}

	if ret.Name, offset, err = decodeUTF8(buf, offset, arena, "import name"); err != nil {
		err = fmt.Errorf("import[%d] error decoding name: %w", idx, err)
		return offset, err
	}

	b, offset, err := readByte(buf, offset)
	if err != nil {
		err = fmt.Errorf("import[%d] error decoding type: %w", idx, err)
		return offset, err
	}
	ret.Type = b
	switch ret.Type {
	case wasm.ExternTypeFunc:
		var n uint64
		ret.DescFunc, n, err = leb128.LoadUint32(buf[offset:])
		offset += int(n)
	case wasm.ExternTypeTable:
		offset, err = decodeTable(buf, offset, enabledFeatures, &ret.DescTable)
	case wasm.ExternTypeMemory:
		ret.DescMem, offset, err = decodeMemory(buf, offset, enabledFeatures, memorySizer, memoryLimitPages)
	case wasm.ExternTypeGlobal:
		ret.DescGlobal, offset, err = decodeGlobalType(buf, offset)
	case wasm.ExternTypeTag:
		if err = enabledFeatures.RequireEnabled(api.CoreFeatureExceptionHandling); err != nil {
			err = fmt.Errorf("tag imports require exception handling feature: %w", err)
			break
		}
		// Tag import: read attribute byte (must be 0x00) then type index.
		var attr byte
		attr, offset, err = readByte(buf, offset)
		if err != nil {
			break
		}
		if attr != 0x00 {
			err = fmt.Errorf("invalid tag attribute: %#x", attr)
			break
		}
		var n uint64
		ret.DescTag, n, err = leb128.LoadUint32(buf[offset:])
		offset += int(n)
	default:
		err = fmt.Errorf("%w: invalid byte for importdesc: %#x", ErrInvalidByte, b)
	}
	if err != nil {
		err = fmt.Errorf("import[%d] %s[%s.%s]: %w", idx, wasm.ExternTypeName(ret.Type), ret.Module, ret.Name, err)
	}
	return offset, err
}
