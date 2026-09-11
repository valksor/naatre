package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
)

// HashPurpose domain-separates semantic identities with different security
// and caching meanings.
type HashPurpose string

const (
	DocumentHash      HashPurpose = "document"
	SchemaHash        HashPurpose = "schema"
	ApprovalHash      HashPurpose = "approval"
	ResultCacheHash   HashPurpose = "result-cache"
	IdempotencyHash   HashPurpose = "idempotency"
	SignedMessageHash HashPurpose = "signed-message"
)

// Digest records both the algorithm and canonicalization revision needed to
// verify a semantic hash.
type Digest struct {
	Algorithm        string `json:"algorithm"`
	CanonicalVersion string `json:"canonicalVersion"`
	Hex              string `json:"digest"`
}

// CanonicalizeJSON validates JSON and returns c14n-1 bytes. Objects use RFC
// 8785 UTF-16 key order while arrays remain ordered.
func CanonicalizeJSON(input []byte, limits Limits) ([]byte, error) {
	root, err := parseJSON(input, limits)
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonical(&output, root); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// SemanticHash hashes an already canonical payload in a purpose-specific
// domain. Callers remain responsible for selecting the purpose payload defined
// by the corresponding profile.
func SemanticHash(purpose HashPurpose, canonicalPayload []byte) (Digest, error) {
	if !validHashPurpose(purpose) {
		return Digest{}, fmt.Errorf("unknown semantic hash purpose %q", purpose)
	}
	prefix := []byte("naatre:" + string(purpose) + ":c14n-1\n")
	hash := sha256.New()
	if _, err := hash.Write(prefix); err != nil {
		return Digest{}, fmt.Errorf("hash domain prefix: %w", err)
	}
	if _, err := hash.Write(canonicalPayload); err != nil {
		return Digest{}, fmt.Errorf("hash canonical payload: %w", err)
	}
	return Digest{Algorithm: "sha-256", CanonicalVersion: "c14n-1", Hex: hex.EncodeToString(hash.Sum(nil))}, nil
}

func validHashPurpose(purpose HashPurpose) bool {
	switch purpose {
	case DocumentHash, SchemaHash, ApprovalHash, ResultCacheHash, IdempotencyHash, SignedMessageHash:
		return true
	default:
		return false
	}
}

func writeCanonical(output *bytes.Buffer, value node) error {
	switch value.kind {
	case nodeNull:
		output.WriteString("null")
	case nodeBool:
		output.WriteString(value.text)
	case nodeString:
		writeCanonicalString(output, value.text)
	case nodeNumber:
		number, err := canonicalJSONNumber(value.text)
		if err != nil {
			return err
		}
		output.WriteString(number)
	case nodeArray:
		output.WriteByte('[')
		for index, item := range value.array {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonical(output, item); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case nodeObject:
		members := append([]member(nil), value.object...)
		sort.Slice(members, func(left, right int) bool {
			return lessUTF16(members[left].name, members[right].name)
		})
		output.WriteByte('{')
		for index, current := range members {
			if index > 0 {
				output.WriteByte(',')
			}
			writeCanonicalString(output, current.name)
			output.WriteByte(':')
			if err := writeCanonical(output, current.value); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("canonicalize unknown JSON node kind %d", value.kind)
	}
	return nil
}

func lessUTF16(left, right string) bool {
	leftUnits := utf16.Encode([]rune(left))
	rightUnits := utf16.Encode([]rune(right))
	limit := min(len(leftUnits), len(rightUnits))
	for index := 0; index < limit; index++ {
		if leftUnits[index] != rightUnits[index] {
			return leftUnits[index] < rightUnits[index]
		}
	}
	return len(leftUnits) < len(rightUnits)
}

func writeCanonicalString(output *bytes.Buffer, value string) {
	const hexadecimal = "0123456789abcdef"
	output.WriteByte('"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(current)
		case '\b':
			output.WriteString(`\b`)
		case '\t':
			output.WriteString(`\t`)
		case '\n':
			output.WriteString(`\n`)
		case '\f':
			output.WriteString(`\f`)
		case '\r':
			output.WriteString(`\r`)
		default:
			if current >= 0 && current <= 0x1f {
				output.WriteString(`\u00`)
				output.WriteByte(hexadecimal[byte(current)>>4])
				output.WriteByte(hexadecimal[byte(current)&0x0f])
			} else {
				output.WriteRune(current)
			}
		}
	}
	output.WriteByte('"')
}

func canonicalJSONNumber(raw string) (string, error) {
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "", fmt.Errorf("canonical JSON number %q is not finite binary64", raw)
	}
	if value == 0 {
		return "0", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode canonical JSON number: %w", err)
	}
	return string(encoded), nil
}
