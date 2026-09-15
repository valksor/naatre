package graphqladapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenName
	tokenString
	tokenNumber
	tokenPunct
)

type token struct {
	kind  tokenKind
	value string
	start int
}

type lexer struct {
	ctx    context.Context
	source []byte
	limits Limits
	offset int
	tokens int
	peeked *token
}

func newLexer(ctx context.Context, source []byte, limits Limits) (*lexer, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(source) == 0 {
		return nil, adapterError(CodeDocumentInvalid, errors.New("empty GraphQL document"))
	}
	if len(source) > limits.MaxDocumentBytes || !utf8.Valid(source) {
		return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL document byte limit or UTF-8 contract violated"))
	}
	return &lexer{ctx: ctx, source: source, limits: limits}, nil
}

func (l *lexer) peek() (token, error) {
	if l.peeked != nil {
		return *l.peeked, nil
	}
	value, err := l.nextRaw()
	if err != nil {
		return token{}, err
	}
	l.peeked = &value
	return value, nil
}

func (l *lexer) next() (token, error) {
	if l.peeked != nil {
		value := *l.peeked
		l.peeked = nil
		return value, nil
	}
	return l.nextRaw()
}

func (l *lexer) nextRaw() (token, error) {
	if err := l.ctx.Err(); err != nil {
		return token{}, adapterError(CodeCancelled, err)
	}
	for l.offset < len(l.source) {
		current := l.source[l.offset]
		if current == '#' {
			for l.offset < len(l.source) && l.source[l.offset] != '\n' && l.source[l.offset] != '\r' {
				l.offset++
			}
			continue
		}
		if current == ',' || current == ' ' || current == '\t' || current == '\n' || current == '\r' {
			l.offset++
			continue
		}
		break
	}
	if l.offset >= len(l.source) {
		return token{kind: tokenEOF, start: l.offset}, nil
	}
	l.tokens++
	if l.tokens > l.limits.MaxTokens {
		return token{}, adapterError(CodeResourceExhausted, errors.New("GraphQL token limit exceeded"))
	}
	start := l.offset
	current := l.source[l.offset]
	if nameStart(current) {
		l.offset++
		for l.offset < len(l.source) && nameContinue(l.source[l.offset]) {
			l.offset++
		}
		return token{kind: tokenName, value: string(l.source[start:l.offset]), start: start}, nil
	}
	if current == '"' {
		return l.readString(start)
	}
	if current == '-' || current >= '0' && current <= '9' {
		return l.readNumber(start)
	}
	if strings.ContainsRune("!$&():=@[]{|}", rune(current)) {
		l.offset++
		return token{kind: tokenPunct, value: string(current), start: start}, nil
	}
	if current == '.' && l.offset+2 < len(l.source) && string(l.source[l.offset:l.offset+3]) == "..." {
		l.offset += 3
		return token{kind: tokenPunct, value: "...", start: start}, nil
	}
	return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL token"))
}

func (l *lexer) readString(start int) (token, error) {
	if l.offset+2 < len(l.source) && string(l.source[l.offset:l.offset+3]) == `"""` {
		return token{}, adapterError(CodeUnsupported, errors.New("GraphQL block strings are unsupported"))
	}
	l.offset++
	for l.offset < len(l.source) {
		if l.source[l.offset] == '\n' || l.source[l.offset] == '\r' {
			return token{}, adapterError(CodeDocumentInvalid, errors.New("newline in GraphQL string"))
		}
		if l.source[l.offset] == '\\' {
			l.offset += 2
			continue
		}
		if l.source[l.offset] == '"' {
			l.offset++
			encoded := l.source[start:l.offset]
			var value string
			err := json.Unmarshal(encoded, &value)
			if err != nil {
				return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL string"))
			}
			return token{kind: tokenString, value: value, start: start}, nil
		}
		l.offset++
	}
	return token{}, adapterError(CodeDocumentInvalid, errors.New("unterminated GraphQL string"))
}

func (l *lexer) readNumber(start int) (token, error) {
	if l.source[l.offset] == '-' {
		l.offset++
	}
	if l.offset >= len(l.source) {
		return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL number"))
	}
	if l.source[l.offset] == '0' {
		l.offset++
		if l.offset < len(l.source) && isDigit(l.source[l.offset]) {
			return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL number"))
		}
	} else if l.source[l.offset] >= '1' && l.source[l.offset] <= '9' {
		for l.offset < len(l.source) && isDigit(l.source[l.offset]) {
			l.offset++
		}
	} else {
		return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL number"))
	}
	if l.offset < len(l.source) && l.source[l.offset] == '.' {
		l.offset++
		if l.offset >= len(l.source) || !isDigit(l.source[l.offset]) {
			return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL number"))
		}
		for l.offset < len(l.source) && isDigit(l.source[l.offset]) {
			l.offset++
		}
	}
	if l.offset < len(l.source) && (l.source[l.offset] == 'e' || l.source[l.offset] == 'E') {
		l.offset++
		if l.offset < len(l.source) && (l.source[l.offset] == '+' || l.source[l.offset] == '-') {
			l.offset++
		}
		if l.offset >= len(l.source) || !isDigit(l.source[l.offset]) {
			return token{}, adapterError(CodeDocumentInvalid, errors.New("invalid GraphQL number"))
		}
		for l.offset < len(l.source) && isDigit(l.source[l.offset]) {
			l.offset++
		}
	}
	value := string(l.source[start:l.offset])
	return token{kind: tokenNumber, value: value, start: start}, nil
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

func (l *lexer) accept(value string) (bool, error) {
	next, err := l.peek()
	if err != nil || next.value != value {
		return false, err
	}
	_, err = l.next()
	return true, err
}

func (l *lexer) require(value string) error {
	next, err := l.next()
	if err != nil {
		return err
	}
	if next.value != value {
		return adapterError(CodeDocumentInvalid, errors.New("required GraphQL token is absent"))
	}
	return nil
}

func (l *lexer) requireName() (string, error) {
	next, err := l.next()
	if err != nil {
		return "", err
	}
	if next.kind != tokenName {
		return "", adapterError(CodeDocumentInvalid, errors.New("GraphQL name is required"))
	}
	return next.value, nil
}

func nameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func nameContinue(value byte) bool {
	return nameStart(value) || value >= '0' && value <= '9'
}

func validGraphQLName(value string) bool {
	if value == "" || !nameStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !nameContinue(value[index]) {
			return false
		}
	}
	return true
}

func validUserGraphQLName(value string) bool {
	return validGraphQLName(value) && !strings.HasPrefix(value, "__")
}
