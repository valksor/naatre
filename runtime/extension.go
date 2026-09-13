package runtime

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// RegisterExtension records one immutable, version-pinned extension contract.
// Runtime behavior is supplied only by named directives in the descriptor, so
// an extension receives no mutable plan or registry handle.
func (r *Registry) RegisterExtension(descriptor schema.ExtensionDescriptor) error {
	prepared, err := prepareExtensionDescriptor(r.types, descriptor)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	if _, exists := r.extensions[prepared.ID]; exists {
		return fmt.Errorf("%w: extension identity %q", ErrDuplicateRegistration, prepared.ID)
	}
	for _, existing := range r.extensions {
		if existing.Capability == prepared.Capability {
			return fmt.Errorf("%w: extension capability %q", ErrDuplicateRegistration, prepared.Capability)
		}
	}
	r.extensions[prepared.ID] = prepared
	return nil
}

func prepareExtensionDescriptor(types schema.Snapshot, descriptor schema.ExtensionDescriptor) (schema.ExtensionDescriptor, error) {
	if err := schema.ValidateExtensionDescriptor(descriptor); err != nil {
		return schema.ExtensionDescriptor{}, err
	}
	document, err := schema.ExportDocument(types, nil, nil, schema.ExportOptions{
		Revision: "extension-registration", Extensions: []schema.ExtensionDescriptor{descriptor},
	})
	if err != nil {
		return schema.ExtensionDescriptor{}, fmt.Errorf("extension %q is not portable: %w", descriptor.ID, err)
	}
	return document.Extensions()[0], nil
}

func freezeExtensions(extensions map[string]schema.ExtensionDescriptor, input map[string]registeredDirective) ([]schema.ExtensionDescriptor, map[string]registeredDirective, error) {
	ordered, err := orderExtensions(extensions)
	if err != nil {
		return nil, nil, err
	}
	directives := cloneRegisteredDirectives(input)
	for _, descriptor := range ordered {
		for _, name := range descriptor.Directives {
			definition, exists := directives[name]
			if !exists || definition.standard {
				return nil, nil, fmt.Errorf("extension %q references unregistered custom directive %q", descriptor.ID, name)
			}
			if definition.extensionID != "" {
				return nil, nil, fmt.Errorf("directive %q is owned by extensions %q and %q", name, definition.extensionID, descriptor.ID)
			}
			if err := validateExtensionDirective(descriptor, definition); err != nil {
				return nil, nil, err
			}
			definition.extensionID = descriptor.ID
			definition.extensionMaxAdditionalCost = descriptor.MaxAdditionalCost
			directives[name] = definition
		}
	}
	return ordered, directives, nil
}

func validateExtensionDirective(extension schema.ExtensionDescriptor, directive registeredDirective) error {
	if directive.Descriptor.Capability != extension.Capability {
		return fmt.Errorf("extension %q directive %q requires capability %q, want %q", extension.ID, directive.Descriptor.Name, directive.Descriptor.Capability, extension.Capability)
	}
	if directive.Descriptor.Effect == string(WriteEffect) && extension.SideEffects != "write" {
		return fmt.Errorf("extension %q directive %q has undeclared write effects", extension.ID, directive.Descriptor.Name)
	}
	if extension.SideEffects == "write" && directive.Descriptor.Effect != string(WriteEffect) {
		return fmt.Errorf("extension %q directive %q does not expose its declared write effect", extension.ID, directive.Descriptor.Name)
	}
	if extension.CostBehavior == schema.ExtensionCostNone && directive.Descriptor.Cost != 0 {
		return fmt.Errorf("extension %q directive %q has undeclared cost", extension.ID, directive.Descriptor.Name)
	}
	for _, phase := range directive.Descriptor.Phases {
		point := extensionPointForDirectivePhase(phase)
		if point != "" && !slices.Contains(extension.Points, point) {
			return fmt.Errorf("extension %q directive %q uses undeclared point %q", extension.ID, directive.Descriptor.Name, point)
		}
	}
	return nil
}

