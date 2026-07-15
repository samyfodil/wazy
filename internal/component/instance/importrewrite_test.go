package instance

import "testing"

func TestRewriteEmptyImportModuleName_TooShort(t *testing.T) {
	_, err := rewriteEmptyImportModuleName([]byte{0x00, 0x61}, "x")
	requireErrContains(t, err, "too short")
}

func TestRewriteEmptyImportModuleName_TruncatedSectionSize(t *testing.T) {
	// A valid preamble followed by a section id with no size byte at all.
	buf := append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0x02)
	_, err := rewriteEmptyImportModuleName(buf, "x")
	requireErrContains(t, err, "read size")
}

func TestRewriteEmptyImportModuleName_SectionSizeExceedsBuffer(t *testing.T) {
	buf := append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0x02, 0x7f) // size 127, no body
	_, err := rewriteEmptyImportModuleName(buf, "x")
	requireErrContains(t, err, "exceeds remaining bytes")
}

func TestRewriteEmptyImportModuleName_PassesThroughNonImportSections(t *testing.T) {
	// magic+version, then a type section (id 1) with 1 byte of (fake, opaque
	// to this function) body, which must survive unchanged.
	buf := append([]byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}, 0x01, 0x01, 0x60)
	out, err := rewriteEmptyImportModuleName(buf, "x")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(out) != len(buf) {
		t.Fatalf("got %d bytes, want %d (non-import sections must pass through byte-for-byte)", len(out), len(buf))
	}
	for i := range buf {
		if out[i] != buf[i] {
			t.Fatalf("byte %d: got %#x, want %#x", i, out[i], buf[i])
		}
	}
}

func TestRewriteImportSectionBody_BadCount(t *testing.T) {
	_, err := rewriteImportSectionBody([]byte{}, "x")
	requireErrContains(t, err, "read count")
}

func TestRewriteImportSectionBody_BadModuleName(t *testing.T) {
	// count=1, then a name whose declared length exceeds the buffer.
	_, err := rewriteImportSectionBody([]byte{0x01, 0x05, 'a'}, "x")
	requireErrContains(t, err, "module name")
}

func TestRewriteImportSectionBody_BadFieldName(t *testing.T) {
	// count=1, module name "" (len 0), then a field name whose declared
	// length exceeds the buffer.
	_, err := rewriteImportSectionBody([]byte{0x01, 0x00, 0x05, 'a'}, "x")
	requireErrContains(t, err, "field name")
}

func TestRewriteImportSectionBody_BadImportDesc(t *testing.T) {
	// count=1, module "" , field "f", then an unsupported importdesc kind.
	_, err := rewriteImportSectionBody([]byte{0x01, 0x00, 0x01, 'f', 0x99}, "x")
	requireErrContains(t, err, "unsupported importdesc kind")
}

func TestReadCoreWasmName_TruncatedLength(t *testing.T) {
	_, _, err := readCoreWasmName([]byte{}, 0)
	requireErrContains(t, err, "read length")
}

func TestSkipImportDesc_Truncated(t *testing.T) {
	_, err := skipImportDesc([]byte{}, 0)
	requireErrContains(t, err, "truncated importdesc")
}

func TestSkipImportDesc_FuncTruncatedTypeIdx(t *testing.T) {
	_, err := skipImportDesc([]byte{0x00}, 0)
	requireErrContains(t, err, "func typeidx")
}

func TestSkipImportDesc_GlobalType(t *testing.T) {
	off, err := skipImportDesc([]byte{0x03, 0x7f, 0x00}, 0)
	if err != nil {
		t.Fatalf("skipImportDesc: %v", err)
	}
	if off != 3 {
		t.Fatalf("offset = %d, want 3", off)
	}
}

func TestSkipLimits_Truncated(t *testing.T) {
	_, err := skipLimits([]byte{}, 0)
	requireErrContains(t, err, "truncated limits")
}

func TestSkipLimits_BadMin(t *testing.T) {
	_, err := skipLimits([]byte{0x00}, 0)
	requireErrContains(t, err, "limits min")
}

func TestSkipLimits_BadMax(t *testing.T) {
	_, err := skipLimits([]byte{0x01, 0x00}, 0)
	requireErrContains(t, err, "limits max")
}

func TestSkipLimits_WithMax(t *testing.T) {
	off, err := skipLimits([]byte{0x01, 0x05, 0x0a}, 0)
	if err != nil {
		t.Fatalf("skipLimits: %v", err)
	}
	if off != 3 {
		t.Fatalf("offset = %d, want 3", off)
	}
}
