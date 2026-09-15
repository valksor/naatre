package lsp

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"sync"

	"github.com/valksor/naatre/schema"
	"github.com/valksor/naatre/tooling"
)

const diagnosticSource = "naatre"

var renamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// Limits bounds retained documents, document size, edit batches, message size,
// and concurrent requests. Zero fields select conservative defaults.
type Limits struct {
	MaxOpenDocuments    int
	MaxDocumentBytes    int
	MaxChangesPerUpdate int
	MaxMessageBytes     int
	MaxConcurrent       int
}

// DefaultServerLimits returns the limits implemented by tooling.lsp-1.
func DefaultServerLimits() Limits {
	return Limits{
		MaxOpenDocuments:    64,
		MaxDocumentBytes:    1 << 20,
		MaxChangesPerUpdate: 256,
		MaxMessageBytes:     4 << 20,
		MaxConcurrent:       16,
	}
}

func (limits Limits) withDefaults() Limits {
	defaults := DefaultServerLimits()
	limits.MaxOpenDocuments = defaultLimit(limits.MaxOpenDocuments, defaults.MaxOpenDocuments)
	limits.MaxDocumentBytes = defaultLimit(limits.MaxDocumentBytes, defaults.MaxDocumentBytes)
	limits.MaxChangesPerUpdate = defaultLimit(limits.MaxChangesPerUpdate, defaults.MaxChangesPerUpdate)
	limits.MaxMessageBytes = defaultLimit(limits.MaxMessageBytes, defaults.MaxMessageBytes)
	limits.MaxConcurrent = defaultLimit(limits.MaxConcurrent, defaults.MaxConcurrent)
	return limits
}

