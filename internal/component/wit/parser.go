package wit

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Error is a WIT error at a place in a source.
type Error struct {
	Pos Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

// maxDepth bounds both the nesting of package blocks and of types
// (list<list<...>>), as wit-parser does.
const maxDepth = 100

// parser is a recursive-descent port of wit-parser's (crates/wit-parser/
// src/ast.rs), so a source parses -- or fails, with the same message -- as
// it does there.
type parser struct {
	lx    lexer
	file  string
	lines []int // the offset each line starts at
}

// Parse parses one WIT file. filename is used for positions.
func Parse(filename, src string) (*Package, error) {
	p := &parser{file: filename, lines: []int{0}}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			p.lines = append(p.lines, i+1)
		}
	}
	lx, err := newLexer(src)
	if err != nil {
		return nil, p.wrap(err)
	}
	p.lx = lx
	pkg, err := p.parseFile()
	if err != nil {
		return nil, p.wrap(err)
	}
	return pkg, nil
}

// pos is where byte offset at of the source is.
func (p *parser) pos(at int) Pos {
	line := sort.Search(len(p.lines), func(i int) bool { return p.lines[i] > at }) - 1
	return Pos{File: p.file, Line: line + 1, Col: at - p.lines[line] + 1}
}

func (p *parser) wrap(err error) error {
	if e, ok := err.(*syntaxError); ok {
		return &Error{Pos: p.pos(e.at), Msg: e.msg}
	}
	return err
}

func (p *parser) parseFile() (*Package, error) {
	pkg := &Package{}
	save := p.lx
	docs, err := p.parseDocs()
	if err != nil {
		return nil, err
	}
	// parseDocs has lexed the token after the docs, and parsePackageName
	// the one after the name, so neither eat can fail.
	isPkg, _ := p.lx.eat(tPackage)
	keep := false
	if isPkg {
		if pkg.Name, pkg.NamePos, err = p.parsePackageName(); err != nil {
			return nil, err
		}
		keep, _ = p.lx.eat(tSemicolon)
	}
	if keep {
		pkg.Docs = docs
	} else {
		// "package a:b { ... }" is a nested package, an item of the file.
		pkg.Name, pkg.NamePos, p.lx = "", Pos{}, save
	}
	pkg.Items, pkg.Nested, err = p.parseDeclList(false, 0)
	return pkg, err
}

