package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	FederationHash    HashPurpose = "federation"
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
	return canonicalizeJSON(input, limits, canonicalGeneric)
}

// CanonicalizeDocument returns the canonical AST bytes used by the document
// hash profile. Callers validate the closed language grammar first; this step
// additionally normalizes the document-level requires set without changing any
// ordered language array.
func CanonicalizeDocument(input []byte, limits Limits) ([]byte, error) {
	return canonicalizeJSON(input, limits, canonicalDocument)
}

// CanonicalizeSchema returns canonical public schema bytes with registration-
// order-independent type and set-like descriptor arrays.
func CanonicalizeSchema(input []byte, limits Limits) ([]byte, error) {
	return canonicalizeJSON(input, limits, canonicalSchema)
}

// CanonicalizeHashPayload applies the finite normalization profile owned by a
// semantic hash purpose before returning c14n-1 bytes.
func CanonicalizeHashPayload(purpose HashPurpose, input []byte, limits Limits) ([]byte, error) {
	switch purpose {
	case DocumentHash:
		return CanonicalizeDocument(input, limits)
	case SchemaHash:
		return CanonicalizeSchema(input, limits)
	case ApprovalHash, ResultCacheHash:
		root, err := parseJSON(input, limits)
		if err != nil {
			return nil, err
		}
		if root.kind != nodeObject {
			return nil, purposePayloadDiagnostic(input, purpose, "hash payload must be an object", "", root.start)
		}
		if err := normalizePurposeCapabilities(input, purpose, &root); err != nil {
			return nil, err
		}
		return canonicalizeNode(root)
	case IdempotencyHash, SignedMessageHash, FederationHash:
		return CanonicalizeJSON(input, limits)
	default:
		return nil, fmt.Errorf("unknown semantic hash purpose %q", purpose)
	}
}

func normalizePurposeCapabilities(input []byte, purpose HashPurpose, root *node) error {
	index, exists := root.memberByID["capabilities"]
	if !exists {
		return nil
	}
	capabilities := &root.object[index].value
	if capabilities.kind != nodeArray {
		return purposePayloadDiagnostic(input, purpose, "capabilities must be an array", "/capabilities", capabilities.start)
	}
	seen := make(map[string]bool, len(capabilities.array))
	for itemIndex, item := range capabilities.array {
		pointer := fmt.Sprintf("/capabilities/%d", itemIndex)
		if item.kind != nodeString || !capabilityPattern(item.text) || seen[item.text] {
			return purposePayloadDiagnostic(input, purpose, "capabilities entries must be unique portable identifiers", pointer, item.start)
		}
		seen[item.text] = true
	}
	sort.Slice(capabilities.array, func(left, right int) bool {
		return capabilities.array[left].text < capabilities.array[right].text
	})
	return nil
}

func purposePayloadDiagnostic(input []byte, purpose HashPurpose, message, pointer string, offset int) error {
	clause := "CANON-221"
	if purpose == ResultCacheHash {
		clause = "CANON-222"
	}
	return newDiagnostic(input, "INVALID_HASH_PAYLOAD", clause, "validate", message, pointer, offset)
}

type canonicalProfile uint8

const (
	canonicalGeneric canonicalProfile = iota
	canonicalDocument
	canonicalSchema
)

func canonicalizeJSON(input []byte, limits Limits, profile canonicalProfile) ([]byte, error) {
	root, err := parseJSON(input, limits)
	if err != nil {
		return nil, err
	}
	switch profile {
	case canonicalGeneric:
	case canonicalDocument:
		if err := normalizeDocument(input, &root); err != nil {
			return nil, err
		}
	case canonicalSchema:
		if err := normalizeSchema(input, &root); err != nil {
			return nil, err
		}
	}
	return canonicalizeNode(root)
}

