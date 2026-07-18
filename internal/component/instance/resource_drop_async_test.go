package instance

import (
	"bytes"
	"context"
	"testing"

	"github.com/samyfodil/wazy"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file covers Phase 3's resource.drop async decode + execution
// (docs/component-model-async-phase3-design.md §4.4, CanonKindResourceDropAsync
// = 0x07): wasm-tools 1.253's OWN text-format parser has no `resource.drop
// async` production yet (confirmed empirically -- its wast grammar's error
// message lists every other canon builtin but not this one), so there is no
// way to author a fresh .wat fixture for it. Both tests below instead
// byte-patch res20_own_dtor.component.wasm's two REAL, already-verified
// `resource.drop` (kind 0x03) canon entries to kind 0x07 -- identical
// payload shape (a single typeidx), confirmed against a `wasm-tools dump`
// of the unmodified fixture (see real_own_dtor_test.go's doc) -- and checks
// the guest's own `start`-section self-test (dtor-run counters, dense
// handle reuse) still passes byte-for-byte identically through the async
// decode path.

// patchResourceDropToAsync returns a copy of wasm with every occurrence of
// the two-byte sequence {0x03, typeIdx} at the given offsets rewritten to
// {0x07, typeIdx} -- i.e. ResourceDrop -> ResourceDropAsync, same payload.
func patchResourceDropToAsync(t *testing.T, wasm []byte, offsets ...int) []byte {
	t.Helper()
	patched := append([]byte(nil), wasm...)
	for _, off := range offsets {
		if patched[off] != 0x03 {
			t.Fatalf("byte at offset %#x = %#x, want 0x03 (resource.drop) -- fixture bytes changed?", off, patched[off])
		}
		patched[off] = binary.CanonKindResourceDropAsync
	}
	return patched
}

// TestDecode_ResourceDropAsync is the decoder-level pin: a canon kind 0x07
// decodes with the same typeidx payload shape as 0x03 (verified against the
// real fixture's own resource.drop entries -- offsets 0xad/0xaf, dumped in
// real_own_dtor_test.go's doc).
func TestDecode_ResourceDropAsync(t *testing.T) {
	patched := patchResourceDropToAsync(t, res20OwnDtorWasm, 0xad, 0xaf)
	comp, err := binary.Decode(bytes.NewReader(patched))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found int
	for _, cn := range comp.Canons {
		if cn.Kind == binary.CanonKindResourceDropAsync {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("found %d CanonKindResourceDropAsync entries, want 2", found)
	}
}

// TestExecute_ResourceDropAsyncRunsDtor is the execution-level pin: routed
// through resourceCanonHostFuncGraph exactly like a sync resource.drop, an
// async-decoded drop of an own handle still runs the destructor -- the
// fixture's own start-section self-test (dtor-run drop counters, dense
// handle-index reuse) passes unchanged.
func TestExecute_ResourceDropAsyncRunsDtor(t *testing.T) {
	ctx := context.Background()
	r := wazy.NewRuntime(ctx)
	defer r.Close(ctx)

	patched := patchResourceDropToAsync(t, res20OwnDtorWasm, 0xad, 0xaf)
	inst, err := Instantiate(ctx, r, patched, WithWASI(WASIConfig{})...)
	if err != nil {
		t.Fatalf("instantiate (start-section self-test failed under resource.drop async): %v", err)
	}
	inst.Close(ctx)
}
