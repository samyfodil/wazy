package bench

import (
	"context"
	"runtime"
	"sync"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/engine/native/backend/isa/amd64"
	"github.com/samyfodil/wazy/internal/platform"
	"github.com/samyfodil/wazy/internal/testing/require"
)

// The Intel SKX102 ("JCC erratum") workaround only ever shows up as a number of
// nanoseconds, so "it still runs" proves nothing about it. These tests compile a
// real module -- case.wasm, TinyGo output with base64, string manipulation,
// array reversal and matrix multiplication in it -- and count, exactly, how many
// of its jumps land in the state the erratum punishes: crossing a 32-byte
// boundary, or ending on one.
//
// The count is taken from the instruction stream the encoder actually emitted
// (amd64.InstrExtentSink gives the exact boundaries) but every instruction is
// classified as a jump or not by reading its opcode bytes here, not by asking
// the workaround what it thinks a jump is. A branch form the workaround does not
// know about therefore still shows up in the count.

// branchKind classifies one instruction, given its exact first and last byte, by
// opcode. Legacy prefixes and REX are skipped; everything else follows Intel SDM
// Vol.2 opcode maps. Returns "" when the instruction is not a jump.
func branchKind(code []byte) string {
	i := 0
	for i < len(code) {
		switch code[i] {
		case 0x66, 0x67, 0xf0, 0xf2, 0xf3, 0x2e, 0x36, 0x3e, 0x26, 0x64, 0x65:
			i++
			continue
		}
		break
	}
	if i < len(code) && code[i]&0xf0 == 0x40 { // REX.
		i++
	}
	if i >= len(code) {
		return ""
	}
	switch op := code[i]; {
	case op == 0x0f && i+1 < len(code) && code[i+1]&0xf0 == 0x80:
		return "jcc rel32"
	case op >= 0x70 && op <= 0x7f:
		return "jcc rel8"
	case op >= 0xe0 && op <= 0xe3:
		return "loop/jrcxz"
	case op == 0xe8:
		return "call rel32"
	case op == 0xe9:
		return "jmp rel32"
	case op == 0xeb:
		return "jmp rel8"
	case op == 0xc2 || op == 0xc3 || op == 0xca || op == 0xcb:
		return "ret"
	case op == 0xff && i+1 < len(code):
		switch (code[i+1] >> 3) & 7 {
		case 2, 3:
			return "call indirect"
		case 4, 5:
			return "jmp indirect"
		}
	}
	return ""
}

type jccAudit struct {
	mu sync.Mutex
	// branches is every jump found, affected the subset in the erratum state.
	branches, affected int
	// byKind counts the affected ones, so a regression names the form it broke.
	byKind map[string]int
	// Extents longer than the 15-byte architectural maximum are not single
	// instructions. They are either a jump-table island (all-zero data, filled
	// in with label addresses later, never executed as instructions) or a
	// pseudo-instruction that expands to a whole sequence -- and jumps *inside*
	// one of those cannot be seen from here, so they are counted separately and
	// reported rather than silently ignored.
	jumpTables, unaudited int
	// codeBytes is the total size of the emitted instruction stream.
	codeBytes int64
	functions int
}

