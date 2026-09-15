package generator_test

import (
	"errors"
	"testing"

	"github.com/valksor/naatre/generator"
	"github.com/valksor/naatre/schema"
)

func TestGenerateJTDFailsBeforeArtifactForUnsupportedRuntimeSemantics(t *testing.T) {
	t.Parallel()
	input := []byte(`{"version":"1","canonicalVersion":"c14n-1","revision":"generator-jtd-r1","types":[{"id":"Result","name":"Result","kind":"object","output":true,"fields":[{"id":"Result.value","name":"value","type":"String"}]}],"operations":[{"id":"query.result","name":"result","kind":"query","output":"Result","effect":"read","authorizationPolicy":"public"}],"members":[]}`)
	document, err := schema.ParseDocument(input, schema.ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, report, err := generator.GenerateJTD(document, "Result")
	var mappingErr *schema.JTDError
	if len(artifact) != 0 || !errors.As(err, &mappingErr) || report.Exact || report.Diagnostics[0].Code != "JTD_OPERATION_METADATA_UNSUPPORTED" {
		t.Fatalf("GenerateJTD = %s, %#v, %v", artifact, report, err)
	}
}
