package instance

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/samyfodil/wazy/internal/component/binary"
)

// This file checks the names a component imports and exports by, as
// wasmparser's names.rs and ComponentState::validate_extern do: each a valid
// extern name -- a kebab-case label, possibly annotated `[constructor]`,
// `[method]` or `[static]`, or an interface name `ns:pkg/iface@version` --
// strongly unique among the imports or the exports, and an annotated one
// naming a func of the right shape for a resource of that name.

// ctIsKebab is wasmparser's KebabStr::is_kebab_case: words of letters and
// digits joined by single hyphens, each word lowercase or uppercase
// throughout and not starting with a digit.
func ctIsKebab(s string) bool {
	lower, upper, first, digit := false, false, true, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' && !upper:
			lower = true
		case c >= 'A' && c <= 'Z' && !lower:
			upper = true
		case c >= '0' && c <= '9' && (lower || upper || !first):
			digit = true
		case c == '-' && (lower || upper || digit):
			lower, upper, first, digit = false, false, false, false
		default:
			return false
		}
	}
	return s != "" && !strings.HasSuffix(s, "-")
}

// ctSameLabel reports whether two labels are the same as strong uniqueness
// compares them: lowercased, their hyphens dropped.
func ctSameLabel(a, b string) bool {
	i, j := 0, 0
	for {
		for i < len(a) && a[i] == '-' {
			i++
		}
		for j < len(b) && b[j] == '-' {
			j++
		}
		if i == len(a) || j == len(b) {
			return i == len(a) && j == len(b)
		}
		if ctLower(a[i]) != ctLower(b[j]) {
			return false
		}
		i++
		j++
	}
}

func ctLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// ctName is a parsed extern name.
type ctName struct {
	raw string
	// iface is set for an interface name.
	iface bool
	// dep is set for a dependency name (ctDependencyName).
	dep bool
	// annotation is "", "[constructor]", "[method]" or "[static]".
	annotation string
	// unannotated is the rest of a plain name: `r`, or `r.f` for a method or
	// static.
	unannotated string
	// suffix is an interface name's `version-suffix`, if it has one.
	suffix string
}

const (
	ctConstructor = "[constructor]"
	ctMethod      = "[method]"
	ctStatic      = "[static]"
)

// resource is the resource an annotated name names.
func (n ctName) resource() string {
	if i := strings.IndexByte(n.unannotated, '.'); i >= 0 && n.annotation != ctConstructor {
		return n.unannotated[:i]
	}
	return n.unannotated
}

// sameAs reports whether n and m are the same name as strong uniqueness
// compares them: PlainName's canonicalized, InterfaceName's components.
func (n ctName) sameAs(m ctName) bool {
	if n.iface != m.iface || n.dep || m.dep {
		return false
	}
	if n.iface {
		// Namespace and projection labels compare as labels; the version
		// exactly.
		a, av, _ := strings.Cut(n.raw, "@")
		b, bv, _ := strings.Cut(m.raw, "@")
		if av != bv {
			return false
		}
		for a != "" || b != "" {
			i := strings.IndexAny(a, ":/")
			j := strings.IndexAny(b, ":/")
			if (i < 0) != (j < 0) || i >= 0 && a[i] != b[j] {
				return false
			}
			if i < 0 {
				return ctSameLabel(a, b)
			}
			if !ctSameLabel(a[:i], b[:j]) {
				return false
			}
			a, b = a[i+1:], b[j+1:]
		}
		return true
	}
	ak, bk := n.plainKey(), m.plainKey()
	return ak.constructor == bk.constructor && ctSameLabel(ak.label, bk.label)
}

// plainKey is what PlainName's canonicalized keeps of a plain name: the
// label, `f` of `[method]f.f` alone, and whether it is a constructor.
func (n ctName) plainKey() (k struct {
	label       string
	constructor bool
},
) {
	k.label = n.unannotated
	if dot := strings.IndexByte(k.label, '.'); dot >= 0 && ctSameLabel(k.label[:dot], k.label[dot+1:]) {
		k.label = k.label[:dot]
		return k
	}
	k.constructor = n.annotation == ctConstructor
	return k
}