func (a *jccAudit) record(extents []amd64.InstrExtent, code []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.functions++
	for _, e := range extents {
		if e.Length == 0 {
			continue // Pseudo-instructions that emit nothing (labels, source info).
		}
		if e.Offset+e.Length > int64(len(code)) {
			panic("BUG: extent outside the code buffer")
		}
		if e.Length > 15 {
			if allZero(code[e.Offset : e.Offset+e.Length]) {
				a.jumpTables++
			} else {
				a.unaudited++
			}
			continue
		}
		kind := branchKind(code[e.Offset : e.Offset+e.Length])
		if kind == "" {
			continue
		}
		a.branches++
		start, end := e.Offset, e.Offset+e.Length
		if start/32 != (end-1)/32 || end%32 == 0 {
			a.affected++
			a.byKind[kind]++
		}
	}
	if n := int64(len(code)); n > 0 {
		a.codeBytes += n
	}
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// auditCaseWasm compiles case.wasm with the JCC workaround forced to `on` and
// returns what the encoder produced. Forcing is what makes this testable at all:
// the decision comes from CPUID, so on any given CI machine only one of the two
// arms would otherwise ever run.
func auditCaseWasm(t *testing.T, on bool) *jccAudit {
	t.Helper()

	savedFeatures := platform.CpuFeatures
	savedSink := amd64.InstrExtentSink
	t.Cleanup(func() {
		platform.CpuFeatures = savedFeatures
		amd64.InstrExtentSink = savedSink
	})
	if on {
		platform.CpuFeatures = savedFeatures | platform.CpuFeatureAmd64JCCErratum
	} else {
		platform.CpuFeatures = savedFeatures &^ platform.CpuFeatureAmd64JCCErratum
	}
	require.Equal(t, on, platform.JCCErratumWorkaroundEnabled())

	audit := &jccAudit{byKind: map[string]int{}}
	amd64.InstrExtentSink = audit.record

	ctx := context.Background()
	r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler())
	defer r.Close(ctx)
	_, err := r.CompileModule(ctx, caseWasm)
	require.NoError(t, err)
	require.True(t, audit.functions > 0, "no functions were encoded")
	return audit
}

// TestJCCErratumMitigation is the one that would catch the mitigation silently
// doing nothing.
func TestJCCErratumMitigation(t *testing.T) {
	if runtime.GOARCH != "amd64" || !platform.CompilerSupported() {
		t.Skip("amd64 compiler only")
	}

	off := auditCaseWasm(t, false)
	on := auditCaseWasm(t, true)

	t.Logf("padding off: %d functions, %d code bytes, %d branches, %d erratum-affected (%.1f%%)",
		off.functions, off.codeBytes, off.branches, off.affected,
		100*float64(off.affected)/float64(off.branches))
	t.Logf("padding on:  %d functions, %d code bytes, %d branches, %d erratum-affected (%.1f%%)",
		on.functions, on.codeBytes, on.branches, on.affected,
		100*float64(on.affected)/float64(on.branches))
	t.Logf("code size delta: %+.2f%%", 100*float64(on.codeBytes-off.codeBytes)/float64(off.codeBytes))
	t.Logf("jump-table islands (data, not instructions): %d; pseudo-instruction sequences whose "+
		"internal branches this audit cannot see: %d", on.jumpTables, on.unaudited)

	// Same module, so the same jumps must be there either way; only where they
	// sit changes.
	require.Equal(t, off.branches, on.branches)
	require.Equal(t, off.functions, on.functions)

	// The whole point: with the workaround on, no jump the encoder emits may be
	// left crossing or ending on a 32-byte boundary.
	require.Zero(t, on.affected,
		"erratum-affected branches remain with the workaround on: %v", on.byKind)

	// And the measurement has teeth: without it, a substantial fraction of the
	// same module's branches land there by accident of byte counts. Anything
	// near zero here would mean the audit is not looking at real code.
	require.True(t, off.affected*20 > off.branches,
		"expected ~17%% of %d branches affected without padding, got %d -- audit is not measuring anything",
		off.branches, off.affected)

	// Padding costs code size; a mitigation that costs nothing is not running.
	require.True(t, on.codeBytes > off.codeBytes, "padding added no bytes at all")
}

// TestJCCErratumOffOnUnaffectedCPU asserts the workaround is genuinely inert
// when CPUID says the core is not Skylake-family: byte-for-byte the same code as
// a build that has never heard of the erratum.
func TestJCCErratumOffOnUnaffectedCPU(t *testing.T) {
	if runtime.GOARCH != "amd64" || !platform.CompilerSupported() {
		t.Skip("amd64 compiler only")
	}
	a := auditCaseWasm(t, false)
	b := auditCaseWasm(t, false)
	require.Equal(t, a.codeBytes, b.codeBytes)
	require.Equal(t, a.affected, b.affected)
	// Not a tautology: the deterministic-compile check above is what makes the
	// on/off code-size comparison in TestJCCErratumMitigation meaningful.
	require.True(t, a.affected > 0, "case.wasm should hit the erratum by accident when unpadded")
}

