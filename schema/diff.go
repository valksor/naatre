package schema

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/valksor/naatre/protocol"
)

// ChangeClassification describes the strongest compatibility effect of a
// portable schema change.
type ChangeClassification string

const (
	ChangeAdditive     ChangeClassification = "additive"
	ChangeBehaviorOnly ChangeClassification = "behavior-only"
	ChangeDangerous    ChangeClassification = "dangerous"
	ChangeBreaking     ChangeClassification = "breaking"
)

// SchemaChange is one stable-ID-addressed change. Before and After contain
// JSON values when the corresponding side exists.
type SchemaChange struct {
	Path           string               `json:"path"`
	Classification ChangeClassification `json:"classification"`
	Before         json.RawMessage      `json:"before,omitempty"`
	After          json.RawMessage      `json:"after,omitempty"`
}

// SchemaDiff is a deterministic compatibility report between two immutable
// documents.
type SchemaDiff struct {
	BeforeRevision string               `json:"beforeRevision"`
	AfterRevision  string               `json:"afterRevision"`
	Classification ChangeClassification `json:"classification,omitempty"`
	Changes        []SchemaChange       `json:"changes"`
}

// DiffDocuments compares declarations by stable identity rather than source
// order or display name.
func DiffDocuments(before, after Document) SchemaDiff {
	diff := SchemaDiff{BeforeRevision: before.wire.Revision, AfterRevision: after.wire.Revision}
	diffTypes(&diff, before.wire.Types, after.wire.Types)
	diffOperations(&diff, before.wire.Operations, after.wire.Operations)
	diffMembers(&diff, before.wire.Members, after.wire.Members)
	diffDirectives(&diff, before.wire.Directives, after.wire.Directives)
	diffExtensions(&diff, before.wire.Extensions, after.wire.Extensions)
	diffRetiredReuse(&diff, before.wire.Retired, after.wire)
	for _, declaration := range before.wire.Types {
		diffRetiredReuse(&diff, declaration.Retired, after.wire)
	}
	sort.Slice(diff.Changes, func(i, j int) bool { return diff.Changes[i].Path < diff.Changes[j].Path })
	return diff
}

func diffExtensions(diff *SchemaDiff, before, after []ExtensionDescriptor) {
	diffDescriptors(diff, "extension:", before, after,
		func(value ExtensionDescriptor) string { return value.ID },
		func(ExtensionDescriptor) ChangeClassification { return ChangeAdditive }, func(diff *SchemaDiff, left, right ExtensionDescriptor) {
			prefix := "extension:" + left.ID
			diff.scalar(prefix+"/version", ChangeBreaking, left.Version, right.Version)
			diff.scalar(prefix+"/capability", ChangeBreaking, left.Capability, right.Capability)
			diff.scalar(prefix+"/implementation", ChangeBreaking, left.Implementation, right.Implementation)
			diff.scalar(prefix+"/points", ChangeDangerous, left.Points, right.Points)
			diff.scalar(prefix+"/directives", ChangeDangerous, left.Directives, right.Directives)
			diff.scalar(prefix+"/deterministic", ChangeDangerous, left.Deterministic, right.Deterministic)
			diff.scalar(prefix+"/sideEffects", ChangeDangerous, left.SideEffects, right.SideEffects)
			diff.scalar(prefix+"/costBehavior", ChangeDangerous, left.CostBehavior, right.CostBehavior)
			diff.scalar(prefix+"/maxAdditionalCost", ChangeDangerous, left.MaxAdditionalCost, right.MaxAdditionalCost)
			diff.scalar(prefix+"/compatibility", ChangeDangerous, left.Compatibility, right.Compatibility)
			diff.scalar(prefix+"/security", ChangeDangerous, left.Security, right.Security)
			diff.scalar(prefix+"/optionalMetadata", ChangeDangerous, left.OptionalMetadata, right.OptionalMetadata)
			diff.scalar(prefix+"/before", ChangeDangerous, left.Before, right.Before)
			diff.scalar(prefix+"/after", ChangeDangerous, left.After, right.After)
			diff.scalar(prefix+"/conflicts", ChangeDangerous, left.Conflicts, right.Conflicts)
		})
}

