package wit

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// token is one kind of WIT token, as wit-parser's lexer (crates/wit-parser/
// src/ast/lex.rs) defines them.
type token int

const (
	tWhitespace token = iota
	tComment

	tEquals
	tComma
	tColon
	tPeriod
	tSemicolon
	tLeftParen
	tRightParen
	tLeftBrace
	tRightBrace
	tLessThan
	tGreaterThan
	tRArrow
	tStar
	tAt
	tSlash
	tPlus
	tMinus
	tStringLiteral

	tUse
	tType
	tFunc
	tU8
	tU16
	tU32
	tU64
	tS8
	tS16
	tS32
	tS64
	tF32
	tF64
	tChar
	tRecord
	tResource
	tOwn
	tBorrow
	tFlags
	tVariant
	tEnum
	tBool
	tString
	tOption
	tResult
	tFuture
	tStream
	tErrorContext
	tList
	tMap
	tUnderscore
	tAs
	tFrom
	tStatic
	tInterface
	tTuple
	tImport
	tExport
	tWorld
	tPackage
	tConstructor
	tAsync

	tID
	tExplicitID

	tInteger

	tInclude
	tWith
)

var keywords = map[string]token{
	"use": tUse, "type": tType, "func": tFunc,
	"u8": tU8, "u16": tU16, "u32": tU32, "u64": tU64,
	"s8": tS8, "s16": tS16, "s32": tS32, "s64": tS64,
	"f32": tF32, "f64": tF64, "char": tChar,
	"resource": tResource, "own": tOwn, "borrow": tBorrow, "record": tRecord,
	"flags": tFlags, "variant": tVariant, "enum": tEnum, "bool": tBool,
	"string": tString, "option": tOption, "result": tResult, "future": tFuture,
	"stream": tStream, "error-context": tErrorContext, "list": tList, "map": tMap,
	"_": tUnderscore, "as": tAs, "from": tFrom, "static": tStatic,
	"interface": tInterface, "tuple": tTuple, "world": tWorld, "import": tImport,
	"export": tExport, "package": tPackage, "constructor": tConstructor,
	"include": tInclude, "with": tWith, "async": tAsync,
}

var descriptions = [...]string{
	tWhitespace: "whitespace", tComment: "a comment",
	tEquals: "'='", tComma: "','", tColon: "':'", tPeriod: "'.'",
	tSemicolon: "';'", tLeftParen: "'('", tRightParen: "')'",
	tLeftBrace: "'{'", tRightBrace: "'}'", tLessThan: "'<'", tGreaterThan: "'>'",
	tRArrow: "`->`", tStar: "`*`", tAt: "`@`", tSlash: "`/`", tPlus: "`+`",
	tMinus: "`-`", tStringLiteral: "a string literal",
	tID: "an identifier", tExplicitID: "an '%' identifier", tInteger: "an integer",
}

// keywordNames are the keywords by token.
var keywordNames = func() map[token]string {
	m := map[token]string{}
	for kw, t := range keywords {
		m[t] = kw
	}
	return m
}()

// describe names a token the way wit-parser's errors do.
func (t token) describe() string {
	if d := descriptions[t]; d != "" {
		return d
	}
	return "keyword `" + keywordNames[t] + "`"
}

// span is a byte range of one source.
type span struct{ start, end int }

// lexer tokenizes one WIT source. It is a value, so a copy is a lookahead
// that can be dropped or kept, as wit-parser clones its Tokenizer.
type lexer struct {
	src string
	pos int
}

// syntaxError is an error at a byte offset of the source being parsed.
type syntaxError struct {
	at  int
	msg string
}

func (e *syntaxError) Error() string { return e.msg }

func errAt(at int, format string, args ...any) *syntaxError {
	return &syntaxError{at: at, msg: fmt.Sprintf(format, args...)}
}

func newLexer(src string) (lexer, error) {
	if err := detectInvalidInput(src); err != nil {
		return lexer{}, err
	}
	l := lexer{src: src}
	l.eatc(0xfeff) // a UTF-8 byte order mark
	return l, nil
}

// peekc returns the next char, folding "\r\n" into '\n', and the bytes it
// takes; ok is false at the end of the source.
func (l *lexer) peekc() (ch rune, size int, ok bool) {
	if l.pos >= len(l.src) {
		return 0, 0, false
	}
	ch, size = utf8.DecodeRuneInString(l.src[l.pos:])
	if ch == '\r' && strings.HasPrefix(l.src[l.pos+1:], "\n") {
		return '\n', 2, true
	}
	return ch, size, true
}

