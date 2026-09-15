package graphqladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

var reportIdentityPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)

// ImportSchema strictly maps the supported SDL subset into the existing
// Naatre schema document format and publishes a direction-specific report.
func ImportSchema(ctx context.Context, source []byte, config ImportConfig) (ImportedSchema, interopadapter.FidelityReport, error) {
	report := newReport(config.AdapterID, config.SchemaIdentity, config.WireVersion, interopadapter.SchemaImport)
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return ImportedSchema{}, report, rejectReport(&report, CodeResourceExhausted, "limits", "GraphQL resource limits are invalid")
	}
	if !validReportIdentities(config.AdapterID, config.SchemaIdentity, config.WireVersion) ||
		config.SchemaIdentity == config.WireVersion || config.Revision == "" {
		return ImportedSchema{}, report, rejectReport(&report, CodeSchemaInvalid, "identity", "GraphQL import identities and revision are required")
	}
	model, err := parseSchema(ctx, source, limits)
	if err != nil {
		code := CodeOf(err)
		_ = rejectReport(&report, code, "schema", publicMessage(code))
		return ImportedSchema{}, report, adapterError(code, err)
	}
	builder := schemaBuilder{
		model: model, config: config, limits: limits,
		descriptors: make(map[schema.TypeID]schema.TypeDescriptor),
		typeIDs:     builtinTypeIDs(), typeNames: builtinTypeNames(),
		typeRefs:   make(map[string]schema.TypeID),
		operations: make(map[string]runtime.Descriptor),
	}
	result, buildErr := builder.build(ctx)
	if buildErr != nil {
		code := CodeOf(buildErr)
		_ = rejectReport(&report, code, "mapping", publicMessage(code))
		return ImportedSchema{}, report, adapterError(code, buildErr)
	}
	report.Status = "ready"
	for name, descriptor := range result.operations {
		report.Operations = append(report.Operations, interopadapter.OperationReport{
			ExternalName: name, NaatreName: descriptor.Name, Approved: false,
			Policy: interopadapter.PolicyClaims{
				Effect: descriptor.Metadata.Effect, Idempotency: descriptor.Metadata.Idempotency,
				Cost: descriptor.Metadata.Cost, RetrySafe: descriptor.Metadata.RetrySafe,
				Cacheable: descriptor.Metadata.Cacheable, Batching: descriptor.Metadata.Batching,
				Transaction: descriptor.Metadata.Transaction,
			},
		})
	}
	sort.Slice(report.Operations, func(i, j int) bool { return report.Operations[i].ExternalName < report.Operations[j].ExternalName })
	return result, report, nil
}

type schemaBuilder struct {
	model                *schemaModel
	config               ImportConfig
	limits               Limits
	descriptors          map[schema.TypeID]schema.TypeDescriptor
	typeIDs              map[string]schema.TypeID
	typeNames            map[schema.TypeID]string
	typeRefs             map[string]schema.TypeID
	operations           map[string]runtime.Descriptor
	operationDescriptors []schema.OperationDescriptor
	members              []schema.MemberDescriptor
}

