package wasmdebug

import (
	"testing"

	"github.com/samyfodil/wazy/internal/testing/require"
)

// TestDWARFLines_MalformedSections pins the invariant behind wazero#2526: a
// module carrying DWARF sections must keep a non-nil DWARFLines even when the
// sections do not parse, because that pointer is what turns source-offset
// recording on. Tying it to dwarf.New's return made stack traces lose their
// offsets on Go 1.27, where dwarf.New answers nil instead of an empty
// *dwarf.Data for input earlier versions accepted.
func TestDWARFLines_MalformedSections(t *testing.T) {
	garbage := []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8}
	d := NewDWARFLines(garbage, garbage, garbage, garbage, garbage)
	require.NotNil(t, d)

	require.Nil(t, d.Line(0))
	// The parse must not be retried: on Go 1.27 a failed one leaves d.d nil,
	// so "have we parsed yet" cannot be "is d.d nil".
	require.True(t, d.parsed)
	require.Nil(t, d.rawInfo)
	require.Nil(t, d.Line(0))
}

// TestDWARFLines_NoDebugInfo covers the other side: with no .debug_info there
// is nothing to record, so the decoder gets nil and skips source offsets.
func TestDWARFLines_NoDebugInfo(t *testing.T) {
	require.Nil(t, NewDWARFLines(nil, nil, nil, nil, nil))
}
