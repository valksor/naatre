package runtime

import (
	"strings"

	"github.com/valksor/naatre/protocol"
)

// ValidationIssue is one deterministic planning failure and any related
// definition or use sites needed to explain it.
type ValidationIssue struct {
	Diagnostic protocol.Diagnostic
	Related    []protocol.Source
}

// ValidationErrors preserves every independent validation failure in document
// order. Returned issues are copies and cannot mutate the original result.
type ValidationErrors struct {
	issues []ValidationIssue
}

func (e *ValidationErrors) Error() string {
	if e == nil || len(e.issues) == 0 {
		return "validation failed"
	}
	messages := make([]string, len(e.issues))
	for index := range e.issues {
		messages[index] = e.issues[index].Diagnostic.Error()
	}
	return strings.Join(messages, "\n")
}

// Unwrap exposes every source diagnostic to errors.Is and errors.As.
func (e *ValidationErrors) Unwrap() []error {
	if e == nil {
		return nil
	}
	errors := make([]error, len(e.issues))
	for index := range e.issues {
		diagnostic := e.issues[index].Diagnostic
		errors[index] = &diagnostic
	}
	return errors
}

// Issues returns an isolated copy of the ordered failures.
func (e *ValidationErrors) Issues() []ValidationIssue {
	if e == nil {
		return nil
	}
	issues := make([]ValidationIssue, len(e.issues))
	for index := range e.issues {
		issues[index] = e.issues[index]
		issues[index].Related = append([]protocol.Source(nil), e.issues[index].Related...)
	}
	return issues
}

func newValidationIssue(code, clause, message string, source protocol.Source, related ...protocol.Source) ValidationIssue {
	return ValidationIssue{
		Diagnostic: protocol.Diagnostic{
			Code: code, Clause: clause, Phase: "validate", Message: message,
			Pointer: source.Pointer, Offset: source.Start, Line: source.Line, Column: source.Column,
		},
		Related: append([]protocol.Source(nil), related...),
	}
}
