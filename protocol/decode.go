package protocol

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	identifierPattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	profileIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
)

// DecodeRequest strictly decodes one Naatre request without resolving schema
// or invoking application behavior.
func DecodeRequest(input []byte, options DecodeOptions) (*Request, error) {
	root, err := parseJSON(input, options.Limits)
	if err != nil {
		return nil, err
	}
	if err := expectKindPhase(input, root, nodeObject, "", "request object", "PROTO-001", "decode"); err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		"version": true, "id": true, "operation": true, "document": true,
		"persisted": true, "variables": true, "capabilities": true, "extensions": true,
	}
	if err := rejectUnknown(input, root, allowed, "PROTO-001", "decode", ""); err != nil {
		return nil, err
	}

	request := &Request{source: sourceAt(input, "", root), variables: make(map[string]json.RawMessage), extensions: make(map[string]json.RawMessage)}
	version, ok := root.member("version")
	if !ok || version.kind != nodeString || version.text != "1" {
		offset := root.start
		if ok {
			offset = version.start
		}
		return nil, newDiagnostic(input, "UNSUPPORTED_VERSION", "PROTO-002", "decode", "version must be \"1\"", "/version", offset)
	}
	request.version = version.text

	if id, exists := root.member("id"); exists {
		if err := expectKindPhase(input, id, nodeString, "/id", "string id", "PROTO-003", "decode"); err != nil {
			return nil, err
		}
		if id.text == "" || utf8.RuneCountInString(id.text) > 128 {
			return nil, newDiagnostic(input, "INVALID_ID", "PROTO-003", "decode", "id must contain 1..128 Unicode scalars", "/id", id.start)
		}
		request.id, request.hasID = id.text, true
	}
	if operation, exists := root.member("operation"); exists {
		if err := expectKindPhase(input, operation, nodeString, "/operation", "operation name", "PROTO-005", "decode"); err != nil {
			return nil, err
		}
		if !identifierPattern.MatchString(operation.text) {
			return nil, newDiagnostic(input, "INVALID_IDENTIFIER", "LANG-002", "validate", "invalid operation identifier", "/operation", operation.start)
		}
		request.operation = operation.text
	}

	documentNode, hasDocument := root.member("document")
	persistedNode, hasPersisted := root.member("persisted")
	if hasDocument == hasPersisted {
		code, message := "MISSING_SOURCE", "exactly one operation source is required"
		if hasDocument {
			code, message = "CONFLICTING_SOURCE", "document and persisted source are mutually exclusive"
		}
		return nil, newDiagnostic(input, code, "PROTO-004", "decode", message, "", root.start)
	}
	if hasDocument {
		document, decodeErr := decodeDocument(input, documentNode)
		if decodeErr != nil {
			return nil, decodeErr
		}
		request.document = &document
	} else {
		persisted, decodeErr := decodePersisted(input, persistedNode)
		if decodeErr != nil {
			return nil, decodeErr
		}
		request.persisted = &persisted
	}

	if variables, exists := root.member("variables"); exists {
		if err := expectKindPhase(input, variables, nodeObject, "/variables", "variables object", "PROTO-006", "decode"); err != nil {
			return nil, err
		}
		for _, variable := range variables.object {
			request.variables[variable.name] = append(json.RawMessage(nil), input[variable.value.start:variable.value.end]...)
		}
	}
	if capabilities, exists := root.member("capabilities"); exists {
		if err := expectKindPhase(input, capabilities, nodeArray, "/capabilities", "capability array", "PROTO-007", "decode"); err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(capabilities.array))
		for index, capability := range capabilities.array {
			pointer := joinPointer("/capabilities", intString(index))
			if capability.kind != nodeString || !capabilityPattern(capability.text) || seen[capability.text] {
				return nil, newDiagnostic(input, "INVALID_CAPABILITY", "PROTO-007", "decode", "capabilities must be unique portable identifiers", pointer, capability.start)
			}
			if !options.Capabilities[capability.text] {
				return nil, newDiagnostic(input, "UNSUPPORTED_CAPABILITY", "PROTO-007", "validate", "required capability is not supported", pointer, capability.start)
			}
			seen[capability.text] = true
			request.capabilities = append(request.capabilities, capability.text)
		}
	}
	if extensions, exists := root.member("extensions"); exists {
		if err := expectKindPhase(input, extensions, nodeObject, "/extensions", "extensions object", "PROTO-008", "decode"); err != nil {
			return nil, err
		}
		for _, extension := range extensions.object {
			pointer := joinPointer("/extensions", extension.name)
			if !namespacePattern(extension.name) || !options.ExtensionNamespaces[extension.name] {
				return nil, newDiagnostic(input, "UNSUPPORTED_EXTENSION", "PROTO-008", "validate", "extension namespace is not negotiated", pointer, extension.start)
			}
			request.extensions[extension.name] = append(json.RawMessage(nil), input[extension.value.start:extension.value.end]...)
		}
	}
	return request, nil
}