func (l *lexer) nextc() (at int, ch rune, ok bool) {
	at = l.pos
	ch, size, ok := l.peekc()
	l.pos += size
	return at, ch, ok
}

func (l *lexer) eatc(want rune) bool {
	if ch, size, ok := l.peekc(); ok && ch == want {
		l.pos += size
		return true
	}
	return false
}

// nextRaw returns the next token, whitespace and comments included; ok is
// false at the end of the source.
func (l *lexer) nextRaw() (sp span, t token, ok bool, err error) {
	start, ch, ok := l.nextc()
	if !ok {
		return span{}, 0, false, nil
	}
	switch {
	case ch == '\n' || ch == '\t' || ch == ' ':
		for l.eatc(' ') || l.eatc('\t') || l.eatc('\n') {
		}
		t = tWhitespace
	case ch == '/':
		switch {
		case l.eatc('/'):
			for {
				_, c, ok := l.nextc()
				if !ok || c == '\n' {
					break
				}
			}
			t = tComment
		case l.eatc('*'):
			for depth := 1; depth > 0; {
				_, c, ok := l.nextc()
				switch {
				case !ok:
					return span{}, 0, false, errAt(start, "unterminated block comment")
				case c == '/' && l.eatc('*'):
					depth++
				case c == '*' && l.eatc('/'):
					depth--
				}
			}
			t = tComment
		default:
			t = tSlash
		}
	case ch == '-':
		t = tMinus
		if l.eatc('>') {
			t = tRArrow
		}
	case ch == '%':
		if c, size, ok := l.peekc(); ok && isKeylikeStart(c) {
			l.pos += size
			l.eatKeylike()
		}
		t = tExplicitID
	case ch == '"':
		if _, err := l.stringLiteral(start); err != nil {
			return span{}, 0, false, err
		}
		t = tStringLiteral
	case isKeylikeStart(ch):
		l.eatKeylike()
		t = tID
		if kw, ok := keywords[l.src[start:l.pos]]; ok {
			t = kw
		}
	case ch >= '0' && ch <= '9':
		for {
			c, size, ok := l.peekc()
			if !ok || c < '0' || c > '9' {
				break
			}
			l.pos += size
		}
		t = tInteger
	default:
		p, ok := punct[ch]
		if !ok {
			return span{}, 0, false, errAt(start, "unexpected character %s", rustChar(ch))
		}
		t = p
	}
	return span{start, l.pos}, t, true, nil
}

var punct = map[rune]token{
	'=': tEquals, ',': tComma, ':': tColon, '.': tPeriod, ';': tSemicolon,
	'(': tLeftParen, ')': tRightParen, '{': tLeftBrace, '}': tRightBrace,
	'<': tLessThan, '>': tGreaterThan, '*': tStar, '@': tAt, '+': tPlus,
}

func (l *lexer) eatKeylike() {
	for {
		c, size, ok := l.peekc()
		if !ok || !isKeylikeContinue(c) {
			return
		}
		l.pos += size
	}
}

// next returns the next token that is not whitespace or a comment.
func (l *lexer) next() (sp span, t token, ok bool, err error) {
	for {
		sp, t, ok, err = l.nextRaw()
		if err != nil || !ok || (t != tWhitespace && t != tComment) {
			return sp, t, ok, err
		}
	}
}

// peek is next without consuming anything.
func (l lexer) peek() (span, token, bool, error) { return l.next() }

func (l *lexer) eat(want token) (bool, error) {
	c := *l
	_, t, ok, err := c.next()
	if err != nil || !ok || t != want {
		return false, err
	}
	*l = c
	return true, nil
}

func (l *lexer) expect(want token) (span, error) {
	sp, t, ok, err := l.next()
	switch {
	case err != nil:
		return span{}, err
	case !ok:
		return span{}, errAt(len(l.src), "expected %s, found eof", want.describe())
	case t != want:
		return span{}, errAt(sp.start, "expected %s, found %s", want.describe(), t.describe())
	}
	return sp, nil
}

func (l *lexer) text(sp span) string { return l.src[sp.start:sp.end] }

// parseID is the identifier sp spans, validated.
func (l *lexer) parseID(sp span) (string, error) {
	s := l.text(sp)
	return s, validateID(sp.start, s)
}