func canonicalizeNode(root node) ([]byte, error) {
	var output bytes.Buffer
	if err := writeCanonical(&output, root); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func normalizeDocument(input []byte, root *node) error {
	if root.kind != nodeObject {
		return newDiagnostic(input, "INVALID_DOCUMENT", "CANON-100", "validate", "document must be an object", "", root.start)
	}
	index, exists := root.memberByID["requires"]
	if !exists {
		return nil
	}
	requires := &root.object[index].value
	if requires.kind != nodeArray {
		return newDiagnostic(input, "INVALID_DOCUMENT", "CANON-100", "validate", "requires must be an array", "/requires", requires.start)
	}
	seen := make(map[string]bool, len(requires.array))
	for itemIndex, item := range requires.array {
		pointer := fmt.Sprintf("/requires/%d", itemIndex)
		if item.kind != nodeString || !capabilityPattern(item.text) {
			return newDiagnostic(input, "INVALID_DOCUMENT", "CANON-100", "validate", "requires entries must be portable profile identifiers", pointer, item.start)
		}
		if seen[item.text] {
			return newDiagnostic(input, "DUPLICATE_CAPABILITY", "CANON-100", "validate", "requires entries must be unique", pointer, item.start)
		}
		seen[item.text] = true
	}
	sort.Slice(requires.array, func(left, right int) bool {
		return requires.array[left].text < requires.array[right].text
	})
	return nil
}

func normalizeSchema(input []byte, root *node) error {
	if root.kind != nodeObject {
		return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "schema must be an object", "", root.start)
	}
	revision, hasRevision := root.member("revision")
	if !hasRevision || revision.kind != nodeString || !capabilityPattern(revision.text) {
		return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "schema requires a portable revision", "/revision", revision.start)
	}
	if err := normalizeStringSetMember(input, root, "capabilities", "/capabilities", true); err != nil {
		return err
	}
	if err := normalizeSchemaTypes(input, root); err != nil {
		return err
	}
	if err := normalizeSchemaCallables(input, root); err != nil {
		return err
	}
	if err := normalizeSchemaDirectives(input, root); err != nil {
		return err
	}
	for _, member := range []string{"retired", "traits"} {
		if _, err := normalizeSchemaIDArrayMember(input, root, member, "/"+member); err != nil {
			return err
		}
	}
	return normalizeSchemaReferences(input, root)
}

func normalizeSchemaTypes(input []byte, root *node) error {
	typesIndex, exists := root.memberByID["types"]
	if !exists || root.object[typesIndex].value.kind != nodeArray {
		return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "schema requires a types array", "/types", root.start)
	}
	types := &root.object[typesIndex].value
	identifiers := make(map[string]bool, len(types.array))
	for typeIndex := range types.array {
		current := &types.array[typeIndex]
		pointer := fmt.Sprintf("/types/%d", typeIndex)
		identifier, ok := schemaTypeIdentifier(current)
		if !ok || identifiers[identifier] {
			return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "types require unique portable identifiers", pointer+"/id", current.start)
		}
		identifiers[identifier] = true
		if err := normalizeSchemaType(input, current, pointer); err != nil {
			return err
		}
	}
	sort.Slice(types.array, func(left, right int) bool {
		leftID, _ := schemaTypeIdentifier(&types.array[left])
		rightID, _ := schemaTypeIdentifier(&types.array[right])
		return leftID < rightID
	})
	return nil
}

