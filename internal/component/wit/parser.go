package wit

import (
	"fmt"
	"strings"
)

// Parser parses WIT source code into an AST.
type Parser struct {
	tokens    []Token
	pos       int
	current   Token
	filename  string
}

// Parse parses WIT source code and returns a Package AST.
// name is used for error reporting.
func Parse(name, src string) (*Package, error) {
	tokens, err := Tokenize(src)
	if err != nil {
		return nil, err
	}

	p := &Parser{
		tokens:    tokens,
		pos:       0,
		filename:  name,
	}
	if len(tokens) > 0 {
		p.current = tokens[0]
	}

	pkg := &Package{Name: ""}
	var items []PackageItem

	// Parse optional package declaration
	if p.current.Type == TokenPackage {
		pkgName, err := p.parsePackageDecl()
		if err != nil {
			return nil, err
		}
		pkg.Name = pkgName
	}

	// Parse package items
	for p.current.Type != TokenEOF {
		item, err := p.parsePackageItem()
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}

	pkg.Items = items
	return pkg, nil
}

// parsePackageDecl parses "package namespace:name@version;" or "package namespace:name;"
func (p *Parser) parsePackageDecl() (string, error) {
	if !p.expect(TokenPackage) {
		return "", p.errorf("expected 'package'")
	}
	p.advance()

	var name strings.Builder

	// Parse package name components (namespace:name/path@version)
	for {
		if p.isIdentifierToken() {
			name.WriteString(p.current.Text)
			p.advance()
		} else if p.current.Type == TokenColon {
			name.WriteString(":")
			p.advance()
		} else if p.current.Type == TokenSlash {
			name.WriteString("/")
			p.advance()
		} else if p.current.Type == TokenAt {
			name.WriteString("@")
			p.advance()
			// Parse version
			if p.current.Type != TokenVersion && !p.isIdentifierToken() {
				return "", p.errorf("expected version after @")
			}
			name.WriteString(p.current.Text)
			p.advance()
			break
		} else {
			break
		}
	}

	if !p.expect(TokenSemicolon) {
		return "", p.errorf("expected ';' after package declaration")
	}
	p.advance()

	return name.String(), nil
}

// parsePackageItem parses a top-level package item (interface, world, type def, use, etc).
func (p *Parser) parsePackageItem() (PackageItem, error) {
	switch p.current.Type {
	case TokenInterface:
		return p.parseInterface()
	case TokenWorld:
		return p.parseWorld()
	case TokenKeywordType:
		return p.parseTypeDef()
	case TokenUse:
		return p.parseUse()
	case TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in package: %s", p.current.Type)
	}
}

// parseInterface parses "interface Name { ... }".
func (p *Parser) parseInterface() (*Interface, error) {
	if !p.expect(TokenInterface) {
		return nil, p.errorf("expected 'interface'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected interface name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{' after interface name")
	}
	p.advance()

	var items []InterfaceItem
	for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
		item, err := p.parseInterfaceItem()
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}

	if !p.expect(TokenRBrace) {
		return nil, p.errorf("expected '}' to close interface")
	}
	p.advance()

	return &Interface{Name: name, Items: items}, nil
}

// parseInterfaceItem parses an item inside an interface.
func (p *Parser) parseInterfaceItem() (InterfaceItem, error) {
	switch p.current.Type {
	case TokenKeywordType:
		return p.parseTypeDef()
	case TokenRecord, TokenVariant, TokenEnum, TokenFlags, TokenResource:
		// Shorthand syntax for type definitions: record Name { ... } instead of type Name = record { ... }
		return p.parseShorthandTypeDef()
	case TokenUse:
		return p.parseUse()
	case TokenIdent:
		// Could be a function definition like "add: func(...)"
		return p.parseInterfaceFunc()
	case TokenRBrace, TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in interface: %s", p.current.Type)
	}
}

// parseInterfaceFunc parses a function definition in an interface.
func (p *Parser) parseInterfaceFunc() (InterfaceItem, error) {
	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected function name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after function name")
	}
	p.advance()

	_, err := p.parseFunc()
	if err != nil {
		return nil, err
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after function")
	}
	p.advance()

	// For now, store as a TypeDef with a Func body (placeholder)
	return &TypeDef{Name: name, Type: &TypeAlias{Target: TypeRef{Kind: "func", Name: name}}}, nil
}

