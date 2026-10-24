package binary

import (
	"bytes"
	"testing"
)

// The point of precomputing at decode: ResolveType is then a pure read, so it
// needs no lock on the path abi.Resolver calls once per nested TypeRef of every
// lifted and lowered value. Assert the invariant the lock-free path rests on --
// a decoded Component is frozen, and resolving does not touch its type tables.
func TestDecodedComponentIsFrozenAndImmutable(t *testing.T) {
	// The same `use iface.{T}` shape TestDecode_ImportedInstanceType_EndToEnd
	// decodes: the one alias form that interns.
	caseOther := append(synthLabel("other"), 0x01, 0x00, 0x00)
	variantBody := append([]byte{0x71, 0x01}, caseOther...)

	body := []byte{0x03}
	body = append(body, synthInstanceDeclType([]byte{0x6b, 0x73})...) // option<string>
	body = append(body, synthInstanceDeclType(variantBody)...)        // variant error { other(local 0) }
	body = append(body, synthInstanceDeclExportEq("error", 1)...)

	typeSec := synthSection(7, append([]byte{0x01, 0x42}, body...))
	importSec := synthSection(10, append([]byte{0x01}, synthInstanceImport("test:pkg/types")...))
	aliasSec := synthSection(6, append([]byte{0x01, 0x03, 0x00, 0x00}, synthLabel("error")...))

	c, err := Decode(bytes.NewReader(synthComponent(typeSec, importSec, aliasSec)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !c.typesFrozen {
		t.Fatal("a component whose aliases all resolved was left unfrozen, so ResolveType still locks")
	}

	// Decode already interned it, so the first resolution must not add to
	// either table -- nor may any resolution after it.
	extra, cached := len(c.extraTypes), len(c.aliasCache)
	if extra == 0 {
		t.Fatal("decode interned nothing, so this component does not exercise the path")
	}
	for i := 0; i < 5; i++ {
		if _, err := c.ResolveType(1); err != nil {
			t.Fatalf("ResolveType: %v", err)
		}
	}
	if len(c.extraTypes) != extra || len(c.aliasCache) != cached {
		t.Errorf("resolving mutated a frozen component: extraTypes %d->%d, aliasCache %d->%d",
			extra, len(c.extraTypes), cached, len(c.aliasCache))
	}
}

// An alias the decoder cannot resolve must leave the component unfrozen (so
// the locked path still guards the interning a lazy resolve would do), and
// must not leave the reserved-but-unfilled slots of its failed attempt behind.
func TestUnresolvableAliasLeavesNoDebris(t *testing.T) {
	// A `sub`-bound (abstract resource) export is reserved as a nil local
	// slot, which globalizeLocalRef refuses -- so the alias cannot resolve.
	subExport := append([]byte{0x04, 0x00}, synthLabel("r")...)
	subExport = append(subExport, 0x03, 0x01) // externdesc: type-sort, `sub resource` bound (no index)

	typeSec := synthSection(7, append([]byte{0x01, 0x42, 0x01}, subExport...))
	importSec := synthSection(10, append([]byte{0x01}, synthInstanceImport("test:pkg/types")...))
	aliasSec := synthSection(6, append([]byte{0x01, 0x03, 0x00, 0x00}, synthLabel("r")...))

	c, err := Decode(bytes.NewReader(synthComponent(typeSec, importSec, aliasSec)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.typesFrozen {
		t.Error("an unresolvable alias left the component frozen, so its later interning is unguarded")
	}
	if len(c.extraTypes) != 0 {
		t.Errorf("a failed precompute left %d reserved slot(s) behind", len(c.extraTypes))
	}

	// It must still fail at the call site, with its reason, however often.
	for i := 0; i < 3; i++ {
		if _, err := c.ResolveType(1); err == nil {
			t.Fatal("ResolveType resolved an alias the decoder cannot follow")
		}
	}
	if len(c.extraTypes) != 0 {
		t.Errorf("repeated failing resolutions grew extraTypes to %d", len(c.extraTypes))
	}
}
