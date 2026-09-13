package schema_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/valksor/naatre/schema"
)

func TestExtensionDescriptorsCanonicalizeAndRemainImmutable(t *testing.T) {
	t.Parallel()
	extensions := []schema.ExtensionDescriptor{
		validExtension("org.example.zeta", "1.2.0", "org.example.zeta-1", "org.example.zeta.impl-1"),
		validExtension("com.example.alpha", "2.0.1", "com.example.alpha-2", "com.example.alpha.impl-2"),
	}
	extensions[0].Points = []schema.ExtensionPoint{schema.ExtensionResponse, schema.ExtensionValidation}
	extensions[0].Directives = []string{"zeta", "alpha"}
	extensions[0].Before = []string{"org.example.two", "org.example.one"}

	document, err := schema.ExportDocument(portableSchemaSnapshot(t), nil, nil, schema.ExportOptions{
		Revision: "extensions-r1", Extensions: extensions,
	})
	if err != nil {
		t.Fatalf("ExportDocument: %v", err)
	}
	got := document.Extensions()
	if len(got) != 2 || got[0].ID != "com.example.alpha" || got[1].ID != "org.example.zeta" {
		t.Fatalf("extension order = %#v", got)
	}
	if want := []string{"alpha", "zeta"}; !equalStrings(got[1].Directives, want) {
		t.Fatalf("directive order = %v, want %v", got[1].Directives, want)
	}
	canonical, err := document.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	got[0].Points[0] = schema.ExtensionExecution
	got[1].Directives[0] = "mutated"
	options := document.ExportOptions()
	options.Extensions[0].Conflicts = append(options.Extensions[0].Conflicts, "com.example.changed")
	again, _ := document.CanonicalJSON()
	if !bytes.Equal(canonical, again) {
		t.Fatal("extension descriptors mutated through accessors")
	}
}

func TestExtensionDescriptorValidationRejectsUnsafeContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*schema.ExtensionDescriptor)
		want   string
	}{
		{"invalid id", func(value *schema.ExtensionDescriptor) { value.ID = "example" }, "identifier"},
		{"id label ending in hyphen", func(value *schema.ExtensionDescriptor) { value.ID = "com.example-" }, "identifier"},
		{"reserved core id", func(value *schema.ExtensionDescriptor) { value.ID = "core.example" }, "reserved"},
		{"reserved naatre id", func(value *schema.ExtensionDescriptor) { value.ID = "naatre.example" }, "reserved"},
		{"invalid semver", func(value *schema.ExtensionDescriptor) { value.Version = "1" }, "semantic version"},
		{"invalid semver prerelease", func(value *schema.ExtensionDescriptor) { value.Version = "1.0.0-01" }, "semantic version"},
		{"invalid implementation", func(value *schema.ExtensionDescriptor) { value.Implementation = "build identity" }, "implementation identity"},
		{"invalid directive name", func(value *schema.ExtensionDescriptor) { value.Directives = []string{"$write"} }, "valid and unique"},
		{"nondeterministic planning", func(value *schema.ExtensionDescriptor) {
			value.Points = append(value.Points, schema.ExtensionPlanning)
			value.Deterministic = false
		}, "planning must be deterministic"},
		{"blank security", func(value *schema.ExtensionDescriptor) { value.Security = "  " }, "security implications"},
		{"unbounded cost", func(value *schema.ExtensionDescriptor) {
			value.CostBehavior = schema.ExtensionCostBounded
			value.MaxAdditionalCost = 0
		}, "bounded non-zero"},
		{"dynamic declared cost", func(value *schema.ExtensionDescriptor) {
			value.CostBehavior = schema.ExtensionCostDeclared
			value.MaxAdditionalCost = 1
		}, "cannot add dynamic cost"},
		{"write without execution", func(value *schema.ExtensionDescriptor) { value.SideEffects = "write" }, "require execution"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := validExtension("com.example.audit", "1.0.0", "com.example.audit-1", "com.example.audit.impl-1")
			test.mutate(&value)
			if err := schema.ValidateExtensionDescriptor(value); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateExtensionDescriptor() = %v, want %q", err, test.want)
			}
		})
	}
}

func TestExtensionDescriptorAcceptsSemVerHyphenIdentifiers(t *testing.T) {
	t.Parallel()
	descriptor := validExtension("com.example.audit", "1.0.0-alpha--beta+build--meta", "com.example.audit-1", "com.example.audit.impl-1")
	if err := schema.ValidateExtensionDescriptor(descriptor); err != nil {
		t.Fatalf("ValidateExtensionDescriptor: %v", err)
	}
}

func TestSchemaDocumentRejectsDuplicateExtensionIdentities(t *testing.T) {
	t.Parallel()
	base := validExtension("com.example.audit", "1.0.0", "com.example.audit-1", "com.example.audit.impl-1")
	for _, duplicate := range []schema.ExtensionDescriptor{
		base,
		validExtension("com.example.other", "1.0.0", base.Capability, "com.example.other.impl-1"),
	} {
		_, err := schema.ExportDocument(portableSchemaSnapshot(t), nil, nil, schema.ExportOptions{
			Revision: "duplicate-r1", Extensions: []schema.ExtensionDescriptor{base, duplicate},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate extension") {
			t.Fatalf("ExportDocument duplicate = %v", err)
		}
	}
}

func validExtension(id, version, capability, implementation string) schema.ExtensionDescriptor {
	return schema.ExtensionDescriptor{
		ExtensionReference: schema.ExtensionReference{ID: id, Version: version, Capability: capability},
		Implementation:     implementation, Points: []schema.ExtensionPoint{schema.ExtensionValidation},
		Deterministic: true, SideEffects: "none", CostBehavior: schema.ExtensionCostNone,
		Compatibility: schema.ChangeAdditive, Security: "does not access application data",
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