// parseWorld parses "world Name { ... }".
func (p *Parser) parseWorld() (*World, error) {
	if !p.expect(TokenWorld) {
		return nil, p.errorf("expected 'world'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected world name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{' after world name")
	}
	p.advance()

	var items []WorldItem
	for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
		item, err := p.parseWorldItem()
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, item)
		}
	}

	if !p.expect(TokenRBrace) {
		return nil, p.errorf("expected '}' to close world")
	}
	p.advance()

	return &World{Name: name, Items: items}, nil
}

// parseWorldItem parses an item inside a world.
func (p *Parser) parseWorldItem() (WorldItem, error) {
	switch p.current.Type {
	case TokenKeywordType:
		return p.parseTypeDef()
	case TokenRecord, TokenVariant, TokenEnum, TokenFlags, TokenResource:
		// Shorthand syntax for type definitions
		return p.parseShorthandTypeDef()
	case TokenImport:
		return p.parseImport()
	case TokenExport:
		return p.parseExport()
	case TokenUse:
		return p.parseUse()
	case TokenInclude:
		return p.parseInclude()
	case TokenRBrace, TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in world: %s", p.current.Type)
	}
}

// parseTypeDef parses "type Name = Type;" or "record Name { ... }" etc.
func (p *Parser) parseTypeDef() (*TypeDef, error) {
	if !p.expect(TokenKeywordType) {
		return nil, p.errorf("expected 'type'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected type name")
	}
	name := p.current.Text
	p.advance()

	// Check what kind of type definition this is
	if p.current.Type == TokenEq {
		// Type alias (or type definition with = keyword)
		return p.parseTypeAlias(name)
	}

	return nil, p.errorf("unexpected token after type name: %s", p.current.Type)
}

// parseTypeAlias parses "type Name = Type;" or "type Name = record {...};" etc.
func (p *Parser) parseTypeAlias(name string) (*TypeDef, error) {
	if !p.expect(TokenEq) {
		return nil, p.errorf("expected '='")
	}
	p.advance()

	// Check if it's an aggregate type definition (record, variant, enum, flags)
	switch p.current.Type {
	case TokenRecord:
		p.advance()
		if !p.expect(TokenLBrace) {
			return nil, p.errorf("expected '{' after 'record'")
		}
		p.advance()

		var fields []RecordField
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected field name in record")
			}
			fieldName := p.current.Text
			p.advance()

			if !p.expect(TokenColon) {
				return nil, p.errorf("expected ':' after field name")
			}
			p.advance()

			typeRef, err := p.parseType()
			if err != nil {
				return nil, err
			}
			fields = append(fields, RecordField{Name: fieldName, Type: typeRef})

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in record fields")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close record")
		}
		p.advance()

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after record")
		}
		p.advance()

		return &TypeDef{Name: name, Type: &Record{Fields: fields}}, nil

	case TokenVariant:
		p.advance()
		if !p.expect(TokenLBrace) {
			return nil, p.errorf("expected '{' after 'variant'")
		}
		p.advance()

		var cases []VariantCase
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected case name in variant")
			}
			caseName := p.current.Text
			p.advance()

			var typeRef *TypeRef
			if p.current.Type == TokenLParen {
				p.advance()
				t, err := p.parseType()
				if err != nil {
					return nil, err
				}
				typeRef = &t

				if !p.expect(TokenRParen) {
					return nil, p.errorf("expected ')' after variant case type")
				}
				p.advance()
			}

			cases = append(cases, VariantCase{Name: caseName, Type: typeRef})

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in variant cases")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close variant")
		}
		p.advance()

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after variant")
		}
		p.advance()

		return &TypeDef{Name: name, Type: &Variant{Cases: cases}}, nil

	case TokenEnum:
		p.advance()
		if !p.expect(TokenLBrace) {
			return nil, p.errorf("expected '{' after 'enum'")
		}
		p.advance()

		var cases []string
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected case name in enum")
			}
			cases = append(cases, p.current.Text)
			p.advance()

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in enum cases")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close enum")
		}
		p.advance()

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after enum")
		}
		p.advance()

		return &TypeDef{Name: name, Type: &Enum{Cases: cases}}, nil

	case TokenFlags:
		p.advance()
		if !p.expect(TokenLBrace) {
			return nil, p.errorf("expected '{' after 'flags'")
		}
		p.advance()

		var flags []string
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected flag name in flags")
			}
			flags = append(flags, p.current.Text)
			p.advance()

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in flags")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close flags")
		}
		p.advance()

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after flags")
		}
		p.advance()

		return &TypeDef{Name: name, Type: &Flags{Flags: flags}}, nil

	default:
		// Parse as a regular type alias
		typeRef, err := p.parseType()
		if err != nil {
			return nil, err
		}

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after type alias")
		}
		p.advance()

		return &TypeDef{Name: name, Type: &TypeAlias{Target: typeRef}}, nil
	}
}

