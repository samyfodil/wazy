package wit

import (
	"strings"
	"testing"
)

// TestTypeRefString exercises every branch of TypeRef.String(), including the
// nil receiver, the Unsupported escape hatch, every Kind case with both the
// Inner-present and Inner-nil forms where applicable, and the default
// (named-type) case.
func TestTypeRefString(t *testing.T) {
	u32 := &TypeRef{Kind: "u32"}
	str := &TypeRef{Kind: "string"}

	tests := []struct {
		name string
		in   *TypeRef
		want string
	}{
		{"nil", nil, "nil"},
		{"unsupported", &TypeRef{Unsupported: "core-type"}, "UNSUPPORTED(core-type)"},
		{"u8", &TypeRef{Kind: "u8"}, "u8"},
		{"u16", &TypeRef{Kind: "u16"}, "u16"},
		{"u32", &TypeRef{Kind: "u32"}, "u32"},
		{"u64", &TypeRef{Kind: "u64"}, "u64"},
		{"s8", &TypeRef{Kind: "s8"}, "s8"},
		{"s16", &TypeRef{Kind: "s16"}, "s16"},
		{"s32", &TypeRef{Kind: "s32"}, "s32"},
		{"s64", &TypeRef{Kind: "s64"}, "s64"},
		{"f32", &TypeRef{Kind: "f32"}, "f32"},
		{"f64", &TypeRef{Kind: "f64"}, "f64"},
		{"bool", &TypeRef{Kind: "bool"}, "bool"},
		{"char", &TypeRef{Kind: "char"}, "char"},
		{"string", &TypeRef{Kind: "string"}, "string"},
		{"list with inner", &TypeRef{Kind: "list", Inner: u32}, "list<u32>"},
		{"list without inner", &TypeRef{Kind: "list"}, "list"},
		{"option with inner", &TypeRef{Kind: "option", Inner: str}, "option<string>"},
		{"option without inner", &TypeRef{Kind: "option"}, "option"},
		{"result both", &TypeRef{Kind: "result", Inner: u32, Inner2: str}, "result<u32,string>"},
		{"result ok only", &TypeRef{Kind: "result", Inner: u32}, "result<u32>"},
		{"result bare", &TypeRef{Kind: "result"}, "result"},
		{"tuple", &TypeRef{Kind: "tuple", Tuple: []*TypeRef{u32, str}}, "tuple<[u32 string]>"},
		{"tuple empty", &TypeRef{Kind: "tuple"}, "tuple<[]>"},
		{"own with inner", &TypeRef{Kind: "own", Inner: &TypeRef{Kind: "named", Name: "res"}}, "own<res>"},
		{"own without inner", &TypeRef{Kind: "own", Name: "res"}, "own<res>"},
		{"borrow with inner", &TypeRef{Kind: "borrow", Inner: &TypeRef{Kind: "named", Name: "res"}}, "borrow<res>"},
		{"borrow without inner", &TypeRef{Kind: "borrow", Name: "res"}, "borrow<res>"},
		{"map with both", &TypeRef{Kind: "map", Inner: str, Inner2: u32}, "map<string,u32>"},
		{"map without inner", &TypeRef{Kind: "map"}, "map"},
		{"future with inner", &TypeRef{Kind: "future", Inner: u32}, "future<u32>"},
		{"future without inner", &TypeRef{Kind: "future"}, "future"},
		{"stream with inner", &TypeRef{Kind: "stream", Inner: u32}, "stream<u32>"},
		{"stream without inner", &TypeRef{Kind: "stream"}, "stream"},
		{"named", &TypeRef{Kind: "named", Name: "my-resource"}, "my-resource"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestASTMarkerMethods directly invokes every zero-width marker method used
// to satisfy the PackageItem / InterfaceItem / WorldItem / TypeDefBody /
// ImportType / ExportType marker interfaces. These methods have empty bodies
// and are never invoked through normal interface satisfaction (only through
// static typing), so they are only exercised by calling them directly.
func TestASTMarkerMethods(t *testing.T) {
	(&Interface{}).packageItem()
	(&InterfaceFunc{}).interfaceItem()
	(&World{}).packageItem()
	(&TypeDef{}).packageItem()
	(&TypeDef{}).interfaceItem()
	(&TypeDef{}).worldItem()
	(&TypeAlias{}).typeDefBody()
	(&Record{}).typeDefBody()
	(&Variant{}).typeDefBody()
	(&Enum{}).typeDefBody()
	(&Flags{}).typeDefBody()
	(&Resource{}).typeDefBody()
	(&Use{}).packageItem()
	(&Use{}).interfaceItem()
	(&Use{}).worldItem()
	(&Include{}).worldItem()
	(&Import{}).worldItem()
	(&ImportFunc{}).importType()
	(&ImportInterface{}).importType()
	(&Export{}).worldItem()
	(&ExportFunc{}).exportType()
	(&ExportInterface{}).exportType()
}

// TestTokenTypeStringAll exercises every case of TokenType.String(), plus the
// default branch for an out-of-range token type.
func TestTokenTypeStringAll(t *testing.T) {
	all := []TokenType{
		TokenEOF, TokenError, TokenIdent, TokenString, TokenVersion,
		TokenPackage, TokenInterface, TokenWorld, TokenKeywordType, TokenRecord,
		TokenVariant, TokenEnum, TokenFlags, TokenResource, TokenFunc, TokenUse,
		TokenImport, TokenExport, TokenAs, TokenAsync, TokenConstructor, TokenStatic,
		TokenBool, TokenS8, TokenS16, TokenS32, TokenS64, TokenU8, TokenU16,
		TokenU32, TokenU64, TokenF32, TokenF64, TokenChar, TokenString_Keyword,
		TokenList, TokenOption, TokenResult, TokenTuple, TokenMap, TokenFuture,
		TokenStream, TokenOwn, TokenBorrow, TokenLParen, TokenRParen, TokenLBrace,
		TokenRBrace, TokenLAngle, TokenRAngle, TokenColon, TokenSemicolon,
		TokenComma, TokenDot, TokenEq, TokenArrow, TokenSlash, TokenAt, TokenStar,
		TokenUnstable, TokenSince, TokenDeprecated, TokenExternalID, TokenFeature,
		TokenVersion_Keyword, TokenInclude, TokenWith,
	}
	seen := make(map[string]bool)
	for _, tt := range all {
		s := tt.String()
		if s == "" {
			t.Errorf("TokenType(%d).String() is empty", tt)
		}
		seen[s] = true
	}

	// Out-of-range value hits the default branch.
	unknown := TokenType(9999)
	if got := unknown.String(); got != "TokenType(9999)" {
		t.Errorf("unknown.String() = %q, want %q", got, "TokenType(9999)")
	}
}

// TestLexerDirect exercises Lexer.NextToken branches not reachable through
// Parse() alone: the bare '*' token, string literals (plain, escaped,
// unterminated), the lone '-' error, unknown '@' attributes, and peek() at
// end-of-input.
func TestLexerDirect(t *testing.T) {
	t.Run("star", func(t *testing.T) {
		lex := NewLexer("*")
		tok := lex.NextToken()
		if tok.Type != TokenStar || tok.Text != "*" {
			t.Errorf("got %+v, want TokenStar", tok)
		}
	})

	t.Run("plain string", func(t *testing.T) {
		lex := NewLexer(`"hello"`)
		tok := lex.NextToken()
		if tok.Type != TokenString || tok.Text != "hello" {
			t.Errorf("got %+v, want TokenString hello", tok)
		}
	})

	t.Run("escaped string", func(t *testing.T) {
		lex := NewLexer(`"a\"b\\c"`)
		tok := lex.NextToken()
		if tok.Type != TokenString {
			t.Errorf("got %+v, want TokenString", tok)
		}
	})

	t.Run("unterminated string", func(t *testing.T) {
		lex := NewLexer(`"unterminated`)
		tok := lex.NextToken()
		if tok.Type != TokenString {
			t.Errorf("got %+v, want TokenString even when unterminated", tok)
		}
		// Lexer should now be at EOF.
		next := lex.NextToken()
		if next.Type != TokenEOF {
			t.Errorf("expected EOF after unterminated string, got %+v", next)
		}
	})

	t.Run("unterminated string with trailing backslash", func(t *testing.T) {
		lex := NewLexer(`"abc\`)
		tok := lex.NextToken()
		if tok.Type != TokenString {
			t.Errorf("got %+v, want TokenString", tok)
		}
	})

	t.Run("lone dash", func(t *testing.T) {
		lex := NewLexer("- x")
		tok := lex.NextToken()
		if tok.Type != TokenError {
			t.Errorf("got %+v, want TokenError for lone '-'", tok)
		}
	})

	t.Run("arrow", func(t *testing.T) {
		lex := NewLexer("->")
		tok := lex.NextToken()
		if tok.Type != TokenArrow {
			t.Errorf("got %+v, want TokenArrow", tok)
		}
	})

	t.Run("unknown attribute", func(t *testing.T) {
		lex := NewLexer("@unknown-thing")
		tok := lex.NextToken()
		if tok.Type != TokenError {
			t.Errorf("got %+v, want TokenError for unknown attribute", tok)
		}
	})

	t.Run("external-id attribute", func(t *testing.T) {
		lex := NewLexer("@external-id")
		tok := lex.NextToken()
		if tok.Type != TokenExternalID {
			t.Errorf("got %+v, want TokenExternalID", tok)
		}
	})

	t.Run("bare at", func(t *testing.T) {
		// '@' not followed by e/u/s/d falls through with just TokenAt.
		lex := NewLexer("@x")
		tok := lex.NextToken()
		if tok.Type != TokenAt {
			t.Errorf("got %+v, want TokenAt", tok)
		}
	})

	t.Run("unexpected character", func(t *testing.T) {
		lex := NewLexer("#")
		tok := lex.NextToken()
		if tok.Type != TokenError {
			t.Errorf("got %+v, want TokenError for unexpected char", tok)
		}
	})

	t.Run("peek at end of input after slash", func(t *testing.T) {
		// A lone trailing '/' exercises peek() returning 0 at end of input,
		// inside skipWhitespaceAndComments' comment-start check.
		lex := NewLexer("/")
		tok := lex.NextToken()
		if tok.Type != TokenSlash {
			t.Errorf("got %+v, want TokenSlash", tok)
		}
	})

	t.Run("version with sign and fraction", func(t *testing.T) {
		lex := NewLexer("1.2.3-alpha+build")
		tok := lex.NextToken()
		if tok.Type != TokenVersion {
			t.Errorf("got %+v, want TokenVersion", tok)
		}
	})
}

// TestTokenizeError checks that Tokenize propagates a lexer error with
// line:column information.
func TestTokenizeError(t *testing.T) {
	_, err := Tokenize("package foo:bar; #")
	if err == nil {
		t.Fatal("expected error from Tokenize on unexpected character")
	}
	if !strings.Contains(err.Error(), "line") || !strings.Contains(err.Error(), "column") {
		t.Errorf("error missing line/column info: %v", err)
	}
}

// TestParseErrors is a table of malformed WIT snippets, each crafted to
// trigger exactly one reachable fail-loud error branch in the parser. Every
// entry must produce a non-nil error whose message contains filename and
// line:column info.
func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"package version missing after at", "package foo:bar@;"},
		{"package unterminated (no semicolon)", "package foo:bar"},

		{"since missing open paren", "package t:t; interface i { @since version = 0.1.0) f: func(); }"},
		{"since wrong keyword", "package t:t; interface i { @since(ver = 0.1.0) f: func(); }"},
		{"since missing eq", "package t:t; interface i { @since(version 0.1.0) f: func(); }"},
		{"since missing value", "package t:t; interface i { @since(version = ,) f: func(); }"},
		{"since missing close paren", "package t:t; interface i { @since(version = 0.1.0 f: func(); }"},

		{"unstable missing open paren", "package t:t; interface i { @unstable feature = x) f: func(); }"},
		{"unstable wrong keyword", "package t:t; interface i { @unstable(feat = x) f: func(); }"},
		{"unstable missing eq", "package t:t; interface i { @unstable(feature x) f: func(); }"},
		{"unstable missing feature name", "package t:t; interface i { @unstable(feature = ) f: func(); }"},
		{"unstable missing close paren", "package t:t; interface i { @unstable(feature = x f: func(); }"},

		{"external-id missing open paren", `package t:t; interface i { @external-id "x") f: func(); }`},
		{"external-id non-string", "package t:t; interface i { @external-id(123) f: func(); }"},
		{"external-id missing close paren", `package t:t; interface i { @external-id("x" f: func(); }`},

		{"unexpected token at package level", "package t:t; )"},

		{"interface missing name", "package t:t; interface { }"},
		{"interface missing lbrace", "package t:t; interface i }"},
		{"interface missing rbrace", "package t:t; interface i { "},
		{"interface unexpected item", "package t:t; interface i { 123 }"},

		{"inline interface missing lbrace in export", "package t:t; world w { export h: interface }"},
		{"inline interface missing rbrace in import", "package t:t; world w { import h: interface { } "},

		{"interface func missing name", "package t:t; interface i { : func(); }"},
		{"interface func missing colon", "package t:t; interface i { f func(); }"},
		{"interface func missing semicolon", "package t:t; interface i { f: func() }"},
		{"interface func bogus body", "package t:t; interface i { f: bogus; }"},

		{"world missing name", "package t:t; world { }"},
		{"world missing lbrace", "package t:t; world w }"},
		{"world missing rbrace", "package t:t; world w { "},
		{"world unexpected item", "package t:t; world w { 123 }"},

		{"typedef missing name", "package t:t; interface i { type = u32; }"},
		{"typedef unexpected after name", "package t:t; interface i { type foo bar; }"},

		{"type alias missing eq", "package t:t; interface i { type foo bar; }"},

		{"record missing lbrace", "package t:t; interface i { type r = record x: u32 }; }"},
		{"record missing rbrace", "package t:t; interface i { type r = record { x: u32 ; }"},
		{"record missing semicolon", "package t:t; interface i { type r = record { x: u32 } }"},

		{"variant missing lbrace", "package t:t; interface i { type v = variant x(u32) }; }"},
		{"variant missing rbrace", "package t:t; interface i { type v = variant { x(u32) ; }"},
		{"variant missing semicolon", "package t:t; interface i { type v = variant { x(u32) } }"},
		{"variant case missing rparen", "package t:t; interface i { type v = variant { x(u32 }; }"},
		{"variant case missing comma", "package t:t; interface i { type v = variant { x(u32) y }; }"},
		{"variant case bad name", "package t:t; interface i { type v = variant { 123 }; }"},

		{"enum missing lbrace", "package t:t; interface i { type e = enum x }; }"},
		{"enum missing rbrace", "package t:t; interface i { type e = enum { x ; }"},
		{"enum missing semicolon", "package t:t; interface i { type e = enum { x } }"},
		{"enum bad case name", "package t:t; interface i { type e = enum { 123 }; }"},
		{"enum missing comma", "package t:t; interface i { type e = enum { x y }; }"},

		{"flags missing lbrace", "package t:t; interface i { type f = flags x }; }"},
		{"flags missing rbrace", "package t:t; interface i { type f = flags { x ; }"},
		{"flags missing semicolon", "package t:t; interface i { type f = flags { x } }"},
		{"flags bad name", "package t:t; interface i { type f = flags { 123 }; }"},
		{"flags missing comma", "package t:t; interface i { type f = flags { x y }; }"},

		{"type alias missing semicolon", "package t:t; interface i { type a = u32 }"},

		{"shorthand missing name", "package t:t; interface i { record { x: u32 } }"},
		{"shorthand missing lbrace", "package t:t; interface i { record r }"},
		{"shorthand missing rbrace", "package t:t; interface i { record r { x: u32 "},

		{"record field bad name", "package t:t; interface i { record r { 123: u32 } }"},
		{"record field missing colon", "package t:t; interface i { record r { x u32 } }"},
		{"record field missing comma", "package t:t; interface i { record r { x: u32 y: u32 } }"},

		{"resource constructor missing semicolon", "package t:t; interface i { resource r { constructor(x: u32) } }"},
		{"resource bad method name", "package t:t; interface i { resource r { 123: func(); } }"},
		{"resource method missing colon", "package t:t; interface i { resource r { m func(); } }"},
		{"resource method missing semicolon", "package t:t; interface i { resource r { m: func() } }"},

		{"param list missing lparen", "package t:t; interface i { f: func x: u32); }"},
		{"param bad name", "package t:t; interface i { f: func(123: u32); }"},
		{"param missing colon", "package t:t; interface i { f: func(x u32); }"},
		{"param missing comma", "package t:t; interface i { f: func(x: u32 y: u32); }"},
		{"param list missing rparen", "package t:t; interface i { f: func(x: u32 ; }"},

		{"bad type token", "package t:t; interface i { type t = @; }"},

		{"list missing langle", "package t:t; interface i { type t = list u32>; }"},
		{"list missing type", "package t:t; interface i { type t = list<>; }"},
		{"list bad length specifier", "package t:t; interface i { type t = list<u32, >; }"},
		{"list missing rangle", "package t:t; interface i { type t = list<u32; }"},

		{"option missing langle", "package t:t; interface i { type t = option u32>; }"},
		{"option missing type", "package t:t; interface i { type t = option<>; }"},
		{"option missing rangle", "package t:t; interface i { type t = option<u32; }"},

		{"result missing rangle", "package t:t; interface i { type t = result<u32; }"},
		{"result bad first type", "package t:t; interface i { type t = result<@>; }"},
		{"result bad second type", "package t:t; interface i { type t = result<u32, @>; }"},

		{"tuple missing langle", "package t:t; interface i { type t = tuple u32>; }"},
		{"tuple missing comma", "package t:t; interface i { type t = tuple<u32 u32>; }"},
		{"tuple missing rangle", "package t:t; interface i { type t = tuple<u32; }"},
		{"tuple bad elem", "package t:t; interface i { type t = tuple<@>; }"},

		{"map missing langle", "package t:t; interface i { type t = map string, u32>; }"},
		{"map missing comma", "package t:t; interface i { type t = map<string u32>; }"},
		{"map missing rangle", "package t:t; interface i { type t = map<string, u32; }"},
		{"map bad key", "package t:t; interface i { type t = map<@, u32>; }"},
		{"map bad value", "package t:t; interface i { type t = map<string, @>; }"},

		{"future missing rangle", "package t:t; interface i { type t = future<u32; }"},
		{"future bad inner", "package t:t; interface i { type t = future<@>; }"},

		{"stream missing rangle", "package t:t; interface i { type t = stream<u32; }"},
		{"stream bad inner", "package t:t; interface i { type t = stream<@>; }"},

		{"own missing langle", "package t:t; interface i { type t = own r>; }"},
		{"own bad name", "package t:t; interface i { type t = own<123>; }"},
		{"own missing rangle", "package t:t; interface i { type t = own<r; }"},

		{"borrow missing langle", "package t:t; interface i { type t = borrow r>; }"},
		{"borrow bad name", "package t:t; interface i { type t = borrow<123>; }"},
		{"borrow missing rangle", "package t:t; interface i { type t = borrow<r; }"},

		{"use path bad start", "package t:t; use 123.{x};"},
		{"use path bad segment", "package t:t; use a:.{x};"},
		{"use path bad version", "package t:t; use a:b/c@;"},
		{"use missing lbrace after dot", "package t:t; use a.x};"},
		{"use bad ident in names", "package t:t; use a.{123};"},
		{"use missing alias after as", "package t:t; use a.{x as };"},
		{"use missing comma in names", "package t:t; use a.{x y};"},
		{"use missing rbrace in names", "package t:t; use a.{x ;"},
		{"use missing alias after as (bare)", "package t:t; use a as ;"},
		{"use missing semicolon", "package t:t; use a"},

		{"import bare path", "package t:t; world w { import wasi:io/streams; }"},
		{"import missing colon", "package t:t; world w { import foo func(); }"},
		{"import missing semicolon", "package t:t; world w { import foo: func() }"},
		{"import bad body", "package t:t; world w { import foo: bogus; }"},

		{"export bare path", "package t:t; world w { export wasi:io/streams; }"},
		{"export missing colon", "package t:t; world w { export foo func(); }"},
		{"export missing semicolon", "package t:t; world w { export foo: func() }"},
		{"export bad body", "package t:t; world w { export foo: bogus; }"},

		{"include missing with lbrace", "package t:t; world w { include a with x as y }; }"},
		{"include rename bad ident", "package t:t; world w { include a with { 123 as y }; }"},
		{"include rename missing as", "package t:t; world w { include a with { x y }; }"},
		{"include rename missing alias", "package t:t; world w { include a with { x as }; }"},
		{"include rename missing comma", "package t:t; world w { include a with { x as y z as w }; }"},
		{"include rename missing rbrace", "package t:t; world w { include a with { x as y ; }"},
		{"include missing semicolon", "package t:t; world w { include a with { x as y } }"},

		// Invalid characters / unterminated braces / unterminated parens at
		// various nesting depths.
		{"unterminated interface brace", "package t:t; interface i {"},
		{"unterminated world brace", "package t:t; world w {"},
		{"unterminated paren in func", "package t:t; interface i { f: func("},
		{"invalid token stream", "package t:t; interface $$$ { }"},
		{"invalid token in type position", "package t:t; interface i { type t = ###; }"},

		// Top-level (package-scope) attribute-error propagation.
		{"top level gate error", "package t:t; @since(version"},

		// Inline interface body item-error propagation (import/export).
		{"import inline interface bad item", "package t:t; world w { import foo: interface { 123 } }"},
		{"export inline interface bad item", "package t:t; world w { export foo: interface { 123 } }"},

		// World-body gate-error propagation.
		{"world gate error", "package t:t; world w { @since(version }"},

		// Shorthand variant/enum/flags case-error propagation.
		{"shorthand variant bad case", "package t:t; interface i { variant v { 123 } }"},
		{"shorthand enum bad case", "package t:t; interface i { enum e { 123 } }"},
		{"shorthand flags bad name", "package t:t; interface i { flags f { 123 } }"},

		// Shorthand/alias type defs missing closing brace, reached via a
		// trailing comma that lets the fields/cases loop exit cleanly at
		// EOF (as opposed to erroring inside the loop on a missing
		// separator).
		{"shorthand record missing rbrace at eof", "package t:t; interface i { record r { x: u32,"},
		{"alias record missing rbrace at eof", "package t:t; interface i { type r = record { x: u32,"},
		{"alias variant missing rbrace at eof", "package t:t; interface i { type v = variant { x,"},
		{"alias enum missing rbrace at eof", "package t:t; interface i { type e = enum { x,"},
		{"alias flags missing rbrace at eof", "package t:t; interface i { type f = flags { x,"},
		{"tuple missing rangle at eof", "package t:t; interface i { type t = tuple<u32,"},

		// Inline interface body reaching EOF without a closing '}' (as
		// opposed to erroring on a malformed item inside the body).
		{"import inline interface missing rbrace at eof", "package t:t; world w { import foo: interface { "},

		// Field/case type-error propagation.
		{"record field bad type", "package t:t; interface i { record r { x: @ } }"},
		{"variant case bad type", "package t:t; interface i { variant v { x(@) } }"},

		// Resource body: gate error, constructor param error, constructor
		// result-type error, non-constructor method func-error.
		{"resource gate error", "package t:t; interface i { resource r { @since(version = } }"},
		{"resource constructor bad param", "package t:t; interface i { resource r { constructor(123: u32); } }"},
		{"resource constructor bad result", "package t:t; interface i { resource r { constructor() -> @; } }"},
		{"resource method bad func body", "package t:t; interface i { resource r { m: bogus; } }"},

		// Param list / return type error propagation.
		{"param bad type", "package t:t; interface i { f: func(x: @); }"},
		{"func return type bad", "package t:t; interface i { f: func() -> @; }"},

		// Use names list: missing '}' reached via EOF after a trailing
		// comma lets the names loop exit cleanly (not via a stray token).
		{"use names missing rbrace at eof", "package t:t; use a.{x,"},

		// Import/export local-name check hit via a reserved-word collision
		// (the only way this parser can produce a non-TokenIdent token
		// immediately after 'import'/'export').
		{"import name is keyword", "package t:t; world w { import interface: func(); }"},
		{"export name is keyword", "package t:t; world w { export interface: func(); }"},

		// Import/export func-signature and inline-interface-body error
		// propagation.
		{"import func bad param", "package t:t; world w { import foo: func(123: u32); }"},
		{"export func bad param", "package t:t; world w { export foo: func(123: u32); }"},

		// Include: use-path error propagation, and rename-list missing '}'
		// reached via EOF after a trailing comma.
		{"include bad path", "package t:t; world w { include 123; }"},
		{"include rename missing rbrace at eof", "package t:t; world w { include a with { x as y,"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("errtest.wit", tt.src)
			if err == nil {
				t.Fatalf("expected parse error for src: %s", tt.src)
			}
		})
	}
}

// TestTopLevelTypeDef tests a "type" definition directly at package scope
// (not nested in an interface or world), exercising parsePackageItem's
// TokenKeywordType dispatch case.
func TestTopLevelTypeDef(t *testing.T) {
	src := "package t:t; type foo = u32;"
	pkg, err := Parse("toplevel.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(pkg.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(pkg.Items))
	}
	td, ok := pkg.Items[0].(*TypeDef)
	if !ok || td.Name != "foo" {
		t.Errorf("expected TypeDef 'foo', got %+v", pkg.Items[0])
	}
}

// TestDanglingGateAtEOF tests a gate/attribute with nothing following it at
// the very end of the file, exercising parsePackageItem's TokenEOF dispatch
// case (reached after parseAttributes consumes the gate and leaves the
// parser positioned at EOF).
func TestDanglingGateAtEOF(t *testing.T) {
	src := "package t:t; @since(version = 0.1.0)"
	pkg, err := Parse("dangling.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(pkg.Items) != 0 {
		t.Errorf("expected 0 items (dangling gate discarded), got %d: %+v", len(pkg.Items), pkg.Items)
	}
}

// TestListWithLengthSpecifier tests "list<T, N>" (fixed-length list), whose
// length specifier is parsed and discarded.
func TestListWithLengthSpecifier(t *testing.T) {
	src := "package t:t; interface i { type t = list<u32, 4>; }"
	pkg, err := Parse("listlen.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	iface := pkg.Items[0].(*Interface)
	td := iface.Items[0].(*TypeDef)
	alias := td.Type.(*TypeAlias)
	if alias.Target.Kind != "list" || alias.Target.Inner.Kind != "u32" {
		t.Errorf("unexpected list type: %+v", alias.Target)
	}
}

// TestAdditionalSuccessPaths exercises success-path branches that the error
// table can't reach: a fully-consumed @external-id gate, a bodyless resource
// declaration, a top-level "type" and shorthand item inside a world body, a
// shorthand type def followed by an optional semicolon, a bare (unparameterized)
// "result" type, and a "use path.{x as y}" renamed name within a names list.
func TestAdditionalSuccessPaths(t *testing.T) {
	t.Run("external-id gate", func(t *testing.T) {
		src := `package t:t;
interface i {
  @external-id("com.example.foo")
  f: func();
}`
		pkg, err := Parse("extid.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		fn := iface.Items[0].(*InterfaceFunc)
		if fn.ExternalID != "com.example.foo" {
			t.Errorf("ExternalID = %q, want %q", fn.ExternalID, "com.example.foo")
		}
	})

	t.Run("bodyless resource", func(t *testing.T) {
		src := "package t:t; interface i { resource empty; }"
		pkg, err := Parse("bodyless.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		td := iface.Items[0].(*TypeDef)
		res, ok := td.Type.(*Resource)
		if !ok || len(res.Methods) != 0 {
			t.Errorf("unexpected resource: %+v", td.Type)
		}
	})

	t.Run("world level type def", func(t *testing.T) {
		src := "package t:t; world w { type foo = u32; }"
		pkg, err := Parse("worldtype.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		world := pkg.Items[0].(*World)
		if _, ok := world.Items[0].(*TypeDef); !ok {
			t.Errorf("expected TypeDef, got %T", world.Items[0])
		}
	})

	t.Run("world level shorthand record", func(t *testing.T) {
		src := "package t:t; world w { record r { x: u32 } }"
		pkg, err := Parse("worldshort.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		world := pkg.Items[0].(*World)
		td, ok := world.Items[0].(*TypeDef)
		if !ok || td.Name != "r" {
			t.Errorf("expected TypeDef 'r', got %+v", world.Items[0])
		}
	})

	t.Run("shorthand with trailing semicolon", func(t *testing.T) {
		src := "package t:t; interface i { record r { x: u32 }; }"
		pkg, err := Parse("shortsemi.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		if len(iface.Items) != 1 {
			t.Errorf("expected exactly 1 item (trailing ';' consumed, not a second item), got %d", len(iface.Items))
		}
	})

	t.Run("bare result type", func(t *testing.T) {
		src := "package t:t; interface i { type t = result; }"
		pkg, err := Parse("bareresult.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		alias := iface.Items[0].(*TypeDef).Type.(*TypeAlias)
		if alias.Target.Kind != "result" || alias.Target.Inner != nil {
			t.Errorf("unexpected result type: %+v", alias.Target)
		}
	})

	t.Run("dangling gate in interface", func(t *testing.T) {
		src := "package t:t; interface i { @since(version = 0.1.0) }"
		pkg, err := Parse("danglingiface.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		if len(iface.Items) != 0 {
			t.Errorf("expected 0 items (dangling gate discarded), got %d: %+v", len(iface.Items), iface.Items)
		}
	})

	t.Run("dangling gate in world", func(t *testing.T) {
		src := "package t:t; world w { @since(version = 0.1.0) }"
		pkg, err := Parse("danglingworld.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		world := pkg.Items[0].(*World)
		if len(world.Items) != 0 {
			t.Errorf("expected 0 items (dangling gate discarded), got %d: %+v", len(world.Items), world.Items)
		}
	})

	t.Run("use names with as rename", func(t *testing.T) {
		src := "package t:t; interface i { use other.{orig-name as local-name}; }"
		pkg, err := Parse("userename.wit", src)
		if err != nil {
			t.Fatalf("Parse failed: %v", err)
		}
		iface := pkg.Items[0].(*Interface)
		use := iface.Items[0].(*Use)
		if use.Names["local-name"] != "orig-name" {
			t.Errorf("unexpected use names: %+v", use.Names)
		}
	})
}

// TestNestedPackageDeclUnsupported verifies that a brace-bodied package
// declaration (not supported by this parser, which only accepts the
// semicolon-terminated form) fails loudly rather than silently mis-parsing.
func TestNestedPackageDeclUnsupported(t *testing.T) {
	src := `package foo:bar {
  interface i {
    f: func();
  }
}`
	_, err := Parse("nested.wit", src)
	if err == nil {
		t.Fatal("expected error for brace-bodied package declaration")
	}
}

// TestBarePackagePathImportExport verifies that importing/exporting a bare
// package path (without "name:") is rejected with a clear, line-numbered
// error, since this construct isn't yet supported.
func TestBarePackagePathImportExport(t *testing.T) {
	t.Run("import", func(t *testing.T) {
		src := "package t:t; world w { import wasi:io/streams@0.2.0; }"
		_, err := Parse("bare.wit", src)
		if err == nil {
			t.Fatal("expected error for bare package-path import")
		}
		if !strings.Contains(err.Error(), "not yet supported") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("export", func(t *testing.T) {
		src := "package t:t; world w { export wasi:io/streams@0.2.0; }"
		_, err := Parse("bare.wit", src)
		if err == nil {
			t.Fatal("expected error for bare package-path export")
		}
		if !strings.Contains(err.Error(), "not yet supported") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

// TestUseWithAsAlias tests "use path as name;" (single-name alias form),
// distinct from the "use path.{names};" form already covered elsewhere.
func TestUseWithAsAlias(t *testing.T) {
	src := `package t:t;
interface i {
  use other-iface as local-name;
}`
	pkg, err := Parse("usewith.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	iface := pkg.Items[0].(*Interface)
	use, ok := iface.Items[0].(*Use)
	if !ok {
		t.Fatalf("expected *Use, got %T", iface.Items[0])
	}
	if use.Names["local-name"] != "other-iface" {
		t.Errorf("unexpected use names: %+v", use.Names)
	}
}

// TestInlineWorldImportInterface tests an inline "import name: interface {...}"
// world item (as opposed to the export form already covered in
// TestWorldItems).
func TestInlineWorldImportInterface(t *testing.T) {
	src := `package t:t;
world w {
  import handler: interface {
    handle: func(req: string) -> string;
  }
}`
	pkg, err := Parse("inlineimport.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	world := pkg.Items[0].(*World)
	imp, ok := world.Items[0].(*Import)
	if !ok {
		t.Fatalf("expected *Import, got %T", world.Items[0])
	}
	ifaceImp, ok := imp.Type.(*ImportInterface)
	if !ok {
		t.Fatalf("expected *ImportInterface, got %T", imp.Type)
	}
	if len(ifaceImp.Items) != 1 {
		t.Errorf("expected 1 item in inline import interface, got %d", len(ifaceImp.Items))
	}
}

// TestMapTypeParse tests successful parsing of map<K,V>, which appears in
// the Compound Types table but deserves an explicit standalone assertion of
// its Inner/Inner2 shape.
func TestMapTypeParse(t *testing.T) {
	src := `package t:t;
interface i {
  type t = map<string, u32>;
}`
	pkg, err := Parse("map.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	iface := pkg.Items[0].(*Interface)
	td := iface.Items[0].(*TypeDef)
	alias := td.Type.(*TypeAlias)
	if alias.Target.Kind != "map" || alias.Target.Inner.Kind != "string" || alias.Target.Inner2.Kind != "u32" {
		t.Errorf("unexpected map type: %+v", alias.Target)
	}
}

// TestFutureAndStreamBare tests "future" and "stream" without a type
// parameter, distinct from the parameterized forms in TestCompoundTypes.
func TestFutureAndStreamBare(t *testing.T) {
	src := `package t:t;
interface i {
  type f = future;
  type s = stream;
}`
	pkg, err := Parse("bare.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	iface := pkg.Items[0].(*Interface)
	f := iface.Items[0].(*TypeDef).Type.(*TypeAlias)
	if f.Target.Kind != "future" || f.Target.Inner != nil {
		t.Errorf("unexpected future type: %+v", f.Target)
	}
	s := iface.Items[1].(*TypeDef).Type.(*TypeAlias)
	if s.Target.Kind != "stream" || s.Target.Inner != nil {
		t.Errorf("unexpected stream type: %+v", s.Target)
	}
}

// TestCommentPositions tests line and block comments in various positions:
// leading, trailing/inline, between record fields, and immediately before
// EOF with nothing following.
func TestCommentPositions(t *testing.T) {
	src := `// leading line comment
package t:t; // trailing inline comment
/* block before interface */
interface i {
  record r {
    x: u32, // between fields
    /* block between fields */
    y: u32,
  }
}
// trailing comment at EOF`

	pkg, err := Parse("comments.wit", src)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if pkg.Name != "t:t" {
		t.Errorf("package name = %q", pkg.Name)
	}
}

// TestUnterminatedBlockComment ensures a block comment that never closes
// doesn't crash: skipWhitespaceAndComments runs its loop until EOF (there's
// no closing "*/"), leaving the lexer at EOF. Since nothing followed the
// comment, this is a package with no items, not a parse error.
func TestUnterminatedBlockComment(t *testing.T) {
	src := "package t:t; /* never closes"
	pkg, err := Parse("unterminated.wit", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pkg.Name != "t:t" || len(pkg.Items) != 0 {
		t.Errorf("unexpected package: %+v", pkg)
	}
}
