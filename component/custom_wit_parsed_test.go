package component_test

import (
	_ "embed"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/samyfodil/wazy/component"
	"github.com/samyfodil/wazy/component/wit"
)

// The same interface as custom_wit_test.go, with every signature read from
// testdata/custom_wit.wit instead of built by hand: the parsed descriptors
// must describe the same types, and the guest must run on them.

//go:embed testdata/custom_wit.wit
var customWITSource string

func parsedCustomWIT(t *testing.T) *wit.Interface {
	t.Helper()
	pkg, err := wit.Parse("custom_wit.wit", customWITSource)
	if err != nil {
		t.Fatal(err)
	}
	iface := pkg.Interface(customWITIface)
	if iface == nil {
		t.Fatalf("no interface %s", customWITIface)
	}
	return iface
}

var customWITTags = wit.Tags{{Interface: customWITIface, Name: "counter"}: counterResType}

func TestCustomWIT_ParsedMatchesHandBuilt(t *testing.T) {
	iface := parsedCustomWIT(t)
	// TypeTable.Func names params positionally (p0, p1, ...), so the WIT
	// names are checked on their own rather than against the hand-built ones.
	wantParams := map[string][]string{
		"lookup":               {"q"},
		"make":                 {"start"},
		"[method]counter.bump": {"self", "by"},
		"use-it":               {"c", "by"},
	}
	for name, want := range customWITHandBuilt(newCounterHost()) {
		f := iface.Func(name)
		if f == nil {
			t.Fatalf("parsed interface has no func %s", name)
		}
		sig, err := f.Signature(customWITTags)
		if err != nil {
			t.Fatal(err)
		}
		if err := sameFunc(sig.Desc, sig.Types.Resolver(), want.fd, want.resolve); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		var names []string
		for _, p := range sig.Desc.Params {
			names = append(names, p.Name)
		}
		if !slices.Equal(names, wantParams[name]) {
			t.Errorf("%s: param names %v, want %v", name, names, wantParams[name])
		}
	}

	var funcs []string
	for _, f := range iface.Funcs {
		funcs = append(funcs, f.Name)
	}
	if want := []string{"[constructor]counter", "[method]counter.bump", "lookup", "make", "use-it"}; !slices.Equal(funcs, want) {
		t.Errorf("funcs %v, want %v", funcs, want)
	}
	if len(iface.Resources) != 1 || iface.Resources[0].Name != "counter" || len(iface.Resources[0].Funcs) != 2 {
		t.Errorf("resources %+v, want counter with a constructor and bump", iface.Resources)
	}

	sig, err := iface.Func("lookup").Signature(nil)
	if err != nil {
		t.Fatal(err)
	}
	var typeNames []string
	for _, n := range sig.TypeNames {
		typeNames = append(typeNames, n)
	}
	slices.Sort(typeNames)
	if want := []string{"failure", "query"}; !slices.Equal(typeNames, want) {
		t.Errorf("lookup type names %v, want %v", typeNames, want)
	}
}

func TestCustomWIT_GuestRunsOnParsedDescriptors(t *testing.T) {
	iface := parsedCustomWIT(t)
	imports := customWITHandBuilt(newCounterHost())
	for name, imp := range imports {
		sig, err := iface.Func(name).Signature(customWITTags)
		if err != nil {
			t.Fatal(err)
		}
		imports[name] = customWITImport{fd: sig.Desc, resolve: sig.Types.Resolver(), fn: imp.fn}
	}
	runCustomWIT(t, imports)
}

// sameFunc reports how two func signatures differ in shape, comparing each
// type through its own table, since the same type may sit at a different
// index in each. Param names are not compared: they do not reach the wire.
func sameFunc(a component.FuncDesc, ra component.Resolver, b component.FuncDesc, rb component.Resolver) error {
	if len(a.Params) != len(b.Params) {
		return fmt.Errorf("%d params, want %d", len(a.Params), len(b.Params))
	}
	for i := range a.Params {
		if err := sameType(ra, a.Params[i].Type, rb, b.Params[i].Type); err != nil {
			return fmt.Errorf("param %d: %w", i, err)
		}
	}
	if err := sameOptType(ra, a.Results.Unnamed, rb, b.Results.Unnamed); err != nil {
		return fmt.Errorf("result: %w", err)
	}
	if len(a.Results.Named) != 0 || len(b.Results.Named) != 0 || a.Async != b.Async {
		return fmt.Errorf("named results or async differ")
	}
	return nil
}

func sameOptType(ra component.Resolver, a *component.TypeRef, rb component.Resolver, b *component.TypeRef) error {
	if (a == nil) != (b == nil) {
		return fmt.Errorf("present %v, want %v", a != nil, b != nil)
	}
	if a == nil {
		return nil
	}
	return sameType(ra, *a, rb, *b)
}

func sameType(ra component.Resolver, a component.TypeRef, rb component.Resolver, b component.TypeRef) error {
	if a.Primitive != "" || b.Primitive != "" {
		if a.Primitive != b.Primitive {
			return fmt.Errorf("%q, want %q", a.Primitive, b.Primitive)
		}
		return nil
	}
	da, db := ra(*a.TypeIndex), rb(*b.TypeIndex)
	if reflect.TypeOf(da) != reflect.TypeOf(db) {
		return fmt.Errorf("%T, want %T", da, db)
	}
	switch da := da.(type) {
	case component.RecordDesc:
		db := db.(component.RecordDesc)
		if len(da.Fields) != len(db.Fields) {
			return fmt.Errorf("%d fields, want %d", len(da.Fields), len(db.Fields))
		}
		for i := range da.Fields {
			if da.Fields[i].Name != db.Fields[i].Name {
				return fmt.Errorf("field %d named %q, want %q", i, da.Fields[i].Name, db.Fields[i].Name)
			}
			if err := sameType(ra, da.Fields[i].Type, rb, db.Fields[i].Type); err != nil {
				return fmt.Errorf("field %s: %w", da.Fields[i].Name, err)
			}
		}
	case component.VariantDesc:
		db := db.(component.VariantDesc)
		if len(da.Cases) != len(db.Cases) {
			return fmt.Errorf("%d cases, want %d", len(da.Cases), len(db.Cases))
		}
		for i := range da.Cases {
			if da.Cases[i].Name != db.Cases[i].Name {
				return fmt.Errorf("case %d named %q, want %q", i, da.Cases[i].Name, db.Cases[i].Name)
			}
			if err := sameOptType(ra, da.Cases[i].Type, rb, db.Cases[i].Type); err != nil {
				return fmt.Errorf("case %s: %w", da.Cases[i].Name, err)
			}
		}
	case component.ListDesc:
		return sameType(ra, da.Element, rb, db.(component.ListDesc).Element)
	case component.OptionDesc:
		return sameType(ra, da.Element, rb, db.(component.OptionDesc).Element)
	case component.ResultDesc:
		db := db.(component.ResultDesc)
		if err := sameOptType(ra, da.Ok, rb, db.Ok); err != nil {
			return fmt.Errorf("ok: %w", err)
		}
		if err := sameOptType(ra, da.Err, rb, db.Err); err != nil {
			return fmt.Errorf("err: %w", err)
		}
	default:
		// The descriptors left (own, borrow, ...) hold no TypeRef here, so
		// they compare directly.
		if !reflect.DeepEqual(da, db) {
			return fmt.Errorf("%#v, want %#v", da, db)
		}
	}
	return nil
}
