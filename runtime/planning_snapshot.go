package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/valksor/naatre/schema"
)

// ErrPlanningOnly reports an attempted business invocation through an offline
// snapshot. Planning snapshots contain portable metadata and no application
// handlers.
var ErrPlanningOnly = errors.New("offline planning snapshot cannot execute handlers")

// NewPlanningSnapshot creates an immutable metadata-only registry from a
// portable schema document. It is intended for validators, explain tools,
// editors, and mock generators that must prove they invoke no business code.
func NewPlanningSnapshot(document schema.Document) (Snapshot, error) {
	types, err := document.Snapshot()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build planning types: %w", err)
	}
	definitions := make(map[string]Definition, len(document.Operations())+len(document.Members()))
	for _, operation := range document.Operations() {
		descriptor := descriptorFromOperation(operation)
		definitions[registrationKey(descriptor)] = planningDefinition(descriptor, rootBinding)
	}
	for _, member := range document.Members() {
		descriptor, binding, err := descriptorFromMember(member)
		if err != nil {
			return Snapshot{}, err
		}
		definitions[registrationKey(descriptor)] = planningDefinition(descriptor, binding)
	}
	directives := builtInDirectives()
	for _, descriptor := range document.Directives() {
		if existing, exists := directives[descriptor.Name]; exists {
			if existing.Descriptor.ID != descriptor.ID {
				return Snapshot{}, fmt.Errorf("schema directive %q conflicts with a core directive", descriptor.Name)
			}
			continue
		}
		directives[descriptor.Name] = registeredDirective{DirectiveDefinition: DirectiveDefinition{Descriptor: descriptor}}
	}
	return Snapshot{
		types: types, definitions: definitions, directives: directives,
		extensions: document.Extensions(), authorization: AuthorizationConfig{Mode: AuthorizationAllowByDefault},
	}, nil
}

func planningDefinition(descriptor Descriptor, binding bindingKind) Definition {
	return Definition{
		descriptor: descriptor,
		binding:    binding,
		invoke: func(context.Context, any, any) (any, error) {
			return nil, ErrPlanningOnly
		},
	}
}

func descriptorFromOperation(operation schema.OperationDescriptor) Descriptor {
	return Descriptor{
		ID: operation.ID, Name: operation.Name, Scope: RootScope, Kind: operation.Kind, Member: CallMember,
		Input: operation.Input, InputNullable: operation.InputNullable, Output: operation.Output, OutputNullable: operation.OutputNullable,
		Description: operation.Description, Deprecation: cloneDeprecation(operation.Deprecation),
		Capabilities: slices.Clone(operation.Capabilities), Traits: cloneRuntimeTraits(operation.Traits), Source: cloneRuntimeSource(operation.Source),
		Metadata: metadataFromPortable(operation.Effect, operation.Deterministic, operation.Cacheable, operation.RetrySafe,
			operation.ThreadSafety, operation.Batching, operation.Transaction, operation.AuthorizationPolicy,
			operation.Idempotency, operation.Cost, operation.ParallelMutation, operation.Collection),
	}
}

func descriptorFromMember(member schema.MemberDescriptor) (Descriptor, bindingKind, error) {
	kind := MemberKind(member.Kind)
	binding := callBinding
	if kind == FieldMember {
		binding = fieldBinding
	} else if kind != CallMember {
		return Descriptor{}, 0, fmt.Errorf("schema member %q has unsupported kind %q", member.ID, member.Kind)
	}
	return Descriptor{
		ID: member.ID, Name: member.Name, Scope: ObjectScope, Owner: member.Owner, Member: kind,
		Input: member.Input, InputNullable: member.InputNullable, Output: member.Output, OutputNullable: member.OutputNullable,
		Description: member.Description, Deprecation: cloneDeprecation(member.Deprecation),
		Capabilities: slices.Clone(member.Capabilities), Traits: cloneRuntimeTraits(member.Traits), Source: cloneRuntimeSource(member.Source),
		Metadata: metadataFromPortable(member.Effect, member.Deterministic, member.Cacheable, member.RetrySafe,
			member.ThreadSafety, member.Batching, member.Transaction, member.AuthorizationPolicy,
			member.Idempotency, member.Cost, member.ParallelMutation, member.Collection),
	}, binding, nil
}

func metadataFromPortable(effect string, deterministic, cacheable, retrySafe bool, threadSafety, batching, transaction,
	authorization, idempotency string, cost uint64, parallelMutation bool, collection *schema.CollectionDescriptor,
) Metadata {
	return Metadata{
		Effect: Effect(effect), Deterministic: deterministic, Cacheable: cacheable, RetrySafe: retrySafe,
		ThreadSafety: ThreadSafety(threadSafety), Batching: Batching(batching), Transaction: TransactionParticipation(transaction),
		AuthorizationPolicy: authorization, Idempotency: IdempotencyPolicy(idempotency), Cost: cost, ParallelMutation: parallelMutation,
		Collection: planningCollection(collection),
	}
}

func planningCollection(input *schema.CollectionDescriptor) *CollectionMetadata {
	if input == nil {
		return nil
	}
	return &CollectionMetadata{
		MaxPageSize: input.MaxPageSize, TotalCountCost: input.TotalCountCost,
		Query: schema.CloneCollectionQueryDescriptor(input.Query),
	}
}

func cloneDeprecation(input *schema.Deprecation) *schema.Deprecation {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}