func diffDirectives(diff *SchemaDiff, before, after []DirectiveDescriptor) {
	diffDescriptors(diff, "directive:", before, after,
		func(value DirectiveDescriptor) string { return value.ID },
		func(DirectiveDescriptor) ChangeClassification { return ChangeAdditive }, diffDirective)
}

func diffDirective(diff *SchemaDiff, before, after DirectiveDescriptor) {
	prefix := "directive:" + before.ID
	diff.scalar(prefix+"/name", ChangeBreaking, before.Name, after.Name)
	diff.scalar(prefix+"/version", ChangeBreaking, before.Version, after.Version)
	diff.scalar(prefix+"/capability", ChangeBreaking, before.Capability, after.Capability)
	diff.directionalBool(prefix+"/repeatable", before.Repeatable, after.Repeatable)
	diffDirectiveLocations(diff, prefix+"/locations", before.Locations, after.Locations)
	diffDirectiveArguments(diff, before.Arguments, after.Arguments)
	diff.scalar(prefix+"/phases", ChangeDangerous, before.Phases, after.Phases)
	diff.scalar(prefix+"/effect", ChangeDangerous, before.Effect, after.Effect)
	diff.scalar(prefix+"/cost", ChangeDangerous, before.Cost, after.Cost)
	diff.scalar(prefix+"/deterministic", ChangeDangerous, before.Deterministic, after.Deterministic)
	diff.scalar(prefix+"/compatibility", ChangeDangerous, before.Compatibility, after.Compatibility)
	diff.scalar(prefix+"/description", ChangeBehaviorOnly, before.Description, after.Description)
	diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, before.Deprecation, after.Deprecation)
	diff.scalar(prefix+"/capabilities", ChangeDangerous, before.Capabilities, after.Capabilities)
	diff.scalar(prefix+"/traits", ChangeDangerous, before.Traits, after.Traits)
	diff.scalar(prefix+"/source", ChangeBehaviorOnly, before.Source, after.Source)
}

func diffDirectiveLocations(diff *SchemaDiff, path string, before, after []protocol.SelectionKind) {
	if reflect.DeepEqual(before, after) {
		return
	}
	afterSet := make(map[protocol.SelectionKind]bool, len(after))
	for _, location := range after {
		afterSet[location] = true
	}
	classification := ChangeAdditive
	for _, location := range before {
		if !afterSet[location] {
			classification = ChangeBreaking
			break
		}
	}
	diff.add(path, classification, before, after)
}

func diffDirectiveArguments(diff *SchemaDiff, before, after []DirectiveArgumentDescriptor) {
	diffDescriptors(diff, "directive-argument:", before, after,
		func(value DirectiveArgumentDescriptor) string { return value.ID },
		func(value DirectiveArgumentDescriptor) ChangeClassification {
			if value.Required && len(value.Default) == 0 {
				return ChangeBreaking
			}
			return ChangeAdditive
		}, func(diff *SchemaDiff, left, right DirectiveArgumentDescriptor) {
			prefix := "directive-argument:" + left.ID
			diff.scalar(prefix+"/name", ChangeBreaking, left.Name, right.Name)
			diff.scalar(prefix+"/type", ChangeBreaking, left.Type, right.Type)
			diff.directionalBool(prefix+"/required", left.Required, right.Required)
			diff.directionalBool(prefix+"/nullable", left.Nullable, right.Nullable)
			diff.scalar(prefix+"/default", ChangeBehaviorOnly, left.Default, right.Default)
			diff.scalar(prefix+"/description", ChangeBehaviorOnly, left.Description, right.Description)
		})
}

