package main

import (
	"fmt"
	"strconv"
	"strings"
)

// This file implements a parser for the subset of TypeScript emitted by
// json-schema-to-typescript into sdk-tools.d.ts: exported interfaces and type
// aliases built from primitives, literals, object literals (with optional
// index signatures), arrays, tuples (with a rest element), unions and
// parenthesised types, annotated with JSDoc comments.

// Kind classifies a parsed TypeScript type.
type Kind int

const (
	KString Kind = iota
	KNumber
	KBool
	KUnknown
	KNull
	KLiteral // Lit holds string, bool or float64
	KObject
	KArray
	KTuple
	KUnion
	KRef
)

// Type is a parsed TypeScript type expression.
type Type struct {
	Kind    Kind
	Lit     any
	Props   []*Prop // KObject
	Index   *Type   // KObject: value type of a [k: string] index signature
	Elem    *Type   // KArray
	Elems   []*Type // KTuple: fixed elements
	Rest    *Type   // KTuple: element type of a trailing ...T[]
	Members []*Type // KUnion
	Ref     string  // KRef
}

// Prop is an object property.
type Prop struct {
	Name     string
	Optional bool
	Type     *Type
	Doc      string
}

// Decl is a top-level exported declaration.
type Decl struct {
	Name string
	Doc  string
	Type *Type
}

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tString
	tNumber
	tPunct
)

type token struct {
	kind tokKind
	text string // identifier/punct text, or the decoded string literal
	doc  string // JSDoc comment immediately preceding the token, if any
	pos  int
}

func lex(src string) ([]token, error) {
	var toks []token
	var doc string
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case strings.HasPrefix(src[i:], "/**"):
			end := strings.Index(src[i+3:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at %d", i)
			}
			doc = cleanDoc(src[i+3 : i+3+end])
			i += 3 + end + 2
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated comment at %d", i)
			}
			i += 2 + end + 2
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				end = len(src) - i
			}
			i += end
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string at %d", i)
			}
			raw := src[i : j+1]
			if c == '\'' {
				raw = `"` + strings.ReplaceAll(raw[1:len(raw)-1], `"`, `\"`) + `"`
			}
			s, err := strconv.Unquote(raw)
			if err != nil {
				return nil, fmt.Errorf("bad string literal at %d: %v", i, err)
			}
			toks = append(toks, token{kind: tString, text: s, doc: doc, pos: i})
			doc = ""
			i = j + 1
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			toks = append(toks, token{kind: tIdent, text: src[i:j], doc: doc, pos: i})
			doc = ""
			i = j
		case c >= '0' && c <= '9' || c == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E') {
				j++
			}
			toks = append(toks, token{kind: tNumber, text: src[i:j], doc: doc, pos: i})
			doc = ""
			i = j
		case strings.HasPrefix(src[i:], "..."):
			toks = append(toks, token{kind: tPunct, text: "...", doc: doc, pos: i})
			doc = ""
			i += 3
		case strings.ContainsRune("{}[]()|&;:?,=<>", rune(c)):
			toks = append(toks, token{kind: tPunct, text: string(c), doc: doc, pos: i})
			doc = ""
			i++
		default:
			return nil, fmt.Errorf("unexpected character %q at %d", c, i)
		}
	}
	toks = append(toks, token{kind: tEOF, pos: len(src)})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// cleanDoc strips the leading " * " decoration of a JSDoc body.
