package tooling

import (
	"bytes"
	"encoding/json"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// FormatDocument returns readable JSON while retaining the canonical AST's
// ordered arrays and literals. Only nonsemantic object-key order and whitespace
// can change.
func FormatDocument(input []byte) ([]byte, error) {
	document, err := protocol.DecodeDocument(input, protocol.Limits{})
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := json.Indent(&output, document.CanonicalJSON(), "", "  "); err != nil {
		return nil, err
	}
	output.WriteByte('\n')
	return output.Bytes(), nil
}

// CanonicalizeDocument validates and returns the exact identity payload.
func CanonicalizeDocument(input []byte) ([]byte, error) {
	return protocol.CanonicalizeDocument(input, protocol.Limits{})
}

// HashDocument returns the schema-free semantic document identity.
func HashDocument(input []byte) (protocol.Digest, error) {
	canonical, err := CanonicalizeDocument(input)
	if err != nil {
		return protocol.Digest{}, err
	}
	return protocol.SemanticHash(protocol.DocumentHash, canonical)
}

// ExportSchema validates and returns canonical offline schema-bundle bytes.
func ExportSchema(input []byte) ([]byte, error) {
	document, err := schema.ParseDocument(input, schema.ImportOptions{})
	if err != nil {
		return nil, err
	}
	return document.CanonicalJSON()
}

// DiffSchemas compares two portable schema bundles by stable identity.
func DiffSchemas(before, after []byte) (schema.SchemaDiff, error) {
	left, err := schema.ParseDocument(before, schema.ImportOptions{})
	if err != nil {
		return schema.SchemaDiff{}, err
	}
	right, err := schema.ParseDocument(after, schema.ImportOptions{})
	if err != nil {
		return schema.SchemaDiff{}, err
	}
	return schema.DiffDocuments(left, right), nil
}