func diffTypes(diff *SchemaDiff, before, after []TypeDeclaration) {
	diffDescriptors(diff, "type:", before, after,
		func(value TypeDeclaration) string { return string(value.ID) },
		func(TypeDeclaration) ChangeClassification { return ChangeAdditive }, diffType)
}

func diffType(diff *SchemaDiff, before, after TypeDeclaration) {
	prefix := "type:" + string(before.ID)
	diff.scalar(prefix+"/name", ChangeBreaking, before.Name, after.Name)
	diff.scalar(prefix+"/kind", ChangeBreaking, before.Kind, after.Kind)
	diff.scalar(prefix+"/input", ChangeBreaking, before.Input, after.Input)
	diff.scalar(prefix+"/output", ChangeBreaking, before.Output, after.Output)
	if before.Open != after.Open {
		classification := ChangeAdditive
		if before.Open && !after.Open {
			classification = ChangeBreaking
		}
		diff.add(prefix+"/open", classification, before.Open, after.Open)
	}
	diff.scalar(prefix+"/element", ChangeBreaking, before.Element, after.Element)
	diff.directionalBool(prefix+"/elementNullable", before.ElementNullable, after.ElementNullable)
	if before.MaxDepth != after.MaxDepth {
		classification := ChangeBehaviorOnly
		if before.MaxDepth == 0 || (after.MaxDepth != 0 && after.MaxDepth < before.MaxDepth) {
			classification = ChangeDangerous
		}
		diff.add(prefix+"/maxDepth", classification, before.MaxDepth, after.MaxDepth)
	}
	diff.scalar(prefix+"/scalar", ChangeDangerous, before.Scalar, after.Scalar)
	diff.scalar(prefix+"/description", ChangeBehaviorOnly, before.Description, after.Description)
	diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, before.Deprecation, after.Deprecation)
	diff.scalar(prefix+"/entity", ChangeDangerous, before.Entity, after.Entity)
	diff.scalar(prefix+"/capabilities", ChangeDangerous, before.Capabilities, after.Capabilities)
	diff.scalar(prefix+"/traits", ChangeDangerous, before.Traits, after.Traits)
	diff.scalar(prefix+"/source", ChangeBehaviorOnly, before.Source, after.Source)
	diffFields(diff, before, after)
	diffEnumMembers(diff, before, after)
	diffVariantMembers(diff, before, after)
}

func diffFields(diff *SchemaDiff, before, after TypeDeclaration) {
	diffDescriptors(diff, "field:", before.Fields, after.Fields,
		func(value FieldDeclaration) string { return value.ID },
		func(right FieldDeclaration) ChangeClassification {
			classification := ChangeAdditive
			if after.Kind == InputObjectType && right.Required && len(right.Default) == 0 {
				classification = ChangeBreaking
			}
			return classification
		}, diffField)
}

func diffField(diff *SchemaDiff, before, after FieldDeclaration) {
	prefix := "field:" + before.ID
	diff.scalar(prefix+"/name", ChangeBreaking, before.Name, after.Name)
	diff.scalar(prefix+"/type", ChangeBreaking, before.Type, after.Type)
	if before.Required != after.Required {
		classification := ChangeAdditive
		if !before.Required && after.Required && len(after.Default) == 0 {
			classification = ChangeBreaking
		}
		diff.add(prefix+"/required", classification, before.Required, after.Required)
	}
	diff.directionalBool(prefix+"/nullable", before.Nullable, after.Nullable)
	diff.scalar(prefix+"/default", ChangeBehaviorOnly, before.Default, after.Default)
	diff.scalar(prefix+"/description", ChangeBehaviorOnly, before.Description, after.Description)
	diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, before.Deprecation, after.Deprecation)
	diff.scalar(prefix+"/cost", ChangeDangerous, before.Cost, after.Cost)
	diff.scalar(prefix+"/traits", ChangeDangerous, before.Traits, after.Traits)
	diff.scalar(prefix+"/source", ChangeBehaviorOnly, before.Source, after.Source)
}

