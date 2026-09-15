package openrpcadapter

import "slices"

// Diagnostic is bounded, credential-free configuration evidence.
type Diagnostic struct {
	Code    string `json:"code"`
	Pointer string `json:"pointer,omitempty"`
	Message string `json:"message"`
}

// Error exposes only a stable public code and safe diagnostics. Underlying
// parser, transport, authentication, and application errors are never wrapped.
type Error struct {
	Code        string `json:"code"`
	diagnostics []Diagnostic
}

func (e *Error) Error() string { return "openrpc adapter: " + e.Code }

// Diagnostics returns a defensive copy of safe diagnostics.
func (e *Error) Diagnostics() []Diagnostic {
	if e == nil {
		return nil
	}
	return slices.Clone(e.diagnostics)
}

func adapterError(code string, diagnostics ...Diagnostic) *Error {
	return &Error{Code: code, diagnostics: slices.Clone(diagnostics)}
}

func diagnostic(code, pointer, message string) Diagnostic {
	return Diagnostic{Code: code, Pointer: pointer, Message: message}
}
