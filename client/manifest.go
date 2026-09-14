package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"

	"github.com/valksor/naatre/protocol"
)

const ManifestProfile = "sdk.go.operations-1"
const defaultManifestBytes int64 = 1 << 20

// ManifestOperation binds one generated operation name and kind to its exact
// persisted document identity.
type ManifestOperation struct {
	Name      string
	Kind      protocol.OperationKind
	Persisted protocol.PersistedReference
}

// Manifest is an immutable, bounded persisted-operation inventory.
type Manifest struct {
	operations map[string]ManifestOperation
	ordered    []string
}

type manifestWire struct {
	Profile          string                  `json:"profile"`
	Version          string                  `json:"version"`
	ProtocolVersion  string                  `json:"protocolVersion"`
	CanonicalVersion string                  `json:"canonicalVersion"`
	Operations       []manifestOperationWire `json:"operations"`
}

type manifestOperationWire struct {
	Name      string                 `json:"name"`
	Kind      protocol.OperationKind `json:"kind"`
	Persisted manifestPersistedWire  `json:"persisted"`
}

type manifestPersistedWire struct {
	Algorithm        string `json:"algorithm"`
	CanonicalVersion string `json:"canonicalVersion"`
	Digest           string `json:"digest"`
}

// LoadManifest rejects oversized, duplicate-key, unknown-field, version-skewed,
// and structurally invalid manifests before publishing any operation.
func LoadManifest(reader io.Reader, maximumBytes int64) (Manifest, error) {
	if reader == nil {
		return Manifest{}, clientError("INVALID_MANIFEST", 0, errors.New("manifest reader is required"))
	}
	if maximumBytes == 0 {
		maximumBytes = defaultManifestBytes
	}
	if maximumBytes < 1 {
		return Manifest{}, clientError("INVALID_MANIFEST", 0, errors.New("invalid manifest limit"))
	}
	payload, err := readBounded(reader, maximumBytes)
	if err != nil {
		return Manifest{}, clientError("MANIFEST_LIMIT_EXCEEDED", 0, err)
	}
	if err := protocol.ValidateJSON(payload, protocol.Limits{MaxBytes: len(payload)}); err != nil {
		return Manifest{}, clientError("INVALID_MANIFEST", 0, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var wire manifestWire
	if err := decoder.Decode(&wire); err != nil {
		return Manifest{}, clientError("INVALID_MANIFEST", 0, err)
	}
	return manifestFromWire(wire)
}

// NewManifest validates and snapshots generated manifest entries.
func NewManifest(operations ...ManifestOperation) (Manifest, error) {
	wire := manifestWire{
		Profile: ManifestProfile, Version: "1", ProtocolVersion: "1", CanonicalVersion: "c14n-1",
		Operations: make([]manifestOperationWire, len(operations)),
	}
	for index, operation := range operations {
		wire.Operations[index] = manifestOperationWire{
			Name: operation.Name, Kind: operation.Kind,
			Persisted: manifestPersistedWire{
				Algorithm: operation.Persisted.Algorithm, CanonicalVersion: operation.Persisted.CanonicalVersion, Digest: operation.Persisted.Digest,
			},
		}
	}
	return manifestFromWire(wire)
}

func manifestFromWire(wire manifestWire) (Manifest, error) {
	if wire.Profile != ManifestProfile || wire.Version != "1" || wire.ProtocolVersion != "1" || wire.CanonicalVersion != "c14n-1" || len(wire.Operations) == 0 {
		return Manifest{}, clientError("INVALID_MANIFEST", 0, errors.New("unsupported manifest contract"))
	}
	result := Manifest{operations: make(map[string]ManifestOperation, len(wire.Operations)), ordered: make([]string, 0, len(wire.Operations))}
	for _, entry := range wire.Operations {
		if !requestIdentifier.MatchString(entry.Name) || result.operations[entry.Name].Name != "" {
			return Manifest{}, clientError("INVALID_MANIFEST", 0, errors.New("invalid or duplicate manifest operation"))
		}
		persisted := protocol.PersistedReference{
			Algorithm: entry.Persisted.Algorithm, CanonicalVersion: entry.Persisted.CanonicalVersion, Digest: entry.Persisted.Digest,
		}
		request, err := NewPersistedOperation(persisted, entry.Name, entry.Kind)
		if err != nil || request.OperationKind() == "" {
			return Manifest{}, clientError("INVALID_MANIFEST", 0, errors.New("invalid persisted operation"))
		}
		operation := ManifestOperation{Name: entry.Name, Kind: entry.Kind, Persisted: persisted}
		result.operations[entry.Name] = operation
		result.ordered = append(result.ordered, entry.Name)
	}
	sort.Strings(result.ordered)
	return result, nil
}

func (m Manifest) Operations() []ManifestOperation {
	result := make([]ManifestOperation, 0, len(m.ordered))
	for _, name := range m.ordered {
		result = append(result, m.operations[name])
	}
	return result
}

func (m Manifest) Request(name string) (Request, error) {
	operation, found := m.operations[name]
	if !found {
		return Request{}, clientError("UNKNOWN_OPERATION", 0, errors.New("operation is absent from manifest"))
	}
	return NewPersistedOperation(operation.Persisted, operation.Name, operation.Kind)
}

func (m Manifest) CanonicalJSON() ([]byte, error) {
	operations := m.Operations()
	wire := manifestWire{
		Profile: ManifestProfile, Version: "1", ProtocolVersion: "1", CanonicalVersion: "c14n-1",
		Operations: make([]manifestOperationWire, len(operations)),
	}
	for index, operation := range operations {
		wire.Operations[index] = manifestOperationWire{
			Name: operation.Name, Kind: operation.Kind,
			Persisted: manifestPersistedWire{
				Algorithm: operation.Persisted.Algorithm, CanonicalVersion: operation.Persisted.CanonicalVersion, Digest: operation.Persisted.Digest,
			},
		}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, clientError("INVALID_MANIFEST", 0, err)
	}
	return protocol.CanonicalizeJSON(encoded, protocol.Limits{MaxBytes: int(defaultManifestBytes)})
}
