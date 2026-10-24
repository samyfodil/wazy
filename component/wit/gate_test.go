package wit

import (
	"strings"
	"testing"
)

// The gate tests proper are the wit-parser inputs and testdata/cases, run
// under every feature set by oracle_test.go. These cover what they cannot:
// the version arithmetic, and the errors wit-parser reports in words of its
// own or never reaches.

func TestParseSemver(t *testing.T) {
	for _, s := range []string{
		"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7",
		"1.0.0-x.7.z.92", "1.0.0-x-y-z.--", "1.0.0+20130313144700", "1.0.0-beta+exp.sha.5114f85",
		"1.0.0+001", "0.2.0-rc-2023-11-10",
	} {
		if _, ok := parseSemver(s); !ok {
			t.Errorf("%s: rejected", s)
		}
	}
	for _, s := range []string{
		"", "1", "1.0", "1.0.0.0", "01.0.0", "1.01.0", "1.0.01", "a.b.c", "1.0.x",
		"1.0.0-", "1.0.0-01", "1.0.0-a..b", "1.0.0-a.", "1.0.0+", "1.0.0+a..b", "1.0.0-a$b",
		"1.0.0-a_b", "18446744073709551616.0.0",
	} {
		if _, ok := parseSemver(s); ok {
			t.Errorf("%s: accepted", s)
		}
	}
}