func extensionPointForDirectivePhase(phase schema.DirectivePhase) schema.ExtensionPoint {
	switch phase {
	case schema.DirectiveValidation:
		return schema.ExtensionValidation
	case schema.DirectivePlanning:
		return schema.ExtensionPlanning
	case schema.DirectiveExecution:
		return schema.ExtensionExecution
	case schema.DirectiveResponse:
		return schema.ExtensionResponse
	default:
		return ""
	}
}

func orderExtensions(extensions map[string]schema.ExtensionDescriptor) ([]schema.ExtensionDescriptor, error) {
	ids := make([]string, 0, len(extensions))
	for id := range extensions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	edges := make(map[string]map[string]bool, len(ids))
	indegree := make(map[string]int, len(ids))
	for _, id := range ids {
		edges[id] = make(map[string]bool)
		indegree[id] = 0
	}
	addEdge := func(from, to string) {
		if !edges[from][to] {
			edges[from][to] = true
			indegree[to]++
		}
	}
	for _, id := range ids {
		descriptor := extensions[id]
		for _, conflict := range descriptor.Conflicts {
			if _, exists := extensions[conflict]; exists {
				return nil, fmt.Errorf("extensions %q and %q conflict", id, conflict)
			}
		}
		for _, before := range descriptor.Before {
			if _, exists := extensions[before]; exists {
				addEdge(id, before)
			}
		}
		for _, after := range descriptor.After {
			if _, exists := extensions[after]; exists {
				addEdge(after, id)
			}
		}
	}
	ordered := make([]schema.ExtensionDescriptor, 0, len(ids))
	for len(ordered) < len(ids) {
		selected := ""
		for _, id := range ids {
			if indegree[id] == 0 {
				selected = id
				break
			}
		}
		if selected == "" {
			return nil, errors.New("extension ordering contains a cycle")
		}
		indegree[selected] = -1
		ordered = append(ordered, schema.CloneExtensionDescriptor(extensions[selected]))
		for target := range edges[selected] {
			indegree[target]--
		}
	}
	return ordered, nil
}

// ExtensionDescriptors returns the frozen extension contracts in their
// deterministic before/after order.
func (s Snapshot) ExtensionDescriptors() []schema.ExtensionDescriptor {
	result := make([]schema.ExtensionDescriptor, len(s.extensions))
	for index := range s.extensions {
		result[index] = schema.CloneExtensionDescriptor(s.extensions[index])
	}
	return result
}

func (s Snapshot) supportsNegotiatedExtension(candidate protocol.NegotiatedExtension) bool {
	for _, descriptor := range s.extensions {
		if descriptor.ID == candidate.ID && descriptor.Version == candidate.Version && descriptor.Capability == candidate.Capability {
			return true
		}
	}
	return false
}

// DecodeOptions adds this snapshot's exact extension versions and capabilities
// to a detached copy of a caller's protocol policy.
func (s Snapshot) DecodeOptions(base protocol.DecodeOptions) (protocol.DecodeOptions, error) {
	result := base
	result.Capabilities = maps.Clone(base.Capabilities)
	result.Extensions = maps.Clone(base.Extensions)
	result.IgnorableExtensionMetadata = maps.Clone(base.IgnorableExtensionMetadata)
	result.ExtensionNamespaces = maps.Clone(base.ExtensionNamespaces)
	if result.Capabilities == nil {
		result.Capabilities = make(map[string]bool)
	}
	if result.Extensions == nil {
		result.Extensions = make(map[string]protocol.ExtensionSupport)
	}
	for _, descriptor := range s.extensions {
		if result.ExtensionNamespaces[descriptor.ID] || result.IgnorableExtensionMetadata[descriptor.ID] {
			return protocol.DecodeOptions{}, fmt.Errorf("extension namespace %q conflicts with caller decode policy", descriptor.ID)
		}
		support := protocol.ExtensionSupport{
			Version: descriptor.Version, Capability: descriptor.Capability, OptionalMetadata: descriptor.OptionalMetadata,
		}
		if existing, exists := result.Extensions[descriptor.ID]; exists && existing != support {
			return protocol.DecodeOptions{}, fmt.Errorf("extension namespace %q has incompatible version policy", descriptor.ID)
		}
		result.Capabilities[descriptor.Capability] = true
		result.Extensions[descriptor.ID] = support
	}
	return result, nil
}
