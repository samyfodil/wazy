package wit

import (
	"strings"
	"testing"

	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// The parser never produces these shapes, so they are reached directly: a
// kind or a definition this package does not know must fail, not lower to
// a zero TypeRef.
func TestBuilder_RejectsUnknownShapes(t *testing.T) {
	r := &resolver{byName: map[string]*pkgScope{}}
	sc := newScope(r, &fileScope{pkg: &pkgScope{}}, "a:b/i", "interface a:b/i")
	sc.types["odd"] = &ast.TypeDef{Name: "odd"}

	b := newBuilder(nil, false)
	if _, err := b.ref(sc, &ast.TypeRef{Kind: "bogus"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("unknown kind: %v", err)
	}
	if _, err := b.named(sc, "odd"); err == nil || !strings.Contains(err.Error(), "unsupported definition") {
		t.Errorf("unknown definition: %v", err)
	}
}