// parseShorthandTypeDef parses shorthand type definitions: record Name { ... }, etc.
// This handles the syntax where the keyword and name come first, without "type ... =".
func (p *Parser) parseShorthandTypeDef() (*TypeDef, error) {
	var keyword TokenType
	switch p.current.Type {
	case TokenRecord:
		keyword = TokenRecord
	case TokenVariant:
		keyword = TokenVariant
	case TokenEnum:
		keyword = TokenEnum
	case TokenFlags:
		keyword = TokenFlags
	case TokenResource:
		keyword = TokenResource
	default:
		return nil, p.errorf("expected record/variant/enum/flags/resource")
	}

	p.advance()

	if !p.isIdentifierToken() {
		return nil, p.errorf("expected type name after %s", keyword)
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{' after type name")
	}
	p.advance()

	switch keyword {
	case TokenRecord:
		var fields []RecordField
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected field name in record")
			}
			fieldName := p.current.Text
			p.advance()

			if !p.expect(TokenColon) {
				return nil, p.errorf("expected ':' after field name")
			}
			p.advance()

			typeRef, err := p.parseType()
			if err != nil {
				return nil, err
			}
			fields = append(fields, RecordField{Name: fieldName, Type: typeRef})

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in record fields")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close record")
		}
		p.advance()

		// Semicolon is optional in shorthand syntax
		if p.current.Type == TokenSemicolon {
			p.advance()
		}

		return &TypeDef{Name: name, Type: &Record{Fields: fields}}, nil

	case TokenVariant:
		var cases []VariantCase
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected case name in variant")
			}
			caseName := p.current.Text
			p.advance()

			var typeRef *TypeRef
			if p.current.Type == TokenLParen {
				p.advance()
				t, err := p.parseType()
				if err != nil {
					return nil, err
				}
				typeRef = &t

				if !p.expect(TokenRParen) {
					return nil, p.errorf("expected ')' after variant case type")
				}
				p.advance()
			}

			cases = append(cases, VariantCase{Name: caseName, Type: typeRef})

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in variant cases")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close variant")
		}
		p.advance()

		// Semicolon is optional in shorthand syntax
		if p.current.Type == TokenSemicolon {
			p.advance()
		}

		return &TypeDef{Name: name, Type: &Variant{Cases: cases}}, nil

	case TokenEnum:
		var cases []string
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected case name in enum")
			}
			cases = append(cases, p.current.Text)
			p.advance()

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in enum cases")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close enum")
		}
		p.advance()

		// Semicolon is optional in shorthand syntax
		if p.current.Type == TokenSemicolon {
			p.advance()
		}

		return &TypeDef{Name: name, Type: &Enum{Cases: cases}}, nil

	case TokenFlags:
		var flags []string
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected flag name in flags")
			}
			flags = append(flags, p.current.Text)
			p.advance()

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in flags")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close flags")
		}
		p.advance()

		// Semicolon is optional in shorthand syntax
		if p.current.Type == TokenSemicolon {
			p.advance()
		}

		return &TypeDef{Name: name, Type: &Flags{Flags: flags}}, nil

	case TokenResource:
		// Resource with methods - not fully implemented
		// Skip to closing brace
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			p.advance()
		}
		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close resource")
		}
		p.advance()

		// Semicolon is optional in shorthand syntax
		if p.current.Type == TokenSemicolon {
			p.advance()
		}

		return &TypeDef{Name: name, Type: &Resource{Methods: nil}, Unsupported: "resource with methods not yet supported"}, nil

	default:
		return nil, p.errorf("unexpected aggregate type keyword")
	}
}

