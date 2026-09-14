package collectionquery

const (
	CodeInvalid           = "FILTER_INVALID"
	CodeUnsupported       = "FILTER_UNSUPPORTED"
	CodeResourceExhausted = "RESOURCE_EXHAUSTED"
	CodeProviderFailed    = "FILTER_PROVIDER_FAILED"
)

// Error is safe to return across a protocol boundary. Messages never include
// requested field or provider names.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	cause   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.cause }

func diagnostic(code, message string, cause error) error {
	result := &Error{Code: code, Message: message}
	result.cause = cause
	return result
}

func invalid(message string) error     { return diagnostic(CodeInvalid, message, nil) }
func unsupported(message string) error { return diagnostic(CodeUnsupported, message, nil) }
func exhausted(message string) error   { return diagnostic(CodeResourceExhausted, message, nil) }
func providerFailed(cause error) error {
	return diagnostic(CodeProviderFailed, "collection provider failed", cause)
}