func diffEnumMembers(diff *SchemaDiff, before, after TypeDeclaration) {
	diffDescriptors(diff, "enum:", before.EnumMembers, after.EnumMembers,
		func(value EnumMemberDescriptor) string { return value.ID },
		func(EnumMemberDescriptor) ChangeClassification {
			classification := ChangeDangerous
			if after.Open {
				classification = ChangeAdditive
			}
			return classification
		}, func(diff *SchemaDiff, left, right EnumMemberDescriptor) {
			prefix := "enum:" + left.ID
			diff.scalar(prefix+"/name", ChangeBreaking, left.Name, right.Name)
			diff.scalar(prefix+"/description", ChangeBehaviorOnly, left.Description, right.Description)
			diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, left.Deprecation, right.Deprecation)
			diff.scalar(prefix+"/traits", ChangeDangerous, left.Traits, right.Traits)
			diff.scalar(prefix+"/source", ChangeBehaviorOnly, left.Source, right.Source)
		})
}

func diffVariantMembers(diff *SchemaDiff, before, after TypeDeclaration) {
	diffDescriptors(diff, "variant:", before.VariantMembers, after.VariantMembers,
		func(value VariantMemberDescriptor) string { return value.ID },
		func(VariantMemberDescriptor) ChangeClassification {
			classification := ChangeDangerous
			if after.Open {
				classification = ChangeAdditive
			}
			return classification
		}, func(diff *SchemaDiff, left, right VariantMemberDescriptor) {
			prefix := "variant:" + left.ID
			diff.scalar(prefix+"/type", ChangeBreaking, left.Type, right.Type)
			diff.scalar(prefix+"/description", ChangeBehaviorOnly, left.Description, right.Description)
			diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, left.Deprecation, right.Deprecation)
			diff.scalar(prefix+"/traits", ChangeDangerous, left.Traits, right.Traits)
			diff.scalar(prefix+"/source", ChangeBehaviorOnly, left.Source, right.Source)
		})
}

func diffOperations(diff *SchemaDiff, before, after []OperationDescriptor) {
	diffDescriptors(diff, "operation:", before, after,
		func(value OperationDescriptor) string { return value.ID },
		func(OperationDescriptor) ChangeClassification { return ChangeAdditive },
		func(diff *SchemaDiff, left, right OperationDescriptor) {
			prefix := "operation:" + left.ID
			diffCallable(diff, prefix, callableFromOperation(left), callableFromOperation(right))
			diff.scalar(prefix+"/idempotency", ChangeDangerous, left.Idempotency, right.Idempotency)
			diff.scalar(prefix+"/parallelMutation", ChangeDangerous, left.ParallelMutation, right.ParallelMutation)
		})
}

func diffMembers(diff *SchemaDiff, before, after []MemberDescriptor) {
	diffDescriptors(diff, "member:", before, after,
		func(value MemberDescriptor) string { return value.ID },
		func(MemberDescriptor) ChangeClassification { return ChangeAdditive },
		func(diff *SchemaDiff, left, right MemberDescriptor) {
			prefix := "member:" + left.ID
			diff.scalar(prefix+"/owner", ChangeBreaking, left.Owner, right.Owner)
			diff.scalar(prefix+"/kind", ChangeBreaking, left.Kind, right.Kind)
			diffCallable(diff, prefix, callableFromMember(left), callableFromMember(right))
			diff.scalar(prefix+"/idempotency", ChangeDangerous, left.Idempotency, right.Idempotency)
			diff.scalar(prefix+"/parallelMutation", ChangeDangerous, left.ParallelMutation, right.ParallelMutation)
		})
}

type callableContract struct {
	Name, Description, Effect, ThreadSafety, Batching, Transaction, Authorization string
	Kind                                                                          any
	Input, Output                                                                 TypeID
	InputNullable, OutputNullable, Deterministic, Cacheable, RetrySafe            bool
	Deprecation                                                                   *Deprecation
	Cost                                                                          uint64
	Collection                                                                    *CollectionDescriptor
	Capabilities                                                                  []string
	Traits                                                                        []TraitDescriptor
	Source                                                                        *SourceMetadata
}