// parseAggregateType parses record/variant/enum/flags definitions.
// Ambiguity: The grammar doesn't clearly differentiate from context.
// For now, we'll parse the fields and infer the type.
func (p *Parser) parseAggregateType(name string) (*TypeDef, error) {
	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{'")
	}
	p.advance()

	// Peek ahead to determine the type
	// Records have "name: type" fields
	// Variants have "name" or "name(type)" cases
	// Enums have "name" cases
	// Flags have "name" flags
	// We'll parse as a generic structure and infer.

	var fields []RecordField
	var variantCases []VariantCase
	var enumCases []string
	var flagItems []string
	var isRecord, isVariant, isEnum, isFlags bool

	for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
		if p.current.Type != TokenIdent {
			return nil, p.errorf("expected identifier in aggregate type")
		}
		fieldName := p.current.Text
		p.advance()

		if p.current.Type == TokenColon {
			// It's a record field: "name: type"
			isRecord = true
			p.advance()
			typeRef, err := p.parseType()
			if err != nil {
				return nil, err
			}
			fields = append(fields, RecordField{Name: fieldName, Type: typeRef})
		} else if p.current.Type == TokenLParen {
			// It's a variant case with data: "name(type)"
			isVariant = true
			p.advance()
			typeRef, err := p.parseType()
			if err != nil {
				return nil, err
			}
			if !p.expect(TokenRParen) {
				return nil, p.errorf("expected ')' after variant case type")
			}
			p.advance()
			variantCases = append(variantCases, VariantCase{Name: fieldName, Type: &typeRef})
		} else {
			// It's an enum case or variant case without data or flag
			// We'll default to enum if no other context
			enumCases = append(enumCases, fieldName)
			isEnum = true
		}

		if p.current.Type == TokenComma {
			p.advance()
		} else if p.current.Type != TokenRBrace {
			return nil, p.errorf("expected ',' or '}' in aggregate type")
		}
	}

	if !p.expect(TokenRBrace) {
		return nil, p.errorf("expected '}' to close aggregate type")
	}
	p.advance()

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after aggregate type")
	}
	p.advance()

	var typeBody TypeDefBody
	if isRecord && len(fields) > 0 {
		typeBody = &Record{Fields: fields}
	} else if isVariant && len(variantCases) > 0 {
		typeBody = &Variant{Cases: variantCases}
	} else if isEnum && len(enumCases) > 0 {
		typeBody = &Enum{Cases: enumCases}
	} else if isFlags {
		typeBody = &Flags{Flags: flagItems}
	} else {
		// Default to enum if we have cases
		if len(enumCases) > 0 {
			typeBody = &Enum{Cases: enumCases}
		} else {
			return nil, p.errorf("could not determine aggregate type kind for %s", name)
		}
	}

	return &TypeDef{Name: name, Type: typeBody}, nil
}

// parseFunc parses a function signature: "async? func (param-list) result-list".
func (p *Parser) parseFunc() (*Func, error) {
	async := false
	if p.current.Type == TokenAsync {
		async = true
		p.advance()
	}

	if !p.expect(TokenFunc) {
		return nil, p.errorf("expected 'func'")
	}
	p.advance()

	// Parse parameters
	if !p.expect(TokenLParen) {
		return nil, p.errorf("expected '(' in function signature")
	}
	p.advance()

	var params []FuncParam
	for p.current.Type != TokenRParen && p.current.Type != TokenEOF {
		if p.current.Type != TokenIdent {
			return nil, p.errorf("expected parameter name")
		}
		paramName := p.current.Text
		p.advance()

		if !p.expect(TokenColon) {
			return nil, p.errorf("expected ':' after parameter name")
		}
		p.advance()

		typeRef, err := p.parseType()
		if err != nil {
			return nil, err
		}
		params = append(params, FuncParam{Name: paramName, Type: typeRef})

		if p.current.Type == TokenComma {
			p.advance()
		} else if p.current.Type != TokenRParen {
			return nil, p.errorf("expected ',' or ')' in parameter list")
		}
	}

	if !p.expect(TokenRParen) {
		return nil, p.errorf("expected ')' to close parameter list")
	}
	p.advance()

	// Parse optional return type
	var resultType *TypeRef
	if p.current.Type == TokenArrow {
		p.advance()
		typeRef, err := p.parseType()
		if err != nil {
			return nil, err
		}
		resultType = &typeRef
	}

	return &Func{Params: params, Result: resultType, Async: async}, nil
}

