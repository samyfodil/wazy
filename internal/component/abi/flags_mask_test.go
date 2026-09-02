package abi

import (
	"encoding/binary"
	"fmt"
	"testing"

	bintype "github.com/samyfodil/wazy/internal/component/binary"
)

// flagsLabels builds a flags descriptor with n distinct labels.
func flagsLabels(n int) bintype.FlagsDesc {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("f%d", i)
	}
	return bintype.FlagsDesc{Names: names}
}

// A flags value carries one bit per label; bits ABOVE the label count are
// meaningless and must be DISCARDED, not trapped on and not passed through --
// that is the reference's unpack_flags_from_int, which shifts out exactly
// len(labels) bits and drops the rest.
//
// The vectors are fused.22.wast's own: it lowers these over-wide patterns into
// a component whose core funcs assert they observe exactly the masked value.
// wazy returned the raw i32, so every one of them tripped the guest's
// `i32.ne / unreachable`.
func TestFlagsDiscardBitsAboveTheLabelCount(t *testing.T) {
	tests := []struct {
		labels int
		raw    uint32
		want   uint32
	}{
		{labels: 1, raw: 0xFFFFFF01, want: 1},
		{labels: 8, raw: 0xFFFFFF11, want: 17},
		{labels: 9, raw: 0xFFFFFF11, want: 273},
		{labels: 16, raw: 0xFFFF1111, want: 4369},
		{labels: 17, raw: 0xFFFF1111, want: 69905},
		{labels: 32, raw: 0x11111111, want: 286331153},
		// A 32-label flags has no bits to discard: every bit is meaningful.
		{labels: 32, raw: 0xFFFFFFFF, want: 0xFFFFFFFF},
		// The widths a flags occupies are 1, 2 and 4 bytes, so a 1..7-label
		// flags shares a byte's worth of room with nothing, and a 9..15-label
		// one a u16's -- the load path has to mask for the same reason.
		{labels: 3, raw: 0xFF, want: 0x7},
		{labels: 12, raw: 0xFFFF, want: 0xFFF},
	}
	for _, tt := range tests {
		desc := flagsLabels(tt.labels)
		t.Run(fmt.Sprintf("flat/%d-labels", tt.labels), func(t *testing.T) {
			got, err := LiftFlat([]CoreValue{NewCoreValueI32(tt.raw)}, desc, nil, nil)
			if err != nil {
				t.Fatalf("LiftFlat: %v", err)
			}
			if got != Value(tt.want) {
				t.Errorf("LiftFlat(%#x) = %v, want %d", tt.raw, got, tt.want)
			}
		})
		t.Run(fmt.Sprintf("memory/%d-labels", tt.labels), func(t *testing.T) {
			mem := make([]byte, 8)
			binary.LittleEndian.PutUint32(mem, tt.raw)
			got, err := Load(mem, 0, desc, nil)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got != Value(tt.want) {
				t.Errorf("Load(%#x) = %v, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// The fail-loud branch masking relies on: a flags descriptor outside the
// spec's 1..32 labels has no meaningful mask, so it must error rather than
// produce a nonsense shift.
func TestFlagsInvalidLabelCountFailsLoud(t *testing.T) {
	for _, n := range []int{0, 33} {
		desc := flagsLabels(n)
		if _, err := LiftFlat([]CoreValue{NewCoreValueI32(0)}, desc, nil, nil); err == nil {
			t.Errorf("LiftFlat with %d labels: expected an error", n)
		}
		if _, err := Load(make([]byte, 8), 0, desc, nil); err == nil {
			t.Errorf("Load with %d labels: expected an error", n)
		}
		// LiftFlatKinds takes an already-computed flat list, so it reaches
		// liftFlatFlags without FlatWidth having rejected the descriptor
		// first -- liftFlatFlags' own guard is what fails loud here.
		if _, err := LiftFlatKinds([]string{"i32"}, []uint64{0}, desc, nil, nil); err == nil {
			t.Errorf("LiftFlatKinds with %d labels: expected an error", n)
		}
	}
}
