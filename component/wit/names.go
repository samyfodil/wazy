package wit

import (
	"strings"

	ast "github.com/samyfodil/wazy/internal/component/wit"
)

// Component names are compared for "strong uniqueness" as the component
// model (and wasmparser's names.rs) compares them: case and '-' do not
// count, a resource func named after its resource is its resource, and the
// [method], [static] and [get] annotations do not count either.

// plainName is a component name split into its annotations and the rest.
type plainName struct {
	ctor, method, static, get, set bool
	rest                           string
}

func splitName(name string) plainName {
	var n plainName
	for _, a := range []struct {
		prefix string
		flag   *bool
	}{{"[constructor]", &n.ctor}, {"[method]", &n.method}, {"[static]", &n.static}} {
		if rest, ok := strings.CutPrefix(name, a.prefix); ok {
			*a.flag, name = true, rest
			break
		}
	}
	if rest, ok := strings.CutPrefix(name, "[get]"); ok {
		n.get, name = true, rest
	} else if rest, ok := strings.CutPrefix(name, "[set]"); ok {
		n.set, name = true, rest
	}
	n.rest = name
	return n
}

// kebabCanonical lowercases a name and drops its '-'s.
func kebabCanonical(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "-", ""))
}

// canonicalName is the form two names share when they are not strongly
// unique.
func canonicalName(name string) string {
	n := splitName(name)
	lower := kebabCanonical(n.rest)
	if res, method, ok := strings.Cut(lower, "."); ok && res == method {
		return res
	}
	switch {
	case n.ctor:
		return "[constructor]" + lower
	case n.set:
		return "[set]" + lower
	}
	return lower
}

// nameScope is wit-parser's NameScope: the names of one interface, or of a
// world's imports or exports, which must be strongly unique.
type nameScope struct {
	canon map[string]string // canonical -> name
	raw   map[string]bool
	order []nameAt
}

type nameAt struct {
	name string
	pos  ast.Pos
}

func newNameScope() *nameScope {
	return &nameScope{canon: map[string]string{}, raw: map[string]bool{}}
}

func (s *nameScope) define(name string, pos ast.Pos) error {
	c := canonicalName(name)
	if prev, ok := s.canon[c]; ok {
		if prev == name {
			return errAt(pos, "duplicate name `%s`", name)
		}
		return errAt(pos, "name `%s` conflicts with previous name `%s`", name, prev)
	}
	s.canon[c] = name
	s.raw[name] = true
	s.order = append(s.order, nameAt{name, pos})
	return nil
}

// getterFor is the getter a setter name needs: "[set]x" needs "[get]x", and
// "[method][set]r.x" needs "[method][get]r.x".
func getterFor(setter string) string {
	return strings.Replace(setter, "[set]", "[get]", 1)
}

// checkSetters fails a setter with no getter of the same name in scope.
func (s *nameScope) checkSetters() error {
	for _, n := range s.order {
		if !splitName(n.name).set {
			continue
		}
		if g := getterFor(n.name); !s.raw[g] {
			return errAt(n.pos, "setter `%s` has no corresponding getter `%s`", n.name, g)
		}
	}
	return nil
}

// checkUnique fails the first of names (a record's fields, a variant's or
// enum's cases, a flags' flags) that is not strongly unique. A WIT
// identifier is always kebab-case, which wit-parser checks for first.
func checkUnique(desc string, names []string, pos []ast.Pos) error {
	seen := map[string]string{}
	for i, name := range names {
		c := kebabCanonical(name)
		if prev, ok := seen[c]; ok {
			if prev == name {
				return errAt(pos[i], "duplicate %s `%s`", desc, prev)
			}
			return errAt(pos[i], "%s `%s` conflicts with previous %s `%s`", desc, name, desc, prev)
		}
		seen[c] = name
	}
	return nil
}
