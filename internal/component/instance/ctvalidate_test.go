package instance

import (
	"strings"
	"testing"

	"github.com/samyfodil/wazy/api"
	"github.com/samyfodil/wazy/internal/component/binary"
)

// ctCanonEnv is an env over a hand-built component with one core memory and
// the types the canon option checks read: a stream of u8, a stream of
// string, a bare stream, and a func.
func ctCanonEnv(t *testing.T) *ctEnv {
	t.Helper()
	u8, str := binary.TypeRef{Primitive: "u8"}, binary.TypeRef{Primitive: "string"}
	descs := []binary.TypeDesc{
		binary.StreamDesc{Element: &u8},
		binary.StreamDesc{Element: &str},
		binary.StreamDesc{},
		binary.FuncDesc{Params: []binary.FuncParam{{Name: "s", Type: str}}},
	}
	c := &binary.Component{Aliases: []binary.AliasDef{{Sort: 0x00, CoreSort: 0x02, TargetKind: 0x01, Name: "mem"}}}
	for i, d := range descs {
		c.Types = append(c.Types, binary.Type{Descriptor: d})
		c.TypeSpace = append(c.TypeSpace, binary.TypeSpaceEntry{Kind: binary.TypeSpaceDef, Def: uint32(i)})
	}
	e := newCTEnv(c, nil)
	t.Cleanup(e.release)
	return e
}

