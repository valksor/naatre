package tooling

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const ManifestVersion = "naatre.operation-manifest-1"

type ManifestOperation struct {
	Name      string                 `json:"name"`
	Kind      protocol.OperationKind `json:"kind"`
	Document  json.RawMessage        `json:"document"`
	Persisted protocol.Digest        `json:"persisted"`
}

type OperationManifest struct {
	Profile    string              `json:"profile"`
	Version    string              `json:"version"`
	Operations []ManifestOperation `json:"operations"`
}

func BuildManifest(input []byte) (OperationManifest, error) {
	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		return OperationManifest{}, err
	}
	canonical := document.CanonicalJSON()
	digest, err := protocol.SemanticHash(protocol.DocumentHash, canonical)
	if err != nil {
		return OperationManifest{}, err
	}
	operations := document.Operations()
	manifest := OperationManifest{Profile: ManifestVersion, Version: "1", Operations: make([]ManifestOperation, len(operations))}
	for index, operation := range operations {
		manifest.Operations[index] = ManifestOperation{
			Name: operation.Name(), Kind: operation.Kind(), Document: append(json.RawMessage(nil), canonical...), Persisted: digest,
		}
	}
	return manifest, nil
}

func (m OperationManifest) CanonicalJSON() ([]byte, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return protocol.CanonicalizeJSON(encoded, protocol.Limits{})
}

func LoadManifest(input []byte) (OperationManifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var manifest OperationManifest
	if err := decoder.Decode(&manifest); err != nil {
		return OperationManifest{}, fmt.Errorf("decode operation manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return OperationManifest{}, errors.New("operation manifest contains trailing data")
	}
	if manifest.Profile != ManifestVersion || manifest.Version != "1" || len(manifest.Operations) == 0 {
		return OperationManifest{}, errors.New("invalid operation manifest version or inventory")
	}
	return manifest, nil
}

type CompatibilityReport struct {
	Version     string          `json:"version"`
	Compatible  bool            `json:"compatible"`
	Schema      protocol.Digest `json:"schema"`
	Operations  int             `json:"operations"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
}

// CheckCompatibility validates every registered manifest document against the
// proposed offline schema using a metadata-only registry.
func CheckCompatibility(schemaDocument schema.Document, manifest OperationManifest) CompatibilityReport {
	report := CompatibilityReport{Version: "naatre.compatibility-1", Compatible: true, Operations: len(manifest.Operations), Diagnostics: []Diagnostic{}}
	report.Schema, _ = schemaDocument.Hash()
	snapshot, err := runtime.NewPlanningSnapshot(schemaDocument)
	if err != nil {
		report.Compatible = false
		report.Diagnostics = append(report.Diagnostics, toolDiagnostic("schema", "TOOL_SCHEMA_INVALID", "", err))
		return report
	}
	for index, entry := range manifest.Operations {
		path := fmt.Sprintf("/operations/%d", index)
		document, decodeErr := protocol.DecodeDocument(entry.Document, protocol.Limits{})
		if decodeErr != nil {
			report.Diagnostics = append(report.Diagnostics, diagnosticsFromError(decodeErr)...)
			continue
		}
		digest, digestErr := protocol.SemanticHash(protocol.DocumentHash, document.CanonicalJSON())
		if digestErr != nil || digest != entry.Persisted {
			report.Diagnostics = append(report.Diagnostics, toolDiagnostic("manifest", "MANIFEST_DIGEST_MISMATCH", path+"/persisted", digestErr))
			continue
		}
		operationFound := false
		for _, operation := range document.Operations() {
			if operation.Name() == entry.Name && operation.Kind() == entry.Kind {
				operationFound = true
				break
			}
		}
		if !operationFound {
			report.Diagnostics = append(report.Diagnostics, toolDiagnostic("manifest", "MANIFEST_OPERATION_MISMATCH", path, nil))
			continue
		}
		request, requestErr := requestForDocument(document, entry.Name)
		if requestErr == nil {
			_, requestErr = runtime.Prepare(snapshot, request)
		}
		if requestErr != nil {
			report.Diagnostics = append(report.Diagnostics, diagnosticsFromError(requestErr)...)
		}
	}
	report.Compatible = len(report.Diagnostics) == 0
	return report
}
