package amd64

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// The encode-time float-to-int sequences duplicate, byte for byte, what
// lowerFcvtTo{S,U}intSequenceAfterRegalloc expands into real instructions. The
// duplication is deliberate, since encode() cannot call back into the machine to
// insert instructions, but nothing except this test stops the two copies from
// drifting apart: neither is reachable from wasm today, so no other test would
// notice a change to one of them.
func TestEncodeTimeFcvtSequencesMatchLowering(t *testing.T) {
	for _, sat := range []bool{false, true} {
		for _, src64 := range []bool{false, true} {
			for _, dst64 := range []bool{false, true} {
				name := fmt.Sprintf("sat=%v/src64=%v/dst64=%v", sat, src64, dst64)

				t.Run("signed/"+name, func(t *testing.T) {
					_, _, m := newSetupWithMockContext()
					seq := m.allocateFcvtToSintSequence(raxVReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, src64, dst64, sat)
					m.lowerFcvtToSintSequenceAfterRegalloc(seq)
					lowered := hex.EncodeToString(encodePending(m))

					_, _, m2 := newSetupWithMockContext()
					enc := m2.allocateFcvtToSintSequence(raxVReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, src64, dst64, sat)
					enc.kind = cvtFloatToSintSeq
					enc.encode(m2.c)
					require.Equal(t, lowered, hex.EncodeToString(m2.c.Buf()))
				})

				t.Run("unsigned/"+name, func(t *testing.T) {
					_, _, m := newSetupWithMockContext()
					seq := m.allocateFcvtToUintSequence(raxVReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, xmm2VReg, src64, dst64, sat)
					m.lowerFcvtToUintSequenceAfterRegalloc(seq)
					lowered := hex.EncodeToString(encodePending(m))

					_, _, m2 := newSetupWithMockContext()
					enc := m2.allocateFcvtToUintSequence(raxVReg, xmm0VReg, rcxVReg, rdxVReg, xmm1VReg, xmm2VReg, src64, dst64, sat)
					enc.kind = cvtFloatToUintSeq
					enc.encode(m2.c)
					require.Equal(t, lowered, hex.EncodeToString(m2.c.Buf()))
				})
			}
		}
	}
}

// encodePending links the instructions the lowering appended to the machine's
// pending list and encodes them, which is what the post-regalloc pass does for
// real.
func encodePending(m *machine) []byte {
	pending := m.pendingInstructions
	if len(pending) == 0 {
		return nil
	}
	for i := 0; i < len(pending)-1; i++ {
		pending[i].next = pending[i+1]
		pending[i+1].prev = pending[i]
	}
	m.rootInstr = pending[0]
	m.encodeWithoutSSA(m.rootInstr)
	return m.c.Buf()
}
