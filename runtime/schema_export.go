package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/valksor/naatre/schema"
)

// ExportSchema converts the frozen type and handler registries into the
// canonical, language-neutral schema authority.
func (s Snapshot) ExportSchema(options schema.ExportOptions) (schema.Document, error) {
	options.Directives = s.DirectiveDescriptors()
	options.Extensions = s.ExtensionDescriptors()
	descriptors := s.Descriptors()
	operations := make([]schema.OperationDescriptor, 0, len(descriptors))
	members := make([]schema.MemberDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Scope == RootScope {
			operations = append(operations, portableOperation(descriptor))
		} else {
			members = append(members, portableMember(descriptor))
		}
	}
	return schema.ExportDocument(s.types, operations, members, options)
}

// ValidateSchema proves that a proposed portable schema is exactly the
// manifest implemented by this frozen registry, including behavior metadata.
func (s Snapshot) ValidateSchema(document schema.Document) error {
	exported, err := s.ExportSchema(document.ExportOptions())
	if err != nil {
		return fmt.Errorf("export runtime schema: %w", err)
	}
	expected, err := document.CanonicalJSON()
	if err != nil {
		return err
	}
	actual, err := exported.CanonicalJSON()
	if err != nil {
		return err
	}
	if bytes.Equal(expected, actual) {
		return nil
	}
	diff := schema.DiffDocuments(document, exported)
	if len(diff.Changes) != 0 {
		change := diff.Changes[0]
		return fmt.Errorf("schema manifest does not match runtime snapshot at %s (%s)", change.Path, change.Classification)
	}
	return fmt.Errorf("schema manifest does not match runtime snapshot")
}

func portableOperation(descriptor Descriptor) schema.OperationDescriptor {
	metadata := descriptor.Metadata
	return schema.OperationDescriptor{
		ID: portableDescriptorID(descriptor), Name: descriptor.Name, Kind: descriptor.Kind,
		Input: descriptor.Input, InputNullable: descriptor.InputNullable,
		Output: descriptor.Output, OutputNullable: descriptor.OutputNullable,
		Description: descriptor.Description, Deprecation: portableDeprecation(descriptor),
		Effect: string(metadata.Effect), Deterministic: metadata.Deterministic,
		Cacheable: metadata.Cacheable, RetrySafe: metadata.RetrySafe,
		ThreadSafety: string(metadata.ThreadSafety), Batching: string(metadata.Batching),
		Transaction: string(metadata.Transaction), AuthorizationPolicy: metadata.AuthorizationPolicy,
		Idempotency: string(metadata.Idempotency), Cost: metadata.Cost, ParallelMutation: metadata.ParallelMutation,
		Collection:   portableCollection(metadata.Collection),
		Capabilities: slices.Clone(descriptor.Capabilities), Traits: cloneRuntimeTraits(descriptor.Traits),
		Source: cloneRuntimeSource(descriptor.Source),
	}
}

func portableMember(descriptor Descriptor) schema.MemberDescriptor {
	metadata := descriptor.Metadata
	return schema.MemberDescriptor{
		ID: portableDescriptorID(descriptor), Name: descriptor.Name, Owner: descriptor.Owner, Kind: string(descriptor.Member),
		Input: descriptor.Input, InputNullable: descriptor.InputNullable,
		Output: descriptor.Output, OutputNullable: descriptor.OutputNullable,
		Description: descriptor.Description, Deprecation: portableDeprecation(descriptor),
		Effect: string(metadata.Effect), Deterministic: metadata.Deterministic,
		Cacheable: metadata.Cacheable, RetrySafe: metadata.RetrySafe,
		ThreadSafety: string(metadata.ThreadSafety), Batching: string(metadata.Batching),
		Transaction: string(metadata.Transaction), AuthorizationPolicy: metadata.AuthorizationPolicy,
		Idempotency: string(metadata.Idempotency), Cost: metadata.Cost, ParallelMutation: metadata.ParallelMutation,
		Collection:   portableCollection(metadata.Collection),
		Capabilities: slices.Clone(descriptor.Capabilities), Traits: cloneRuntimeTraits(descriptor.Traits),
		Source: cloneRuntimeSource(descriptor.Source),
	}
}

func portableCollection(metadata *CollectionMetadata) *schema.CollectionDescriptor {
	if metadata == nil {
		return nil
	}
	return &schema.CollectionDescriptor{MaxPageSize: metadata.MaxPageSize, TotalCountCost: metadata.TotalCountCost}
}

func portableDescriptorID(descriptor Descriptor) string {
	if descriptor.ID != "" {
		return descriptor.ID
	}
	if descriptor.Scope == RootScope {
		return string(descriptor.Kind) + "." + descriptor.Name
	}
	return string(descriptor.Owner) + "." + descriptor.Name + ".resolver"
}

func portableDeprecation(descriptor Descriptor) *schema.Deprecation {
	if descriptor.Deprecation != nil {
		result := *descriptor.Deprecation
		return &result
	}
	if descriptor.Metadata.Deprecation != "" {
		return &schema.Deprecation{Reason: descriptor.Metadata.Deprecation}
	}
	return nil
}

func cloneRuntimeDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Capabilities = slices.Clone(descriptor.Capabilities)
	descriptor.Traits = cloneRuntimeTraits(descriptor.Traits)
	descriptor.Source = cloneRuntimeSource(descriptor.Source)
	if descriptor.Deprecation != nil {
		deprecation := *descriptor.Deprecation
		descriptor.Deprecation = &deprecation
	}
	if descriptor.Metadata.Collection != nil {
		collection := *descriptor.Metadata.Collection
		descriptor.Metadata.Collection = &collection
	}
	return descriptor
}

func cloneRuntimeTraits(input []schema.TraitDescriptor) []schema.TraitDescriptor {
	result := make([]schema.TraitDescriptor, len(input))
	for index, trait := range input {
		result[index] = trait
		result[index].Value = append(json.RawMessage(nil), trait.Value...)
	}
	return result
}

func cloneRuntimeSource(input *schema.SourceMetadata) *schema.SourceMetadata {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}
