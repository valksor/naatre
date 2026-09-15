package mcpadapter

type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "mcp adapter: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

func adapterError(code string, cause error) *Error { return &Error{Code: code, cause: cause} }
