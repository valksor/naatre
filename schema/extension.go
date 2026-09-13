package schema

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/valksor/naatre/protocol"
)

type ExtensionPoint string

const (
	ExtensionASTMetadata ExtensionPoint = "ast-metadata"
	ExtensionValidation  ExtensionPoint = "validation"
	ExtensionPlanning    ExtensionPoint = "planning"
	ExtensionExecution   ExtensionPoint = "execution"
	ExtensionSchema      ExtensionPoint = "schema"
	ExtensionResponse    ExtensionPoint = "response"
)

type ExtensionCostBehavior string

const (
	ExtensionCostNone     ExtensionCostBehavior = "none"
	ExtensionCostDeclared ExtensionCostBehavior = "declared"
	ExtensionCostBounded  ExtensionCostBehavior = "bounded"
)

// ExtensionReference pins the semantics required by a document or persisted
// operation. Capability is version-specific and participates in document
// identity through Document.Requires.
type ExtensionReference struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Capability string `json:"capability"`
}

// ExtensionDescriptor is the portable discovery contract for one optional
// protocol/runtime extension. Callback code and mutable state are never part
// of schema identity.
type ExtensionDescriptor struct {
	ExtensionReference
	Implementation    string                `json:"implementation"`
	Points            []ExtensionPoint      `json:"points"`
	Directives        []string              `json:"directives,omitempty"`
	Deterministic     bool                  `json:"deterministic"`
	SideEffects       string                `json:"sideEffects"`
	CostBehavior      ExtensionCostBehavior `json:"costBehavior"`
	MaxAdditionalCost uint64                `json:"maxAdditionalCost,omitempty"`
	Compatibility     ChangeClassification  `json:"compatibility"`
	Security          string                `json:"security"`
	OptionalMetadata  bool                  `json:"optionalMetadata,omitempty"`
	Before            []string              `json:"before,omitempty"`
	After             []string              `json:"after,omitempty"`
	Conflicts         []string              `json:"conflicts,omitempty"`
}

func ValidateExtensionDescriptor(descriptor ExtensionDescriptor) error {
	if !validExtensionID(descriptor.ID) {
		return fmt.Errorf("invalid or reserved extension identifier %q", descriptor.ID)
	}
	if !protocol.ValidSemanticVersion(descriptor.Version) {
		return fmt.Errorf("extension %q has invalid semantic version %q", descriptor.ID, descriptor.Version)
	}
	if !typeIDPattern.MatchString(descriptor.Capability) || !typeIDPattern.MatchString(descriptor.Implementation) {
		return fmt.Errorf("extension %q requires a version-pinned capability and implementation identity", descriptor.ID)
	}
	if strings.HasPrefix(descriptor.Capability, "core.") || strings.HasPrefix(descriptor.Capability, "naatre.") ||
		strings.HasPrefix(descriptor.Implementation, "core.") || strings.HasPrefix(descriptor.Implementation, "naatre.") {
		return fmt.Errorf("extension %q uses a reserved capability or implementation identity", descriptor.ID)
	}
	if len(descriptor.Points) == 0 || strings.TrimSpace(descriptor.Security) == "" {
		return fmt.Errorf("extension %q requires extension points and security implications", descriptor.ID)
	}
	if descriptor.SideEffects != "none" && descriptor.SideEffects != "read" && descriptor.SideEffects != "write" {
		return fmt.Errorf("extension %q has invalid side effects %q", descriptor.ID, descriptor.SideEffects)
	}
	if descriptor.Compatibility != ChangeBreaking && descriptor.Compatibility != ChangeDangerous && descriptor.Compatibility != ChangeAdditive && descriptor.Compatibility != ChangeBehaviorOnly {
		return fmt.Errorf("extension %q has invalid compatibility %q", descriptor.ID, descriptor.Compatibility)
	}
	if err := validateExtensionCost(descriptor); err != nil {
		return err
	}
	seenPoints := make(map[ExtensionPoint]bool, len(descriptor.Points))
	for _, point := range descriptor.Points {
		if seenPoints[point] || !slices.Contains([]ExtensionPoint{ExtensionASTMetadata, ExtensionValidation, ExtensionPlanning, ExtensionExecution, ExtensionSchema, ExtensionResponse}, point) {
			return fmt.Errorf("extension %q has invalid or duplicate point %q", descriptor.ID, point)
		}
		seenPoints[point] = true
	}
	if seenPoints[ExtensionPlanning] && !descriptor.Deterministic {
		return fmt.Errorf("extension %q planning must be deterministic", descriptor.ID)
	}
	if descriptor.SideEffects != "none" && !seenPoints[ExtensionExecution] {
		return fmt.Errorf("extension %q side effects require execution", descriptor.ID)
	}
	if descriptor.OptionalMetadata && (len(descriptor.Points) != 1 || descriptor.Points[0] != ExtensionSchema ||
		descriptor.SideEffects != "none" || descriptor.CostBehavior != ExtensionCostNone || len(descriptor.Directives) != 0 ||
		len(descriptor.Before) != 0 || len(descriptor.After) != 0 || len(descriptor.Conflicts) != 0) {
		return fmt.Errorf("extension %q optional metadata must be inert schema metadata", descriptor.ID)
	}
	return validateExtensionRelations(descriptor)
}

func validExtensionID(id string) bool {
	return protocol.ValidExtensionID(id)
}

func validateExtensionCost(descriptor ExtensionDescriptor) error {
	switch descriptor.CostBehavior {
	case ExtensionCostNone:
		if descriptor.MaxAdditionalCost != 0 {
			return fmt.Errorf("extension %q with no cost behavior cannot add cost", descriptor.ID)
		}
	case ExtensionCostDeclared:
		if descriptor.MaxAdditionalCost != 0 {
			return fmt.Errorf("extension %q with declared cost cannot add dynamic cost", descriptor.ID)
		}
	case ExtensionCostBounded:
		if descriptor.MaxAdditionalCost == 0 || descriptor.MaxAdditionalCost > MaxDirectiveCost {
			return fmt.Errorf("extension %q requires a bounded non-zero maximum cost", descriptor.ID)
		}
	default:
		return fmt.Errorf("extension %q has invalid cost behavior %q", descriptor.ID, descriptor.CostBehavior)
	}
	return nil
}

func validateExtensionRelations(descriptor ExtensionDescriptor) error {
	seen := make(map[string]string)
	relations := []struct {
		name   string
		values []string
	}{{"before", descriptor.Before}, {"after", descriptor.After}, {"conflicts", descriptor.Conflicts}}
	for _, relation := range relations {
		for _, id := range relation.values {
			if !validExtensionID(id) || id == descriptor.ID {
				return fmt.Errorf("extension %q has invalid %s relation %q", descriptor.ID, relation.name, id)
			}
			if prior, exists := seen[id]; exists {
				return fmt.Errorf("extension %q repeats relation %q in %s and %s", descriptor.ID, id, prior, relation.name)
			}
			seen[id] = relation.name
		}
	}
	seenDirectives := make(map[string]bool, len(descriptor.Directives))
	for _, directive := range descriptor.Directives {
		if !directiveNamePattern.MatchString(directive) || seenDirectives[directive] {
			return errors.New("extension directives must be valid and unique")
		}
		seenDirectives[directive] = true
	}
	return nil
}

func CloneExtensionDescriptor(input ExtensionDescriptor) ExtensionDescriptor {
	input.Points = slices.Clone(input.Points)
	input.Directives = slices.Clone(input.Directives)
	input.Before = slices.Clone(input.Before)
	input.After = slices.Clone(input.After)
	input.Conflicts = slices.Clone(input.Conflicts)
	return input
}