// ctParseName is wasmparser's ComponentNameParser for the names this
// runtime's features allow: no `[get]`/`[set]` accessors, and no nested
// namespaces or projections.
func ctParseName(name string) (ctName, error) {
	p := ctNameParser{next: name}
	n, err := p.parse()
	if err != nil {
		return n, err
	}
	if p.next != "" {
		return n, fmt.Errorf("trailing characters found: `%s`", p.next)
	}
	n.raw = name
	return n, nil
}

type ctNameParser struct{ next string }

func (p *ctNameParser) parse() (ctName, error) {
	if strings.ContainsRune(p.next, ':') {
		return ctName{iface: true}, p.interfaceName()
	}
	var n ctName
	for _, a := range []string{ctConstructor, ctMethod, ctStatic} {
		if p.eat(a) {
			n.annotation = a
			break
		}
	}
	if strings.HasPrefix(p.next, "[get]") || strings.HasPrefix(p.next, "[set]") {
		return n, fmt.Errorf("`[get]` and `[set]` names require the component model accessors feature")
	}
	n.unannotated = p.next
	if n.annotation == ctMethod || n.annotation == ctStatic {
		i := strings.IndexByte(p.next, '.')
		if i < 0 {
			return n, fmt.Errorf("failed to find `.` character")
		}
		if err := ctKebab(p.next[:i]); err != nil {
			return n, err
		}
		p.next = p.next[i+1:]
	}
	err := ctKebab(p.next)
	p.next = ""
	return n, err
}

func (p *ctNameParser) interfaceName() error {
	if _, err := p.lowercaseKebab(); err != nil {
		return err
	}
	if !p.eat(":") {
		return fmt.Errorf("expected `:` at `%s`", p.next)
	}
	if _, err := p.lowercaseKebab(); err != nil {
		return err
	}
	if !p.eat("/") {
		return fmt.Errorf("expected `/` after package name")
	}
	if _, err := p.kebab(); err != nil {
		return err
	}
	if p.eat("@") {
		p.next = ""
	}
	return nil
}

func (p *ctNameParser) eat(prefix string) bool {
	if strings.HasPrefix(p.next, prefix) {
		p.next = p.next[len(prefix):]
		return true
	}
	return false
}

// kebab takes the kebab label that starts p.next: up to the first byte no
// label holds, or all of it.
func (p *ctNameParser) kebab() (string, error) {
	i := strings.IndexFunc(p.next, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-')
	})
	if i < 0 {
		i = len(p.next)
	}
	s := p.next[:i]
	p.next = p.next[i:]
	return s, ctKebab(s)
}

func (p *ctNameParser) lowercaseKebab() (string, error) {
	s, err := p.kebab()
	if err != nil {
		return s, err
	}
	for _, c := range s {
		if c >= 'A' && c <= 'Z' {
			return s, fmt.Errorf("character `%c` is not lowercase in package name/namespace", c)
		}
	}
	return s, nil
}

func ctKebab(s string) error {
	if !ctIsKebab(s) {
		return fmt.Errorf("`%s` is not in kebab case", s)
	}
	return nil
}

// ctNameCx is one context names must be unique in -- the imports, or the
// exports, of a component or of a component or instance type -- and the
// resources it names, which an annotated func's name must agree with.
type ctNameCx struct {
	kind string // "import" or "export"
	// bag is set for the exports of an instance made of exports, which
	// introduces no index and so names no resource.
	bag bool
	// names are the names so far; a context rarely holds many, so each new
	// one is compared with each of them.
	names []ctName
	// res names the resources this context imports or exports, by the
	// identity of the type index that names them.
	res []ctResBinding
}

// emptied is cx with nothing in it but room, for reuse.
func (cx ctNameCx) emptied() ctNameCx {
	return ctNameCx{names: ctEmpty(cx.names), res: ctEmpty(cx.res)}
}

