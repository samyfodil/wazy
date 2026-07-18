package wit

import (
	"fmt"
	"strings"
)

// Parser parses WIT source code into an AST.
type Parser struct {
	tokens   []Token
	pos      int
	current  Token
	filename string
}

// Parse parses WIT source code and returns a Package AST.
// name is used for error reporting.
func Parse(name, src string) (*Package, error) {
	tokens, err := Tokenize(src)
	if err != nil {
		return nil, err
	}

	p := &Parser{
		tokens:   tokens,
		pos:      0,
		filename: name,
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

// parseAttributes parses zero or more feature-gate / external-id attributes
// immediately preceding a WIT item:
//
//	gate      ::= gate-item*
//	gate-item ::= '@unstable' '(' 'feature' '=' id ')'
//	            | '@since' '(' 'version' '=' <semver> ')'
//	            | '@deprecated' '(' 'version' '=' <semver> ')'
//	external-id ::= '@external-id' '(' string-literal ')'
//
// It returns the accumulated Gate (since/unstable/deprecated items, in the
// order they appeared) and the external-id string, if any.
func (p *Parser) parseAttributes() (Gate, string, error) {
	var gate Gate
	externalID := ""

	for {
		switch p.current.Type {
		case TokenSince, TokenDeprecated:
			kind := "since"
			if p.current.Type == TokenDeprecated {
				kind = "deprecated"
			}
			p.advance()

			if !p.expect(TokenLParen) {
				return nil, "", p.errorf("expected '(' after @%s", kind)
			}
			p.advance()

			if !p.expect(TokenVersion_Keyword) {
				return nil, "", p.errorf("expected 'version' in @%s(...)", kind)
			}
			p.advance()

			if !p.expect(TokenEq) {
				return nil, "", p.errorf("expected '=' after 'version' in @%s(...)", kind)
			}
			p.advance()

			if p.current.Type != TokenVersion && !p.isIdentifierToken() {
				return nil, "", p.errorf("expected version value in @%s(...)", kind)
			}
			version := p.current.Text
			p.advance()

			if !p.expect(TokenRParen) {
				return nil, "", p.errorf("expected ')' to close @%s(...)", kind)
			}
			p.advance()

			gate = append(gate, GateItem{Kind: kind, Version: version})

		case TokenUnstable:
			p.advance()

			if !p.expect(TokenLParen) {
				return nil, "", p.errorf("expected '(' after @unstable")
			}
			p.advance()

			if !p.expect(TokenFeature) {
				return nil, "", p.errorf("expected 'feature' in @unstable(...)")
			}
			p.advance()

			if !p.expect(TokenEq) {
				return nil, "", p.errorf("expected '=' after 'feature' in @unstable(...)")
			}
			p.advance()

			if !p.isIdentifierToken() {
				return nil, "", p.errorf("expected feature name in @unstable(...)")
			}
			feature := p.current.Text
			p.advance()

			if !p.expect(TokenRParen) {
				return nil, "", p.errorf("expected ')' to close @unstable(...)")
			}
			p.advance()

			gate = append(gate, GateItem{Kind: "unstable", Feature: feature})

		case TokenExternalID:
			p.advance()

			if !p.expect(TokenLParen) {
				return nil, "", p.errorf("expected '(' after @external-id")
			}
			p.advance()

			if p.current.Type != TokenString {
				return nil, "", p.errorf("expected string literal in @external-id(...)")
			}
			externalID = p.current.Text
			p.advance()

			if !p.expect(TokenRParen) {
				return nil, "", p.errorf("expected ')' to close @external-id(...)")
			}
			p.advance()

		default:
			return gate, externalID, nil
		}
	}
}

// parsePackageItem parses a top-level package item (interface, world, type def, use, etc).
func (p *Parser) parsePackageItem() (PackageItem, error) {
	gate, externalID, err := p.parseAttributes()
	if err != nil {
		return nil, err
	}

	switch p.current.Type {
	case TokenInterface:
		return p.parseInterface(gate, externalID)
	case TokenWorld:
		return p.parseWorld(gate, externalID)
	case TokenKeywordType:
		return p.parseTypeDef(gate, externalID)
	case TokenUse:
		return p.parseUse(gate, externalID)
	case TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in package: %s", p.current.Type)
	}
}

// parseInterface parses "interface Name { ... }", including any leading gate.
func (p *Parser) parseInterface(gate Gate, externalID string) (*Interface, error) {
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

	return &Interface{Name: name, Items: items, Gate: gate, ExternalID: externalID}, nil
}

// parseInlineInterfaceBody parses "interface { ... }" as used inline within
// an import or export item.
func (p *Parser) parseInlineInterfaceBody() ([]InterfaceItem, error) {
	if !p.expect(TokenInterface) {
		return nil, p.errorf("expected 'interface'")
	}
	p.advance()

	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{' after 'interface'")
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
		return nil, p.errorf("expected '}' to close inline interface")
	}
	p.advance()

	return items, nil
}

// parseInterfaceItem parses an item inside an interface, including any
// leading gate.
func (p *Parser) parseInterfaceItem() (InterfaceItem, error) {
	gate, externalID, err := p.parseAttributes()
	if err != nil {
		return nil, err
	}

	switch p.current.Type {
	case TokenKeywordType:
		return p.parseTypeDef(gate, externalID)
	case TokenRecord, TokenVariant, TokenEnum, TokenFlags, TokenResource:
		// Shorthand syntax for type definitions: record Name { ... } instead of type Name = record { ... }
		return p.parseShorthandTypeDef(gate, externalID)
	case TokenUse:
		return p.parseUse(gate, externalID)
	case TokenIdent:
		// Could be a function definition like "add: func(...)"
		return p.parseInterfaceFunc(gate, externalID)
	case TokenRBrace, TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in interface: %s", p.current.Type)
	}
}

// parseInterfaceFunc parses a named function item in an interface:
// "name: func(...) -> result;".
func (p *Parser) parseInterfaceFunc(gate Gate, externalID string) (InterfaceItem, error) {
	if p.current.Type != TokenIdent {
		return nil, p.errorf("expected function name")
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after function name")
	}
	p.advance()

	fn, err := p.parseFunc()
	if err != nil {
		return nil, err
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after function")
	}
	p.advance()

	return &InterfaceFunc{Name: name, Func: *fn, Gate: gate, ExternalID: externalID}, nil
}

// parseWorld parses "world Name { ... }", including any leading gate.
func (p *Parser) parseWorld(gate Gate, externalID string) (*World, error) {
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

	return &World{Name: name, Items: items, Gate: gate, ExternalID: externalID}, nil
}

// parseWorldItem parses an item inside a world, including any leading gate.
func (p *Parser) parseWorldItem() (WorldItem, error) {
	gate, externalID, err := p.parseAttributes()
	if err != nil {
		return nil, err
	}

	switch p.current.Type {
	case TokenKeywordType:
		return p.parseTypeDef(gate, externalID)
	case TokenRecord, TokenVariant, TokenEnum, TokenFlags, TokenResource:
		// Shorthand syntax for type definitions
		return p.parseShorthandTypeDef(gate, externalID)
	case TokenImport:
		return p.parseImport(gate, externalID)
	case TokenExport:
		return p.parseExport(gate, externalID)
	case TokenUse:
		return p.parseUse(gate, externalID)
	case TokenInclude:
		return p.parseInclude(gate, externalID)
	case TokenRBrace, TokenEOF:
		return nil, nil
	default:
		return nil, p.errorf("unexpected token in world: %s", p.current.Type)
	}
}

// parseTypeDef parses "type Name = Type;", including any leading gate.
func (p *Parser) parseTypeDef(gate Gate, externalID string) (*TypeDef, error) {
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
		td, err := p.parseTypeAlias(name)
		if err != nil {
			return nil, err
		}
		td.Gate = gate
		td.ExternalID = externalID
		return td, nil
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

		fields, err := p.parseRecordFields()
		if err != nil {
			return nil, err
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

		cases, err := p.parseVariantCases()
		if err != nil {
			return nil, err
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

		cases, err := p.parseEnumCases()
		if err != nil {
			return nil, err
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

		flags, err := p.parseFlagsList()
		if err != nil {
			return nil, err
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

// parseShorthandTypeDef parses shorthand type definitions: record Name { ... },
// variant Name { ... }, enum Name { ... }, flags Name { ... },
// resource Name { ... } (or the bodyless "resource Name;").
// This handles the syntax where the keyword and name come first, without "type ... =".
func (p *Parser) parseShorthandTypeDef(gate Gate, externalID string) (*TypeDef, error) {
	keyword := p.current.Type
	switch keyword {
	case TokenRecord, TokenVariant, TokenEnum, TokenFlags, TokenResource:
	default:
		return nil, p.errorf("expected record/variant/enum/flags/resource")
	}
	p.advance()

	if !p.isIdentifierToken() {
		return nil, p.errorf("expected type name after %s", keyword)
	}
	name := p.current.Text
	p.advance()

	// Bodyless resource declaration: "resource name;"
	if keyword == TokenResource && p.current.Type == TokenSemicolon {
		p.advance()
		return &TypeDef{Name: name, Type: &Resource{}, Gate: gate, ExternalID: externalID}, nil
	}

	if !p.expect(TokenLBrace) {
		return nil, p.errorf("expected '{' after type name")
	}
	p.advance()

	var body TypeDefBody
	switch keyword {
	case TokenRecord:
		fields, err := p.parseRecordFields()
		if err != nil {
			return nil, err
		}
		body = &Record{Fields: fields}

	case TokenVariant:
		cases, err := p.parseVariantCases()
		if err != nil {
			return nil, err
		}
		body = &Variant{Cases: cases}

	case TokenEnum:
		cases, err := p.parseEnumCases()
		if err != nil {
			return nil, err
		}
		body = &Enum{Cases: cases}

	case TokenFlags:
		flags, err := p.parseFlagsList()
		if err != nil {
			return nil, err
		}
		body = &Flags{Flags: flags}

	case TokenResource:
		methods, err := p.parseResourceMethods()
		if err != nil {
			return nil, err
		}
		body = &Resource{Methods: methods}
	}

	if !p.expect(TokenRBrace) {
		return nil, p.errorf("expected '}' to close %s", keyword)
	}
	p.advance()

	// Semicolon is optional in shorthand syntax
	if p.current.Type == TokenSemicolon {
		p.advance()
	}

	return &TypeDef{Name: name, Type: body, Gate: gate, ExternalID: externalID}, nil
}

// parseRecordFields parses comma-separated "name: type" fields up to (but not
// consuming) the closing '}'.
func (p *Parser) parseRecordFields() ([]RecordField, error) {
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
	return fields, nil
}

// parseVariantCases parses comma-separated "name" or "name(type)" cases up to
// (but not consuming) the closing '}'.
func (p *Parser) parseVariantCases() ([]VariantCase, error) {
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
	return cases, nil
}

// parseEnumCases parses comma-separated case names up to (but not consuming)
// the closing '}'.
func (p *Parser) parseEnumCases() ([]string, error) {
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
	return cases, nil
}

// parseFlagsList parses comma-separated flag names up to (but not consuming)
// the closing '}'.
func (p *Parser) parseFlagsList() ([]string, error) {
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
	return flags, nil
}

// parseResourceMethods parses the body of a resource:
//
//	resource-method ::= func-item
//	                  | id ':' 'static' func-type ';'
//	                  | 'constructor' param-list result-list? ';'
//
// each optionally preceded by a gate/external-id, up to (but not consuming)
// the closing '}'.
func (p *Parser) parseResourceMethods() ([]ResourceMethod, error) {
	var methods []ResourceMethod
	for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
		gate, externalID, err := p.parseAttributes()
		if err != nil {
			return nil, err
		}

		if p.current.Type == TokenConstructor {
			p.advance()

			params, err := p.parseParamList()
			if err != nil {
				return nil, err
			}

			var result *TypeRef
			if p.current.Type == TokenArrow {
				p.advance()
				t, err := p.parseType()
				if err != nil {
					return nil, err
				}
				result = &t
			}

			if !p.expect(TokenSemicolon) {
				return nil, p.errorf("expected ';' after constructor")
			}
			p.advance()

			methods = append(methods, ResourceMethod{
				Name:          "constructor",
				IsConstructor: true,
				Func:          Func{Params: params, Result: result},
				Gate:          gate,
				ExternalID:    externalID,
			})
			continue
		}

		if !p.isIdentifierToken() {
			return nil, p.errorf("expected method name, 'constructor', or '}' in resource body")
		}
		methodName := p.current.Text
		p.advance()

		if !p.expect(TokenColon) {
			return nil, p.errorf("expected ':' after resource method name")
		}
		p.advance()

		isStatic := false
		if p.current.Type == TokenStatic {
			isStatic = true
			p.advance()
		}

		fn, err := p.parseFunc()
		if err != nil {
			return nil, err
		}

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after resource method")
		}
		p.advance()

		methods = append(methods, ResourceMethod{
			Name:       methodName,
			IsStatic:   isStatic,
			Func:       *fn,
			Gate:       gate,
			ExternalID: externalID,
		})
	}
	return methods, nil
}

// parseParamList parses "(name: type, ...)".
func (p *Parser) parseParamList() ([]FuncParam, error) {
	if !p.expect(TokenLParen) {
		return nil, p.errorf("expected '(' in parameter list")
	}
	p.advance()

	var params []FuncParam
	for p.current.Type != TokenRParen && p.current.Type != TokenEOF {
		if !p.isIdentifierToken() {
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

	return params, nil
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

	params, err := p.parseParamList()
	if err != nil {
		return nil, err
	}

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
	case TokenErrorContext:
		p.advance()
		return TypeRef{Kind: "error-context"}, nil
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

	if !p.isIdentifierToken() {
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

	if !p.isIdentifierToken() {
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

// parseUsePath parses a use-path:
//
//	use-path ::= id
//	           | id ':' id '/' id ('@' valid-semver)?
func (p *Parser) parseUsePath() (string, error) {
	if !p.isIdentifierToken() {
		return "", p.errorf("expected identifier in path")
	}
	var b strings.Builder
	b.WriteString(p.current.Text)
	p.advance()

	for p.current.Type == TokenColon || p.current.Type == TokenSlash {
		b.WriteString(p.current.Text)
		p.advance()
		if !p.isIdentifierToken() {
			return "", p.errorf("expected identifier in path")
		}
		b.WriteString(p.current.Text)
		p.advance()
	}

	if p.current.Type == TokenAt {
		b.WriteString("@")
		p.advance()
		if p.current.Type != TokenVersion && !p.isIdentifierToken() {
			return "", p.errorf("expected version after '@' in path")
		}
		b.WriteString(p.current.Text)
		p.advance()
	}

	return b.String(), nil
}

// parseUse parses "use path.{names};" or "use path as name;", including any
// leading gate.
func (p *Parser) parseUse(gate Gate, externalID string) (*Use, error) {
	if !p.expect(TokenUse) {
		return nil, p.errorf("expected 'use'")
	}
	p.advance()

	path, err := p.parseUsePath()
	if err != nil {
		return nil, err
	}

	use := &Use{Path: path, Names: make(map[string]string), Gate: gate, ExternalID: externalID}

	if p.current.Type == TokenDot {
		p.advance()
		if !p.expect(TokenLBrace) {
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

// parseImport parses "import name : func-type;" or "import name : interface { ... };",
// including any leading gate.
func (p *Parser) parseImport(gate Gate, externalID string) (*Import, error) {
	if !p.expect(TokenImport) {
		return nil, p.errorf("expected 'import'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, fmt.Errorf("line %d: import of a bare package path (without 'name:') is not yet supported", p.current.Line)
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after import name")
	}
	p.advance()

	// Could be a func type ("func(...) -> T;", semicolon-terminated) or an
	// inline interface ("interface { ... }", no trailing semicolon).
	var importType ImportType
	switch p.current.Type {
	case TokenFunc, TokenAsync:
		fn, err := p.parseFunc()
		if err != nil {
			return nil, err
		}
		importType = &ImportFunc{Func: *fn}

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after import")
		}
		p.advance()
	case TokenInterface:
		items, err := p.parseInlineInterfaceBody()
		if err != nil {
			return nil, err
		}
		importType = &ImportInterface{Items: items}
	default:
		// Neither 'func' nor 'interface' follows "name:". In practice this
		// means "name" was actually the first segment of a bare package
		// path (e.g. "import wasi:io/streams@0.2.0;"), since the lexer
		// tokenizes that leading namespace exactly like a local import
		// name; the ':' we just consumed is the path separator, not the
		// "name:" declarator. Report the specific, actionable error
		// instead of a generic "expected 'func' or 'interface'".
		return nil, fmt.Errorf("line %d: import of a bare package path (without 'name:') is not yet supported", p.current.Line)
	}

	return &Import{Name: name, Type: importType, Gate: gate, ExternalID: externalID}, nil
}

// parseExport parses "export name : func-type;" or "export name : interface { ... };",
// including any leading gate.
func (p *Parser) parseExport(gate Gate, externalID string) (*Export, error) {
	if !p.expect(TokenExport) {
		return nil, p.errorf("expected 'export'")
	}
	p.advance()

	if p.current.Type != TokenIdent {
		return nil, fmt.Errorf("line %d: export of a bare package path (without 'name:') is not yet supported", p.current.Line)
	}
	name := p.current.Text
	p.advance()

	if !p.expect(TokenColon) {
		return nil, p.errorf("expected ':' after export name")
	}
	p.advance()

	// Could be a func type ("func(...) -> T;", semicolon-terminated) or an
	// inline interface ("interface { ... }", no trailing semicolon).
	var exportType ExportType
	switch p.current.Type {
	case TokenFunc, TokenAsync:
		fn, err := p.parseFunc()
		if err != nil {
			return nil, err
		}
		exportType = &ExportFunc{Func: *fn}

		if !p.expect(TokenSemicolon) {
			return nil, p.errorf("expected ';' after export")
		}
		p.advance()
	case TokenInterface:
		items, err := p.parseInlineInterfaceBody()
		if err != nil {
			return nil, err
		}
		exportType = &ExportInterface{Items: items}
	default:
		// See the matching comment in parseImport: this is reached for a
		// bare package-path export (e.g. "export wasi:io/streams@0.2.0;"),
		// not just a plain syntax error.
		return nil, fmt.Errorf("line %d: export of a bare package path (without 'name:') is not yet supported", p.current.Line)
	}

	return &Export{Name: name, Type: exportType, Gate: gate, ExternalID: externalID}, nil
}

// parseInclude parses "include path;" or "include path with { renames };",
// including any leading gate.
func (p *Parser) parseInclude(gate Gate, externalID string) (WorldItem, error) {
	if !p.expect(TokenInclude) {
		return nil, p.errorf("expected 'include'")
	}
	p.advance()

	path, err := p.parseUsePath()
	if err != nil {
		return nil, err
	}

	renames := make(map[string]string)
	if p.current.Type == TokenWith {
		p.advance()
		if !p.expect(TokenLBrace) {
			return nil, p.errorf("expected '{' after 'with'")
		}
		p.advance()

		for p.current.Type != TokenRBrace && p.current.Type != TokenEOF {
			if !p.isIdentifierToken() {
				return nil, p.errorf("expected identifier in include rename list")
			}
			original := p.current.Text
			p.advance()

			if !p.expect(TokenAs) {
				return nil, p.errorf("expected 'as' in include rename")
			}
			p.advance()

			if !p.isIdentifierToken() {
				return nil, p.errorf("expected alias after 'as'")
			}
			alias := p.current.Text
			p.advance()

			renames[original] = alias

			if p.current.Type == TokenComma {
				p.advance()
			} else if p.current.Type != TokenRBrace {
				return nil, p.errorf("expected ',' or '}' in include rename list")
			}
		}

		if !p.expect(TokenRBrace) {
			return nil, p.errorf("expected '}' to close include rename list")
		}
		p.advance()
	}

	if !p.expect(TokenSemicolon) {
		return nil, p.errorf("expected ';' after include")
	}
	p.advance()

	return &Include{Path: path, Renames: renames, Gate: gate, ExternalID: externalID}, nil
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
		TokenFuture, TokenStream, TokenOwn, TokenBorrow, TokenErrorContext, TokenInclude, TokenWith,
		TokenFeature, TokenVersion_Keyword, TokenUnstable, TokenSince, TokenDeprecated,
		TokenExternalID:
		return true
	default:
		return false
	}
}

func (p *Parser) errorf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	return fmt.Errorf("%s:%d:%d: %s", p.filename, p.current.Line, p.current.Column, msg)
}