func callableFromOperation(value OperationDescriptor) callableContract {
	return callableContract{
		Name: value.Name, Kind: value.Kind, Input: value.Input, InputNullable: value.InputNullable,
		Output: value.Output, OutputNullable: value.OutputNullable, Description: value.Description,
		Deprecation: value.Deprecation, Effect: value.Effect, Deterministic: value.Deterministic,
		Cacheable: value.Cacheable, RetrySafe: value.RetrySafe, ThreadSafety: value.ThreadSafety,
		Batching: value.Batching, Transaction: value.Transaction, Authorization: value.AuthorizationPolicy,
		Cost: value.Cost, Collection: value.Collection, Capabilities: value.Capabilities, Traits: value.Traits, Source: value.Source,
	}
}

func callableFromMember(value MemberDescriptor) callableContract {
	contract := callableFromOperation(OperationDescriptor{
		Name: value.Name, Input: value.Input, InputNullable: value.InputNullable,
		Output: value.Output, OutputNullable: value.OutputNullable, Description: value.Description,
		Deprecation: value.Deprecation, Effect: value.Effect, Deterministic: value.Deterministic,
		Cacheable: value.Cacheable, RetrySafe: value.RetrySafe, ThreadSafety: value.ThreadSafety,
		Batching: value.Batching, Transaction: value.Transaction, AuthorizationPolicy: value.AuthorizationPolicy,
		Cost: value.Cost, Collection: value.Collection, Capabilities: value.Capabilities, Traits: value.Traits, Source: value.Source,
	})
	contract.Kind = ""
	return contract
}

func diffCallable(diff *SchemaDiff, prefix string, before, after callableContract) {
	diff.scalar(prefix+"/name", ChangeBreaking, before.Name, after.Name)
	diff.scalar(prefix+"/kind", ChangeBreaking, before.Kind, after.Kind)
	diff.scalar(prefix+"/input", ChangeBreaking, before.Input, after.Input)
	diff.directionalBool(prefix+"/inputNullable", before.InputNullable, after.InputNullable)
	diff.scalar(prefix+"/output", ChangeBreaking, before.Output, after.Output)
	diff.directionalBool(prefix+"/outputNullable", before.OutputNullable, after.OutputNullable)
	diff.scalar(prefix+"/description", ChangeBehaviorOnly, before.Description, after.Description)
	diff.scalar(prefix+"/deprecation", ChangeBehaviorOnly, before.Deprecation, after.Deprecation)
	diff.scalar(prefix+"/effect", ChangeDangerous, before.Effect, after.Effect)
	diff.scalar(prefix+"/deterministic", ChangeDangerous, before.Deterministic, after.Deterministic)
	diff.scalar(prefix+"/cacheable", ChangeDangerous, before.Cacheable, after.Cacheable)
	diff.scalar(prefix+"/retrySafe", ChangeDangerous, before.RetrySafe, after.RetrySafe)
	diff.scalar(prefix+"/threadSafety", ChangeDangerous, before.ThreadSafety, after.ThreadSafety)
	diff.scalar(prefix+"/batching", ChangeDangerous, before.Batching, after.Batching)
	diff.scalar(prefix+"/transaction", ChangeDangerous, before.Transaction, after.Transaction)
	diff.scalar(prefix+"/authorizationPolicy", ChangeDangerous, before.Authorization, after.Authorization)
	diff.scalar(prefix+"/cost", ChangeDangerous, before.Cost, after.Cost)
	diffCollection(diff, prefix+"/collection", before.Collection, after.Collection)
	diff.scalar(prefix+"/capabilities", ChangeDangerous, before.Capabilities, after.Capabilities)
	diff.scalar(prefix+"/traits", ChangeDangerous, before.Traits, after.Traits)
	diff.scalar(prefix+"/source", ChangeBehaviorOnly, before.Source, after.Source)
}