// parseType parses a type reference.
func (p *Parser) parseType() (TypeRef, error) {
	switch p.current.Type {
	case TokenU8:
		p.advance()
		return TypeRef{Kind: "u8"}, nil
	case TokenU16:
		p.advance()
		return TypeRef{Kind: "u16"}, nil
	case TokenU32:
		p.advance()
		return TypeRef{Kind: "u32"}, nil
	case TokenU64:
		p.advance()
		return TypeRef{Kind: "u64"}, nil
	case TokenS8:
		p.advance()
		return TypeRef{Kind: "s8"}, nil
	case TokenS16:
		p.advance()
		return TypeRef{Kind: "s16"}, nil
	case TokenS32:
		p.advance()
		return TypeRef{Kind: "s32"}, nil
	case TokenS64:
		p.advance()
		return TypeRef{Kind: "s64"}, nil
	case TokenF32:
		p.advance()
		return TypeRef{Kind: "f32"}, nil
	case TokenF64:
		p.advance()
		return TypeRef{Kind: "f64"}, nil
	case TokenBool:
		p.advance()
		return TypeRef{Kind: "bool"}, nil
	case TokenChar:
		p.advance()
		return TypeRef{Kind: "char"}, nil
	case TokenString_Keyword:
		p.advance()
		return TypeRef{Kind: "string"}, nil
	case TokenList:
		return p.parseListType()
	case TokenOption:
		return p.parseOptionType()
	case TokenResult:
		return p.parseResultType()
	case TokenTuple:
		return p.parseTupleType()
	case TokenMap:
		return p.parseMapType()
	case TokenFuture:
		return p.parseFutureType()
	case TokenStream:
		return p.parseStreamType()
	case TokenOwn:
		return p.parseOwnType()
	case TokenBorrow:
		return p.parseBorrowType()
	case TokenIdent:
		name := p.current.Text
		p.advance()
		return TypeRef{Kind: "named", Name: name}, nil
	default:
		return TypeRef{}, p.errorf("unexpected token in type: %s", p.current.Type)
	}
}

// parseListType parses "list<Type>" or "list<Type, uint>".
func (p *Parser) parseListType() (TypeRef, error) {
	if !p.expect(TokenList) {
		return TypeRef{}, p.errorf("expected 'list'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'list'")
	}
	p.advance()

	innerType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if p.current.Type == TokenComma {
		p.advance()
		// Skip the length specifier for now
		if p.current.Type != TokenIdent && p.current.Type != TokenVersion {
			return TypeRef{}, p.errorf("expected length specifier in list")
		}
		p.advance()
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close list type")
	}
	p.advance()

	return TypeRef{Kind: "list", Inner: &innerType}, nil
}

// parseOptionType parses "option<Type>".
func (p *Parser) parseOptionType() (TypeRef, error) {
	if !p.expect(TokenOption) {
		return TypeRef{}, p.errorf("expected 'option'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'option'")
	}
	p.advance()

	innerType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close option type")
	}
	p.advance()

	return TypeRef{Kind: "option", Inner: &innerType}, nil
}

// parseResultType parses "result<T, E>" or "result<T>" or "result".
func (p *Parser) parseResultType() (TypeRef, error) {
	if !p.expect(TokenResult) {
		return TypeRef{}, p.errorf("expected 'result'")
	}
	p.advance()

	if p.current.Type != TokenLAngle {
		// Bare "result" with no type parameters
		return TypeRef{Kind: "result"}, nil
	}

	p.advance()

	// Parse first type or "_" (error placeholder)
	var innerType *TypeRef
	if p.current.Type == TokenIdent && p.current.Text == "_" {
		p.advance()
	} else {
		t, err := p.parseType()
		if err != nil {
			return TypeRef{}, err
		}
		innerType = &t
	}

	var innerType2 *TypeRef
	if p.current.Type == TokenComma {
		p.advance()
		t, err := p.parseType()
		if err != nil {
			return TypeRef{}, err
		}
		innerType2 = &t
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close result type")
	}
	p.advance()

	return TypeRef{Kind: "result", Inner: innerType, Inner2: innerType2}, nil
}

// parseTupleType parses "tuple<T1, T2, ...>".
func (p *Parser) parseTupleType() (TypeRef, error) {
	if !p.expect(TokenTuple) {
		return TypeRef{}, p.errorf("expected 'tuple'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'tuple'")
	}
	p.advance()

	var elems []*TypeRef
	for p.current.Type != TokenRAngle && p.current.Type != TokenEOF {
		t, err := p.parseType()
		if err != nil {
			return TypeRef{}, err
		}
		elems = append(elems, &t)

		if p.current.Type == TokenComma {
			p.advance()
		} else if p.current.Type != TokenRAngle {
			return TypeRef{}, p.errorf("expected ',' or '>' in tuple")
		}
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close tuple type")
	}
	p.advance()

	return TypeRef{Kind: "tuple", Tuple: elems}, nil
}