// parseExplicitID is the identifier a %-prefixed token spans, without the %.
func (l *lexer) parseExplicitID(sp span) (string, error) {
	s := l.text(sp)[1:]
	return s, validateID(sp.start, s)
}

// stringLiteral reads the rest of a string literal whose opening quote is at
// start, and returns its value.
func (l *lexer) stringLiteral(start int) (string, error) {
	var buf []byte
	for {
		pos, c, ok := l.nextc()
		if !ok {
			return "", errAt(len(l.src), "unexpected end of file")
		}
		switch {
		case c == '"':
			if msg := utf8Error(buf); msg != "" {
				return "", errAt(start, "invalid UTF-8: %s", msg)
			}
			return string(buf), nil
		case c == '\\':
			pos, c, ok := l.nextc()
			if !ok {
				return "", errAt(len(l.src), "unexpected end of file")
			}
			switch c {
			case '"', '\'', '\\':
				buf = append(buf, byte(c))
			case 't':
				buf = append(buf, '\t')
			case 'n':
				buf = append(buf, '\n')
			case 'r':
				buf = append(buf, '\r')
			case 'u':
				if err := l.expectChar('{'); err != nil {
					return "", err
				}
				n, err := l.hexNum()
				if err != nil {
					return "", err
				}
				if n > unicode.MaxRune || n >= 0xd800 && n < 0xe000 {
					return "", errAt(pos, "invalid unicode value %#x", n)
				}
				buf = utf8.AppendRune(buf, rune(n))
				if err := l.expectChar('}'); err != nil {
					return "", err
				}
			default:
				if !isHexDigit(c) {
					return "", errAt(pos, "invalid string escape %s", rustChar(c))
				}
				lo, err := l.hexDigit()
				if err != nil {
					return "", err
				}
				buf = append(buf, hexVal(c)*16+lo)
			}
		case c < 0x20 || c == 0x7f:
			return "", errAt(pos, "invalid string character %s", rustChar(c))
		default:
			buf = utf8.AppendRune(buf, c)
		}
	}
}

func (l *lexer) expectChar(want rune) error {
	pos, c, ok := l.nextc()
	switch {
	case !ok:
		return errAt(len(l.src), "unexpected end of file")
	case c != want:
		return errAt(pos, "expected character %s", rustChar(want))
	}
	return nil
}

func (l *lexer) hexDigit() (byte, error) {
	pos, c, ok := l.nextc()
	switch {
	case !ok:
		return 0, errAt(len(l.src), "unexpected end of file")
	case !isHexDigit(c):
		return 0, errAt(pos, "invalid hex digit %s", rustChar(c))
	}
	return hexVal(c), nil
}

func (l *lexer) hexNum() (uint32, error) {
	start := l.pos
	d, err := l.hexDigit()
	if err != nil {
		return 0, err
	}
	n := uint32(d)
	lastUnderscore := false
	for {
		if l.eatc('_') {
			lastUnderscore = true
			continue
		}
		c, size, ok := l.peekc()
		if !ok {
			return 0, errAt(len(l.src), "unexpected end of file")
		}
		if !isHexDigit(c) {
			break
		}
		lastUnderscore = false
		if n > (1<<32-1-uint32(hexVal(c)))/16 {
			return 0, errAt(l.pos, "number is too big to fit in a u32")
		}
		l.pos += size
		n = n*16 + uint32(hexVal(c))
	}
	if lastUnderscore {
		return 0, errAt(start, "trailing underscore in number")
	}
	return n, nil
}

// utf8Error is how Rust's Utf8Error describes the first invalid sequence
// in b, or "" when b is valid UTF-8.
func utf8Error(b []byte) string {
	for i := 0; i < len(b); {
		// The ranges each byte after the first may take, by the first.
		var ranges [][2]byte
		switch first := b[i]; {
		case first < 0x80:
			i++
			continue
		case first >= 0xc2 && first <= 0xdf:
			ranges = [][2]byte{{0x80, 0xbf}}
		case first == 0xe0:
			ranges = [][2]byte{{0xa0, 0xbf}, {0x80, 0xbf}}
		case first == 0xed:
			ranges = [][2]byte{{0x80, 0x9f}, {0x80, 0xbf}}
		case first >= 0xe1 && first <= 0xef:
			ranges = [][2]byte{{0x80, 0xbf}, {0x80, 0xbf}}
		case first == 0xf0:
			ranges = [][2]byte{{0x90, 0xbf}, {0x80, 0xbf}, {0x80, 0xbf}}
		case first == 0xf4:
			ranges = [][2]byte{{0x80, 0x8f}, {0x80, 0xbf}, {0x80, 0xbf}}
		case first >= 0xf1 && first <= 0xf3:
			ranges = [][2]byte{{0x80, 0xbf}, {0x80, 0xbf}, {0x80, 0xbf}}
		default:
			return fmt.Sprintf("invalid utf-8 sequence of 1 bytes from index %d", i)
		}
		for k, r := range ranges {
			switch j := i + k + 1; {
			case j >= len(b):
				return fmt.Sprintf("incomplete utf-8 byte sequence from index %d", i)
			case b[j] < r[0] || b[j] > r[1]:
				return fmt.Sprintf("invalid utf-8 sequence of %d bytes from index %d", k+1, i)
			}
		}
		i += len(ranges) + 1
	}
	return ""
}

