package wit

import "testing"

// The gate tests proper are the wit-parser inputs and testdata/cases, run
// under every feature set by oracle_test.go. These cover what they cannot:
// the version arithmetic, and the errors wit-parser reports in words of its
// own or never reaches.

func TestSemverOrder(t *testing.T) {
	// Ascending, per semver.org's precedence example, then build metadata,
	// which the Rust semver crate (and so wit-parser) also orders.
	order := []string{
		"0.9.9", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.0+1", "1.0.0+2",
		"1.0.0+10", "1.0.0+a", "1.0.0+a.1", "1.0.1", "1.1.0", "2.0.0",
	}
	for i, a := range order {
		va := parseSemver(a)
		for j, b := range order {
			vb := parseSemver(b)
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
	if parseSemver("1.0.0+01").compare(parseSemver("1.0.0+1")) != 0 {
		t.Error("build 01 and 1 differ")
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
