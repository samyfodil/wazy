package wit

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// Option configures how a parse applies feature gates.
type Option func(*config)

type config struct {
	features map[string]bool
	all      bool
}

// WithFeatures enables the named @unstable features: an item gated by
// @unstable(feature = name) is read only when name is enabled. It may be
// given more than once; the names add up.
func WithFeatures(names ...string) Option {
	return func(c *config) {
		if c.features == nil {
			c.features = map[string]bool{}
		}
		for _, n := range names {
			c.features[n] = true
		}
	}
}

// WithAllFeatures enables every @unstable feature.
func WithAllFeatures() Option {
	return func(c *config) { c.all = true }
}

func newConfig(opts []Option) config {
	var c config
	for _, o := range opts {
		o(&c)
	}
	return c
}

// stability is what an item's gate says, as wit-parser reads it: nothing (the
// item is always there), @since a version, or @unstable behind a feature.
// @deprecated only qualifies one of the other two.
type stability struct {
	since   string
	sinceV  semver
	feature string
}

// readGate checks a gate's attributes the way wit-parser does for every item,
// whether or not the item ends up read: each attribute at most once, @since
// and @unstable not both, @deprecated never alone, and versions that are
// semver. Error text follows wit-parser's, so it greps the same.
func readGate(g ast.Gate) (stability, error) {
	var st stability
	seen := map[string]bool{}
	for _, it := range g {
		if seen[it.Kind] {
			return st, fmt.Errorf("cannot specify @%s twice", it.Kind)
		}
		seen[it.Kind] = true
		if it.Kind == "unstable" {
			st.feature = it.Feature
			continue
		}
		v, ok := parseSemver(it.Version)
		if !ok {
			return st, fmt.Errorf("@%s(version = %s): not a valid semver version", it.Kind, it.Version)
		}
		if it.Kind == "since" {
			st.since, st.sinceV = it.Version, v
		}
	}
	switch {
	case seen["since"] && seen["unstable"]:
		return st, errors.New("cannot specify both @since and @unstable")
	case seen["deprecated"] && !seen["since"] && !seen["unstable"]:
		return st, errors.New("cannot specify both @deprecated without @since or @unstable")
	}
	return st, nil
}

// enabled reports whether the item gate g guards in package p is read, as
// wit-parser's include_stability decides it: an ungated or @since item always
// is, an @unstable one only when its feature is enabled. @since must name a
// released version of the package, so the package needs a version no earlier
// than the gate's.
func (r *resolver) enabled(p *pkgScope, g ast.Gate) (bool, error) {
	st, err := readGate(g)
	if err != nil {
		return false, err
	}
	switch {
	case st.feature != "":
		return r.cfg.all || r.cfg.features[st.feature], nil
	case st.since == "":
		return true, nil
	}
	_, ver, ok := strings.Cut(p.name, "@")
	if !ok {
		return false, fmt.Errorf("package [%s] contains a feature gate with a version specifier, so it must have a version", p.name)
	}
	pv, ok := parseSemver(ver)
	if !ok {
		return false, fmt.Errorf("package [%s]: version %s is not a valid semver version", p.name, ver)
	}
	if st.sinceV.compare(pv) > 0 {
		return false, fmt.Errorf("feature gate cannot reference unreleased version %s of package [%s] (current version %s)", st.since, p.name, ver)
	}
	return true, nil
}

// excludedError is wit-parser's error for a reference to an item that a gate
// left out of the same package. Another package's left-out items are simply
// not there, as in wit-parser, which resolves each package on its own.
type excludedError struct{ kind string }

func (e *excludedError) Error() string {
	return "found a reference to a " + e.kind + " which is excluded due to its feature not being activated"
}

func errExcluded(kind string) error { return &excludedError{kind} }

// offItem checks one item of an interface or world (owner) that its gate
// left out. The container's funcs go with it unchecked, as in wit-parser, but
// a type -- and so a use, which declares one -- is gated on its own: one its
// own gate keeps would belong to a container that is not there.
func (r *resolver) offItem(p *pkgScope, where, owner string, item any) error {
	var g ast.Gate
	var name string
	switch it := item.(type) {
	case *ast.TypeDef:
		if err := checkMethodGates(it); err != nil {
			return fmt.Errorf("wit: %s: %w", where, err)
		}
		g, name = it.Gate, it.Name
	case *ast.Use:
		g, name = it.Gate, "use "+it.Path
	case *ast.InterfaceFunc:
		return gateError(where, it.Name, it.Gate)
	case *ast.Import:
		return r.offWorldItem(p, where, it.Name, it.Gate, importParts(it.Type))
	case *ast.Export:
		return r.offWorldItem(p, where, it.Name, it.Gate, exportParts(it.Type))
	case *ast.Include:
		return gateError(where, "include "+it.Path, it.Gate)
	}
	on, err := r.enabled(p, g)
	if err != nil {
		return fmt.Errorf("wit: %s: %s: %w", where, name, err)
	}
	if on {
		return fmt.Errorf("wit: %s: %s: %w; this type is not gated by a feature but its %s is", where, name, errExcluded(owner), owner)
	}
	return nil
}

