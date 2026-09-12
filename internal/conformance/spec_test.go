package conformance_test

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestCoreSpecificationClauseContract(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../spec/v1/core.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	clausePattern := regexp.MustCompile(`(?m)^- \*\*(CORE-([0-9]{3}))(?: [^:*]+)?:\*\*`)
	matches := clausePattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		t.Fatal("core specification contains no clauses")
	}
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		identifier := match[1]
		if seen[identifier] {
			t.Fatalf("duplicate core clause %s", identifier)
		}
		seen[identifier] = true
		number, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatal(err)
		}
		if number >= 100 {
			row := "| " + identifier + " |"
			if strings.Count(text, row) != 1 {
				t.Errorf("normative clause %s requires exactly one valid/invalid example row", identifier)
			}
		}
	}

	assertCoreVocabulary(t, text)
}

func TestSpecificationIndexLinksEveryNormativeDocument(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../spec/v1/README.md")
	if err != nil {
		t.Fatal(err)
	}
	index := string(content)
	for _, name := range []string{"core.md", "protocol.md", "schema.md", "language.md", "canonicalization.md", "security.md", "http.md", "persisted.md", "collections.md", "mutations.md"} {
		if !strings.Contains(index, "]("+name+")") {
			t.Errorf("specification index does not link %s", name)
		}
		if _, err := os.Stat("../../spec/v1/" + name); err != nil {
			t.Errorf("normative document %s: %v", name, err)
		}
	}
}

func assertCoreVocabulary(t *testing.T, text string) {
	t.Helper()
	vocabulary := map[string]string{
		"CORE-001": "Request",
		"CORE-002": "Document",
		"CORE-003": "Operation",
		"CORE-004": "Selection",
		"CORE-005": "Field",
		"CORE-006": "Call",
		"CORE-007": "Collection",
		"CORE-008": "Alias",
		"CORE-009": "Variable",
		"CORE-010": "Fragment",
		"CORE-011": "Directive",
		"CORE-012": "Capability",
		"CORE-013": "Profile",
		"CORE-014": "Missing",
		"CORE-015": "Error path",
		"CORE-016": "Query",
		"CORE-017": "Mutation",
		"CORE-018": "Subscription",
		"CORE-019": "Effect",
		"CORE-020": "Transport method",
		"CORE-021": "Partial data",
	}
	for identifier, term := range vocabulary {
		definition := fmt.Sprintf("**%s %s:**", identifier, term)
		if strings.Count(text, definition) != 1 {
			t.Errorf("vocabulary term %s requires exactly one definition", term)
		}
	}
}
