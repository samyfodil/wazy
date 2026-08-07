package wasip2

import (
	"strings"
	"testing"
)

// requireErrContains fails unless err is non-nil and its message contains
// substr. Every fail-loud branch in this package is asserted through it, so
// a guard that stops firing is a test failure rather than a silent pass.
func requireErrContains(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("expected error to contain %q, got: %v", substr, err)
	}
}