func defaultLimit(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

func (limits Limits) valid() bool {
	return limits.MaxOpenDocuments > 0 && limits.MaxDocumentBytes > 0 && limits.MaxChangesPerUpdate > 0 && limits.MaxMessageBytes > 0 && limits.MaxConcurrent > 0
}

// Server retains editor documents against one immutable schema revision.
// Restarting the server is the only supported way to select another schema.
type Server struct {
	mu             sync.RWMutex
	adapter        tooling.EditorAdapter
	schemaRevision string
	limits         Limits
	documents      map[string]document
	initialized    bool
	shutdown       bool
	beforeRequest  func(context.Context, string) error
}

// New constructs a server from an already parsed schema. Schema text is data:
// the LSP package never evaluates plugins, callbacks, scripts, or references.
func New(schemaDocument *schema.Document, limits Limits) (*Server, error) {
	limits = limits.withDefaults()
	if !limits.valid() {
		return nil, newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid server limits")
	}
	revision := ""
	if schemaDocument != nil {
		revision = schemaDocument.Revision()
		if revision == "" {
			return nil, newResponseError(rpcInvalidParams, CodeInvalidParams, "schema revision is required")
		}
	}
	return &Server{
		adapter: tooling.EditorAdapter{Schema: schemaDocument}, schemaRevision: revision,
		limits: limits, documents: make(map[string]document),
	}, nil
}

// SchemaRevision returns the immutable revision pinned by this server.
func (server *Server) SchemaRevision() string { return server.schemaRevision }

type handled struct {
	result        any
	notifications []message
	err           *responseError
}

func (server *Server) handleSafe(ctx context.Context, request message) (result handled) {
	defer func() {
		if recover() != nil {
			result = handled{err: newResponseError(rpcInternal, CodeInternal, "internal server failure")}
		}
	}()
	return server.handle(ctx, request)
}

func (server *Server) handle(ctx context.Context, request message) handled {
	if request.JSONRPC != "2.0" || request.Method == "" {
		return handled{err: newResponseError(rpcInvalidRequest, CodeInvalidRequest, "invalid JSON-RPC request")}
	}
	if err := ctx.Err(); err != nil {
		return handled{err: newResponseError(rpcCancelled, CodeCancelled, "request cancelled")}
	}
	server.mu.RLock()
	initialized, shutdown := server.initialized, server.shutdown
	hook := server.beforeRequest
	server.mu.RUnlock()
	if hook != nil {
		if err := hook(ctx, request.Method); err != nil || ctx.Err() != nil {
			return handled{err: newResponseError(rpcCancelled, CodeCancelled, "request cancelled")}
		}
	}
	if request.Method != "initialize" && request.Method != "exit" && !initialized {
		return handled{err: newResponseError(rpcInvalidRequest, CodeNotInitialized, "server is not initialized")}
	}
	if shutdown && request.Method != "exit" {
		return handled{err: newResponseError(rpcInvalidRequest, CodeInvalidRequest, "server is shut down")}
	}
	switch request.Method {
	case "initialize":
		return server.initialize(request.Params)
	case "initialized", "$/setTrace":
		return handled{result: nil}
	case "shutdown":
		server.mu.Lock()
		server.shutdown = true
		server.mu.Unlock()
		return handled{result: nil}
	case "exit":
		return handled{result: nil}
	case "textDocument/didOpen":
		return server.didOpen(request.Params)
	case "textDocument/didChange":
		return server.didChange(request.Params)
	case "textDocument/didClose":
		return server.didClose(request.Params)
	case "textDocument/diagnostic":
		return server.documentDiagnostic(request.Params)
	case "textDocument/completion":
		return server.completion(request.Params)
	case "textDocument/hover":
		return server.hover(request.Params)
	case "textDocument/definition":
		return server.definition(request.Params)
	case "textDocument/prepareRename":
		return server.prepareRename(request.Params)
	case "textDocument/rename":
		return server.rename(request.Params)
	default:
		return handled{err: newResponseError(rpcMethodMissing, CodeMethodMissing, "method is not supported")}
	}
}

func (server *Server) initialize(raw json.RawMessage) handled {
	var params struct {
		InitializationOptions struct {
			SchemaRevision string `json:"schemaRevision"`
		} `json:"initializationOptions"`
	}
	if len(raw) != 0 && json.Unmarshal(raw, &params) != nil {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid initialize parameters")}
	}
	if pin := params.InitializationOptions.SchemaRevision; pin != "" && pin != server.schemaRevision {
		return handled{err: newResponseError(rpcContentChanged, CodeSchemaMismatch, "schema revision does not match")}
	}
	server.mu.Lock()
	if server.initialized {
		server.mu.Unlock()
		return handled{err: newResponseError(rpcInvalidRequest, CodeInvalidRequest, "server is already initialized")}
	}
	server.initialized = true
	server.mu.Unlock()
	return handled{result: map[string]any{
		"capabilities": map[string]any{
			"positionEncoding":   PositionEncoding,
			"textDocumentSync":   map[string]any{"openClose": true, "change": 2},
			"diagnosticProvider": map[string]any{"identifier": diagnosticSource, "interFileDependencies": false, "workspaceDiagnostics": false},
			"completionProvider": map[string]any{"resolveProvider": false},
			"hoverProvider":      true, "definitionProvider": true,
			"renameProvider": map[string]any{"prepareProvider": true},
		},
		"serverInfo":   map[string]any{"name": "naatre-lsp", "version": "1.0.0"},
		"experimental": map[string]any{"profile": Profile, "schemaRevision": server.schemaRevision},
	}}
}

func (server *Server) didOpen(raw json.RawMessage) handled {
	var params struct {
		TextDocument struct {
			URI            string `json:"uri"`
			LanguageID     string `json:"languageId"`
			Version        int    `json:"version"`
			Text           string `json:"text"`
			SchemaRevision string `json:"schemaRevision,omitempty"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(raw, &params) != nil || params.TextDocument.URI == "" {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid document open parameters")}
	}
	item := params.TextDocument
	if item.SchemaRevision != "" && item.SchemaRevision != server.schemaRevision {
		return handled{notifications: []message{server.failureDiagnostics(item.URI, item.Version, CodeSchemaMismatch)}, err: newResponseError(rpcContentChanged, CodeSchemaMismatch, "schema revision does not match")}
	}
	if len(item.Text) > server.limits.MaxDocumentBytes {
		return handled{notifications: []message{server.failureDiagnostics(item.URI, item.Version, CodeResourceLimit)}, err: newResponseError(rpcInvalidParams, CodeResourceLimit, "document exceeds resource limit")}
	}
	server.mu.Lock()
	if _, exists := server.documents[item.URI]; !exists && len(server.documents) >= server.limits.MaxOpenDocuments {
		server.mu.Unlock()
		return handled{err: newResponseError(rpcInvalidParams, CodeResourceLimit, "open document limit reached")}
	}
	server.documents[item.URI] = document{uri: item.URI, text: item.Text, version: item.Version, schemaRevision: server.schemaRevision}
	server.mu.Unlock()
	return handled{notifications: []message{server.publishDiagnostics(item.URI)}}
}

func (server *Server) didChange(raw json.RawMessage) handled {
	var params struct {
		TextDocument   versionedTextDocumentIdentifier `json:"textDocument"`
		ContentChanges []contentChange                 `json:"contentChanges"`
	}
	if json.Unmarshal(raw, &params) != nil || params.TextDocument.URI == "" || len(params.ContentChanges) == 0 {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid document change parameters")}
	}
	identifier := params.TextDocument
	server.mu.Lock()
	doc, exists := server.documents[identifier.URI]
	if !exists {
		server.mu.Unlock()
		return handled{err: newResponseError(rpcInvalidParams, CodeDocumentClosed, "document is not open")}
	}
	if (identifier.SchemaRevision != "" && identifier.SchemaRevision != doc.schemaRevision) || doc.schemaRevision != server.schemaRevision {
		server.mu.Unlock()
		return handled{notifications: []message{server.failureDiagnostics(identifier.URI, doc.version, CodeSchemaMismatch)}, err: newResponseError(rpcContentChanged, CodeSchemaMismatch, "schema revision does not match")}
	}
	if identifier.Version <= doc.version {
		server.mu.Unlock()
		return handled{notifications: []message{server.failureDiagnostics(identifier.URI, doc.version, CodeVersionStale)}, err: newResponseError(rpcContentChanged, CodeVersionStale, "document version is stale")}
	}
	if len(params.ContentChanges) > server.limits.MaxChangesPerUpdate {
		server.mu.Unlock()
		return handled{err: newResponseError(rpcInvalidParams, CodeResourceLimit, "edit batch exceeds resource limit")}
	}
	updated, err := applyChanges(doc.text, params.ContentChanges, server.limits.MaxDocumentBytes)
	if err != nil {
		server.mu.Unlock()
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "incremental edit is invalid")}
	}
	doc.text, doc.version = updated, identifier.Version
	server.documents[identifier.URI] = doc
	server.mu.Unlock()
	return handled{notifications: []message{server.publishDiagnostics(identifier.URI)}}
}

func (server *Server) didClose(raw json.RawMessage) handled {
	var params struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
	}
	if json.Unmarshal(raw, &params) != nil || params.TextDocument.URI == "" {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid document close parameters")}
	}
	server.mu.Lock()
	delete(server.documents, params.TextDocument.URI)
	server.mu.Unlock()
	return handled{notifications: []message{{
		JSONRPC: "2.0",
		Method:  "textDocument/publishDiagnostics",
		Params:  mustJSON(map[string]any{"uri": params.TextDocument.URI, "diagnostics": []any{}}),
	}}}
}

func (server *Server) documentDiagnostic(raw json.RawMessage) handled {
	doc, failure := server.documentFromParams(raw)
	if failure != nil {
		return handled{err: failure}
	}
	return handled{result: map[string]any{"kind": "full", "items": server.diagnostics(doc)}}
}

func (server *Server) completion(raw json.RawMessage) handled {
	doc, params, failure := server.positionParams(raw)
	if failure != nil {
		return handled{err: failure}
	}
	_, prefix, err := wordRange(doc.text, params.Position)
	if err != nil {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "position is invalid")}
	}
	items := server.adapter.Completions(prefix)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{"label": item.Label, "kind": completionKind(item.Kind), "detail": item.Detail, "deprecated": item.Deprecated})
	}
	return handled{result: map[string]any{"isIncomplete": false, "items": result}}
}

func (server *Server) hover(raw json.RawMessage) handled {
	doc, params, failure := server.positionParams(raw)
	if failure != nil {
		return handled{err: failure}
	}
	rangeValue, symbol, err := wordRange(doc.text, params.Position)
	if err != nil {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "position is invalid")}
	}
	hover, ok := server.adapter.Hover(symbol)
	if !ok {
		return handled{result: nil}
	}
	text := hover.Kind + " " + hover.Symbol
	if hover.Documentation != "" {
		text += "\n\n" + hover.Documentation
	}
	if hover.Deprecation != nil {
		text += "\n\nDeprecated."
	}
	return handled{result: map[string]any{"contents": map[string]any{"kind": "plaintext", "value": text}, "range": rangeValue}}
}

func (server *Server) definition(raw json.RawMessage) handled {
	doc, params, failure := server.positionParams(raw)
	if failure != nil {
		return handled{err: failure}
	}
	_, symbol, err := wordRange(doc.text, params.Position)
	if err != nil {
		return handled{err: newResponseError(rpcInvalidParams, CodeInvalidParams, "position is invalid")}
	}
	if source, ok, decodeErr := server.adapter.FragmentDefinition([]byte(doc.text), symbol); decodeErr == nil && ok {
		return handled{result: location{URI: doc.uri, Range: Range{Start: positionAt(doc.text, source.Start), End: positionAt(doc.text, source.End)}}}
	}
	metadata, ok := server.adapter.Definition(symbol)
	if !ok {
		return handled{result: nil}
	}
	line, column := metadata.Line-1, metadata.Column-1
	if line < 0 {
		line = 0
	}
	if column < 0 {
		column = 0
	}
	point := Position{Line: line, Character: column}
	return handled{result: location{URI: metadata.URI, Range: Range{Start: point, End: point}}}
}

func (server *Server) prepareRename(raw json.RawMessage) handled {
	doc, params, failure := server.positionParams(raw)
	if failure != nil {
		return handled{err: failure}
	}
	rangeValue, symbol, err := wordRange(doc.text, params.Position)
	if err != nil || symbol == "" {
		return handled{err: newResponseError(rpcInvalidParams, CodeRenameInvalid, "symbol cannot be renamed")}
	}
	edits, err := server.adapter.RenameEdits([]byte(doc.text), symbol, symbol)
	if err != nil || len(edits) == 0 {
		return handled{err: newResponseError(rpcInvalidParams, CodeRenameInvalid, "symbol cannot be renamed")}
	}
	return handled{result: map[string]any{"range": rangeValue, "placeholder": symbol}}
}

func (server *Server) rename(raw json.RawMessage) handled {
	var params struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
		Position     Position               `json:"position"`
		NewName      string                 `json:"newName"`
	}
	if json.Unmarshal(raw, &params) != nil || !renamePattern.MatchString(params.NewName) {
		return handled{err: newResponseError(rpcInvalidParams, CodeRenameInvalid, "rename target is invalid")}
	}
	doc, failure := server.document(params.TextDocument.URI)
	if failure != nil {
		return handled{err: failure}
	}
	_, symbol, err := wordRange(doc.text, params.Position)
	if err != nil || symbol == "" {
		return handled{err: newResponseError(rpcInvalidParams, CodeRenameInvalid, "symbol cannot be renamed")}
	}
	edits, err := server.adapter.RenameEdits([]byte(doc.text), symbol, params.NewName)
	if err != nil || len(edits) == 0 {
		return handled{err: newResponseError(rpcInvalidParams, CodeRenameInvalid, "document cannot be renamed")}
	}
	index := indexJSON(doc.text)
	textEdits := make([]map[string]any, 0, len(edits))
	for _, edit := range edits {
		rangeValue, ok := index.ranges[edit.Path]
		if ok {
			textEdits = append(textEdits, map[string]any{"range": rangeValue, "newText": edit.NewText})
		}
	}
	return handled{result: map[string]any{"changes": map[string]any{doc.uri: textEdits}}}
}

func (server *Server) positionParams(raw json.RawMessage) (document, textDocumentPositionParams, *responseError) {
	var params textDocumentPositionParams
	if json.Unmarshal(raw, &params) != nil {
		return document{}, params, newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid text document position")
	}
	doc, failure := server.document(params.TextDocument.URI)
	return doc, params, failure
}

func (server *Server) documentFromParams(raw json.RawMessage) (document, *responseError) {
	var params struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return document{}, newResponseError(rpcInvalidParams, CodeInvalidParams, "invalid text document parameters")
	}
	return server.document(params.TextDocument.URI)
}

func (server *Server) document(uri string) (document, *responseError) {
	server.mu.RLock()
	doc, exists := server.documents[uri]
	server.mu.RUnlock()
	if !exists || uri == "" {
		return document{}, newResponseError(rpcInvalidParams, CodeDocumentClosed, "document is not open")
	}
	return doc, nil
}

func (server *Server) publishDiagnostics(uri string) message {
	doc, failure := server.document(uri)
	if failure != nil {
		return server.failureDiagnostics(uri, 0, CodeDocumentClosed)
	}
	return message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: mustJSON(map[string]any{
		"uri": uri, "version": doc.version, "diagnostics": server.diagnostics(doc),
	})}
}

func (server *Server) failureDiagnostics(uri string, version int, code string) message {
	diagnostic := map[string]any{"range": Range{}, "severity": 1, "code": code, "source": diagnosticSource, "message": safeDiagnosticMessage(code)}
	return message{JSONRPC: "2.0", Method: "textDocument/publishDiagnostics", Params: mustJSON(map[string]any{"uri": uri, "version": version, "diagnostics": []any{diagnostic}})}
}

func (server *Server) diagnostics(doc document) []map[string]any {
	report := server.adapter.Diagnostics([]byte(doc.text), "")
	report.Diagnostics = append(report.Diagnostics, server.adapter.DeprecationWarnings([]byte(doc.text))...)
	index := indexJSON(doc.text)
	result := make([]map[string]any, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		rangeValue, ok := index.ranges[diagnostic.Path]
		if !ok {
			line, column := diagnostic.Line-1, diagnostic.Column-1
			if line < 0 {
				line = 0
			}
			if column < 0 {
				column = 0
			}
			rangeValue = Range{Start: Position{Line: line, Character: column}, End: Position{Line: line, Character: column}}
		}
		item := map[string]any{
			"range": rangeValue, "severity": diagnosticSeverity(diagnostic.Code), "code": diagnostic.Code,
			"source": diagnosticSource, "message": safeDiagnosticMessage(diagnostic.Code),
		}
		if diagnostic.Code == "DEPRECATED" {
			item["tags"] = []int{2}
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := result[i]["range"].(Range), result[j]["range"].(Range)
		if left.Start.Line != right.Start.Line {
			return left.Start.Line < right.Start.Line
		}
		if left.Start.Character != right.Start.Character {
			return left.Start.Character < right.Start.Character
		}
		return result[i]["code"].(string) < result[j]["code"].(string)
	})
	return result
}

func completionKind(kind string) int {
	switch kind {
	case "operation":
		return 3
	case "member":
		return 5
	case "type":
		return 7
	default:
		return 1
	}
}

func diagnosticSeverity(code string) int {
	if code == "DEPRECATED" {
		return 2
	}
	return 1
}

func safeDiagnosticMessage(code string) string {
	switch code {
	case "DEPRECATED":
		return "schema symbol is deprecated"
	case CodeSchemaMismatch:
		return "schema revision does not match"
	case CodeVersionStale:
		return "document version is stale"
	case CodeResourceLimit:
		return "resource limit exceeded"
	case CodeDocumentClosed:
		return "document is not open"
	default:
		return "document validation failed"
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