func isHexDigit(c rune) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c rune) byte {
	switch {
	case c >= 'a':
		return byte(c-'a') + 10
	case c >= 'A':
		return byte(c-'A') + 10
	}
	return byte(c - '0')
}

func detectInvalidInput(src string) error {
	for pos, ch := range src {
		switch {
		case ch == '\n' || ch == '\r' || ch == '\t':
		// wit-parser prints these two with {:?} of char::escape_unicode.
		case ch >= 0x202a && ch <= 0x202e || ch >= 0x2066 && ch <= 0x2069:
			return errAt(pos, "Input contains bidirectional override codepoint EscapeUnicode(EscapeIterInner('%s'))", escapeUnicode(ch))
		case ch == 0x149 || ch == 0x673 || ch == 0xf77 || ch == 0xf79 || ch == 0x17a3 || ch == 0x17a4 || ch == 0x17b4 || ch == 0x17b5:
			return errAt(pos, "Codepoint EscapeUnicode(EscapeIterInner('%s')) is discouraged by Unicode", escapeUnicode(ch))
		case unicode.IsControl(ch):
			return errAt(pos, "Control code '%s'", escapeUnicode(ch))
		}
	}
	return nil
}

// escapeUnicode is Rust's char::escape_unicode: \u{hex}.
func escapeUnicode(ch rune) string { return fmt.Sprintf(`\u{%x}`, ch) }

// rustChar is a char as Rust's {:?} prints it.
func rustChar(ch rune) string {
	switch ch {
	case '\'':
		return `'\''`
	case '\\':
		return `'\\'`
	case '\n':
		return `'\n'`
	case '\r':
		return `'\r'`
	case '\t':
		return `'\t'`
	}
	if unicode.IsPrint(ch) {
		return "'" + string(ch) + "'"
	}
	return "'" + escapeUnicode(ch) + "'"
}

// isKeylikeStart and isKeylikeContinue lex any XID start (or continue)
// char, '_' and '-': not all valid in an identifier, which validateID
// diagnoses once the whole word is read.
func isKeylikeStart(ch rune) bool {
	return ch == '_' || ch == '-' || unicode.IsLetter(ch) || unicode.Is(unicode.Nl, ch)
}

func isKeylikeContinue(ch rune) bool {
	return isKeylikeStart(ch) || unicode.In(ch, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc)
}

// validateID checks a WIT identifier: '-'-separated parts, each all
// lowercase or all uppercase ASCII letters and digits, the first starting
// with a letter.
func validateID(start int, id string) error {
	if id == "" {
		return errAt(start, "identifiers must have characters between '-'s")
	}
	for i, part := range strings.Split(id, "-") {
		if part == "" {
			return errAt(start, "identifiers must have characters between '-'s")
		}
		first, _ := utf8.DecodeRuneInString(part)
		if i == 0 && !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z') {
			return errAt(start, "invalid character in identifier %s", rustChar(first))
		}
		upper := 0 // 0 unknown, 1 upper, 2 lower
		for _, ch := range part {
			switch {
			case ch >= '0' && ch <= '9':
			case ch >= 'A' && ch <= 'Z' && upper != 2:
				upper = 1
			case ch >= 'a' && ch <= 'z' && upper != 1:
				upper = 2
			default:
				return errAt(start, "invalid character in identifier %s", rustChar(ch))
			}
		}
	}
	return nil
}

// parseU32 is Rust's u32::from_str on a run of digits, with its error.
func parseU32(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("number too large to fit in target type")
	}
	return uint32(n), nil
}
