package wit

import (
	"fmt"
	"unicode"
)

// TokenType represents the kind of token.
type TokenType int

const (
	TokenEOF TokenType = iota
	TokenError

	// Literals
	TokenIdent
	TokenString
	TokenVersion // semver

	// Keywords
	TokenPackage
	TokenInterface
	TokenWorld
	TokenKeywordType // "type" keyword
	TokenRecord
	TokenVariant
	TokenEnum
	TokenFlags
	TokenResource
	TokenFunc
	TokenUse
	TokenImport
	TokenExport
	TokenAs
	TokenAsync
	TokenConstructor
	TokenStatic

	// Primitive types
	TokenBool
	TokenS8
	TokenS16
	TokenS32
	TokenS64
	TokenU8
	TokenU16
	TokenU32
	TokenU64
	TokenF32
	TokenF64
	TokenChar
	TokenString_Keyword // "string" as a type keyword
	TokenList
	TokenOption
	TokenResult
	TokenTuple
	TokenMap
	TokenFuture
	TokenStream
	TokenOwn
	TokenBorrow
	TokenErrorContext // "error-context": the async ABI's terminal error-context type

	// Punctuation
	TokenLParen    // (
	TokenRParen    // )
	TokenLBrace    // {
	TokenRBrace    // }
	TokenLAngle    // <
	TokenRAngle    // >
	TokenColon     // :
	TokenSemicolon // ;
	TokenComma     // ,
	TokenDot       // .
	TokenEq        // =
	TokenArrow     // ->
	TokenSlash     // /
	TokenAt        // @
	TokenStar      // *

	// Feature gates
	TokenUnstable
	TokenSince
	TokenDeprecated
	TokenExternalID
	TokenFeature
	TokenVersion_Keyword
	TokenInclude
	TokenWith
)

// Token represents a lexical token.
type Token struct {
	Type   TokenType
	Text   string
	Line   int
	Column int
}

// Lexer tokenizes WIT source code.
type Lexer struct {
	input  string
	pos    int
	line   int
	column int
	ch     rune
}

// NewLexer creates a new lexer for the given source.
func NewLexer(input string) *Lexer {
	lex := &Lexer{
		input:  input,
		pos:    0,
		line:   1,
		column: 0,
	}
	if len(input) > 0 {
		lex.ch = rune(input[0])
	}
	return lex
}

// NextToken returns the next token from the input.
func (lex *Lexer) NextToken() Token {
	lex.skipWhitespaceAndComments()

	if lex.pos >= len(lex.input) {
		return Token{Type: TokenEOF, Line: lex.line, Column: lex.column}
	}

	tok := Token{Line: lex.line, Column: lex.column}

	switch lex.ch {
	case '(':
		tok.Type = TokenLParen
		tok.Text = "("
		lex.advance()
	case ')':
		tok.Type = TokenRParen
		tok.Text = ")"
		lex.advance()
	case '{':
		tok.Type = TokenLBrace
		tok.Text = "{"
		lex.advance()
	case '}':
		tok.Type = TokenRBrace
		tok.Text = "}"
		lex.advance()
	case '<':
		tok.Type = TokenLAngle
		tok.Text = "<"
		lex.advance()
	case '>':
		tok.Type = TokenRAngle
		tok.Text = ">"
		lex.advance()
	case ':':
		tok.Type = TokenColon
		tok.Text = ":"
		lex.advance()
	case ';':
		tok.Type = TokenSemicolon
		tok.Text = ";"
		lex.advance()
	case ',':
		tok.Type = TokenComma
		tok.Text = ","
		lex.advance()
	case '.':
		tok.Type = TokenDot
		tok.Text = "."
		lex.advance()
	case '=':
		tok.Type = TokenEq
		tok.Text = "="
		lex.advance()
	case '/':
		tok.Type = TokenSlash
		tok.Text = "/"
		lex.advance()
	case '*':
		tok.Type = TokenStar
		tok.Text = "*"
		lex.advance()
	case '@':
		tok.Type = TokenAt
		tok.Text = "@"
		lex.advance()
		// Check for @external-id, @unstable, @since, @deprecated
		if lex.ch == 'e' || lex.ch == 'u' || lex.ch == 's' || lex.ch == 'd' {
			text := lex.readIdent()
			switch text {
			case "external-id":
				tok.Type = TokenExternalID
				tok.Text = "@" + text
			case "unstable":
				tok.Type = TokenUnstable
				tok.Text = "@" + text
			case "since":
				tok.Type = TokenSince
				tok.Text = "@" + text
			case "deprecated":
				tok.Type = TokenDeprecated
				tok.Text = "@" + text
			default:
				tok.Type = TokenError
				tok.Text = fmt.Sprintf("unknown attribute @%s", text)
			}
		}
	case '-':
		lex.advance()
		if lex.ch == '>' {
			tok.Type = TokenArrow
			tok.Text = "->"
			lex.advance()
		} else {
			tok.Type = TokenError
			tok.Text = "unexpected '-', did you mean '->'?"
		}
	case '"':
		tok.Type = TokenString
		tok.Text = lex.readString()
	default:
		if isIdentStart(lex.ch) {
			text := lex.readIdent()
			tok.Text = text
			tok.Type = keywordType(text)
		} else if unicode.IsDigit(lex.ch) {
			tok.Type = TokenVersion
			tok.Text = lex.readVersion()
		} else {
			tok.Type = TokenError
			tok.Text = fmt.Sprintf("unexpected character: %c", lex.ch)
			lex.advance()
		}
	}

	return tok
}