// parseMapType parses "map<KT, VT>".
func (p *Parser) parseMapType() (TypeRef, error) {
	if !p.expect(TokenMap) {
		return TypeRef{}, p.errorf("expected 'map'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'map'")
	}
	p.advance()

	keyType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if !p.expect(TokenComma) {
		return TypeRef{}, p.errorf("expected ',' after map key type")
	}
	p.advance()

	valueType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close map type")
	}
	p.advance()

	return TypeRef{Kind: "map", Inner: &keyType, Inner2: &valueType}, nil
}

// parseFutureType parses "future<T>" or "future".
func (p *Parser) parseFutureType() (TypeRef, error) {
	if !p.expect(TokenFuture) {
		return TypeRef{}, p.errorf("expected 'future'")
	}
	p.advance()

	if p.current.Type != TokenLAngle {
		return TypeRef{Kind: "future"}, nil
	}

	p.advance()
	innerType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close future type")
	}
	p.advance()

	return TypeRef{Kind: "future", Inner: &innerType}, nil
}

// parseStreamType parses "stream<T>" or "stream".
func (p *Parser) parseStreamType() (TypeRef, error) {
	if !p.expect(TokenStream) {
		return TypeRef{}, p.errorf("expected 'stream'")
	}
	p.advance()

	if p.current.Type != TokenLAngle {
		return TypeRef{Kind: "stream"}, nil
	}

	p.advance()
	innerType, err := p.parseType()
	if err != nil {
		return TypeRef{}, err
	}

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close stream type")
	}
	p.advance()

	return TypeRef{Kind: "stream", Inner: &innerType}, nil
}

// parseOwnType parses "own<ResourceName>".
func (p *Parser) parseOwnType() (TypeRef, error) {
	if !p.expect(TokenOwn) {
		return TypeRef{}, p.errorf("expected 'own'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'own'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return TypeRef{}, p.errorf("expected resource name in own<>")
	}
	resourceName := p.current.Text
	p.advance()

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close own type")
	}
	p.advance()

	return TypeRef{Kind: "own", Name: resourceName}, nil
}

// parseBorrowType parses "borrow<ResourceName>".
func (p *Parser) parseBorrowType() (TypeRef, error) {
	if !p.expect(TokenBorrow) {
		return TypeRef{}, p.errorf("expected 'borrow'")
	}
	p.advance()

	if !p.expect(TokenLAngle) {
		return TypeRef{}, p.errorf("expected '<' after 'borrow'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return TypeRef{}, p.errorf("expected resource name in borrow<>")
	}
	resourceName := p.current.Text
	p.advance()

	if !p.expect(TokenRAngle) {
		return TypeRef{}, p.errorf("expected '>' to close borrow type")
	}
	p.advance()

	return TypeRef{Kind: "borrow", Name: resourceName}, nil
}

// parseUse parses "use path.{names};" or "use path as name;".
func (p *Parser) parseUse() (*Use, error) {
	if !p.expect(TokenUse) {
		return nil, p.errorf("expected 'use'")
	}
	p.advance()

	// Parse path (can include identifiers or keywords)
	var pathParts []string
	for p.isIdentifierToken() || p.current.Type == TokenColon || p.current.Type == TokenSlash || p.current.Type == TokenAt {
		if p.isIdentifierToken() {
			pathParts = append(pathParts, p.current.Text)
			p.advance()
		} else {
			pathParts = append(pathParts, p.current.Text)
			p.advance()
		}
	}

	path := strings.Join(pathParts, "")
	use := &Use{Path: path, Names: make(map[string]string)}

	if p.current.Type == TokenDot {
		p.advance()
		if p.current.Type != TokenLBrace {
			return nil, p.errorf("expected '{' after '.' in use statement")
		}
		p.advance()

		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if p.current.Type != TokenIdent {
				return nil, p.errorf("expected identifier in use names")
			}
			name := p.current.Text
			p.advance()

			alias := name
			if p.current.Type == TokenAs {
				p.advance()
				if p.current.Type != TokenIdent {
					return nil, p.errorf("expected alias after 'as'")
				}
				alias = p.current.Text
				p.advance()
			}

			use.Names[alias] = name

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in use names")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close use names")
		}
		p.advance()
	} else if p.current.Type == TokenAs {
		p.advance()
		if p.current.Type != TokenIdent {
			return nil, p.errorf("expected alias after 'as'")
		}
		alias := p.current.Text
		use.Names[alias] = path
		p.advance()
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after use statement")
	}
	p.advance()

	return use, nil
}