// parseDeclList parses package items up to the end of the source, or with
// inBlock up to and including the '}' closing a package block.
func (p *parser) parseDeclList(inBlock bool, depth int) ([]PackageItem, []*Package, error) {
	var items []PackageItem
	var nested []*Package
	for {
		var done bool
		var err error
		if inBlock {
			done, err = p.lx.eat(tRightBrace)
		} else {
			_, _, more, perr := p.lx.peek()
			done, err = !more, perr
		}
		if err != nil || done {
			return items, nested, err
		}
		attrs, err := p.parseAttrs()
		if err != nil {
			return nil, nil, err
		}
		// parseAttrs has lexed the next token.
		sp, t, ok, _ := p.lx.peek()
		var item PackageItem
		switch {
		case ok && t == tInterface:
			item, err = p.parseInterface(sp, attrs)
		case ok && t == tWorld:
			item, err = p.parseWorld(sp, attrs)
		case ok && t == tUse:
			item, err = p.parseToplevelUse(sp, attrs)
		case ok && t == tPackage:
			var pkg *Package
			pkg, err = p.parseNested(sp, attrs, depth)
			nested = append(nested, pkg)
		default:
			err = p.expected("`world`, `interface` or `use`", sp, t, ok)
		}
		if err != nil {
			return nil, nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}
}

func (p *parser) parseNested(sp span, attrs Attrs, depth int) (*Package, error) {
	p.lx.pos = sp.end
	if len(attrs.Gate) > 0 || len(attrs.ExternalID) > 0 {
		return nil, errAt(sp.start, "cannot place attributes on nested packages")
	}
	if depth >= maxDepth {
		return nil, errAt(sp.start, "package nesting too deep")
	}
	name, at, err := p.parsePackageName()
	if err != nil {
		return nil, err
	}
	if _, err := p.lx.expect(tLeftBrace); err != nil {
		return nil, err
	}
	items, nested, err := p.parseDeclList(true, depth+1)
	return &Package{Name: name, NamePos: at, Items: items, Nested: nested}, err
}

func (p *parser) parsePackageName() (string, Pos, error) {
	ns, at, err := p.parseID()
	if err != nil {
		return "", Pos{}, err
	}
	if _, err := p.lx.expect(tColon); err != nil {
		return "", Pos{}, err
	}
	name, _, err := p.parseID()
	if err != nil {
		return "", Pos{}, err
	}
	ver, err := p.parseOptVersion()
	return ns + ":" + name + ver, at, err
}

func (p *parser) parseToplevelUse(sp span, attrs Attrs) (*ToplevelUse, error) {
	p.lx.pos = sp.end
	u := &ToplevelUse{Pos: p.pos(sp.start), Attrs: attrs}
	var err error
	if u.Path, u.PathPos, err = p.parseUsePath(); err != nil {
		return nil, err
	}
	as, err := p.lx.eat(tAs)
	if err == nil && as {
		u.As, u.AsPos, err = p.parseID()
	}
	if err != nil {
		return nil, err
	}
	_, err = p.lx.expect(tSemicolon)
	return u, err
}

// rejectExternalID fails an item that may not carry an @external-id.
func rejectExternalID(attrs Attrs, what string) error {
	if len(attrs.ExternalID) > 0 {
		return &Error{Pos: attrs.ExternalID[0], Msg: "@external-id is not allowed on " + what}
	}
	return nil
}

func (p *parser) parseWorld(sp span, attrs Attrs) (*World, error) {
	if err := rejectExternalID(attrs, "world declarations"); err != nil {
		return nil, err
	}
	p.lx.pos = sp.end
	name, at, err := p.parseID()
	if err != nil {
		return nil, err
	}
	w := &World{Name: name, Pos: at, Attrs: attrs}
	if _, err := p.lx.expect(tLeftBrace); err != nil {
		return nil, err
	}
	for {
		if done, err := p.lx.eat(tRightBrace); err != nil || done {
			return w, err
		}
		attrs, err := p.parseAttrs()
		if err != nil {
			return nil, err
		}
		item, err := p.parseWorldItem(attrs)
		if err != nil {
			return nil, err
		}
		w.Items = append(w.Items, item)
	}
}

func (p *parser) parseWorldItem(attrs Attrs) (WorldItem, error) {
	// parseAttrs has lexed the next token.
	sp, t, ok, _ := p.lx.peek()
	switch {
	case ok && (t == tImport || t == tExport):
		p.lx.pos = sp.end
		ext, err := p.parseExtern(attrs)
		if err != nil {
			return nil, err
		}
		if t == tImport {
			return &Import{ext}, nil
		}
		return &Export{ext}, nil
	case ok && t == tUse:
		return p.parseUse(sp, attrs)
	case ok && t == tInclude:
		return p.parseInclude(sp, attrs)
	case ok && isTypeDefStart(t):
		return p.parseTypeDef(sp, t, attrs)
	}
	return nil, p.expected("`import`, `export`, `include`, `use`, or type definition", sp, t, ok)
}

func isTypeDefStart(t token) bool {
	switch t {
	case tType, tFlags, tResource, tRecord, tVariant, tEnum:
		return true
	}
	return false
}

// parseExtern parses what follows `import` or `export`.
func (p *parser) parseExtern(attrs Attrs) (Extern, error) {
	ext := Extern{Attrs: attrs}
	clone := *p
	name, at, err := clone.parseID()
	if err != nil {
		return ext, err
	}
	colon, err := clone.lx.eat(tColon)
	if err != nil {
		return ext, err
	}
	if colon {
		ext.Name, ext.Pos = name, at
		isFunc, err := clone.startsFunc()
		if err != nil {
			return ext, err
		}
		if isFunc {
			p.lx = clone.lx
			f, err := p.parseFunc()
			if err != nil {
				return ext, err
			}
			ext.Kind = &ExternFunc{Func: f}
			_, err = p.lx.expect(tSemicolon)
			return ext, err
		}
		// startsFunc has lexed the next token.
		if iface, _ := clone.lx.eat(tInterface); iface {
			p.lx = clone.lx
			items, err := p.parseInterfaceItems()
			ext.Kind = &ExternInterface{Items: items}
			return ext, err
		}
		// "label: path;" -- unless "id /" follows, which makes the ':' part
		// of a qualified "ns:pkg/iface" path.
		peek := clone
		_, _, idErr := peek.parseID()
		slash, _ := peek.lx.eat(tSlash)
		if idErr != nil || !slash {
			p.lx = clone.lx
			path, pathPos, err := p.parseUsePath()
			if err != nil {
				return ext, err
			}
			ext.Kind = &ExternNamedPath{Path: path, PathPos: pathPos}
			_, err = p.lx.expect(tSemicolon)
			return ext, err
		}
	}
	if ext.Name, ext.Pos, err = p.parseUsePath(); err != nil {
		return ext, err
	}
	ext.Kind = &ExternPath{}
	_, err = p.lx.expect(tSemicolon)
	return ext, err
}

// startsFunc says whether a func type comes next: `func`, `async`, or an
// accessor's `get(` or `set(`.
func (p *parser) startsFunc() (bool, error) {
	for _, t := range []token{tFunc, tAsync} {
		c := p.lx
		if ok, err := c.eat(t); err != nil || ok {
			return ok, err
		}
	}
	c := p.lx
	_, acc, err := eatAccessor(&c)
	return acc != NoAccessor, err
}

// eatAccessor eats the `get` or `set` of an accessor -- an identifier so
// named followed by '(' -- or nothing.
func eatAccessor(lx *lexer) (span, Accessor, error) {
	c := *lx
	sp, t, ok, err := c.next()
	if err != nil || !ok || t != tID {
		return span{}, NoAccessor, err
	}
	id, err := c.parseID(sp)
	if err != nil {
		return span{}, NoAccessor, err
	}
	acc := map[string]Accessor{"get": Getter, "set": Setter}[id]
	if acc == NoAccessor {
		return span{}, NoAccessor, nil
	}
	if paren, err := c.eat(tLeftParen); err != nil || !paren {
		return span{}, NoAccessor, err
	}
	lx.pos = sp.end
	return sp, acc, nil
}

func (p *parser) parseInterface(sp span, attrs Attrs) (*Interface, error) {
	if err := rejectExternalID(attrs, "interface declarations"); err != nil {
		return nil, err
	}
	p.lx.pos = sp.end
	name, at, err := p.parseID()
	if err != nil {
		return nil, err
	}
	items, err := p.parseInterfaceItems()
	return &Interface{Name: name, Pos: at, Items: items, Attrs: attrs}, err
}

func (p *parser) parseInterfaceItems() ([]InterfaceItem, error) {
	if _, err := p.lx.expect(tLeftBrace); err != nil {
		return nil, err
	}
	var items []InterfaceItem
	for {
		if done, err := p.lx.eat(tRightBrace); err != nil || done {
			return items, err
		}
		attrs, err := p.parseAttrs()
		if err != nil {
			return nil, err
		}
		// parseAttrs has lexed the next token.
		sp, t, ok, _ := p.lx.peek()
		var item InterfaceItem
		switch {
		case ok && isTypeDefStart(t):
			item, err = p.parseTypeDef(sp, t, attrs)
		case ok && (t == tID || t == tExplicitID):
			item, err = p.parseNamedFunc(attrs)
		case ok && t == tUse:
			item, err = p.parseUse(sp, attrs)
		default:
			err = p.expected("`type`, `resource` or `func`", sp, t, ok)
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
}

func (p *parser) parseNamedFunc(attrs Attrs) (*InterfaceFunc, error) {
	name, at, err := p.parseID()
	if err != nil {
		return nil, err
	}
	if _, err := p.lx.expect(tColon); err != nil {
		return nil, err
	}
	f, err := p.parseFunc()
	if err != nil {
		return nil, err
	}
	_, err = p.lx.expect(tSemicolon)
	return &InterfaceFunc{Name: name, Pos: at, Func: f, Attrs: attrs}, err
}

// parseUsePath parses "id" or "ns:pkg/id@ver".
func (p *parser) parseUsePath() (string, Pos, error) {
	id, at, err := p.parseID()
	if err != nil {
		return "", Pos{}, err
	}
	colon, err := p.lx.eat(tColon)
	if err != nil || !colon {
		return id, at, err
	}
	pkg, _, err := p.parseID()
	if err != nil {
		return "", Pos{}, err
	}
	if _, err := p.lx.expect(tSlash); err != nil {
		return "", Pos{}, err
	}
	name, _, err := p.parseID()
	if err != nil {
		return "", Pos{}, err
	}
	ver, err := p.parseOptVersion()
	return id + ":" + pkg + "/" + name + ver, at, err
}

func (p *parser) parseUse(sp span, attrs Attrs) (*Use, error) {
	p.lx.pos = sp.end
	u := &Use{Attrs: attrs}
	var err error
	if u.Path, u.PathPos, err = p.parseUsePath(); err != nil {
		return nil, err
	}
	for _, t := range []token{tPeriod, tLeftBrace} {
		if _, err := p.lx.expect(t); err != nil {
			return nil, err
		}
	}
	for {
		done, err := p.lx.eat(tRightBrace)
		if err != nil {
			return nil, err
		}
		if done {
			break
		}
		var n UseName
		if n.Name, n.Pos, err = p.parseID(); err != nil {
			return nil, err
		}
		as, err := p.lx.eat(tAs)
		if err == nil && as {
			n.As, n.AsPos, err = p.parseID()
		}
		if err != nil {
			return nil, err
		}
		u.Names = append(u.Names, n)
		comma, err := p.lx.eat(tComma)
		if err != nil {
			return nil, err
		}
		if !comma {
			if _, err := p.lx.expect(tRightBrace); err != nil {
				return nil, err
			}
			break
		}
	}
	_, err = p.lx.expect(tSemicolon)
	return u, err
}

func (p *parser) parseInclude(sp span, attrs Attrs) (*Include, error) {
	if err := rejectExternalID(attrs, "include statements"); err != nil {
		return nil, err
	}
	p.lx.pos = sp.end
	inc := &Include{Attrs: attrs}
	var err error
	if inc.Path, inc.PathPos, err = p.parseUsePath(); err != nil {
		return nil, err
	}
	// parseUsePath has lexed the next token.
	if with, _ := p.lx.eat(tWith); !with {
		_, err = p.lx.expect(tSemicolon)
		return inc, err
	}
	err = p.parseList(tLeftBrace, tRightBrace, func() error {
		name, at, err := p.parseID()
		if err != nil {
			return err
		}
		if _, err := p.lx.expect(tAs); err != nil {
			return err
		}
		as, _, err := p.parseID()
		inc.Renames = append(inc.Renames, IncludeName{Name: name, As: as, Pos: at})
		return err
	})
	return inc, err
}

// parseTypeDef parses a type definition starting with keyword t, at sp:
// "type x = T;" or one of the record, variant, enum, flags and resource
// forms.
func (p *parser) parseTypeDef(sp span, t token, attrs Attrs) (*TypeDef, error) {
	p.lx.pos = sp.end
	name, at, err := p.parseID()
	if err != nil {
		return nil, err
	}
	td := &TypeDef{Name: name, Pos: at, Attrs: attrs}
	switch t {
	case tType:
		if _, err := p.lx.expect(tEquals); err != nil {
			return nil, err
		}
		ty, err := p.parseType(0)
		if err != nil {
			return nil, err
		}
		td.Type = &TypeAlias{Target: ty}
		_, err = p.lx.expect(tSemicolon)
		return td, err
	case tFlags, tEnum:
		var names []string
		var poss []Pos
		err = p.parseList(tLeftBrace, tRightBrace, func() error {
			name, at, err := p.parseID()
			names, poss = append(names, name), append(poss, at)
			return err
		})
		if t == tFlags {
			td.Type = &Flags{Flags: names, Pos: poss}
		} else {
			td.Type = &Enum{Cases: names, Pos: poss}
		}
	case tRecord:
		rec := &Record{}
		td.Type = rec
		err = p.parseList(tLeftBrace, tRightBrace, func() error {
			name, at, err := p.parseID()
			if err != nil {
				return err
			}
			if _, err := p.lx.expect(tColon); err != nil {
				return err
			}
			ty, err := p.parseType(0)
			rec.Fields = append(rec.Fields, RecordField{Name: name, Pos: at, Type: ty})
			return err
		})
	case tVariant:
		v := &Variant{}
		td.Type = v
		err = p.parseList(tLeftBrace, tRightBrace, func() error {
			name, at, err := p.parseID()
			if err != nil {
				return err
			}
			c := VariantCase{Name: name, Pos: at}
			paren, err := p.lx.eat(tLeftParen)
			if err == nil && paren {
				var ty TypeRef
				if ty, err = p.parseType(0); err == nil {
					c.Type = &ty
					_, err = p.lx.expect(tRightParen)
				}
			}
			v.Cases = append(v.Cases, c)
			return err
		})
	default: // tResource
		res := &Resource{}
		td.Type = res
		err = p.parseResourceBody(res)
	}
	return td, err
}

// parseResourceBody parses "{ funcs }" or ";" after a resource's name.
func (p *parser) parseResourceBody(res *Resource) error {
	brace, err := p.lx.eat(tLeftBrace)
	if err != nil {
		return err
	}
	if !brace {
		_, err = p.lx.expect(tSemicolon)
		return err
	}
	for {
		done, err := p.lx.eat(tRightBrace)
		if err != nil || done {
			return err
		}
		attrs, err := p.parseAttrs()
		if err != nil {
			return err
		}
		m, err := p.parseResourceFunc(attrs)
		if err != nil {
			return err
		}
		res.Methods = append(res.Methods, m)
	}
}

func (p *parser) parseResourceFunc(attrs Attrs) (ResourceMethod, error) {
	m := ResourceMethod{Attrs: attrs}
	// parseAttrs has lexed the next token.
	sp, t, ok, _ := p.lx.peek()
	var err error
	switch {
	case ok && t == tConstructor:
		p.lx.pos = sp.end
		m.Name, m.Pos, m.IsConstructor = "constructor", p.pos(sp.start), true
		m.Func.Pos = m.Pos
		if _, err := p.lx.expect(tLeftParen); err != nil {
			return m, err
		}
		if m.Func.Params, err = p.parseParams(); err != nil {
			return m, err
		}
		if m.Func.Result, err = p.parseResult(); err != nil {
			return m, err
		}
	case ok && (t == tID || t == tExplicitID):
		if m.Name, m.Pos, err = p.parseID(); err != nil {
			return m, err
		}
		if _, err := p.lx.expect(tColon); err != nil {
			return m, err
		}
		if m.IsStatic, err = p.lx.eat(tStatic); err != nil {
			return m, err
		}
		if m.Func, err = p.parseFunc(); err != nil {
			return m, err
		}
	default:
		return m, p.expected("`constructor` or identifier", sp, t, ok)
	}
	_, err = p.lx.expect(tSemicolon)
	return m, err
}

// parseFunc parses "async? func(params) (-> T)?", or an accessor's
// "get(params) (-> T)?" or "set(params) (-> T)?".
func (p *parser) parseFunc() (Func, error) {
	var f Func
	var err error
	if f.Async, err = p.lx.eat(tAsync); err != nil {
		return f, err
	}
	var sp span
	if !f.Async {
		if sp, f.Accessor, err = eatAccessor(&p.lx); err != nil {
			return f, err
		}
	}
	if f.Accessor == NoAccessor {
		if sp, err = p.lx.expect(tFunc); err != nil {
			return f, err
		}
	}
	f.Pos = p.pos(sp.start)
	if _, err := p.lx.expect(tLeftParen); err != nil {
		return f, err
	}
	if f.Params, err = p.parseParams(); err != nil {
		return f, err
	}
	f.Result, err = p.parseResult()
	return f, err
}

// parseParams parses "name: T, ...)" after the '('.
func (p *parser) parseParams() ([]FuncParam, error) {
	var params []FuncParam
	err := p.parseListTrailer(tRightParen, func() error {
		name, at, err := p.parseID()
		if err != nil {
			return err
		}
		if _, err := p.lx.expect(tColon); err != nil {
			return err
		}
		ty, err := p.parseType(0)
		params = append(params, FuncParam{Name: name, Pos: at, Type: ty})
		return err
	})
	return params, err
}

func (p *parser) parseResult() (*TypeRef, error) {
	arrow, err := p.lx.eat(tRArrow)
	if err != nil || !arrow {
		return nil, err
	}
	ty, err := p.parseType(0)
	return &ty, err
}

// parseID parses an identifier: "name" or "%name".
func (p *parser) parseID() (string, Pos, error) {
	id, at, err := p.parseIDAt()
	return id, p.pos(at), err
}

func (p *parser) parseIDAt() (string, int, error) {
	sp, t, ok, err := p.lx.next()
	switch {
	case err != nil:
		return "", 0, err
	case ok && t == tID:
		id, err := p.lx.parseID(sp)
		return id, sp.start, err
	case ok && t == tExplicitID:
		id, err := p.lx.parseExplicitID(sp)
		return id, sp.start, err
	}
	return "", 0, p.expected("an identifier or string", sp, t, ok)
}

// expected is wit-parser's err_expected: what was wanted, and the token (or
// end of file) found instead.
func (p *parser) expected(what string, sp span, t token, ok bool) error {
	if !ok {
		return errAt(len(p.lx.src), "expected %s, found eof", what)
	}
	return errAt(sp.start, "expected %s, found %s", what, t.describe())
}

func (p *parser) parseOptVersion() (string, error) {
	at, err := p.lx.eat(tAt)
	if err != nil || !at {
		return "", err
	}
	v, err := p.parseVersion()
	return "@" + v, err
}

// parseVersion parses a semver version from its tokens: three integers,
// then any pre-release and build parts.
func (p *parser) parseVersion() (string, error) {
	first, err := p.lx.expect(tInteger)
	if err != nil {
		return "", err
	}
	end := first.end
	for _, t := range []token{tPeriod, tInteger, tPeriod, tInteger} {
		sp, err := p.lx.expect(t)
		if err != nil {
			return "", err
		}
		end = sp.end
	}
	for _, prefix := range []token{tMinus, tPlus} {
		if end, err = p.eatVersionIDs(prefix, end); err != nil {
			return "", err
		}
	}
	v := p.lx.src[first.start:end]
	if msg := checkSemver(v); msg != "" {
		return "", errAt(first.start, "%s", msg)
	}
	return v, nil
}

// eatVersionIDs eats a version's pre-release ('-') or build ('+') part --
// identifiers, integers and '-'s, '.'-separated -- returning where it ends.
// A '.' followed by anything else is WIT's, as in "use a:b/c@1.0.0-a.{x}".
func (p *parser) eatVersionIDs(prefix token, end int) (int, error) {
	if ok, err := p.lx.eat(prefix); err != nil || !ok {
		return end, err
	}
	for {
		c := p.lx
		sp, t, ok, err := c.next()
		if err == nil && ok && t == tPeriod {
			sp, t, ok, err = c.next()
		}
		if err != nil {
			return end, err
		}
		if !ok || t != tID && t != tInteger && t != tMinus {
			return end, nil
		}
		end, p.lx = sp.end, c
	}
}

// checkSemver validates a version as the semver crate does, returning its
// error message, or "". Its major, minor and patch numbers are digits, from
// the integer tokens they were lexed as.
func checkSemver(v string) string {
	text := v
	for i, what := range []string{"major", "minor", "patch"} {
		digits := len(text) - len(strings.TrimLeft(text, "0123456789"))
		n := text[:digits]
		text = text[digits:]
		if i < 2 {
			text = text[1:] // the '.'
		}
		what += " version number"
		switch {
		case len(n) > 1 && n[0] == '0':
			return "invalid leading zero in " + what
		case len(n) > 20 || len(n) == 20 && n > "18446744073709551615":
			return "value of " + what + " exceeds u64::MAX"
		}
	}
	pos := "patch version number"
	for _, part := range []struct {
		prefix, pos string
	}{{"-", "pre-release identifier"}, {"+", "build metadata"}} {
		rest, ok := strings.CutPrefix(text, part.prefix)
		if !ok {
			continue
		}
		pos = part.pos
		id, rest, msg := semverIdent(rest, part.prefix == "-")
		if msg == "" && id == "" {
			msg = "empty identifier segment in " + pos
		}
		if msg != "" {
			return strings.ReplaceAll(msg, "%s", pos)
		}
		text = rest
	}
	if text != "" {
		r, _ := utf8.DecodeRuneInString(text)
		return fmt.Sprintf("unexpected character %s after %s", rustChar(r), pos)
	}
	return ""
}

// semverIdent reads the '.'-separated identifiers of a pre-release (pre)
// or build part, as the semver crate does, returning them, the text after
// them and any error, with %s standing for the part's name.
func semverIdent(s string, pre bool) (id, rest, msg string) {
	n := 0
	for {
		seg := len(s[n:]) - len(strings.TrimLeft(s[n:], "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-"))
		dot := n+seg < len(s) && s[n+seg] == '.'
		switch {
		case seg == 0 && n == 0 && !dot:
			return "", s, ""
		case seg == 0:
			return "", "", "empty identifier segment in %s"
		case pre && seg > 1 && s[n] == '0' && strings.Trim(s[n:n+seg], "0123456789") == "":
			return "", "", "invalid leading zero in %s"
		}
		n += seg
		if !dot {
			return s[:n], s[n:], ""
		}
		n++
	}
}

// parseDocs skips the whitespace and comments ahead, saying whether there
// were comments: wit-parser takes every comment before an item as its
// docs. Only a package header's docs matter -- no two may have them -- so
// the other items skip theirs as they skip any whitespace.
func (p *parser) parseDocs() (bool, error) {
	docs := false
	for {
		c := p.lx
		_, t, ok, err := c.nextRaw()
		if err != nil {
			return false, err
		}
		if !ok || t != tWhitespace && t != tComment {
			return docs, nil
		}
		docs = docs || t == tComment
		p.lx = c
	}
}

// parseType parses a type at nesting depth depth.
func (p *parser) parseType(depth int) (TypeRef, error) {
	sp, t, ok, err := p.lx.next()
	if err != nil {
		return TypeRef{}, err
	}
	if depth >= maxDepth {
		at := len(p.lx.src)
		if ok {
			at = sp.start
		}
		return TypeRef{}, errAt(at, "type nesting too deep")
	}
	if !ok {
		return TypeRef{}, p.expected("a type", sp, t, ok)
	}
	ty := TypeRef{Pos: p.pos(sp.start), Kind: p.lx.text(sp)}
	inner := func() (*TypeRef, error) {
		t, err := p.parseType(depth + 1)
		return &t, err
	}
	switch t {
	case tU8, tU16, tU32, tU64, tS8, tS16, tS32, tS64, tF32, tF64, tChar, tBool, tString, tErrorContext:
		return ty, nil
	case tID:
		ty.Kind = "named"
		ty.Name, err = p.lx.parseID(sp)
		return ty, err
	case tExplicitID:
		ty.Kind = "named"
		ty.Name, err = p.lx.parseExplicitID(sp)
		return ty, err
	case tTuple:
		err = p.parseList(tLessThan, tGreaterThan, func() error {
			e, err := inner()
			ty.Tuple = append(ty.Tuple, e)
			return err
		})
		return ty, err
	case tList, tMap, tOption:
		if _, err := p.lx.expect(tLessThan); err != nil {
			return ty, err
		}
		if ty.Inner, err = inner(); err != nil {
			return ty, err
		}
		switch t {
		case tList:
			err = p.parseFixedLength(&ty)
		case tMap:
			if _, err = p.lx.expect(tComma); err == nil {
				ty.Inner2, err = inner()
			}
		}
		if err != nil {
			return ty, err
		}
		_, err = p.lx.expect(tGreaterThan)
		return ty, err
	case tResult:
		if lt, err := p.lx.eat(tLessThan); err != nil || !lt {
			return ty, err
		}
		under, err := p.lx.eat(tUnderscore)
		if err != nil {
			return ty, err
		}
		if under {
			if _, err := p.lx.expect(tComma); err != nil {
				return ty, err
			}
			if ty.Inner2, err = inner(); err != nil {
				return ty, err
			}
		} else {
			if ty.Inner, err = inner(); err != nil {
				return ty, err
			}
			comma, err := p.lx.eat(tComma)
			if err == nil && comma {
				ty.Inner2, err = inner()
			}
			if err != nil {
				return ty, err
			}
		}
		_, err = p.lx.expect(tGreaterThan)
		return ty, err
	case tFuture, tStream:
		if lt, err := p.lx.eat(tLessThan); err != nil || !lt {
			return ty, err
		}
		if ty.Inner, err = inner(); err != nil {
			return ty, err
		}
		_, err = p.lx.expect(tGreaterThan)
		return ty, err
	case tOwn, tBorrow:
		if _, err := p.lx.expect(tLessThan); err != nil {
			return ty, err
		}
		if ty.Name, ty.Pos, err = p.parseID(); err != nil {
			return ty, err
		}
		_, err = p.lx.expect(tGreaterThan)
		return ty, err
	}
	return ty, p.expected("a type", sp, t, ok)
}

// parseFixedLength parses the ", N" of list<T, N>, if there is one.
func (p *parser) parseFixedLength(ty *TypeRef) error {
	var err error
	if ty.Fixed, err = p.lx.eat(tComma); err != nil || !ty.Fixed {
		return err
	}
	sp, t, ok, err := p.lx.next()
	switch {
	case err != nil:
		return err
	case !ok || t != tInteger:
		return p.expected("fixed-length", sp, t, ok)
	}
	if ty.Length, err = parseU32(p.lx.text(sp)); err != nil {
		return errAt(sp.start, "invalid list size: %v", err)
	}
	return nil
}

// parseList parses start, then items separated by ',' up to end.
func (p *parser) parseList(start, end token, item func() error) error {
	if _, err := p.lx.expect(start); err != nil {
		return err
	}
	return p.parseListTrailer(end, item)
}

// parseListTrailer parses items separated by ',' (with an optional trailing
// one) up to and including end.
func (p *parser) parseListTrailer(end token, item func() error) error {
	for {
		if done, err := p.lx.eat(end); err != nil || done {
			return err
		}
		if err := item(); err != nil {
			return err
		}
		comma, err := p.lx.eat(tComma)
		if err != nil {
			return err
		}
		if !comma {
			_, err := p.lx.expect(end)
			return err
		}
	}
}

// parseAttrs parses the attributes before an item: @since, @unstable,
// @deprecated and @external-id.
func (p *parser) parseAttrs() (Attrs, error) {
	var a Attrs
	for {
		at, err := p.lx.eat(tAt)
		if err != nil || !at {
			return a, err
		}
		name, pos, err := p.parseID()
		if err != nil {
			return a, err
		}
		item := GateItem{Kind: name, Pos: pos}
		switch name {
		case "since", "deprecated", "unstable":
			key := "version"
			if name == "unstable" {
				key = "feature"
			}
			if err := p.parseKey(key); err != nil {
				return a, err
			}
			if name == "unstable" {
				item.Feature, _, err = p.parseID()
			} else {
				item.Version, err = p.parseVersion()
			}
			a.Gate = append(a.Gate, item)
		case "external-id":
			if _, err = p.lx.expect(tLeftParen); err == nil {
				_, err = p.lx.expect(tStringLiteral)
			}
			a.ExternalID = append(a.ExternalID, pos)
		default:
			return a, &Error{Pos: pos, Msg: fmt.Sprintf("unknown attribute `%s`", name)}
		}
		if err == nil {
			_, err = p.lx.expect(tRightParen)
		}
		if err != nil {
			return a, err
		}
	}
}

// parseKey parses the "(key =" of an attribute, failing on another key as
// wit-parser's eat_id does.
func (p *parser) parseKey(key string) error {
	if _, err := p.lx.expect(tLeftParen); err != nil {
		return err
	}
	id, at, err := p.parseIDAt()
	if err != nil {
		return err
	}
	if id != key {
		return errAt(at, "expected `%s`, found `%s`", key, id)
	}
	_, err = p.lx.expect(tEquals)
	return err
}