func TestSemverOrder(t *testing.T) {
	// Ascending, per semver.org's precedence example, then build metadata,
	// which the Rust semver crate (and so wit-parser) also orders.
	order := []string{
		"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.0+1", "1.0.0+2",
		"1.0.0+10", "1.0.0+a", "1.0.0+a.1", "1.0.1", "1.1.0", "2.0.0",
	}
	for i, a := range order {
		va, _ := parseSemver(a)
		for j, b := range order {
			vb, _ := parseSemver(b)
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := va.compare(vb); got != want {
				t.Errorf("compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
	a, _ := parseSemver("1.0.0+01")
	b, _ := parseSemver("1.0.0+1")
	if a.compare(b) != 0 {
		t.Error("build 01 and 1 differ")
	}
}

func TestGate_Errors(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"since not semver", "package a:b@1.0.0; interface i { @since(version = 1.0) f: func(); }", "interface a:b/i@1.0.0: f: @since(version = 1.0): not a valid semver version"},
		{"deprecated not semver", "package a:b@1.0.0; @since(version = 1.0.0) @deprecated(version = 1) interface i {}", "interface a:b/i@1.0.0: @deprecated(version = 1): not a valid semver version"},
		{"package version not semver", "package a:b@1.0; world w { @since(version = 1.0.0) import f: func(); }", "package [a:b@1.0]: version 1.0 is not a valid semver version"},
		{"world gate", "package a:b; @since(version = 1.0.0) world w {}", "wit: world w: package [a:b] contains a feature gate"},
		{"package use gate", "package a:b; interface i {} @since(version = 1.0.0) use i as j;", "package a:b: use i: package [a:b] contains"},
		{"package use alias left out", "package a:b; interface i {} @unstable(feature = x) use i as j; world w { import j; }", "world w: j: found a reference to a interface which is excluded"},
		{"package use left out", "package a:b; @unstable(feature = x) use c:d/e; world w { import e; }", "world w: e: found a reference to a interface which is excluded"},
		{"package use names types", "package a:b; interface i {} @unstable(feature = x) use i.{t};", "only an interface or world can"},
		{"method gate", "package a:b; interface i { resource r { @since(version = 1.0.0) m: func(); } }", "interface a:b/i: [method]r.m: package [a:b]"},
		{"use gate", "package a:b; interface j { type t = u8; } interface i { @since(version = 1.0.0) use j.{t}; }", "interface a:b/i: use j: package [a:b]"},
		{"include gate", "package a:b; world v {} world w { @since(version = 1.0.0) include v; }", "world w: include v: package [a:b]"},
		{"world item gate", "package a:b; world w { @since(version = 1.0.0) import f: func(); }", "world w: f: package [a:b]"},
		{"off resource method gate", "package a:b; interface i { @unstable(feature = x) resource r { @since(version = 1.0.0) m: func(); } }", "interface a:b/i: r.m: package [a:b]"},
		// Inside a container its gate leaves out, every item's gate is
		// still read, though only a type's decides anything.
		{"off func", "package a:b; @unstable(feature = x) interface i { @since(version = 1.0.0) @since(version = 1.0.0) f: func(); }", "interface a:b/i: f: cannot specify @since twice"},
		{"off type", "package a:b; @unstable(feature = x) interface i { @deprecated(version = 1.0.0) type t = u8; }", "interface a:b/i: t: cannot specify both @deprecated"},
		{"off use", "package a:b; interface j { type t = u8; } @unstable(feature = x) interface i { @deprecated(version = 1.0.0) use j.{t}; }", "interface a:b/i: use j: cannot specify both"},
		{"off method", "package a:b; @unstable(feature = x) interface i { @unstable(feature = x) resource r { @deprecated(version = 1.0.0) constructor(); } }", "interface a:b/i: r.constructor: cannot specify both"},
		{"off include", "package a:b; @unstable(feature = x) world w { @deprecated(version = 1.0.0) include v; }", "world w: include v: cannot specify both"},
		{"off import", "package a:b; @unstable(feature = x) world w { @deprecated(version = 1.0.0) import f: func(); }", "world w: f: cannot specify both"},
		{"off export inline", "package a:b; @unstable(feature = x) world w { @deprecated(version = 1.0.0) export e: interface {} }", "world w: e: cannot specify both"},
		{"off inline item", "package a:b; @unstable(feature = x) world w { @unstable(feature = x) import e: interface { @deprecated(version = 1.0.0) f: func(); } }", "interface e: f: cannot specify both"},
		{"kept inline in off world", "package a:b; @unstable(feature = x) world w { import e: interface { f: func(); f: func(); } }", "interface e: f is defined more than once"},
		{"off inline in kept world", "package a:b; world w { @unstable(feature = x) export e: interface { type t = u8; } }", "interface e: t: found a reference to a interface which is excluded due to its feature not being activated; this type is not gated by a feature but its interface is"},
		{"off interface twice", "package a:b; @unstable(feature = x) interface i {} interface i {}", "interface i is defined more than once"},
		{"off world twice", "package a:b; @unstable(feature = x) world w {} world w {}", "world w is defined more than once"},
		{"off func clashes", "package a:b; interface i { @unstable(feature = x) f: func(); f: func(); }", "f is defined more than once"},
		{"off type clashes", "package a:b; interface i { @unstable(feature = x) type t = u8; type t = u8; }", "t is defined more than once"},
		{"off use clashes", "package a:b; interface j { type t = u8; } interface i { @unstable(feature = x) use j.{t}; type t = u8; }", "t is defined more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse("test.wit", tc.src)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v\nwant it to contain %q", err, tc.want)
			}
		})
	}
}

func TestWithFeatures_AddsUp(t *testing.T) {
	const src = `package a:b;
interface i {
  @unstable(feature = x)
  fx: func();
  @unstable(feature = y)
  fy: func();
  @unstable(feature = z)
  fz: func();
}
@unstable(feature = z)
interface gone {}`
	pkg, err := Parse("test.wit", src, WithFeatures("x"), WithFeatures("y"))
	if err != nil {
		t.Fatal(err)
	}
	i := pkg.Interface("i")
	if i.Func("fx") == nil || i.Func("fy") == nil || i.Func("fz") != nil {
		t.Errorf("funcs %v", i.Funcs)
	}
	if pkg.Interface("gone") != nil || pkg.Interface("a:b/gone") != nil {
		t.Error("a left-out interface is found by name")
	}
	all, err := Parse("test.wit", src, WithAllFeatures())
	if err != nil || len(all.Interfaces) != 2 || len(all.Interface("i").Funcs) != 3 {
		t.Errorf("all features: %v, %v", all, err)
	}
}
