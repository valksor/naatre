// Package tooling provides the shared, side-effect-free engine used by the
// Naatre CLI and editor integrations.
package tooling

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const DiagnosticVersion = "naatre.tooling.diagnostics-1"

// Diagnostic is the versioned machine-readable error shared by every tooling
// surface. Path is always an RFC 6901 document pointer.
type Diagnostic struct {
	Phase   string `json:"phase"`
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

// DiagnosticReport is the stable envelope emitted by CLI and editor adapters.
type DiagnosticReport struct {
	Version     string       `json:"version"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ValidateOptions supplies an optional offline schema and selected operation.
type ValidateOptions struct {
	Schema    *schema.Document
	Operation string
}

// ValidateDocument performs strict syntax and optional schema-aware planning.
// It never invokes handlers.
func ValidateDocument(input []byte, options ValidateOptions) DiagnosticReport {
	report := DiagnosticReport{Version: DiagnosticVersion, Diagnostics: []Diagnostic{}}
	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		report.Diagnostics = diagnosticsFromError(err)
		return report
	}
	if options.Schema == nil {
		return report
	}
	snapshot, err := runtime.NewPlanningSnapshot(*options.Schema)
	if err != nil {
		report.Diagnostics = []Diagnostic{toolDiagnostic("schema", "TOOL_SCHEMA_INVALID", "", err)}
		return report
	}
	request, err := requestForDocument(document, options.Operation)
	if err == nil {
		_, err = runtime.Prepare(snapshot, request)
	}
	if err != nil {
		report.Diagnostics = diagnosticsFromError(err)
	}
	return report
}

func requestForDocument(document protocol.Document, operation string) (*protocol.Request, error) {
	requirements := document.Requires()
	capabilities := make(map[string]bool, len(requirements))
	for _, capability := range requirements {
		capabilities[capability] = true
	}
	wire := struct {
		Version      string          `json:"version"`
		Operation    string          `json:"operation,omitempty"`
		Document     json.RawMessage `json:"document"`
		Capabilities []string        `json:"capabilities,omitempty"`
	}{Version: "1", Operation: operation, Document: document.CanonicalJSON(), Capabilities: slices.Clone(requirements)}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode tooling request: %w", err)
	}
	return protocol.DecodeRequest(encoded, protocol.DecodeOptions{Capabilities: capabilities})
}

func diagnosticsFromError(err error) []Diagnostic {
	var validation *runtime.ValidationErrors
	if errors.As(err, &validation) {
		issues := validation.Issues()
		result := make([]Diagnostic, len(issues))
		for index, issue := range issues {
			result[index] = diagnosticFromProtocol(issue.Diagnostic)
		}
		return result
	}
	var diagnostic *protocol.Diagnostic
	if errors.As(err, &diagnostic) {
		return []Diagnostic{diagnosticFromProtocol(*diagnostic)}
	}
	return []Diagnostic{toolDiagnostic("tooling", "TOOL_INTERNAL", "", err)}
}

func diagnosticFromProtocol(input protocol.Diagnostic) Diagnostic {
	return Diagnostic{Phase: input.Phase, Code: input.Code, Path: input.Pointer, Message: input.Message, Line: input.Line, Column: input.Column}
}

func toolDiagnostic(phase, code, path string, err error) Diagnostic {
	message := code
	if err != nil {
		message = err.Error()
	}
	return Diagnostic{Phase: phase, Code: code, Path: path, Message: message}
}
