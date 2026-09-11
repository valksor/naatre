package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type nodeKind uint8

const (
	nodeNull nodeKind = iota
	nodeBool
	nodeString
	nodeNumber
	nodeArray
	nodeObject
)

type member struct {
	name  string
	start int
	value node
}

type node struct {
	kind       nodeKind
	start      int
	end        int
	text       string
	array      []node
	object     []member
	memberByID map[string]int
}

func (n node) member(name string) (node, bool) {
	index, ok := n.memberByID[name]
	if !ok {
		return node{}, false
	}
	return n.object[index].value, true
}

type strictParser struct {
	input  []byte
	index  int
	tokens int
	limits Limits
}

var numberPattern = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

func parseJSON(input []byte, limits Limits) (node, error) {
	limits = limits.withDefaults()
	if len(input) > limits.MaxBytes {
		return node{}, newDiagnostic(input, "LIMIT_BYTES", "PROTO-201", "decode", "request exceeds byte limit", "", limits.MaxBytes)
	}
	if !utf8.Valid(input) {
		return node{}, newDiagnostic(input, "INVALID_UTF8", "CANON-001", "decode", "request is not valid UTF-8", "", firstInvalidUTF8(input))
	}
	parser := strictParser{input: input, limits: limits}
	value, err := parser.parseValue("", 1)
	if err != nil {
		return node{}, err
	}
	parser.skipSpace()
	if parser.index != len(input) {
		return node{}, newDiagnostic(input, "TRAILING_DATA", "PROTO-009", "decode", "trailing data after request", "", parser.index)
	}
	return value, nil
}

func (p *strictParser) parseValue(pointer string, depth int) (node, error) {
	p.skipSpace()
	if depth > p.limits.MaxDepth {
		return node{}, newDiagnostic(p.input, "LIMIT_DEPTH", "PROTO-201", "decode", "JSON nesting exceeds limit", pointer, p.index)
	}
	p.tokens++
	if p.tokens > p.limits.MaxTokens {
		return node{}, newDiagnostic(p.input, "LIMIT_TOKENS", "PROTO-201", "decode", "JSON token count exceeds limit", pointer, p.index)
	}
	if p.index >= len(p.input) {
		return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "unexpected end of JSON", pointer, p.index)
	}

	switch p.input[p.index] {
	case '{':
		return p.parseObject(pointer, depth)
	case '[':
		return p.parseArray(pointer, depth)
	case '"':
		start := p.index
		text, err := p.parseString(pointer)
		return node{kind: nodeString, start: start, end: p.index, text: text}, err
	case 't':
		return p.parseKeyword(pointer, "true", nodeBool)
	case 'f':
		return p.parseKeyword(pointer, "false", nodeBool)
	case 'n':
		return p.parseKeyword(pointer, "null", nodeNull)
	default:
		return p.parseNumber(pointer)
	}
}

func (p *strictParser) parseObject(pointer string, depth int) (node, error) {
	start := p.index
	p.index++
	p.skipSpace()
	result := node{kind: nodeObject, start: start, memberByID: make(map[string]int)}
	if p.consume('}') {
		result.end = p.index
		return result, nil
	}
	for {
		if len(result.object) >= p.limits.MaxMembers {
			return node{}, newDiagnostic(p.input, "LIMIT_MEMBERS", "PROTO-201", "decode", "object member count exceeds limit", pointer, p.index)
		}
		keyStart := p.index
		name, err := p.parseString(pointer)
		if err != nil {
			return node{}, err
		}
		memberPointer := joinPointer(pointer, name)
		if _, exists := result.memberByID[name]; exists {
			return node{}, newDiagnostic(p.input, "DUPLICATE_KEY", "PROTO-009", "decode", "duplicate object member", memberPointer, keyStart)
		}
		p.skipSpace()
		if !p.consume(':') {
			return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "expected colon after member name", memberPointer, p.index)
		}
		value, err := p.parseValue(memberPointer, depth+1)
		if err != nil {
			return node{}, err
		}
		result.memberByID[name] = len(result.object)
		result.object = append(result.object, member{name: name, start: keyStart, value: value})
		p.skipSpace()
		if p.consume('}') {
			result.end = p.index
			return result, nil
		}
		if !p.consume(',') {
			return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "expected comma or object end", pointer, p.index)
		}
		p.skipSpace()
	}
}

func (p *strictParser) parseArray(pointer string, depth int) (node, error) {
	start := p.index
	p.index++
	p.skipSpace()
	result := node{kind: nodeArray, start: start}
	if p.consume(']') {
		result.end = p.index
		return result, nil
	}
	for {
		if len(result.array) >= p.limits.MaxArrayItems {
			return node{}, newDiagnostic(p.input, "LIMIT_ARRAY", "PROTO-201", "decode", "array item count exceeds limit", pointer, p.index)
		}
		itemPointer := joinPointer(pointer, strconv.Itoa(len(result.array)))
		value, err := p.parseValue(itemPointer, depth+1)
		if err != nil {
			return node{}, err
		}
		result.array = append(result.array, value)
		p.skipSpace()
		if p.consume(']') {
			result.end = p.index
			return result, nil
		}
		if !p.consume(',') {
			return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "expected comma or array end", pointer, p.index)
		}
		p.skipSpace()
	}
}