// offWorldItem checks an import or export of a world. An inline interface is
// an interface of its own, gated by the import's gate: when that is enabled
// it is resolved like any other, even in a world that is itself left out,
// which is what wit-parser does with it.
func (r *resolver) offWorldItem(p *pkgScope, where, name string, g ast.Gate, parts itemParts) error {
	if !parts.isIfc {
		return gateError(where, name, g)
	}
	on, err := r.enabled(p, g)
	if err != nil {
		return fmt.Errorf("wit: %s: %s: %w", where, name, err)
	}
	if on {
		_, err := r.newInterface(p, name, parts.inline)
		return err
	}
	for _, item := range parts.inline {
		if err := r.offItem(p, "interface "+name, "interface", item); err != nil {
			return err
		}
	}
	return nil
}

// checkMethodGates checks the gates of a resource's funcs.
func checkMethodGates(td *ast.TypeDef) error {
	res, ok := td.Type.(*ast.Resource)
	if !ok {
		return nil
	}
	for _, m := range res.Methods {
		if _, err := readGate(m.Gate); err != nil {
			return fmt.Errorf("%s.%s: %w", td.Name, methodName(m), err)
		}
	}
	return nil
}

func methodName(m ast.ResourceMethod) string {
	if m.IsConstructor {
		return "constructor"
	}
	return m.Name
}

func gateError(where, name string, g ast.Gate) error {
	if _, err := readGate(g); err != nil {
		return fmt.Errorf("wit: %s: %s: %w", where, name, err)
	}
	return nil
}

// semver is a semantic version, ordered as the Rust semver crate orders them,
// which is what wit-parser compares an @since gate with.
type semver struct {
	major, minor, patch uint64
	pre, build          []string
}

// parseSemver parses major.minor.patch, then an optional -pre-release and
// +build. Numbers have no leading zeros, except in build metadata.
func parseSemver(s string) (semver, bool) {
	var v semver
	rest, build, hasBuild := strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(rest, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, dst := range []*uint64{&v.major, &v.minor, &v.patch} {
		if !isNumeric(parts[i]) || hasLeadingZero(parts[i]) {
			return v, false
		}
		n, err := strconv.ParseUint(parts[i], 10, 64)
		if err != nil {
			return v, false
		}
		*dst = n
	}
	ok := true
	if hasPre {
		v.pre, ok = idents(pre, true)
	}
	if ok && hasBuild {
		v.build, ok = idents(build, false)
	}
	return v, ok
}

// idents splits dot-separated identifiers: non-empty runs of [0-9A-Za-z-],
// where a numeric pre-release identifier has no leading zero.
func idents(s string, pre bool) ([]string, bool) {
	ids := strings.Split(s, ".")
	for _, id := range ids {
		if id == "" || strings.TrimLeft(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
			return nil, false
		}
		if pre && isNumeric(id) && hasLeadingZero(id) {
			return nil, false
		}
	}
	return ids, true
}

func isNumeric(s string) bool {
	return s != "" && strings.TrimLeft(s, "0123456789") == ""
}

func hasLeadingZero(s string) bool { return len(s) > 1 && s[0] == '0' }

// compare orders a against b: -1, 0 or 1. A pre-release sorts before its
// release, and build metadata breaks the remaining ties, empty first.
func (a semver) compare(b semver) int {
	for _, c := range [...]int{cmp.Compare(a.major, b.major), cmp.Compare(a.minor, b.minor), cmp.Compare(a.patch, b.patch)} {
		if c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) != 0:
		return 1
	case len(a.pre) != 0 && len(b.pre) == 0:
		return -1
	}
	if c := cmpIdents(a.pre, b.pre); c != 0 {
		return c
	}
	return cmpIdents(a.build, b.build)
}

// cmpIdents compares identifier lists: numeric ones numerically and before
// any alphanumeric one, alphanumeric ones in ASCII order, and a list that is
// a prefix of the other first.
func cmpIdents(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		xn, yn := isNumeric(x), isNumeric(y)
		var c int
		switch {
		case xn && yn:
			x, y = strings.TrimLeft(x, "0"), strings.TrimLeft(y, "0")
			c = cmp.Compare(len(x), len(y))
			if c == 0 {
				c = strings.Compare(x, y)
			}
		case xn:
			c = -1
		case yn:
			c = 1
		default:
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}
