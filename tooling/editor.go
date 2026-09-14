package tooling

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// EditorAdapter is the protocol-neutral core used by an LSP server or a
// documented editor plugin. Issue #94 owns protocol transport integrations.
type EditorAdapter struct {
	Schema *schema.Document
}

func (a EditorAdapter) Diagnostics(input []byte, operation string) DiagnosticReport {
	return ValidateDocument(input, ValidateOptions{Schema: a.Schema, Operation: operation})
}

type Completion struct {
	Label      string `json:"label"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail,omitempty"`
	Deprecated bool   `json:"deprecated,omitempty"`
}

func (a EditorAdapter) Completions(prefix string) []Completion {
	if a.Schema == nil {
		return []Completion{}
	}
	var result []Completion
	for _, operation := range a.Schema.Operations() {
		result = appendCompletion(result, prefix, operation.Name, "operation", operation.Description, operation.Deprecation != nil)
	}
	for _, member := range a.Schema.Members() {
		result = appendCompletion(result, prefix, member.Name, "member", member.Description, member.Deprecation != nil)
	}
	for _, declaration := range a.Schema.Types() {
		result = appendCompletion(result, prefix, declaration.Name, "type", declaration.Description, declaration.Deprecation != nil)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Label == result[j].Label {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Label < result[j].Label
	})
	return result
}

func appendCompletion(result []Completion, prefix, label, kind, detail string, deprecated bool) []Completion {
	if strings.HasPrefix(label, prefix) {
		return append(result, Completion{Label: label, Kind: kind, Detail: detail, Deprecated: deprecated})
	}
	return result
}

type Hover struct {
	Symbol        string              `json:"symbol"`
	Kind          string              `json:"kind"`
	Documentation string              `json:"documentation,omitempty"`
	Deprecation   *schema.Deprecation `json:"deprecation,omitempty"`
}

func (a EditorAdapter) Hover(symbol string) (Hover, bool) {
	if a.Schema == nil {
		return Hover{}, false
	}
	for _, operation := range a.Schema.Operations() {
		if operation.Name == symbol || operation.ID == symbol {
			return Hover{Symbol: operation.Name, Kind: "operation", Documentation: operation.Description, Deprecation: operation.Deprecation}, true
		}
	}
	for _, member := range a.Schema.Members() {
		if member.Name == symbol || member.ID == symbol {
			return Hover{Symbol: member.Name, Kind: "member", Documentation: member.Description, Deprecation: member.Deprecation}, true
		}
	}
	for _, declaration := range a.Schema.Types() {
		if declaration.Name == symbol || string(declaration.ID) == symbol {
			return Hover{Symbol: declaration.Name, Kind: "type", Documentation: declaration.Description, Deprecation: declaration.Deprecation}, true
		}
	}
	return Hover{}, false
}

func (a EditorAdapter) Definition(symbol string) (schema.SourceMetadata, bool) {
	if a.Schema == nil {
		return schema.SourceMetadata{}, false
	}
	for _, operation := range a.Schema.Operations() {
		if (operation.Name == symbol || operation.ID == symbol) && operation.Source != nil {
			return *operation.Source, true
		}
	}
	for _, member := range a.Schema.Members() {
		if (member.Name == symbol || member.ID == symbol) && member.Source != nil {
			return *member.Source, true
		}
	}
	for _, declaration := range a.Schema.Types() {
		if (declaration.Name == symbol || string(declaration.ID) == symbol) && declaration.Source != nil {
			return *declaration.Source, true
		}
	}
	return schema.SourceMetadata{}, false
}

// FragmentDefinition resolves a reusable document fragment to its exact source
// range. Schema type definitions remain available through Definition.
func (EditorAdapter) FragmentDefinition(input []byte, symbol string) (protocol.Source, bool, error) {
	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		return protocol.Source{}, false, err
	}
	for _, fragment := range document.Fragments() {
		if fragment.Name() == symbol {
			return fragment.Source(), true, nil
		}
	}
	return protocol.Source{}, false, nil
}

// DeprecationWarnings returns source-addressable advisory diagnostics for
// deprecated schema symbols referenced by a document.
func (a EditorAdapter) DeprecationWarnings(input []byte) []Diagnostic {
	if a.Schema == nil {
		return []Diagnostic{}
	}
	deprecated := make(map[string]*schema.Deprecation)
	for _, operation := range a.Schema.Operations() {
		if operation.Deprecation != nil {
			deprecated[operation.Name] = operation.Deprecation
		}
	}
	for _, member := range a.Schema.Members() {
		if member.Deprecation != nil {
			deprecated[member.Name] = member.Deprecation
		}
	}
	for _, declaration := range a.Schema.Types() {
		if declaration.Deprecation != nil {
			deprecated[declaration.Name] = declaration.Deprecation
		}
	}
	var warnings []Diagnostic
	for symbol, deprecation := range deprecated {
		edits, err := (EditorAdapter{}).RenameEdits(input, symbol, symbol)
		if err != nil {
			return []Diagnostic{}
		}
		for _, edit := range edits {
			warnings = append(warnings, Diagnostic{
				Phase: "editor", Code: "DEPRECATED", Path: edit.Path, Message: deprecation.Reason,
			})
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Path < warnings[j].Path })
	return warnings
}

type RenameEdit struct {
	Path    string `json:"path"`
	NewText string `json:"newText"`
}

// RenameEdits returns structural JSON-pointer edits and never performs a
// textual replacement inside literals or documentation.
func (EditorAdapter) RenameEdits(input []byte, oldName, newName string) ([]RenameEdit, error) {
	if oldName == "" || newName == "" {
		return nil, fmt.Errorf("rename symbols must be non-empty")
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, err
	}
	var edits []RenameEdit
	collectRenameEdits(value, "", oldName, newName, &edits)
	sort.Slice(edits, func(i, j int) bool { return edits[i].Path < edits[j].Path })
	return edits, nil
}

func collectRenameEdits(value any, pointer, oldName, newName string, edits *[]RenameEdit) {
	switch typed := value.(type) {
	case []any:
		for index, child := range typed {
			collectRenameEdits(child, fmt.Sprintf("%s/%d", pointer, index), oldName, newName, edits)
		}
	case map[string]any:
		for key, child := range typed {
			childPointer := pointer + "/" + escapePointer(key)
			if text, ok := child.(string); ok && text == oldName && renameKey(key) {
				*edits = append(*edits, RenameEdit{Path: childPointer, NewText: newName})
			}
			collectRenameEdits(child, childPointer, oldName, newName, edits)
		}
	}
}

func renameKey(key string) bool {
	switch key {
	case "name", "type", "on", "owner", "operation":
		return true
	default:
		return false
	}
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
