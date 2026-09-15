package generator

import "github.com/valksor/naatre/schema"

// GenerateJTD produces the deterministic third-party schema artifact through
// the same fail-closed mapper used by validation and CLI workflows.
func GenerateJTD(document schema.Document, root schema.TypeID) ([]byte, schema.JTDFidelityReport, error) {
	return schema.ExportJTD(document, schema.JTDExportOptions{Root: root})
}