// skipWhitespaceAndComments skips over whitespace and comments.
func (lex *Lexer) skipWhitespaceAndComments() {
	for lex.pos < len(lex.input) {
		if lex.ch == ' ' || lex.ch == '\t' || lex.ch == '\r' || lex.ch == '\n' {
			lex.advance()
		} else if lex.ch == '/' {
			if lex.peek() == '/' {
				// Line comment
				lex.advance()
				lex.advance()
				for lex.pos < len(lex.input) && lex.ch != '\n' {
					lex.advance()
				}
			} else if lex.peek() == '*' {
				// Block comment
				lex.advance()
				lex.advance()
				for lex.pos < len(lex.input) {
					if lex.ch == '*' && lex.peek() == '/' {
						lex.advance()
						lex.advance()
						break
					}
					lex.advance()
				}
			} else {
				break
			}
		} else {
			break
		}
	}
}

// readIdent reads an identifier or keyword.
func (lex *Lexer) readIdent() string {
	start := lex.pos
	for lex.pos < len(lex.input) && isIdentMiddle(lex.ch) {
		lex.advance()
	}
	return lex.input[start:lex.pos]
}

// readString reads a string literal (double-quoted).
func (lex *Lexer) readString() string {
	lex.advance() // skip opening "
	start := lex.pos
	for lex.pos < len(lex.input) && lex.ch != '"' {
		if lex.ch == '\\' {
			lex.advance()
			if lex.pos < len(lex.input) {
				lex.advance()
			}
		} else {
			lex.advance()
		}
	}
	result := lex.input[start:lex.pos]
	if lex.pos < len(lex.input) {
		lex.advance() // skip closing "
	}
	return result
}

// readVersion reads a semantic version or integer. A '.' is only consumed as
// part of the version when followed by a digit, so that constructs like
// "pkg@0.2.8.{name}" (a version immediately followed by a use-names list)
// don't have their trailing '.' swallowed into the version text.
func (lex *Lexer) readVersion() string {
	start := lex.pos
	for lex.pos < len(lex.input) {
		if unicode.IsDigit(lex.ch) || lex.ch == '-' || lex.ch == '+' {
			lex.advance()
		} else if lex.ch == '.' && unicode.IsDigit(lex.peek()) {
			lex.advance()
		} else {
			break
		}
	}
	return lex.input[start:lex.pos]
}

// advance moves to the next character.
func (lex *Lexer) advance() {
	if lex.ch == '\n' {
		lex.line++
		lex.column = 0
	} else if lex.pos >= 0 && lex.pos < len(lex.input) {
		lex.column++
	}

	lex.pos++
	if lex.pos < len(lex.input) {
		lex.ch = rune(lex.input[lex.pos])
	} else {
		lex.ch = 0
	}
}

// peek returns the next character without advancing.
func (lex *Lexer) peek() rune {
	if lex.pos+1 < len(lex.input) {
		return rune(lex.input[lex.pos+1])
	}
	return 0
}

// isIdentStart returns true if r can start an identifier.
func isIdentStart(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
}

// isIdentMiddle returns true if r can be part of an identifier.
func isIdentMiddle(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9') || r == '-'
}