func cleanDoc(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		l = strings.TrimPrefix(l, "*")
		if strings.HasPrefix(l, " ") {
			l = l[1:]
		}
		out = append(out, strings.TrimRight(l, " \t"))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

type parser struct {
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }

func (p *parser) is(text string) bool {
	t := p.peek()
	return (t.kind == tPunct || t.kind == tIdent) && t.text == text
}

func (p *parser) expect(text string) (token, error) {
	t := p.next()
	if (t.kind != tPunct && t.kind != tIdent) || t.text != text {
		return t, fmt.Errorf("at offset %d: expected %q, got %q", t.pos, text, t.text)
	}
	return t, nil
}

// Parse parses sdk-tools.d.ts into its exported declarations, in file order.
func Parse(src string) ([]*Decl, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	var decls []*Decl
	for p.peek().kind != tEOF {
		exp, err := p.expect("export")
		if err != nil {
			return nil, err
		}
		kw := p.next()
		name := p.next()
		if name.kind != tIdent {
			return nil, fmt.Errorf("at offset %d: expected declaration name", name.pos)
		}
		d := &Decl{Name: name.text, Doc: exp.doc}
		switch kw.text {
		case "interface":
			t, err := p.parseObject()
			if err != nil {
				return nil, err
			}
			d.Type = t
		case "type":
			if _, err := p.expect("="); err != nil {
				return nil, err
			}
			t, err := p.parseType()
			if err != nil {
				return nil, err
			}
			if _, err := p.expect(";"); err != nil {
				return nil, err
			}
			d.Type = t
		default:
			return nil, fmt.Errorf("at offset %d: unsupported declaration %q", kw.pos, kw.text)
		}
		decls = append(decls, d)
	}
	return decls, nil
}

func (p *parser) parseType() (*Type, error) {
	if p.is("|") { // leading pipe of a multi-line union
		p.next()
	}
	first, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	if !p.is("|") {
		return first, nil
	}
	u := &Type{Kind: KUnion, Members: []*Type{first}}
	for p.is("|") {
		p.next()
		m, err := p.parsePostfix()
		if err != nil {
			return nil, err
		}
		u.Members = append(u.Members, m)
	}
	return u, nil
}

func (p *parser) parsePostfix() (*Type, error) {
	t, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for p.is("[") && p.toks[p.i+1].text == "]" {
		p.next()
		p.next()
		t = &Type{Kind: KArray, Elem: t}
	}
	return t, nil
}

func (p *parser) parsePrimary() (*Type, error) {
	t := p.peek()
	switch {
	case t.kind == tString:
		p.next()
		return &Type{Kind: KLiteral, Lit: t.text}, nil
	case t.kind == tNumber:
		p.next()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return &Type{Kind: KLiteral, Lit: f}, nil
	case t.kind == tPunct && t.text == "{":
		return p.parseObject()
	case t.kind == tPunct && t.text == "[":
		return p.parseTuple()
	case t.kind == tPunct && t.text == "(":
		p.next()
		inner, err := p.parseType()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(")"); err != nil {
			return nil, err
		}
		return inner, nil
	case t.kind == tIdent:
		p.next()
		switch t.text {
		case "string":
			return &Type{Kind: KString}, nil
		case "number":
			return &Type{Kind: KNumber}, nil
		case "boolean":
			return &Type{Kind: KBool}, nil
		case "unknown", "any":
			return &Type{Kind: KUnknown}, nil
		case "null", "undefined":
			return &Type{Kind: KNull}, nil
		case "true":
			return &Type{Kind: KLiteral, Lit: true}, nil
		case "false":
			return &Type{Kind: KLiteral, Lit: false}, nil
		}
		return &Type{Kind: KRef, Ref: t.text}, nil
	}
	return nil, fmt.Errorf("at offset %d: unexpected token %q", t.pos, t.text)
}

func (p *parser) parseTuple() (*Type, error) {
	if _, err := p.expect("["); err != nil {
		return nil, err
	}
	tu := &Type{Kind: KTuple}
	for !p.is("]") {
		if p.is("...") {
			p.next()
			rest, err := p.parseType()
			if err != nil {
				return nil, err
			}
			if rest.Kind != KArray {
				return nil, fmt.Errorf("tuple rest element must be an array")
			}
			tu.Rest = rest.Elem
		} else {
			e, err := p.parseType()
			if err != nil {
				return nil, err
			}
			tu.Elems = append(tu.Elems, e)
		}
		if p.is(",") {
			p.next()
		}
	}
	p.next()
	return tu, nil
}

func (p *parser) parseObject() (*Type, error) {
	if _, err := p.expect("{"); err != nil {
		return nil, err
	}
	obj := &Type{Kind: KObject}
	for !p.is("}") {
		start := p.peek()
		if start.kind == tPunct && start.text == "[" { // index signature
			p.next()
			p.next() // key name
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			if _, err := p.expect("string"); err != nil {
				return nil, err
			}
			if _, err := p.expect("]"); err != nil {
				return nil, err
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseType()
			if err != nil {
				return nil, err
			}
			obj.Index = v
		} else {
			name := p.next()
			if name.kind != tIdent && name.kind != tString {
				return nil, fmt.Errorf("at offset %d: expected property name, got %q", name.pos, name.text)
			}
			prop := &Prop{Name: name.text, Doc: name.doc}
			if p.is("?") {
				p.next()
				prop.Optional = true
			}
			if _, err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.parseType()
			if err != nil {
				return nil, err
			}
			prop.Type = v
			obj.Props = append(obj.Props, prop)
		}
		if p.is(";") || p.is(",") {
			p.next()
		}
	}
	p.next()
	return obj, nil
}