func (b *schemaBuilder) build(ctx context.Context) (ImportedSchema, error) {
	maxDepth := b.config.MaxTypeDepth
	if maxDepth == 0 {
		maxDepth = b.limits.MaxDepth
	}
	if maxDepth < 1 || maxDepth > b.limits.MaxDepth {
		return ImportedSchema{}, adapterError(CodeResourceExhausted, errors.New("invalid GraphQL recursive type bound"))
	}
	for name, value := range b.model.types {
		if value.kind == schema.ScalarType {
			mapped, ok := b.config.ScalarMappings[name]
			if !ok || mapped == "" {
				return ImportedSchema{}, adapterError(CodeUnsupported, errors.New("custom GraphQL scalar has no explicit Naatre mapping"))
			}
			b.typeIDs[name] = mapped
			if _, exists := b.typeNames[mapped]; !exists {
				b.typeNames[mapped] = name
			}
			continue
		}
		identifier := schema.TypeID("graphql." + name)
		b.typeIDs[name], b.typeNames[identifier] = identifier, name
	}
	for _, name := range sortedGraphTypeNames(b.model.types) {
		if err := contextErr(ctx); err != nil {
			return ImportedSchema{}, err
		}
		value := b.model.types[name]
		if value.kind == schema.ScalarType || b.isRoot(name) {
			continue
		}
		descriptor := schema.TypeDescriptor{ID: b.typeIDs[name], Name: name, Kind: value.kind, MaxDepth: maxDepth}
		switch value.kind {
		case schema.ObjectType:
			descriptor.Output = true
		case schema.InputObjectType:
			descriptor.Input = true
		case schema.EnumType:
			descriptor.Input, descriptor.Output = true, true
			descriptor.EnumValues = slices.Clone(value.values)
		case schema.UnionType:
			descriptor.Output = true
			for _, variant := range value.variants {
				identifier, ok := b.typeIDs[variant]
				variantType := b.model.types[variant]
				if !ok || variantType == nil || variantType.kind != schema.ObjectType || b.isRoot(variant) {
					return ImportedSchema{}, adapterError(CodeSchemaInvalid, errors.New("GraphQL union references an unknown type"))
				}
				descriptor.Variants = append(descriptor.Variants, identifier)
			}
		case schema.ScalarType, schema.ListType, schema.MapType, schema.InterfaceType, schema.OneOfType:
			return ImportedSchema{}, adapterError(CodeUnsupported, errors.New("unsupported GraphQL type mapping"))
		}
		if value.kind == schema.ObjectType || value.kind == schema.InputObjectType {
			descriptor.Fields = make(map[string]schema.FieldDescriptor, len(value.fields))
			for _, field := range value.fields {
				fieldType, mapErr := b.typeIDFor(field.typeRef, value.kind == schema.InputObjectType)
				if mapErr != nil {
					return ImportedSchema{}, mapErr
				}
				descriptor.Fields[field.name] = schema.FieldDescriptor{
					ID:   "graphql.field." + name + "." + field.name,
					Type: fieldType, Required: field.typeRef.nonNull, Nullable: !field.typeRef.nonNull,
				}
			}
		}
		b.descriptors[descriptor.ID] = descriptor
	}
	if err := b.buildCallables(); err != nil {
		return ImportedSchema{}, err
	}
	catalog := schema.NewCatalog()
	for _, identifier := range sortedDescriptorIDs(b.descriptors) {
		if err := catalog.Register(b.descriptors[identifier]); err != nil {
			return ImportedSchema{}, adapterError(CodeSchemaInvalid, err)
		}
	}
	snapshot, err := catalog.Freeze()
	if err != nil {
		return ImportedSchema{}, adapterError(CodeSchemaInvalid, err)
	}
	for name, identifier := range b.typeIDs {
		if b.isRoot(name) {
			continue
		}
		if _, ok := snapshot.Lookup(identifier); !ok {
			return ImportedSchema{}, adapterError(CodeSchemaInvalid, fmt.Errorf("GraphQL type %s maps to unknown Naatre type", name))
		}
	}
	document, err := schema.ExportDocument(snapshot, b.operationDescriptors, b.members, schema.ExportOptions{
		Revision: b.config.Revision, Capabilities: []string{Profile},
	})
	if err != nil {
		return ImportedSchema{}, adapterError(CodeSchemaInvalid, err)
	}
	return ImportedSchema{
		document: document, model: b.model, typeIDs: b.typeIDs,
		typeNames: b.typeNames, typeRefs: b.typeRefs, operations: b.operations,
		adapterID: b.config.AdapterID, schemaIdentity: b.config.SchemaIdentity,
		wireVersion: b.config.WireVersion,
	}, nil
}

func (b *schemaBuilder) buildCallables() error {
	for kind, rootName := range b.model.roots {
		root, exists := b.model.types[rootName]
		if !exists {
			continue
		}
		if root.kind != schema.ObjectType {
			return adapterError(CodeSchemaInvalid, errors.New("GraphQL root must be an object"))
		}
		for _, field := range root.fields {
			if _, duplicate := b.operations[field.name]; duplicate {
				return adapterError(CodeSchemaInvalid, errors.New("GraphQL root operation names must be globally unique"))
			}
			policy, ok := b.config.Policies[rootName+"."+field.name]
			if !ok || !policy.valid() {
				return adapterError(CodePolicyRequired, errors.New("GraphQL root operation requires explicit application policy"))
			}
			output, err := b.typeIDFor(field.typeRef, false)
			if err != nil {
				return err
			}
			input, err := b.inputFor(rootName, field)
			if err != nil {
				return err
			}
			descriptor := runtime.Descriptor{
				ID:   "graphql.operation." + string(kind) + "." + field.name,
				Name: field.name, Scope: runtime.RootScope, Kind: kind, Member: runtime.CallMember,
				Input: input, Output: output, OutputNullable: !field.typeRef.nonNull,
				Capabilities: []string{Profile}, Metadata: policy.metadata(),
			}
			b.operations[field.name] = descriptor
			b.operationDescriptors = append(b.operationDescriptors, portableOperation(descriptor))
		}
	}
	for _, typeName := range sortedGraphTypeNames(b.model.types) {
		value := b.model.types[typeName]
		if value.kind != schema.ObjectType || b.isRoot(typeName) {
			continue
		}
		for _, field := range value.fields {
			policy, ok := b.config.Policies[typeName+"."+field.name]
			if !ok || !policy.valid() {
				return adapterError(CodePolicyRequired, errors.New("GraphQL object field requires explicit application policy"))
			}
			output, err := b.typeIDFor(field.typeRef, false)
			if err != nil {
				return err
			}
			input, err := b.inputFor(typeName, field)
			if err != nil {
				return err
			}
			memberKind := "field"
			if len(field.arguments) != 0 {
				memberKind = "call"
			}
			metadata := policy.metadata()
			b.members = append(b.members, schema.MemberDescriptor{
				ID:   "graphql.member." + typeName + "." + field.name,
				Name: field.name, Owner: b.typeIDs[typeName], Kind: memberKind,
				Input: input, Output: output, OutputNullable: !field.typeRef.nonNull,
				Effect: string(metadata.Effect), Deterministic: metadata.Deterministic,
				Cacheable: metadata.Cacheable, RetrySafe: metadata.RetrySafe,
				ThreadSafety: string(metadata.ThreadSafety), Batching: string(metadata.Batching),
				Transaction: string(metadata.Transaction), AuthorizationPolicy: metadata.AuthorizationPolicy,
				Idempotency: string(metadata.Idempotency), Cost: metadata.Cost,
				Capabilities: []string{Profile},
			})
		}
	}
	return nil
}

