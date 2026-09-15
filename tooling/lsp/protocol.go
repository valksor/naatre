// Package lsp exposes the LSP 3.17 stdio integration for Naatre tooling.
//
// The package translates editor requests to tooling.EditorAdapter. It does not
// define language, schema, validation, or diagnostic semantics of its own.
package lsp

import "encoding/json"

const (
	Profile            = "tooling.lsp-1"
	ProtocolVersion    = "3.17"
	PositionEncoding   = "utf-16"
	CodeParseError     = "LSP_PARSE_ERROR"
	CodeInvalidRequest = "LSP_INVALID_REQUEST"
	CodeMethodMissing  = "LSP_METHOD_NOT_FOUND"
	CodeInvalidParams  = "LSP_INVALID_PARAMS"
	CodeInternal       = "LSP_INTERNAL"
	CodeNotInitialized = "LSP_NOT_INITIALIZED"
	CodeDocumentClosed = "LSP_DOCUMENT_NOT_OPEN"
	CodeVersionStale   = "LSP_DOCUMENT_VERSION_STALE"
	CodeSchemaMismatch = "LSP_SCHEMA_REVISION_MISMATCH"
	CodeCancelled      = "LSP_REQUEST_CANCELLED"
	CodeResourceLimit  = "LSP_RESOURCE_LIMIT"
	CodeRenameInvalid  = "LSP_RENAME_INVALID"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodMissing  = -32601
	rpcInvalidParams  = -32602
	rpcInternal       = -32603
	rpcCancelled      = -32800
	rpcContentChanged = -32801
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *responseError  `json:"error,omitempty"`
}

type responseError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    publicError `json:"data"`
}

func (failure *responseError) Error() string { return failure.Data.Code }

type publicError struct {
	Code string `json:"code"`
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type versionedTextDocumentIdentifier struct {
	URI            string `json:"uri"`
	Version        int    `json:"version"`
	SchemaRevision string `json:"schemaRevision,omitempty"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

func newResponseError(code int, publicCode, message string) *responseError {
	failure := new(responseError)
	failure.Code = code
	failure.Message = message
	failure.Data.Code = publicCode
	return failure
}