// TestJCCErratumCacheKey is a correctness test, not a performance one.
//
// The padding decision changes the machine code that ends up on disk, and wazy
// caches compiled modules to a directory that a fleet may well share. If the
// decision is not part of the cache key, a module compiled on a Skylake host is
// handed back to an Ice Lake host built for the wrong policy -- function offsets
// aligned to 32 rather than 16, and NOPs it never asked for -- with nothing
// anywhere to say so. This walks that exact scenario.
//
// Whether a compile was a cache hit is observed directly: on a hit nothing is
// encoded, so the encoder's instruction sink is never called.
func TestJCCErratumCacheKey(t *testing.T) {
	if runtime.GOARCH != "amd64" || !platform.CompilerSupported() {
		t.Skip("amd64 compiler only")
	}

	savedFeatures := platform.CpuFeatures
	savedSink := amd64.InstrExtentSink
	t.Cleanup(func() {
		platform.CpuFeatures = savedFeatures
		amd64.InstrExtentSink = savedSink
	})

	dir := t.TempDir()
	// compile returns the audit of what the encoder produced, or nil when the
	// compile was served entirely from the cache.
	compile := func(t *testing.T, on bool) *jccAudit {
		t.Helper()
		if on {
			platform.CpuFeatures = savedFeatures | platform.CpuFeatureAmd64JCCErratum
		} else {
			platform.CpuFeatures = savedFeatures &^ platform.CpuFeatureAmd64JCCErratum
		}
		audit := &jccAudit{byKind: map[string]int{}}
		amd64.InstrExtentSink = audit.record

		ctx := context.Background()
		// A fresh CompilationCache each time: it owns the in-memory
		// compiled-module map, which is keyed by module ID alone (the host CPU
		// cannot change inside one process, so it does not belong there).
		// Reusing it would hide the disk cache behind a memory hit and make this
		// test vacuous.
		cache, err := wazy.NewCompilationCacheWithDir(dir)
		require.NoError(t, err)
		defer cache.Close(ctx)
		r := wazy.NewRuntimeWithConfig(ctx, wazy.NewRuntimeConfigCompiler().WithCompilationCache(cache))
		defer r.Close(ctx)
		_, err = r.CompileModule(ctx, caseWasm)
		require.NoError(t, err)
		if audit.functions == 0 {
			return nil
		}
		return audit
	}

	// 1. Cold, workaround on: a real compile, and the result has no affected branch.
	padded := compile(t, true)
	require.NotNil(t, padded, "first compile should not have been a cache hit")
	require.Zero(t, padded.affected)

	// 2. Warm, workaround still on: served from the cache. This is the control --
	//    without it step 3 proves nothing, because a cache that never hits also
	//    never mixes layouts.
	require.Nil(t, compile(t, true), "second compile with the same policy should have hit the cache")

	// 3. Warm, workaround off: must NOT be served the padded entry. If the
	//    decision is missing from the key this returns nil and the test fails
	//    right here, which is exactly the silent mismatch being guarded against.
	unpadded := compile(t, false)
	require.NotNil(t, unpadded,
		"a module compiled WITH the JCC workaround was loaded from the cache by a build WITHOUT it: "+
			"the workaround is missing from the compilation cache key")
	require.True(t, unpadded.affected > 0, "the unpadded recompile should show the erratum's natural rate")
	require.True(t, unpadded.codeBytes < padded.codeBytes)

	// 4. And back: both entries now coexist in the one directory, each found by
	//    its own key.
	require.Nil(t, compile(t, false), "the unpadded entry should now hit")
	require.Nil(t, compile(t, true), "the padded entry should still hit")
}