func normalizeSchemaCallables(input []byte, root *node) error {
	for _, member := range []string{"operations", "members"} {
		values, err := normalizeSchemaIDArrayMember(input, root, member, "/"+member)
		if err != nil {
			return err
		}
		if values == nil {
			continue
		}
		for index := range values.array {
			if err := normalizeSchemaCallable(input, &values.array[index], fmt.Sprintf("/%s/%d", member, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeSchemaDirectives(input []byte, root *node) error {
	values, err := normalizeSchemaIDArrayMember(input, root, "directives", "/directives")
	if err != nil || values == nil {
		return err
	}
	for index := range values.array {
		if err := normalizeSchemaDirective(input, &values.array[index], index); err != nil {
			return err
		}
	}
	return nil
}

func normalizeSchemaDirective(input []byte, directive *node, index int) error {
	pointer := fmt.Sprintf("/directives/%d", index)
	for _, member := range []string{"locations", "phases", "capabilities"} {
		if err := normalizeStringSetMember(input, directive, member, pointer+"/"+member, true); err != nil {
			return err
		}
	}
	if _, err := normalizeSchemaIDArrayMember(input, directive, "arguments", pointer+"/arguments"); err != nil {
		return err
	}
	_, err := normalizeSchemaIDArrayMember(input, directive, "traits", pointer+"/traits")
	return err
}

func normalizeSchemaType(input []byte, current *node, pointer string) error {
	if err := normalizeSchemaTypeCollections(input, current, pointer); err != nil {
		return err
	}
	if err := normalizeSchemaObjectDescriptor(input, current, pointer, "entity", func(value *node) error {
		return normalizeStringSetMember(input, value, "keys", pointer+"/entity/keys", false)
	}); err != nil {
		return err
	}
	return normalizeSchemaObjectDescriptor(input, current, pointer, "scalar", func(value *node) error {
		return normalizeStringSetMember(input, value, "acceptedWireShapes", pointer+"/scalar/acceptedWireShapes", true)
	})
}

func normalizeSchemaTypeCollections(input []byte, current *node, pointer string) error {
	for _, member := range []string{"variants", "enumValues", "capabilities"} {
		if err := normalizeStringSetMember(input, current, member, pointer+"/"+member, true); err != nil {
			return err
		}
	}
	for _, member := range []string{"fields", "enumMembers", "variantMembers"} {
		values, err := normalizeSchemaIDArrayMember(input, current, member, pointer+"/"+member)
		if err != nil {
			return err
		}
		if values == nil {
			continue
		}
		for index := range values.array {
			itemPointer := fmt.Sprintf("%s/%s/%d", pointer, member, index)
			if _, err := normalizeSchemaIDArrayMember(input, &values.array[index], "traits", itemPointer+"/traits"); err != nil {
				return err
			}
		}
	}
	for _, member := range []string{"retired", "traits"} {
		if _, err := normalizeSchemaIDArrayMember(input, current, member, pointer+"/"+member); err != nil {
			return err
		}
	}
	return nil
}

func normalizeSchemaObjectDescriptor(input []byte, parent *node, pointer, name string, normalize func(*node) error) error {
	index, exists := parent.memberByID[name]
	if !exists {
		return nil
	}
	value := &parent.object[index].value
	if value.kind != nodeObject {
		return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", name+" descriptor must be an object", pointer+"/"+name, value.start)
	}
	return normalize(value)
}

func normalizeSchemaCallable(input []byte, current *node, pointer string) error {
	if err := normalizeStringSetMember(input, current, "capabilities", pointer+"/capabilities", true); err != nil {
		return err
	}
	_, err := normalizeSchemaIDArrayMember(input, current, "traits", pointer+"/traits")
	return err
}

func normalizeSchemaIDArrayMember(input []byte, parent *node, name, pointer string) (*node, error) {
	return normalizeSchemaArrayMember(input, parent, name, pointer, "/id", func(item *node) (string, bool) {
		return schemaTypeIdentifier(item)
	})
}

func normalizeSchemaArrayMember(input []byte, parent *node, name, pointer, itemSuffix string, key func(*node) (string, bool)) (*node, error) {
	index, exists := parent.memberByID[name]
	if !exists {
		return nil, nil
	}
	values := &parent.object[index].value
	if values.kind != nodeArray {
		return nil, newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", name+" must be an array", pointer, values.start)
	}
	seen := make(map[string]bool, len(values.array))
	for itemIndex := range values.array {
		item := &values.array[itemIndex]
		identifier, ok := key(item)
		if !ok || seen[identifier] {
			return nil, newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", name+" entries require unique portable identifiers", fmt.Sprintf("%s/%d%s", pointer, itemIndex, itemSuffix), item.start)
		}
		seen[identifier] = true
	}
	sort.Slice(values.array, func(left, right int) bool {
		leftID, _ := key(&values.array[left])
		rightID, _ := key(&values.array[right])
		return leftID < rightID
	})
	return values, nil
}

func normalizeSchemaReferences(input []byte, root *node) error {
	index, exists := root.memberByID["references"]
	if !exists {
		return nil
	}
	values := &root.object[index].value
	if values.kind != nodeArray {
		return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "references must be an array", "/references", values.start)
	}
	seen := make(map[string]bool, len(values.array))
	for itemIndex := range values.array {
		item := &values.array[itemIndex]
		uri, hasURI := item.member("uri")
		revision, hasRevision := item.member("revision")
		if item.kind != nodeObject || !hasURI || uri.kind != nodeString || !hasRevision || revision.kind != nodeString {
			return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "references require uri and revision strings", fmt.Sprintf("/references/%d", itemIndex), item.start)
		}
		key := uri.text + "\x00" + revision.text
		if seen[key] {
			return newDiagnostic(input, "INVALID_SCHEMA", "CANON-104", "validate", "references must be unique", fmt.Sprintf("/references/%d", itemIndex), item.start)
		}
		seen[key] = true
	}
	sort.Slice(values.array, func(left, right int) bool {
		leftURI, _ := values.array[left].member("uri")
		leftRevision, _ := values.array[left].member("revision")
		rightURI, _ := values.array[right].member("uri")
		rightRevision, _ := values.array[right].member("revision")
		if leftURI.text == rightURI.text {
			return leftRevision.text < rightRevision.text
		}
		return leftURI.text < rightURI.text
	})
	return nil
}

func schemaTypeIdentifier(value *node) (string, bool) {
	if value.kind != nodeObject {
		return "", false
	}
	identifier, exists := value.member("id")
	return identifier.text, exists && identifier.kind == nodeString && capabilityPattern(identifier.text)
}

func normalizeStringSetMember(input []byte, parent *node, name, pointer string, identifiers bool) error {
	_, err := normalizeSchemaArrayMember(input, parent, name, pointer, "", func(item *node) (string, bool) {
		return item.text, item.kind == nodeString && (!identifiers || capabilityPattern(item.text))
	})
	return err
}

// ValidateJSON applies the same strict decoder and resource limits used by
// CanonicalizeJSON without changing the input representation. In particular,
// duplicate object members, invalid UTF-8, and unpaired surrogate escapes are
// rejected before callers hand values to encoding/json.
func ValidateJSON(input []byte, limits Limits) error {
	_, err := parseJSON(input, limits)
	return err
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
	case DocumentHash, SchemaHash, ApprovalHash, ResultCacheHash, IdempotencyHash, SignedMessageHash, FederationHash:
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
	if (err != nil && !errors.Is(err, strconv.ErrRange)) || math.IsInf(value, 0) || math.IsNaN(value) {
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