func TestCheckCanonOptions(t *testing.T) {
	mem := binary.CanonOpt{Kind: ctOptMemory}
	realloc := binary.CanonOpt{Kind: ctOptRealloc}
	tests := []struct {
		name string
		cn   binary.Canon
		want string // "" for accepted
	}{
		{"memory out of bounds", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 3, Opts: []binary.CanonOpt{{Kind: ctOptMemory, Idx: 1}}}, "unknown memory 1: memory index out of bounds"},
		{"async twice", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 3, Opts: []binary.CanonOpt{{Kind: ctOptAsync}, {Kind: ctOptAsync}}}, "`async` is specified more than once"},
		{"callback twice", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 3, Opts: []binary.CanonOpt{{Kind: ctOptAsync}, {Kind: ctOptCallback}, {Kind: ctOptCallback}}}, "`callback` is specified more than once"},
		{"callback without async", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 3, Opts: []binary.CanonOpt{{Kind: ctOptCallback}}}, "cannot specify callback without async"},
		{"post-return in async", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 3, Opts: []binary.CanonOpt{{Kind: ctOptAsync}, {Kind: ctOptPostReturn}}}, "cannot specify post-return function in async"},
		{"lift of a non-func", binary.Canon{Kind: binary.CanonKindLift, TypeIdx: 0}, "type index 0 is not a function type"},
		{"lower with a callback", binary.Canon{Kind: binary.CanonKindLower, Opts: []binary.CanonOpt{{Kind: ctOptAsync}, {Kind: ctOptCallback}}}, "`callback` cannot be specified for lowerings"},
		{"task.return with realloc", binary.Canon{Kind: binary.CanonKindTaskReturn, Opts: []binary.CanonOpt{mem, realloc}}, "cannot specify `realloc` option on `task.return`"},
		{"task.return with post-return", binary.Canon{Kind: binary.CanonKindTaskReturn, Opts: []binary.CanonOpt{{Kind: ctOptPostReturn}}}, "cannot specify `post-return` option on `task.return`"},
		{"task.return async", binary.Canon{Kind: binary.CanonKindTaskReturn, Opts: []binary.CanonOpt{{Kind: ctOptAsync}}}, "cannot specify `async` option on `task.return`"},
		{"task.return of a string", binary.Canon{Kind: binary.CanonKindTaskReturn, TaskReturnResult: binary.FuncResults{Unnamed: &binary.TypeRef{Primitive: "string"}}}, "canonical option `memory` is required"},
		{"task.return of a string with memory", binary.Canon{Kind: binary.CanonKindTaskReturn, Opts: []binary.CanonOpt{mem}, TaskReturnResult: binary.FuncResults{Unnamed: &binary.TypeRef{Primitive: "string"}}}, ""},
		{"task.return named", binary.Canon{Kind: binary.CanonKindTaskReturn, TaskReturnResult: binary.FuncResults{Named: []binary.FuncResult{{Name: "a", Type: binary.TypeRef{Primitive: "u32"}}}}}, ""},
		{"task.return of a bad type", binary.Canon{Kind: binary.CanonKindTaskReturn, TaskReturnResult: binary.FuncResults{Unnamed: &binary.TypeRef{TypeIndex: ctIdx(3)}}}, "not a defined type"},
		{"task.return named of a bad type", binary.Canon{Kind: binary.CanonKindTaskReturn, TaskReturnResult: binary.FuncResults{Named: []binary.FuncResult{{Name: "a", Type: binary.TypeRef{TypeIndex: ctIdx(3)}}}}}, "not a defined type"},
		{"stream.read without memory", binary.Canon{Kind: binary.CanonKindStreamRead, TypeIdx: 0}, "canonical option `memory` is required"},
		{"stream.read of strings without realloc", binary.Canon{Kind: binary.CanonKindStreamRead, TypeIdx: 1, Opts: []binary.CanonOpt{mem}}, "canonical option `realloc` is required"},
		{"stream.write of strings", binary.Canon{Kind: binary.CanonKindStreamWrite, TypeIdx: 1, Opts: []binary.CanonOpt{mem}}, ""},
		{"bare stream.read", binary.Canon{Kind: binary.CanonKindStreamRead, TypeIdx: 2}, ""},
		{"future.read of a bad type", binary.Canon{Kind: binary.CanonKindFutureRead, TypeIdx: 9}, "type index out of bounds"},
		{"stream.read with post-return", binary.Canon{Kind: binary.CanonKindStreamRead, TypeIdx: 2, Opts: []binary.CanonOpt{{Kind: ctOptPostReturn}}}, "`post-return` cannot be specified for lowerings"},
		{"error-context.new without memory", binary.Canon{Kind: binary.CanonKindErrorContextNew}, "canonical option `memory` is required"},
		{"error-context.new async", binary.Canon{Kind: binary.CanonKindErrorContextNew, Opts: []binary.CanonOpt{mem, {Kind: ctOptAsync}}}, "cannot specify `async` option on `error-context.new`"},
		{"error-context.new", binary.Canon{Kind: binary.CanonKindErrorContextNew, Opts: []binary.CanonOpt{mem}}, ""},
		{"error-context.debug-message without realloc", binary.Canon{Kind: binary.CanonKindErrorContextDebugMessage, Opts: []binary.CanonOpt{mem}}, "canonical option `realloc` is required"},
		{"error-context.debug-message async", binary.Canon{Kind: binary.CanonKindErrorContextDebugMessage, Opts: []binary.CanonOpt{mem, realloc, {Kind: ctOptAsync}}}, "cannot specify `async` option on `error-context.debug-message`"},
		{"error-context.debug-message", binary.Canon{Kind: binary.CanonKindErrorContextDebugMessage, Opts: []binary.CanonOpt{mem, realloc}}, ""},
		{"no options to check", binary.Canon{Kind: binary.CanonKindResourceNew}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ctCanonEnv(t).checkCanonOptions(tc.cn)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The memory and realloc an async lowering or lifting needs, and the
// flattening that decides it.
func TestCTOptionsLowerAsync(t *testing.T) {
	str := ctPrim("string", false)
	u32 := ctPrim("u32", false)
	withResult := &ctFunc{async: true, results: []ctField{{t: u32}}}
	if err := (ctOptions{async: true}).lower(withResult, true); err == nil || !strings.Contains(err.Error(), "`memory` is required") {
		t.Fatalf("async lower with a result: %v", err)
	}
	if err := (ctOptions{async: true}).lower(withResult, false); err != nil {
		t.Fatalf("async lift of a u32: %v", err)
	}
	strResult := &ctFunc{async: true, results: []ctField{{t: str}}}
	if err := (ctOptions{async: true}).lower(strResult, false); err == nil || !strings.Contains(err.Error(), "`memory` is required") {
		t.Fatalf("async lift of a string: %v", err)
	}
	many := &ctFunc{params: make([]ctField, 5)}
	for i := range many.params {
		many.params[i].t = u32
	}
	if err := (ctOptions{async: true}).lower(many, true); err == nil || !strings.Contains(err.Error(), "`memory` is required") {
		t.Fatalf("async lower of 5 params: %v", err)
	}
	seventeen := &ctFunc{params: make([]ctField, 17)}
	for i := range seventeen.params {
		seventeen.params[i].t = u32
	}
	if err := (ctOptions{memory: true}).lower(seventeen, false); err == nil || !strings.Contains(err.Error(), "`realloc` is required") {
		t.Fatalf("lift of 17 params: %v", err)
	}

	flags := &ctVal{kind: ctvFlags, labels: make([]string, 33)}
	fixed := &ctVal{kind: ctvFixedList, length: 3, elems: []*ctVal{u32}}
	variant := &ctVal{kind: ctvVariant, elems: []*ctVal{nil, str}}
	for _, tc := range []struct {
		v    *ctVal
		want int
	}{{flags, 2}, {fixed, 3}, {variant, 3}, {&ctVal{kind: ctvMap}, 2}, {&ctVal{kind: ctvEnum}, 1}} {
		if got := ctFlatCount(tc.v); got != tc.want {
			t.Errorf("ctFlatCount(%s) = %d, want %d", tc.v.kind.desc(), got, tc.want)
		}
	}
	if !ctContainsPtr(&ctVal{kind: ctvOption, elems: []*ctVal{str}}) || ctContainsPtr(&ctVal{kind: ctvOwn}) {
		t.Error("ctContainsPtr")
	}
	if !ctContainsHandle(&ctVal{kind: ctvRecord, elems: []*ctVal{ctPrim("error-context", false)}}) || ctContainsHandle(str) {
		t.Error("ctContainsHandle")
	}
}

func TestCTMemoryLimits(t *testing.T) {
	for _, tc := range []struct {
		x    ctCoreExtern
		want string
	}{
		{ctCoreExtern{min: 65536, pageLog2: 16}, ""},
		{ctCoreExtern{min: 1, max: 65537, hasMax: true, pageLog2: 16}, "at most 65536 pages (4GiB)"},
		{ctCoreExtern{min: 1 << 48, is64: true, pageLog2: 16}, ""},
		{ctCoreExtern{min: 1<<48 + 1, is64: true, pageLog2: 16}, "at most 2**48 pages"},
		{ctCoreExtern{min: 1 << 32, pageLog2: 0}, ""},
		{ctCoreExtern{min: 1<<32 + 1, pageLog2: 0}, "at most 4294967296 pages"},
		{ctCoreExtern{min: 1, pageLog2: 40}, ""},
	} {
		err := ctMemoryLimits(&tc.x)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ctMemoryLimits(%+v) = %v, want %q", tc.x, err, tc.want)
		}
	}
}

