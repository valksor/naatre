package playground_test

import (
	"bytes"
	"os"
	"testing"

	"github.com/valksor/naatre/playground"
)

func TestPinnedPlaygroundConformanceEvidence(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../conformance/tooling/playground-mock.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := playground.ValidateProfileEvidence(content); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(content, []byte(playground.Issue55Revision), []byte("0000000000000000000000000000000000000000"), 1)
	if playground.ValidateProfileEvidence(tampered) == nil {
		t.Fatal("conformance validator accepted a changed dependency revision")
	}
}
