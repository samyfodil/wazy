package instance

import (
	"bytes"
	"fmt"

	"github.com/samyfodil/wazy/internal/leb128"
)

// rewriteEmptyImportModuleName returns a copy of moduleBytes (a real,
// already-embedded core wasm module) with every import clause whose module
// name is "" rewritten to newName. Every other section, and every other
// import's module/field name and import-descriptor bytes, is copied
// byte-for-byte unchanged.
//
// # Why this exists
//
// wazy's module registry treats ModuleConfig.WithName("") as "anonymous":
// explicitly NOT resolvable by another module's by-name import (see
// registerModule in internal/wasm/store_module_list.go, which skips the
// nameToModule map entirely when the name is empty). But wit-component
// sometimes chooses the empty string as a core:instantiatearg name purely as
// a distinct namespace label with no other semantic meaning -- e.g. the
// "indirect function table" trampoline wiring emitted alongside a wasip2 CLI
// adapter groups a handful of lowered-import funcs and a shared funcref
// table under the name "", consumed by one small core module whose only job
// is to fill that table's elements.
//
// Rather than extend wazy's public module-registration semantics (a much
// larger change) to support anonymous-but-resolvable modules, this package
// registers that grouping under an ordinary synthesized name and rewrites
// the one real core module that imports it (via this function) to ask for
// that name instead of "" before instantiating it.
// The returned changed reports whether any empty import module name was
// actually rewritten. It lets the caller keep the result out of the
// compile cache: newName is per-instantiation unique (see nextSynthNamePrefix),
// so a rewritten module's bytes -- and thus its byte-keyed cache entry -- differ
// every instantiation, which would grow the cache without ever hitting. A
// module with no empty import re-encodes byte-identically (changed == false) and
// still caches normally.
func rewriteEmptyImportModuleName(moduleBytes []byte, newName string) ([]byte, bool, error) {
	const preambleLen = 8 // magic (4) + version (4)
	if len(moduleBytes) < preambleLen {
		return nil, false, fmt.Errorf("component/instance: rewriteEmptyImportModuleName: module is only %d bytes, too short for a wasm preamble", len(moduleBytes))
	}

	var out bytes.Buffer
	out.Write(moduleBytes[:preambleLen])

	changed := false
	offset := preambleLen
	for offset < len(moduleBytes) {
		id := moduleBytes[offset]
		offset++
		size, n, err := leb128.LoadUint32(moduleBytes[offset:])
		if err != nil {
			return nil, false, fmt.Errorf("component/instance: rewriteEmptyImportModuleName: section %d: read size: %w", id, err)
		}
		offset += int(n)
		if offset+int(size) > len(moduleBytes) {
			return nil, false, fmt.Errorf("component/instance: rewriteEmptyImportModuleName: section %d: size %d exceeds remaining bytes", id, size)
		}
		body := moduleBytes[offset : offset+int(size)]
		offset += int(size)

		if id != 2 { // not the import section: copy verbatim
			out.WriteByte(id)
			out.Write(leb128.EncodeUint32(size))
			out.Write(body)
			continue
		}

		newBody, sectionChanged, err := rewriteImportSectionBody(body, newName)
		if err != nil {
			return nil, false, fmt.Errorf("component/instance: rewriteEmptyImportModuleName: %w", err)
		}
		changed = changed || sectionChanged
		out.WriteByte(id)
		out.Write(leb128.EncodeUint32(uint32(len(newBody))))
		out.Write(newBody)
	}
	return out.Bytes(), changed, nil
}

// rewriteImportSectionBody rewrites every "" module name within a core wasm
// import section's already-sliced body (vec(import), not including the
// section id/size header) to newName.
func rewriteImportSectionBody(body []byte, newName string) ([]byte, bool, error) {
	offset := 0
	count, n, err := leb128.LoadUint32(body[offset:])
	if err != nil {
		return nil, false, fmt.Errorf("import section: read count: %w", err)
	}
	offset += int(n)

	var out bytes.Buffer
	out.Write(leb128.EncodeUint32(count))

	changed := false
	for i := range count {
		modName, off, err := readCoreWasmName(body, offset)
		if err != nil {
			return nil, false, fmt.Errorf("import[%d] module name: %w", i, err)
		}
		offset = off

		fieldName, off, err := readCoreWasmName(body, offset)
		if err != nil {
			return nil, false, fmt.Errorf("import[%d] field name: %w", i, err)
		}
		offset = off

		descStart := offset
		off, err = skipImportDesc(body, offset)
		if err != nil {
			return nil, false, fmt.Errorf("import[%d] (%q.%q): %w", i, modName, fieldName, err)
		}
		offset = off

		if modName == "" {
			modName = newName
			changed = true
		}
		writeName(&out, modName)
		writeName(&out, fieldName)
		out.Write(body[descStart:offset])
	}
	return out.Bytes(), changed, nil
}

// readCoreWasmName reads a plain core-wasm name (uleb32 length + raw utf8
// bytes -- used for import/export module and field names, unlike the
// component model's externname, which carries an extra kind-byte prefix).
func readCoreWasmName(buf []byte, offset int) (string, int, error) {
	length, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return "", offset, fmt.Errorf("read length: %w", err)
	}
	offset += int(n)
	if offset+int(length) > len(buf) {
		return "", offset, fmt.Errorf("name of length %d exceeds remaining bytes", length)
	}
	name := string(buf[offset : offset+int(length)])
	return name, offset + int(length), nil
}

// skipImportDesc advances past one importdesc (the kind byte plus its
// payload: a func's typeidx, a table's elemtype+limits, a memory's limits,
// or a global's valtype+mutability), returning the offset just past it. Its
// content is never interpreted beyond what's needed to find that boundary --
// callers needing the bytes slice them directly from the original buffer,
// preserving them byte-for-byte.
func skipImportDesc(buf []byte, offset int) (int, error) {
	if offset >= len(buf) {
		return offset, fmt.Errorf("truncated importdesc")
	}
	kind := buf[offset]
	offset++
	switch kind {
	case 0x00: // func: typeidx
		_, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("func typeidx: %w", err)
		}
		return offset + int(n), nil

	case 0x01: // table: elemtype limits
		offset++ // elemtype
		return skipLimits(buf, offset)

	case 0x02: // memory: limits
		return skipLimits(buf, offset)

	case 0x03: // global: valtype mutability
		return offset + 2, nil

	default:
		return offset, fmt.Errorf("unsupported importdesc kind %#x", kind)
	}
}

// skipLimits advances past a limits (0x00 min | 0x01 min max), returning the
// offset just past it.
func skipLimits(buf []byte, offset int) (int, error) {
	if offset >= len(buf) {
		return offset, fmt.Errorf("truncated limits")
	}
	flag := buf[offset]
	offset++
	_, n, err := leb128.LoadUint32(buf[offset:])
	if err != nil {
		return offset, fmt.Errorf("limits min: %w", err)
	}
	offset += int(n)
	if flag == 0x01 {
		_, n, err := leb128.LoadUint32(buf[offset:])
		if err != nil {
			return offset, fmt.Errorf("limits max: %w", err)
		}
		offset += int(n)
	}
	return offset, nil
}