func diffCollection(diff *SchemaDiff, path string, before, after *CollectionDescriptor) {
	if reflect.DeepEqual(before, after) {
		return
	}
	if before == nil {
		diff.add(path, ChangeAdditive, before, after)
		return
	}
	if after == nil {
		diff.add(path, ChangeBreaking, before, after)
		return
	}
	maximumClassification := ChangeAdditive
	if after.MaxPageSize < before.MaxPageSize {
		maximumClassification = ChangeBreaking
	}
	if before.MaxPageSize != after.MaxPageSize {
		diff.add(path+"/maxPageSize", maximumClassification, before.MaxPageSize, after.MaxPageSize)
	}
	diff.scalar(path+"/totalCountCost", ChangeDangerous, before.TotalCountCost, after.TotalCountCost)
}

func diffRetiredReuse(diff *SchemaDiff, retired []RetiredIdentity, after documentWire) {
	active := make(map[string]bool)
	for _, declaration := range after.Types {
		active[string(declaration.ID)] = true
		active[declaration.Name] = true
		for _, field := range declaration.Fields {
			active[field.ID] = true
			active[field.Name] = true
		}
		for _, member := range declaration.EnumMembers {
			active[member.ID] = true
			active[member.Name] = true
		}
		for _, member := range declaration.VariantMembers {
			active[member.ID] = true
			active[string(member.Type)] = true
		}
	}
	for _, operation := range after.Operations {
		active[operation.ID] = true
		active[operation.Name] = true
	}
	for _, member := range after.Members {
		active[member.ID] = true
		active[member.Name] = true
	}
	for _, identity := range retired {
		if active[identity.ID] || (identity.Name != "" && active[identity.Name]) {
			diff.add("identity:"+identity.ID+"/reuse", ChangeBreaking, identity, identity.ID)
		}
	}
}

func (diff *SchemaDiff) directionalBool(path string, before, after bool) {
	if before == after {
		return
	}
	classification := ChangeAdditive
	if before && !after {
		classification = ChangeBreaking
	}
	diff.add(path, classification, before, after)
}

func (diff *SchemaDiff) scalar(path string, classification ChangeClassification, before, after any) {
	if !reflect.DeepEqual(before, after) {
		diff.add(path, classification, before, after)
	}
}

func (diff *SchemaDiff) add(path string, classification ChangeClassification, before, after any) {
	if changeSeverity(classification) > changeSeverity(diff.Classification) {
		diff.Classification = classification
	}
	diff.Changes = append(diff.Changes, SchemaChange{
		Path: path, Classification: classification, Before: changeJSON(before), After: changeJSON(after),
	})
}

func changeSeverity(classification ChangeClassification) int {
	switch classification {
	case ChangeBreaking:
		return 4
	case ChangeDangerous:
		return 3
	case ChangeBehaviorOnly:
		return 2
	case ChangeAdditive:
		return 1
	default:
		return 0
	}
}

func changeJSON(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

func diffDescriptors[T any](diff *SchemaDiff, prefix string, before, after []T, identifier func(T) string,
	added func(T) ChangeClassification, compare func(*SchemaDiff, T, T),
) {
	old := make(map[string]T, len(before))
	for index := range before {
		old[identifier(before[index])] = before[index]
	}
	current := make(map[string]T, len(after))
	for index := len(after) - 1; index >= 0; index-- {
		current[identifier(after[index])] = after[index]
	}
	for _, id := range sortedDescriptorIDs(old, current) {
		left, had := old[id]
		right, has := current[id]
		switch {
		case !has:
			diff.add(prefix+id, ChangeBreaking, left, nil)
		case !had:
			diff.add(prefix+id, added(right), nil, right)
		default:
			compare(diff, left, right)
		}
	}
}
func sortedDescriptorIDs[T any](left, right map[string]T) []string {
	all := make(map[string]bool, len(left)+len(right))
	for id := range left {
		all[id] = true
	}
	for id := range right {
		all[id] = true
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
