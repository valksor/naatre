package lsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type document struct {
	uri            string
	text           string
	version        int
	schemaRevision string
}

type contentChange struct {
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

func applyChanges(text string, changes []contentChange, maximumBytes int) (string, error) {
	for _, change := range changes {
		if change.Range == nil {
			text = change.Text
		} else {
			start, err := byteOffset(text, change.Range.Start)
			if err != nil {
				return "", err
			}
			end, err := byteOffset(text, change.Range.End)
			if err != nil || end < start {
				return "", errors.New("invalid incremental edit range")
			}
			text = text[:start] + change.Text + text[end:]
		}
		if len(text) > maximumBytes {
			return "", errors.New("document exceeds configured byte limit")
		}
	}
	return text, nil
}

func byteOffset(text string, position Position) (int, error) {
	if position.Line < 0 || position.Character < 0 {
		return 0, errors.New("negative position")
	}
	line, offset := 0, 0
	for line < position.Line {
		index := strings.IndexByte(text[offset:], '\n')
		if index < 0 {
			return 0, errors.New("line is outside document")
		}
		offset += index + 1
		line++
	}
	units := 0
	for offset < len(text) && text[offset] != '\n' {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size == 1 {
			return 0, errors.New("document is not valid UTF-8")
		}
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units == position.Character {
			return offset, nil
		}
		if units+width > position.Character {
			return 0, errors.New("position splits a UTF-16 surrogate pair")
		}
		units += width
		offset += size
	}
	if units == position.Character {
		return offset, nil
	}
	return 0, errors.New("character is outside line")
}

func positionAt(text string, offset int) Position {
	if offset < 0 {
		offset = 0
	}
	if offset > len(text) {
		offset = len(text)
	}
	lineStart := strings.LastIndexByte(text[:offset], '\n') + 1
	return Position{
		Line:      strings.Count(text[:lineStart], "\n"),
		Character: len(utf16.Encode([]rune(text[lineStart:offset]))),
	}
}

func wordRange(text string, position Position) (Range, string, error) {
	offset, err := byteOffset(text, position)
	if err != nil {
		return Range{}, "", err
	}
	start, end := offset, offset
	for start > 0 && identifierByte(text[start-1]) {
		start--
	}
	for end < len(text) && identifierByte(text[end]) {
		end++
	}
	return Range{Start: positionAt(text, start), End: positionAt(text, end)}, text[start:end], nil
}

func identifierByte(value byte) bool {
	return value == '_' || value == '.' || value == '-' || value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

type sourceIndex struct {
	text   string
	ranges map[string]Range
}

func indexJSON(text string) *sourceIndex {
	index := &sourceIndex{text: text, ranges: make(map[string]Range)}
	var value any
	if json.Unmarshal([]byte(text), &value) != nil {
		return index
	}
	position := 0
	if index.scanValue(&position, "") != nil {
		index.ranges = make(map[string]Range)
	}
	return index
}

func (index *sourceIndex) scanValue(position *int, pointer string) error {
	index.skipSpace(position)
	start := *position
	if start >= len(index.text) {
		return errors.New("missing JSON value")
	}
	switch index.text[start] {
	case '{':
		if err := index.scanObject(position, pointer); err != nil {
			return err
		}
	case '[':
		if err := index.scanArray(position, pointer); err != nil {
			return err
		}
	case '"':
		_, contentStart, contentEnd, err := index.scanString(position)
		if err != nil {
			return err
		}
		index.ranges[pointer] = Range{Start: positionAt(index.text, contentStart), End: positionAt(index.text, contentEnd)}
		return nil
	default:
		for *position < len(index.text) && !strings.ContainsRune(" \t\r\n,]}", rune(index.text[*position])) {
			(*position)++
		}
	}
	index.ranges[pointer] = Range{Start: positionAt(index.text, start), End: positionAt(index.text, *position)}
	return nil
}

func (index *sourceIndex) scanObject(position *int, pointer string) error {
	(*position)++
	index.skipSpace(position)
	for *position < len(index.text) && index.text[*position] != '}' {
		key, _, _, err := index.scanString(position)
		if err != nil {
			return err
		}
		index.skipSpace(position)
		if *position >= len(index.text) || index.text[*position] != ':' {
			return errors.New("missing object colon")
		}
		(*position)++
		if err := index.scanValue(position, pointer+"/"+escapePointer(key)); err != nil {
			return err
		}
		if !index.consumeSeparator(position, '}') {
			break
		}
	}
	return index.consumeEnd(position, '}', "missing object end")
}

func (index *sourceIndex) scanArray(position *int, pointer string) error {
	(*position)++
	index.skipSpace(position)
	for item := 0; *position < len(index.text) && index.text[*position] != ']'; item++ {
		if err := index.scanValue(position, fmt.Sprintf("%s/%d", pointer, item)); err != nil {
			return err
		}
		if !index.consumeSeparator(position, ']') {
			break
		}
	}
	return index.consumeEnd(position, ']', "missing array end")
}

func (index *sourceIndex) consumeSeparator(position *int, end byte) bool {
	index.skipSpace(position)
	if *position >= len(index.text) || index.text[*position] == end || index.text[*position] != ',' {
		return false
	}
	(*position)++
	index.skipSpace(position)
	return true
}

func (index *sourceIndex) consumeEnd(position *int, end byte, message string) error {
	if *position >= len(index.text) || index.text[*position] != end {
		return errors.New(message)
	}
	(*position)++
	return nil
}

func (index *sourceIndex) scanString(position *int) (string, int, int, error) {
	start := *position
	if start >= len(index.text) || index.text[start] != '"' {
		return "", 0, 0, errors.New("expected JSON string")
	}
	(*position)++
	contentStart := *position
	for *position < len(index.text) {
		if index.text[*position] == '\\' {
			*position += 2
			continue
		}
		if index.text[*position] == '"' {
			contentEnd := *position
			(*position)++
			decoded, err := strconv.Unquote(index.text[start:*position])
			return decoded, contentStart, contentEnd, err
		}
		(*position)++
	}
	return "", 0, 0, errors.New("unterminated JSON string")
}

func (index *sourceIndex) skipSpace(position *int) {
	for *position < len(index.text) && strings.ContainsRune(" \t\r\n", rune(index.text[*position])) {
		(*position)++
	}
}

func escapePointer(value string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(value)
}