func TestCTNames(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{"[get]a", "accessors feature"},
		{"NS:pkg/i", "character `N` is not lowercase"},
		{"ns-:pkg/i", "`ns-` is not in kebab case"},
		{"ns:pkg/I-", "`I-` is not in kebab case"},
		{"ns:", "`` is not in kebab case"},
		{"[static]a.B-", "`B-` is not in kebab case"},
		{"[static]a-.b", "`a-` is not in kebab case"},
		{"a:b/c@01.0.0", ""}, // the parse leaves the version to ctSemver
	} {
		_, err := ctParseName(tc.name)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ctParseName(%q) = %v, want %q", tc.name, err, tc.want)
		}
	}
	for _, tc := range []struct {
		version, want string
	}{
		{"01.0.0", "invalid leading zero in major version number"},
		{"18446744073709551616.0.0", "value of major version number exceeds u64::MAX"},
		{"1.0.0-01", "invalid leading zero in pre-release identifier"},
		{"1.0.0-a..b", "empty identifier segment in pre-release identifier"},
		{"1.0.0+b!", "unexpected character '!' after build metadata"},
		{"1.0.0-rc.1+b.2", ""},
	} {
		err := ctSemver(tc.version)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ctSemver(%q) = %v, want %q", tc.version, err, tc.want)
		}
	}
	if got := ctQuote("\x00x"); got != `'\0'` {
		t.Errorf(`ctQuote("\x00") = %s`, got)
	}
	if got := ctQuote(""); got != "''" {
		t.Errorf(`ctQuote("") = %s`, got)
	}

	same := func(a, b string) bool {
		x, err := ctParseName(a)
		if err != nil {
			t.Fatal(err)
		}
		y, err := ctParseName(b)
		if err != nil {
			t.Fatal(err)
		}
		return x.sameAs(y)
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"ns:pkg/i", "ns:pkg/I", true},
		{"ns:pkg/i@1.0.0", "ns:pkg/i@1.0.1", false},
		{"ns:pkg/a", "ns:pkg/b", false},
		{"ns:pkg/i", "ns:other/i", false},
		{"[method]a.a", "a", true},
		{"[constructor]a", "a", false},
		{"ns:pkg/i", "i", false},
	} {
		if got := same(tc.a, tc.b); got != tc.want {
			t.Errorf("%q same as %q = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCTNameCxDependencyNames(t *testing.T) {
	cx := ctNameCx{kind: "import"}
	for _, n := range []string{"unlocked-dep=<a:b>", "url=<https://x>", "integrity=<sha256-x>"} {
		if err := cx.add(n, nil, ctEntity{sort: ctSortFunc, fn: &ctFunc{}}); err != nil {
			t.Fatal(err)
		}
	}
	err := cx.add("unlocked-dep=<a:b>", nil, ctEntity{sort: ctSortFunc, fn: &ctFunc{}})
	if err == nil || !strings.Contains(err.Error(), "conflicts with previous name") {
		t.Fatalf("a dependency name twice: %v", err)
	}
	// Only an import may be one.
	ex := ctNameCx{kind: "export"}
	if err := ex.add("url=<x>", nil, ctEntity{sort: ctSortFunc, fn: &ctFunc{}}); err == nil {
		t.Fatal("an export named as a dependency was accepted")
	}
	if err := ex.add("[constructor]r", nil, ctEntity{sort: ctSortType}); err == nil || !strings.Contains(err.Error(), "item is not a func") {
		t.Fatalf("an annotated non-func: %v", err)
	}
	asyncCtor := ctEntity{sort: ctSortFunc, fn: &ctFunc{async: true}}
	if err := ex.add("[constructor]r", nil, asyncCtor); err == nil || !strings.Contains(err.Error(), "constructor function cannot be async") {
		t.Fatalf("an async constructor: %v", err)
	}
	if err := ex.add("[method]r.m", nil, ctEntity{sort: ctSortFunc, fn: &ctFunc{}}); err == nil || !strings.Contains(err.Error(), "at least one argument") {
		t.Fatalf("a method of no params: %v", err)
	}
	notSelf := &ctFunc{params: []ctField{{name: "this", t: ctPrim("u32", false)}}}
	if err := ex.add("[method]r.m", nil, ctEntity{sort: ctSortFunc, fn: notSelf}); err == nil || !strings.Contains(err.Error(), "called `self`") {
		t.Fatalf("a method without self: %v", err)
	}
	if err := ex.add("ns:pkg/i@x", nil, ctEntity{sort: ctSortInstance, inst: &ctInstance{}}); err == nil || !strings.Contains(err.Error(), "invalid interface version") {
		t.Fatalf("a bad version: %v", err)
	}
}

// wasmparser's test_split_canonical_version, and what is not a semver.
func TestCTSplitCanonicalVersion(t *testing.T) {
	for _, tc := range []struct{ version, canon, suffix string }{
		{"1.2.3", "1", ".2.3"},
		{"1.0.0", "1", ".0.0"},
		{"101.201.301", "101", ".201.301"},
		{"1.2.3+abc", "1", ".2.3+abc"},
		{"0.2.1", "0.2", ".1"},
		{"0.10.0+b.1", "0.10", ".0+b.1"},
		{"0.0.1", "0.0.1", ""},
		{"0.0.0", "0.0.0", ""},
		{"0.0.1+b", "0.0.1", "+b"},
		{"1.0.0-rc1", "1.0.0-rc1", ""},
		{"0.2.6-rc.1", "0.2.6-rc.1", ""},
		{"1.0.0-rc1+b", "1.0.0-rc1", "+b"},
		{"1.0.0-a-b+c-d", "1.0.0-a-b", "+c-d"},
	} {
		if canon, suffix, ok := ctSplitCanonicalVersion(tc.version); !ok || canon != tc.canon || suffix != tc.suffix {
			t.Errorf("%s: got %q %q %v, want %q %q", tc.version, canon, suffix, ok, tc.canon, tc.suffix)
		}
	}
	for _, v := range []string{"0.1.0-a+b-c+d", "1.2", "1.x.3", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3-a..b", "1.2.3+"} {
		if canon, suffix, ok := ctSplitCanonicalVersion(v); ok {
			t.Errorf("%s: got %q %q, want no split", v, canon, suffix)
		}
	}
}

// Attributes on a dependency name are left alone, as the name is; on any
// other they are checked, whatever context the name is in.
func TestCTNameCxAttributes(t *testing.T) {
	inst := ctEntity{sort: ctSortInstance, inst: &ctInstance{}}
	cx := ctNameCx{kind: "import"}
	if err := cx.add("unlocked-dep=<a:b>", &binary.NameAttrs{HasVersionSuffix: true}, inst); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		attrs binary.NameAttrs
		want  string
	}{
		{"[static]r.f", binary.NameAttrs{Implements: "a:b/c", HasImplements: true}, "name `[static]r.f` is not valid with `implements`"},
		{"a", binary.NameAttrs{Implements: "a:b/c@x", HasImplements: true}, "invalid interface version: unexpected character 'x'"},
		{"a:b/c@0.2", binary.NameAttrs{}, "invalid interface version"},
		{"a:b/d@0.2", binary.NameAttrs{VersionSuffix: ".1", HasVersionSuffix: true}, ""},
		{"a:b/d@0.2", binary.NameAttrs{VersionSuffix: ".2", HasVersionSuffix: true}, "import name `a:b/d@0.2.2` conflicts with previous name `a:b/d@0.2.1`"},
	} {
		err := cx.add(tc.name, &tc.attrs, inst)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s %+v: %v, want %q", tc.name, tc.attrs, err, tc.want)
		}
	}
}