func decodeDocument(input []byte, value node) (Document, error) {
	if err := expectKindPhase(input, value, nodeObject, "/document", "document object", "LANG-001", "validate"); err != nil {
		return Document{}, err
	}
	if err := rejectUnknown(input, value, map[string]bool{"operations": true}, "LANG-001", "validate", "/document"); err != nil {
		return Document{}, err
	}
	operationsNode, exists := value.member("operations")
	if !exists {
		return Document{}, newDiagnostic(input, "MISSING_FIELD", "LANG-001", "validate", "document requires operations", "/document/operations", value.start)
	}
	if err := expectKindPhase(input, operationsNode, nodeArray, "/document/operations", "operations array", "LANG-001", "validate"); err != nil {
		return Document{}, err
	}
	document := Document{operations: make([]Operation, 0, len(operationsNode.array))}
	for index, operationNode := range operationsNode.array {
		operation, err := decodeOperation(input, operationNode, joinPointer("/document/operations", intString(index)))
		if err != nil {
			return Document{}, err
		}
		document.operations = append(document.operations, operation)
	}
	return document, nil
}

func decodeOperation(input []byte, value node, pointer string) (Operation, error) {
	if err := expectKindPhase(input, value, nodeObject, pointer, "operation object", "LANG-002", "validate"); err != nil {
		return Operation{}, err
	}
	allowed := map[string]bool{"name": true, "kind": true, "select": true}
	if err := rejectUnknown(input, value, allowed, "LANG-002", "validate", pointer); err != nil {
		return Operation{}, err
	}
	name, hasName := value.member("name")
	kind, hasKind := value.member("kind")
	selectNode, hasSelect := value.member("select")
	if !hasName || name.kind != nodeString || !identifierPattern.MatchString(name.text) {
		return Operation{}, newDiagnostic(input, "INVALID_IDENTIFIER", "LANG-002", "validate", "operation requires a portable name", joinPointer(pointer, "name"), value.start)
	}
	if !hasKind || kind.kind != nodeString || (kind.text != string(Query) && kind.text != string(Mutation) && kind.text != string(Subscription)) {
		return Operation{}, newDiagnostic(input, "INVALID_OPERATION_KIND", "CORE-003", "validate", "operation kind must be query, mutation, or subscription", joinPointer(pointer, "kind"), value.start)
	}
	if !hasSelect || selectNode.kind != nodeArray {
		return Operation{}, newDiagnostic(input, "INVALID_SELECTIONS", "LANG-002", "validate", "operation requires a selection array", joinPointer(pointer, "select"), value.start)
	}
	selections, err := decodeSelections(input, selectNode, joinPointer(pointer, "select"))
	if err != nil {
		return Operation{}, err
	}
	return Operation{name: name.text, kind: OperationKind(kind.text), selections: selections, source: sourceAt(input, pointer, value)}, nil
}

func decodeSelections(input []byte, value node, pointer string) ([]Selection, error) {
	result := make([]Selection, 0, len(value.array))
	for index, selectionNode := range value.array {
		selection, err := decodeSelection(input, selectionNode, joinPointer(pointer, intString(index)))
		if err != nil {
			return nil, err
		}
		result = append(result, selection)
	}
	return result, nil
}

