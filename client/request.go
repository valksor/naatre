// Package client implements the runtime-independent Go SDK client core.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/valksor/naatre/protocol"
)

var requestIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Request is an immutable client request. Its document and variable bytes are
// copied on construction and every With method returns an isolated value.
type Request struct {
	document     json.RawMessage
	persisted    *protocol.PersistedReference
	operation    string
	kind         protocol.OperationKind
	id           string
	hasID        bool
	variables    map[string]json.RawMessage
	capabilities []string
}

// NewRequest builds an inline request from an already strict, immutable
// protocol document.
func NewRequest(document protocol.Document, operation string) (Request, error) {
	operations := document.Operations()
	if operation == "" && len(operations) == 1 {
		operation = operations[0].Name()
	}
	found := false
	kind := protocol.OperationKind("")
	for _, candidate := range operations {
		if candidate.Name() == operation {
			found = true
			kind = candidate.Kind()
			break
		}
	}
	if !found || len(document.CanonicalJSON()) == 0 {
		return Request{}, clientError("INVALID_REQUEST", 0, errors.New("operation is absent from document"))
	}
	request := Request{document: document.CanonicalJSON(), operation: operation, kind: kind, variables: make(map[string]json.RawMessage)}
	if _, err := request.CanonicalJSON(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// NewPersistedRequest builds a request that carries only a canonical document
// reference. The reference is validated by the shared protocol decoder.
func NewPersistedRequest(reference protocol.PersistedReference, operation string) (Request, error) {
	return NewPersistedOperation(reference, operation, "")
}

// NewPersistedOperation carries a known operation kind for generated retry and
// streaming helpers while preserving the same persisted wire envelope.
func NewPersistedOperation(reference protocol.PersistedReference, operation string, kind protocol.OperationKind) (Request, error) {
	if kind != "" && kind != protocol.Query && kind != protocol.Mutation && kind != protocol.Subscription {
		return Request{}, clientError("INVALID_REQUEST", 0, errors.New("invalid operation kind"))
	}
	copy := reference
	request := Request{persisted: &copy, operation: operation, kind: kind, variables: make(map[string]json.RawMessage)}
	if _, err := request.CanonicalJSON(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// OperationKind reports the kind known from the inline document or persisted
// manifest. A request built from a bare persisted reference has no known kind.
func (r Request) OperationKind() protocol.OperationKind { return r.kind }

// WithID returns an isolated request carrying a bounded correlation ID.
func (r Request) WithID(id string) (Request, error) {
	if id == "" || !utf8.ValidString(id) || utf8.RuneCountInString(id) > 128 {
		return Request{}, clientError("INVALID_REQUEST", 0, errors.New("invalid request ID"))
	}
	result := r.clone()
	result.id, result.hasID = id, true
	return result, nil
}

// WithVariable canonicalizes and copies one exact JSON value. Missing remains
// absent, while an explicit JSON null remains present.
func (r Request) WithVariable(name string, value json.RawMessage) (Request, error) {
	if !requestIdentifier.MatchString(name) {
		return Request{}, clientError("INVALID_VARIABLE", 0, errors.New("invalid variable name"))
	}
	canonical, err := protocol.CanonicalizeJSON(value, protocol.Limits{})
	if err != nil {
		return Request{}, clientError("INVALID_VARIABLE", 0, err)
	}
	result := r.clone()
	result.variables[name] = canonical
	return result, nil
}

// WithCapabilities returns an isolated request with a sorted, unique set of
// explicitly requested capability names.
func (r Request) WithCapabilities(capabilities ...string) (Request, error) {
	values, err := normalizeCapabilities(capabilities)
	if err != nil {
		return Request{}, clientError("INVALID_CAPABILITY", 0, err)
	}
	result := r.clone()
	result.capabilities = values
	return result, nil
}

func normalizeCapabilities(capabilities []string) ([]string, error) {
	values := slices.Clone(capabilities)
	sort.Strings(values)
	for index, capability := range values {
		if !typeIdentifier.MatchString(capability) || !utf8.ValidString(capability) || utf8.RuneCountInString(capability) > 128 || index > 0 && values[index-1] == capability {
			return nil, errors.New("invalid or duplicate capability")
		}
	}
	return values, nil
}

// CanonicalJSON returns the exact c14n-1 request envelope bytes.
func (r Request) CanonicalJSON() ([]byte, error) {
	type persistedWire struct {
		Algorithm        string `json:"algorithm"`
		CanonicalVersion string `json:"canonicalVersion"`
		Digest           string `json:"digest"`
	}
	type requestWire struct {
		Version      string                     `json:"version"`
		ID           string                     `json:"id,omitempty"`
		Operation    string                     `json:"operation,omitempty"`
		Document     json.RawMessage            `json:"document,omitempty"`
		Persisted    *persistedWire             `json:"persisted,omitempty"`
		Variables    map[string]json.RawMessage `json:"variables,omitempty"`
		Capabilities []string                   `json:"capabilities,omitempty"`
	}
	wire := requestWire{
		Version: "1", Operation: r.operation, Document: slices.Clone(r.document),
		Variables: cloneRawMap(r.variables), Capabilities: slices.Clone(r.capabilities),
	}
	if r.hasID {
		wire.ID = r.id
	}
	if r.persisted != nil {
		wire.Persisted = &persistedWire{
			Algorithm: r.persisted.Algorithm, CanonicalVersion: r.persisted.CanonicalVersion, Digest: r.persisted.Digest,
		}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, clientError("INVALID_REQUEST", 0, err)
	}
	canonical, err := protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: 8 << 20})
	if err != nil {
		return nil, clientError("INVALID_REQUEST", 0, err)
	}
	capabilitySupport := make(map[string]bool, len(r.capabilities))
	for _, capability := range r.capabilities {
		capabilitySupport[capability] = true
	}
	if _, err := protocol.DecodeRequest(canonical, protocol.DecodeOptions{Capabilities: capabilitySupport}); err != nil {
		return nil, clientError("INVALID_REQUEST", 0, err)
	}
	return canonical, nil
}

// DocumentDigest returns the shared semantic document identity for inline and
// persisted requests.
func (r Request) DocumentDigest() (protocol.Digest, error) {
	if r.persisted != nil {
		return protocol.Digest{
			Algorithm: r.persisted.Algorithm, CanonicalVersion: r.persisted.CanonicalVersion, Hex: r.persisted.Digest,
		}, nil
	}
	digest, err := protocol.SemanticHash(protocol.DocumentHash, r.document)
	if err != nil {
		return protocol.Digest{}, clientError("INVALID_REQUEST", 0, err)
	}
	return digest, nil
}

func (r Request) clone() Request {
	result := Request{
		document:     slices.Clone(r.document),
		operation:    r.operation,
		kind:         r.kind,
		id:           r.id,
		hasID:        r.hasID,
		variables:    cloneRawMap(r.variables),
		capabilities: slices.Clone(r.capabilities),
	}
	if r.persisted != nil {
		persisted := *r.persisted
		result.persisted = &persisted
	}
	return result
}

func cloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage, len(values))
	maps.Copy(result, values)
	for key := range result {
		result[key] = bytes.Clone(result[key])
	}
	return result
}