// parseImport parses "import name : func-type;" or "import name : interface { ... };"
func (p *Parser) parseImport() (*Import, error) {
	if !p.expect(TokenImport) {
		return nil, p.errorf("expected 'import'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected import name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after import name")
	}
	p.advance()

	// Could be a func type or an interface or a use path
	var importType ImportType
	if p.current.Type == TokenFunc {
		fn, err := p.parseFunc()
		if err != nil {
			return nil, err
		}
		importType = &ImportFunc{Func: *fn}
	} else if p.current.Type == TokenInterface {
		return nil, fmt.Errorf("line %d: inline interface in import not yet supported", p.current.Line)
	} else {
		return nil, p.errorf("expected 'func' or interface in import")
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after import")
	}
	p.advance()

	return &Import{Name: name, Type: importType}, nil
}

// parseExport parses "export name : func-type;" or "export name : interface { ... };"
func (p *Parser) parseExport() (*Export, error) {
	if !p.expect(TokenExport) {
		return nil, p.errorf("expected 'export'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected export name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after export name")
	}
	p.advance()

	var exportType ExportType
	if p.current.Type == TokenFunc {
		fn, err := p.parseFunc()
		if err != nil {
			return nil, err
		}
		exportType = &ExportFunc{Func: *fn}
	} else if p.current.Type == TokenInterface {
		return nil, fmt.Errorf("line %d: inline interface in export not yet supported", p.current.Line)
	} else {
		return nil, p.errorf("expected 'func' or interface in export")
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after export")
	}
	p.advance()

	return &Export{Name: name, Type: exportType}, nil
}

// parseInclude parses "include path;" or "include path with { renames };"
func (p *Parser) parseInclude() (WorldItem, error) {
	if !p.expect(TokenInclude) {
		return nil, p.errorf("expected 'include'")
	}
	p.advance()

	// Parse path (for now, just skip it and return nil to mark as unsupported)
	for p.current.Type == TokenIdent || p.current.Type == TokenColon || p.current.Type == TokenSlash {
		p.advance()
	}

	if p.current.Type == TokenWith {
		p.advance()
		if p.current.Type != TokenLBrace {
			return nil, p.errorf("expected '{' after 'with'")
		}
		p.advance()

		// Skip the rename list
		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			p.advance()
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}'")
		}
		p.advance()
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after include")
	}
	p.advance()

	// Return a marker that include is unsupported for now
	return &TypeDef{Name: "include", Unsupported: "include statement not yet supported"}, nil
}

// Helper methods

func (p *Parser) advance() {
	if p.pos+1 < len(p.tokens) {
		p.pos++
		p.current = p.tokens[p.pos]
	}
}

func (p *Parser) expect(tt TokenType) bool {
	return p.current.Type == tt
}

// isIdentifierToken returns true if the current token can be used as an identifier.
// This includes TokenIdent and any keyword that can appear in identifier positions.
func (p *Parser) isIdentifierToken() bool {
	switch p.current.Type {
	case TokenIdent:
		return true
	// Keywords that can be used as identifiers in certain contexts
	case TokenPackage, TokenInterface, TokenWorld, TokenKeywordType, TokenRecord,
		TokenVariant, TokenEnum, TokenFlags, TokenResource, TokenFunc,
		TokenUse, TokenImport, TokenExport, TokenAs, TokenAsync,
		TokenConstructor, TokenStatic, TokenBool, TokenS8, TokenS16, TokenS32, TokenS64,
		TokenU8, TokenU16, TokenU32, TokenU64, TokenF32, TokenF64, TokenChar,
		TokenString_Keyword, TokenList, TokenOption, TokenResult, TokenTuple, TokenMap,
		TokenFuture, TokenStream, TokenOwn, TokenBorrow, TokenInclude, TokenWith,
		TokenFeature, TokenVersion_Keyword, TokenUnstable, TokenSince, TokenDeprecated,
		TokenExternalID:
		return true
	default:
		return false
	}
}

func (p *Parser) errorf(format string, args ...interface{}) error {
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s:%d:%d: %s", p.filename, p.current.Line, p.current.Column, msg)
}