func decodeSelection(input []byte, value node, pointer string) (Selection, error) {
	if value.kind != nodeObject || len(value.object) != 1 {
		return Selection{}, newDiagnostic(input, "INVALID_SELECTION", "LANG-003", "validate", "selection must be a one-key tagged object", pointer, value.start)
	}
	tag := value.object[0].name
	payload := value.object[0].value
	kinds := map[string]SelectionKind{
		"$field": FieldSelection, "$call": CallSelection, "$pipeline": PipelineSelection,
		"$map": MapSelection, "$index": IndexSelection, "$slice": SliceSelection,
		"$page": PageSelection, "$meta": MetaSelection, "$parallel": ParallelSelection,
		"$fragment": FragmentSelection, "$current": CurrentSelection,
		"$nest": NestSelection, "$unnest": UnnestSelection,
	}
	kind, ok := kinds[tag]
	if !ok {
		return Selection{}, newDiagnostic(input, "UNKNOWN_SELECTION", "LANG-003", "validate", "unknown selection tag", joinPointer(pointer, tag), value.object[0].start)
	}
	payloadPointer := joinPointer(pointer, tag)
	if kind != FieldSelection && kind != CallSelection {
		return Selection{}, newDiagnostic(input, "UNSUPPORTED_SELECTION", "LANG-003", "validate", "selection tag is specified but not implemented by this decoder", payloadPointer, payload.start)
	}
	if err := expectKindPhase(input, payload, nodeObject, payloadPointer, "selection payload object", "LANG-003", "validate"); err != nil {
		return Selection{}, err
	}
	allowed := map[string]bool{"name": true, "as": true}
	if kind == FieldSelection {
		allowed["select"] = true
	}
	if err := rejectUnknown(input, payload, allowed, "LANG-004", "validate", payloadPointer); err != nil {
		return Selection{}, err
	}
	selection := Selection{kind: kind, source: sourceAt(input, pointer, value)}
	name, exists := payload.member("name")
	if !exists {
		return Selection{}, newDiagnostic(input, "MISSING_FIELD", "LANG-100", "validate", "selection requires name", joinPointer(payloadPointer, "name"), payload.start)
	}
	if err := expectKindPhase(input, name, nodeString, joinPointer(payloadPointer, "name"), "selection name string", "LANG-100", "validate"); err != nil {
		return Selection{}, err
	}
	selection.name = name.text
	if alias, aliasExists := payload.member("as"); aliasExists {
		if err := expectKindPhase(input, alias, nodeString, joinPointer(payloadPointer, "as"), "response alias string", "LANG-002", "validate"); err != nil {
			return Selection{}, err
		}
		selection.alias = alias.text
	}
	if children, childExists := payload.member("select"); childExists {
		if err := expectKindPhase(input, children, nodeArray, joinPointer(payloadPointer, "select"), "child selection array", "LANG-002", "validate"); err != nil {
			return Selection{}, err
		}
		decoded, err := decodeSelections(input, children, joinPointer(payloadPointer, "select"))
		if err != nil {
			return Selection{}, err
		}
		selection.children = decoded
	}
	if !identifierPattern.MatchString(selection.name) {
		return Selection{}, newDiagnostic(input, "INVALID_IDENTIFIER", "LANG-002", "validate", "selection requires a portable name", joinPointer(payloadPointer, "name"), name.start)
	}
	if selection.alias != "" && !identifierPattern.MatchString(selection.alias) {
		return Selection{}, newDiagnostic(input, "INVALID_IDENTIFIER", "LANG-002", "validate", "invalid response alias", joinPointer(joinPointer(pointer, tag), "as"), payload.start)
	}
	return selection, nil
}

func decodePersisted(input []byte, value node) (PersistedReference, error) {
	if err := expectKindPhase(input, value, nodeObject, "/persisted", "persisted reference", "PROTO-004", "decode"); err != nil {
		return PersistedReference{}, err
	}
	if err := rejectUnknown(input, value, map[string]bool{"algorithm": true, "canonicalVersion": true, "digest": true}, "PROTO-004", "decode", "/persisted"); err != nil {
		return PersistedReference{}, err
	}
	algorithm, hasAlgorithm := value.member("algorithm")
	canonicalVersion, hasCanonicalVersion := value.member("canonicalVersion")
	digest, hasDigest := value.member("digest")
	if !hasAlgorithm || algorithm.kind != nodeString || algorithm.text != "sha-256" || !hasCanonicalVersion || canonicalVersion.kind != nodeString || canonicalVersion.text != "c14n-1" || !hasDigest || digest.kind != nodeString || len(digest.text) != 64 || strings.ToLower(digest.text) != digest.text {
		return PersistedReference{}, newDiagnostic(input, "INVALID_PERSISTED_REFERENCE", "PROTO-004", "decode", "persisted reference requires sha-256, c14n-1, and 64 lowercase hex digits", "/persisted", value.start)
	}
	for _, char := range digest.text {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return PersistedReference{}, newDiagnostic(input, "INVALID_PERSISTED_REFERENCE", "PROTO-004", "decode", "persisted digest is not lowercase hexadecimal", "/persisted/digest", digest.start)
		}
	}
	return PersistedReference{Algorithm: algorithm.text, CanonicalVersion: canonicalVersion.text, Digest: digest.text}, nil
}

func rejectUnknown(input []byte, value node, allowed map[string]bool, clause, phase, pointer string) error {
	for _, current := range value.object {
		if !allowed[current.name] {
			return newDiagnostic(input, "UNKNOWN_FIELD", clause, phase, "unknown normative field", joinPointer(pointer, current.name), current.start)
		}
	}
	return nil
}

func sourceAt(input []byte, pointer string, value node) Source {
	diagnostic := newDiagnostic(input, "", "", "", "", pointer, value.start)
	return Source{Pointer: pointer, Start: value.start, End: value.end, Line: diagnostic.Line, Column: diagnostic.Column}
}

func capabilityPattern(value string) bool {
	return profileIdentifierPattern.MatchString(value)
}

func namespacePattern(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, char := range part {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func intString(value int) string {
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}