type ctResBinding struct {
	id   *ctTypeID
	name string
}

// ctDependencyName reports whether an import name is one of the dependency
// forms an earlier Component Model gave names -- `unlocked-dep=<pkg>`,
// `locked-dep=<pkg>`, `url=<url>`, `relative-url=<url>`, `integrity=<hash>`
// -- which wac still writes when it composes components, and which the
// current names leave to attributes. Such a name is unique by its exact
// text; there is nothing else in it to check.
func ctDependencyName(name string) bool {
	for _, p := range [...]string{"unlocked-dep=", "locked-dep=", "url=", "relative-url=", "integrity="} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// add checks name and its attributes, under which ent is imported or
// exported, and records it.
func (cx *ctNameCx) add(name string, attrs *binary.NameAttrs, ent ctEntity) error {
	if cx.kind == "import" && ctDependencyName(name) {
		for _, prev := range cx.names {
			if prev.raw == name {
				return fmt.Errorf("import name `%s` conflicts with previous name `%s`", name, name)
			}
		}
		cx.names = append(cx.names, ctName{raw: name, dep: true})
		return nil
	}
	n, err := ctParseName(name)
	if err != nil {
		return fmt.Errorf("%s name `%s` is not a valid extern name: %w", cx.kind, name, err)
	}
	if attrs != nil {
		if err := ctCheckAttrs(n, attrs, ent); err != nil {
			return err
		}
		if attrs.HasVersionSuffix && !attrs.HasImplements {
			n.suffix = attrs.VersionSuffix
		}
	}
	if ent.sort == ctSortType && ent.ty.res != nil && !cx.bag {
		cx.res = append(cx.res, ctResBinding{ent.ty.id, name})
	}
	if err := cx.validate(n, ent); err != nil {
		return fmt.Errorf("%s name `%s` is not valid: %w", cx.kind, name, err)
	}
	for _, prev := range cx.names {
		if prev.raw == name || n.sameAs(prev) {
			return fmt.Errorf("%s name `%s` conflicts with previous name `%s`", cx.kind, n.full(), prev.full())
		}
	}
	cx.names = append(cx.names, n)
	return nil
}

// full is n as wasmparser's with_version_suffix prints it: a versioned
// interface name with its `version-suffix` put back.
func (n ctName) full() string {
	if strings.IndexByte(n.raw, '@') >= 0 {
		return n.raw + n.suffix
	}
	return n.raw
}

// ctCheckAttrs is validate_extern's checks of a name's attributes: a
// `version-suffix` and an `implements` only on an instance, the one non-empty
// and finishing the version of an interface name, the other an interface
// name for an instance with a plain name. An `external-id` may be anything.
func ctCheckAttrs(n ctName, a *binary.NameAttrs, ent ctEntity) error {
	if a.HasVersionSuffix {
		switch {
		case ent.sort != ctSortInstance:
			return fmt.Errorf("only instances can have a `version-suffix`")
		case a.VersionSuffix == "":
			return fmt.Errorf("version suffix cannot be empty")
		case !a.HasImplements && !n.iface:
			return fmt.Errorf("`version-suffix` requires an interface name or `implements`")
		}
	}
	if !a.HasImplements {
		return nil
	}
	if n.iface || n.annotation != "" {
		return fmt.Errorf("name `%s` is not valid with `implements`", n.raw)
	}
	if ent.sort != ctSortInstance {
		return fmt.Errorf("only instances can have an `implements`")
	}
	in, err := ctParseName(a.Implements)
	if err != nil {
		return fmt.Errorf("`%s` is not a valid name: %w", a.Implements, err)
	}
	if !in.iface {
		return fmt.Errorf("name `%s` must be an interface", a.Implements)
	}
	if err := ctIfaceVersion(a.Implements, a.VersionSuffix, a.HasVersionSuffix); err != nil {
		return fmt.Errorf("invalid interface version: %w", err)
	}
	return nil
}

// ctIfaceVersion is InterfaceName::version: the version of interface name
// raw, with suffix appended when it has one, is a valid semver, and suffix
// is exactly what canonicalizing that splits off it.
func ctIfaceVersion(raw, suffix string, hasSuffix bool) error {
	at := strings.IndexByte(raw, '@')
	switch {
	case at < 0 && hasSuffix:
		return fmt.Errorf("a version suffix requires the name to have a version")
	case at < 0:
		return nil
	case !hasSuffix:
		return ctSemver(raw[at+1:])
	}
	prefix := raw[at+1:]
	full := prefix + suffix
	if err := ctSemver(full); err != nil {
		return err
	}
	if canon, rest, ok := ctSplitCanonicalVersion(full); !ok || canon != prefix || rest != suffix {
		return fmt.Errorf("`%s` is not the canonical version of `%s`", prefix, full)
	}
	return nil
}

// ctSplitCanonicalVersion is split_canonical_version: version, a valid
// semver, split into the canonical version an interface name carries and
// the `version-suffix` that keeps the rest. Build metadata is always in the
// suffix; a pre-release version is otherwise its own canonical version; and
// otherwise the split is after a nonzero major, or else a nonzero minor.
func ctSplitCanonicalVersion(version string) (canon, suffix string, ok bool) {
	withoutBuild, build, hasBuild := strings.Cut(version, "+")
	if hasBuild && !ctAllSegments(build, ctIsIdentifier) {
		return "", "", false
	}
	core, pre, hasPre := strings.Cut(withoutBuild, "-")
	if hasPre && !ctAllSegments(pre, ctIsIdentifier) {
		return "", "", false
	}
	major, rest, _ := strings.Cut(core, ".")
	minor, patch, _ := strings.Cut(rest, ".")
	if strings.Count(core, ".") != 2 || !ctIsVersionNumber(major) || !ctIsVersionNumber(minor) || !ctIsVersionNumber(patch) {
		return "", "", false
	}
	split := len(withoutBuild)
	switch {
	case !hasPre && major != "0":
		split = len(major)
	case !hasPre && minor != "0":
		split = len(major) + 1 + len(minor)
	}
	return version[:split], version[split:], true
}

func ctAllSegments(s string, ok func(string) bool) bool {
	for seg := range strings.SplitSeq(s, ".") {
		if !ok(seg) {
			return false
		}
	}
	return true
}

func ctIsIdentifier(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return s != ""
}

func ctIsVersionNumber(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s == "0" || s != "" && s[0] != '0'
}

// validate is ComponentState::validate: an interface name's version, with
// its `version-suffix`, parses and splits so, and an annotated name names a
// func of its shape for a resource this context names so.
func (cx *ctNameCx) validate(n ctName, ent ctEntity) error {
	if n.iface {
		if err := ctIfaceVersion(n.raw, n.suffix, n.suffix != ""); err != nil {
			return fmt.Errorf("invalid interface version: %w", err)
		}
		return nil
	}
	if n.annotation == "" {
		return nil
	}
	if ent.sort != ctSortFunc {
		return fmt.Errorf("item is not a func")
	}
	f := ent.fn
	switch n.annotation {
	case ctConstructor:
		if f.async {
			return fmt.Errorf("constructor function cannot be async")
		}
		if len(f.results) == 0 {
			return fmt.Errorf("function should return one value")
		}
		r := f.results[0].t
		if r.kind == ctvResult && r.elems[0] != nil {
			r = r.elems[0]
		}
		if r.kind != ctvOwn {
			return fmt.Errorf("function should return `(own $T)` or `(result (own $T))`")
		}
		return cx.resourceName(r, n.resource())
	case ctMethod:
		if len(f.params) == 0 {
			return fmt.Errorf("function should have at least one argument")
		}
		if f.params[0].name != "self" {
			return fmt.Errorf("function should have a first argument called `self`")
		}
		if f.params[0].t.kind != ctvBorrow {
			return fmt.Errorf("function should take a first argument of `(borrow $T)`")
		}
		return cx.resourceName(f.params[0].t, n.resource())
	default: // ctStatic
		for _, r := range cx.res {
			if r.name == n.resource() {
				return nil
			}
		}
		return fmt.Errorf("static resource name is not known in this context")
	}
}

// resourceName checks the resource handle h names is the one this context
// names name.
func (cx *ctNameCx) resourceName(h *ctVal, name string) error {
	for _, r := range cx.res {
		if r.id == h.id {
			if r.name != name {
				return fmt.Errorf("function does not match expected resource name `%s`", r.name)
			}
			return nil
		}
	}
	return fmt.Errorf("resource used in function does not have a name in this context")
}

// ctSemver parses version as the semver crate's Version::parse does,
// failing in its words.
func ctSemver(version string) error {
	if version == "" {
		return fmt.Errorf("empty string, expected a semver version")
	}
	rest := version
	positions := [...]string{"major version number", "minor version number", "patch version number"}
	for i, pos := range positions {
		var err error
		if rest, err = ctSemverNumber(rest, pos); err != nil {
			return err
		}
		if i == len(positions)-1 {
			break
		}
		switch {
		case strings.HasPrefix(rest, "."):
			rest = rest[1:]
		case rest == "":
			return fmt.Errorf("unexpected end of input while parsing %s", pos)
		default:
			return fmt.Errorf("unexpected character %s after %s", ctQuote(rest), pos)
		}
	}
	pos := positions[2]
	if strings.HasPrefix(rest, "-") {
		pos = "pre-release identifier"
		var err error
		if rest, err = ctSemverIdentifier(rest[1:], pos, true); err != nil {
			return err
		}
	}
	if strings.HasPrefix(rest, "+") {
		pos = "build metadata"
		var err error
		if rest, err = ctSemverIdentifier(rest[1:], pos, false); err != nil {
			return err
		}
	}
	if rest != "" {
		return fmt.Errorf("unexpected character %s after %s", ctQuote(rest), pos)
	}
	return nil
}

func ctSemverNumber(s, pos string) (string, error) {
	n := 0
	var value uint64
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		if value == 0 && n > 0 {
			return s, fmt.Errorf("invalid leading zero in %s", pos)
		}
		d := uint64(s[n] - '0')
		if value > (^uint64(0)-d)/10 {
			return s, fmt.Errorf("value of %s exceeds u64::MAX", pos)
		}
		value = value*10 + d
		n++
	}
	switch {
	case n > 0:
		return s[n:], nil
	case s != "":
		return s, fmt.Errorf("unexpected character %s while parsing %s", ctQuote(s), pos)
	default:
		return s, fmt.Errorf("unexpected end of input while parsing %s", pos)
	}
}

// ctSemverIdentifier reads dot-separated segments of letters, digits and
// hyphens; a pre-release's numeric segments have no leading zero.
func ctSemverIdentifier(s, pos string, pre bool) (string, error) {
	total, seg, nondigit := 0, 0, false
	for {
		var c byte
		if total+seg < len(s) {
			c = s[total+seg]
		}
		switch {
		case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '-':
			seg++
			nondigit = true
			continue
		case c >= '0' && c <= '9':
			seg++
			continue
		}
		if seg == 0 {
			return s, fmt.Errorf("empty identifier segment in %s", pos)
		}
		if pre && seg > 1 && !nondigit && s[total] == '0' {
			return s, fmt.Errorf("invalid leading zero in %s", pos)
		}
		total += seg
		if c != '.' {
			return s[total:], nil
		}
		total++
		seg, nondigit = 0, false
	}
}

// ctQuote quotes the first character of s as Rust's char Debug does.
func ctQuote(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	switch {
	case size == 0:
		return "''"
	case r == 0:
		return `'\0'`
	}
	return fmt.Sprintf("%q", r)
}