// An instance type's own type exports are named within it, and only there.
func TestCTInstanceNamed(t *testing.T) {
	id := &ctTypeID{}
	rec := &ctVal{kind: ctvRecord, id: id, elems: []*ctVal{ctPrim("u32", false)}}
	inst := &ctInstance{exports: []ctNamed{
		{name: "r", ent: ctEntity{sort: ctSortType, ty: ctType{val: rec, id: id}}},
		{name: "f", ent: ctEntity{sort: ctSortFunc, fn: &ctFunc{params: []ctField{{t: rec}}}}},
		{name: "i", ent: ctEntity{sort: ctSortInstance, inst: &ctInstance{}}},
	}}
	if !ctInstanceNamed(inst, nil) {
		t.Fatal("an instance naming its own record was refused")
	}
	unnamed := &ctInstance{exports: []ctNamed{{name: "f", ent: ctEntity{sort: ctSortFunc, fn: &ctFunc{params: []ctField{{t: rec}}}}}}}
	if ctInstanceNamed(unnamed, nil) {
		t.Fatal("an instance using a record it does not name was accepted")
	}
	nested := &ctInstance{exports: []ctNamed{{name: "i", ent: ctEntity{sort: ctSortInstance, inst: unnamed}}}}
	if ctInstanceNamed(nested, nil) {
		t.Fatal("an instance of an instance using an unnamed record was accepted")
	}
	badType := &ctInstance{exports: []ctNamed{{name: "t", ent: ctEntity{sort: ctSortType, ty: ctType{fn: &ctFunc{params: []ctField{{t: rec}}}}}}}}
	if ctInstanceNamed(badType, nil) {
		t.Fatal("an instance exporting a func type using an unnamed record was accepted")
	}
	if !ctTypeContentsNamed(ctType{inst: inst}, nil) {
		t.Fatal("an instance type was refused")
	}
}

func TestCTValidateModule(t *testing.T) {
	if err := ctValidateModule([]byte{0x00, 'a', 's', 'm', 0x01, 0, 0, 0, 0x01}); err == nil {
		t.Fatal("a truncated module validated")
	}
	if !sameCoreTypes(nil, nil) || sameCoreTypes([]string{"i32"}, nil) {
		t.Fatal("sameCoreTypes")
	}
	if got := valueTypesDebug([]api.ValueType{api.ValueTypeI64}); got != "[I64]" {
		t.Fatalf("valueTypesDebug = %s", got)
	}
}

func ctIdx(i uint32) *uint32 { return &i }