func portableOperation(descriptor runtime.Descriptor) schema.OperationDescriptor {
	return schema.OperationDescriptor{
		ID: descriptor.ID, Name: descriptor.Name, Kind: descriptor.Kind,
		Input: descriptor.Input, InputNullable: descriptor.InputNullable,
		Output: descriptor.Output, OutputNullable: descriptor.OutputNullable,
		Effect: string(descriptor.Metadata.Effect), Deterministic: descriptor.Metadata.Deterministic,
		Cacheable: descriptor.Metadata.Cacheable, RetrySafe: descriptor.Metadata.RetrySafe,
		ThreadSafety: string(descriptor.Metadata.ThreadSafety), Batching: string(descriptor.Metadata.Batching),
		Transaction: string(descriptor.Metadata.Transaction), AuthorizationPolicy: descriptor.Metadata.AuthorizationPolicy,
		Idempotency: string(descriptor.Metadata.Idempotency), Cost: descriptor.Metadata.Cost,
		Capabilities: slices.Clone(descriptor.Capabilities),
	}
}

func (b *schemaBuilder) inputFor(owner string, field graphField) (schema.TypeID, error) {
	if len(field.arguments) == 0 {
		return "", nil
	}
	identifier := schema.TypeID("graphql.input." + owner + "." + field.name)
	fields := make(map[string]schema.FieldDescriptor, len(field.arguments))
	for _, argument := range field.arguments {
		argumentType, err := b.typeIDFor(argument.typeRef, true)
		if err != nil {
			return "", err
		}
		fields[argument.name] = schema.FieldDescriptor{
			ID:   "graphql.argument." + owner + "." + field.name + "." + argument.name,
			Type: argumentType, Required: argument.typeRef.nonNull, Nullable: !argument.typeRef.nonNull,
		}
	}
	b.descriptors[identifier] = schema.TypeDescriptor{
		ID: identifier, Name: owner + "_" + field.name + "_Input",
		Kind: schema.InputObjectType, Input: true, Fields: fields, MaxDepth: b.limits.MaxDepth,
	}
	b.typeNames[identifier] = owner + "_" + field.name + "_Input"
	return identifier, nil
}

func (b *schemaBuilder) typeIDFor(ref graphTypeRef, input bool) (schema.TypeID, error) {
	if ref.element == nil {
		identifier, exists := b.typeIDs[ref.name]
		if !exists {
			return "", adapterError(CodeSchemaInvalid, errors.New("GraphQL field references an unknown type"))
		}
		b.typeRefs[ref.string()] = identifier
		return identifier, nil
	}
	element, err := b.typeIDFor(*ref.element, input)
	if err != nil {
		return "", err
	}
	position := "output"
	if input {
		position = "input"
	}
	digest := sha256.Sum256([]byte(position + ":" + ref.string()))
	identifier := schema.TypeID("graphql.list." + hex.EncodeToString(digest[:6]))
	if _, exists := b.descriptors[identifier]; !exists {
		b.descriptors[identifier] = schema.TypeDescriptor{
			ID: identifier, Name: "GraphQLList_" + hex.EncodeToString(digest[:4]), Kind: schema.ListType,
			Input: input, Output: !input, Element: element, ElementNullable: !ref.element.nonNull,
			MaxDepth: b.limits.MaxDepth,
		}
		b.typeNames[identifier] = "GraphQLList_" + hex.EncodeToString(digest[:4])
	}
	b.typeRefs[ref.string()] = identifier
	return identifier, nil
}

func (b *schemaBuilder) isRoot(name string) bool {
	for _, root := range b.model.roots {
		if root == name {
			return true
		}
	}
	return false
}