// keywordType returns the token type for a keyword or ident.
func keywordType(text string) TokenType {
	switch text {
	case "package":
		return TokenPackage
	case "interface":
		return TokenInterface
	case "world":
		return TokenWorld
	case "type":
		return TokenKeywordType
	case "record":
		return TokenRecord
	case "variant":
		return TokenVariant
	case "enum":
		return TokenEnum
	case "flags":
		return TokenFlags
	case "resource":
		return TokenResource
	case "func":
		return TokenFunc
	case "use":
		return TokenUse
	case "import":
		return TokenImport
	case "export":
		return TokenExport
	case "as":
		return TokenAs
	case "async":
		return TokenAsync
	case "constructor":
		return TokenConstructor
	case "static":
		return TokenStatic
	case "bool":
		return TokenBool
	case "s8":
		return TokenS8
	case "s16":
		return TokenS16
	case "s32":
		return TokenS32
	case "s64":
		return TokenS64
	case "u8":
		return TokenU8
	case "u16":
		return TokenU16
	case "u32":
		return TokenU32
	case "u64":
		return TokenU64
	case "f32":
		return TokenF32
	case "f64":
		return TokenF64
	case "char":
		return TokenChar
	case "string":
		return TokenString_Keyword
	case "list":
		return TokenList
	case "option":
		return TokenOption
	case "result":
		return TokenResult
	case "tuple":
		return TokenTuple
	case "map":
		return TokenMap
	case "future":
		return TokenFuture
	case "stream":
		return TokenStream
	case "own":
		return TokenOwn
	case "borrow":
		return TokenBorrow
	case "error-context":
		return TokenErrorContext
	case "include":
		return TokenInclude
	case "with":
		return TokenWith
	case "feature":
		return TokenFeature
	case "version":
		return TokenVersion_Keyword
	default:
		return TokenIdent
	}
}

// TokenTypeString returns a string representation of a token type.
func (tt TokenType) String() string {
	switch tt {
	case TokenEOF:
		return "EOF"
	case TokenError:
		return "ERROR"
	case TokenIdent:
		return "IDENT"
	case TokenString:
		return "STRING"
	case TokenVersion:
		return "VERSION"
	case TokenPackage:
		return "package"
	case TokenInterface:
		return "interface"
	case TokenWorld:
		return "world"
	case TokenKeywordType:
		return "type"
	case TokenRecord:
		return "record"
	case TokenVariant:
		return "variant"
	case TokenEnum:
		return "enum"
	case TokenFlags:
		return "flags"
	case TokenResource:
		return "resource"
	case TokenFunc:
		return "func"
	case TokenUse:
		return "use"
	case TokenImport:
		return "import"
	case TokenExport:
		return "export"
	case TokenAs:
		return "as"
	case TokenAsync:
		return "async"
	case TokenConstructor:
		return "constructor"
	case TokenStatic:
		return "static"
	case TokenBool:
		return "bool"
	case TokenS8:
		return "s8"
	case TokenS16:
		return "s16"
	case TokenS32:
		return "s32"
	case TokenS64:
		return "s64"
	case TokenU8:
		return "u8"
	case TokenU16:
		return "u16"
	case TokenU32:
		return "u32"
	case TokenU64:
		return "u64"
	case TokenF32:
		return "f32"
	case TokenF64:
		return "f64"
	case TokenChar:
		return "char"
	case TokenString_Keyword:
		return "string"
	case TokenList:
		return "list"
	case TokenOption:
		return "option"
	case TokenResult:
		return "result"
	case TokenTuple:
		return "tuple"
	case TokenMap:
		return "map"
	case TokenFuture:
		return "future"
	case TokenStream:
		return "stream"
	case TokenOwn:
		return "own"
	case TokenBorrow:
		return "borrow"
	case TokenErrorContext:
		return "error-context"
	case TokenLParen:
		return "("
	case TokenRParen:
		return ")"
	case TokenLBrace:
		return "{"
	case TokenRBrace:
		return "}"
	case TokenLAngle:
		return "<"
	case TokenRAngle:
		return ">"
	case TokenColon:
		return ":"
	case TokenSemicolon:
		return ";"
	case TokenComma:
		return ","
	case TokenDot:
		return "."
	case TokenEq:
		return "="
	case TokenArrow:
		return "->"
	case TokenSlash:
		return "/"
	case TokenAt:
		return "@"
	case TokenStar:
		return "*"
	case TokenUnstable:
		return "@unstable"
	case TokenSince:
		return "@since"
	case TokenDeprecated:
		return "@deprecated"
	case TokenExternalID:
		return "@external-id"
	case TokenFeature:
		return "feature"
	case TokenVersion_Keyword:
		return "version"
	case TokenInclude:
		return "include"
	case TokenWith:
		return "with"
	default:
		return fmt.Sprintf("TokenType(%d)", tt)
	}
}

// Tokenize returns all tokens from the source.
func Tokenize(source string) ([]Token, error) {
	lex := NewLexer(source)
	var tokens []Token
	for {
		tok := lex.NextToken()
		if tok.Type == TokenError {
			return nil, fmt.Errorf("line %d, column %d: %s", tok.Line, tok.Column, tok.Text)
		}
		tokens = append(tokens, tok)
		if tok.Type == TokenEOF {
			break
		}
	}
	return tokens, nil
}