func (p *strictParser) parseString(pointer string) (string, error) {
	start := p.index
	if !p.consume('"') {
		return "", newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "expected JSON string", pointer, p.index)
	}
	for p.index < len(p.input) {
		switch p.input[p.index] {
		case '"':
			p.index++
			raw := p.input[start:p.index]
			if !validSurrogateEscapes(raw) {
				return "", newDiagnostic(p.input, "INVALID_UNICODE", "PROTO-009", "decode", "unpaired Unicode surrogate", pointer, start)
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", err.Error(), pointer, start)
			}
			if len(value) > p.limits.MaxStringBytes {
				return "", newDiagnostic(p.input, "LIMIT_STRING", "PROTO-201", "decode", "decoded string exceeds limit", pointer, start)
			}
			return value, nil
		case '\\':
			p.index += 2
		case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31:
			return "", newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "unescaped control character", pointer, p.index)
		default:
			p.index++
		}
	}
	return "", newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "unterminated string", pointer, start)
}

func (p *strictParser) parseKeyword(pointer, keyword string, kind nodeKind) (node, error) {
	start := p.index
	if !strings.HasPrefix(string(p.input[p.index:]), keyword) {
		return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "invalid JSON token", pointer, start)
	}
	p.index += len(keyword)
	return node{kind: kind, start: start, end: p.index, text: keyword}, nil
}

func (p *strictParser) parseNumber(pointer string) (node, error) {
	start := p.index
	for p.index < len(p.input) && strings.ContainsRune("-+0123456789.eE", rune(p.input[p.index])) {
		p.index++
	}
	if p.index == start {
		return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "invalid JSON token", pointer, start)
	}
	raw := p.input[start:p.index]
	if len(raw) > p.limits.MaxNumberBytes {
		return node{}, newDiagnostic(p.input, "LIMIT_NUMBER", "PROTO-201", "decode", "numeric token exceeds limit", pointer, start)
	}
	if !numberPattern.Match(raw) {
		return node{}, newDiagnostic(p.input, "MALFORMED_JSON", "PROTO-009", "decode", "invalid JSON number", pointer, start)
	}
	return node{kind: nodeNumber, start: start, end: p.index, text: string(raw)}, nil
}

func (p *strictParser) skipSpace() {
	for p.index < len(p.input) && strings.ContainsRune(" \t\r\n", rune(p.input[p.index])) {
		p.index++
	}
}

func (p *strictParser) consume(expected byte) bool {
	if p.index >= len(p.input) || p.input[p.index] != expected {
		return false
	}
	p.index++
	return true
}

func validSurrogateEscapes(raw []byte) bool {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '\\' {
			continue
		}
		index++
		if index >= len(raw) {
			return false
		}
		if raw[index] != 'u' {
			continue
		}
		first, ok := hexCodeUnit(raw, index+1)
		if !ok {
			return false
		}
		index += 4
		if first >= 0xdc00 && first <= 0xdfff {
			return false
		}
		if first < 0xd800 || first > 0xdbff {
			continue
		}
		if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
			return false
		}
		second, ok := hexCodeUnit(raw, index+3)
		if !ok || second < 0xdc00 || second > 0xdfff {
			return false
		}
		index += 6
	}
	return true
}

func hexCodeUnit(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(raw[start:start+4]), 16, 16)
	return uint16(value), err == nil
}

func firstInvalidUTF8(input []byte) int {
	for index := 0; index < len(input); {
		_, size := utf8.DecodeRune(input[index:])
		if size == 1 && input[index] >= utf8.RuneSelf {
			return index
		}
		index += size
	}
	return 0
}

func newDiagnostic(input []byte, code, clause, phase, message, pointer string, offset int) *Diagnostic {
	if offset < 0 {
		offset = 0
	}
	if offset > len(input) {
		offset = len(input)
	}
	line, column := 1, 1
	for _, current := range input[:offset] {
		if current == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return &Diagnostic{Code: code, Clause: clause, Phase: phase, Message: message, Pointer: pointer, Offset: offset, Line: line, Column: column}
}

func joinPointer(base, component string) string {
	component = strings.ReplaceAll(component, "~", "~0")
	component = strings.ReplaceAll(component, "/", "~1")
	return base + "/" + component
}

func expectKind(input []byte, value node, kind nodeKind, pointer, description string) error {
	if value.kind != kind {
		return newDiagnostic(input, "TYPE_MISMATCH", "PROTO-001", "decode", fmt.Sprintf("expected %s", description), pointer, value.start)
	}
	return nil
}
