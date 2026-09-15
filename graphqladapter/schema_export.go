package graphqladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/valksor/naatre/interopadapter"
	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// ExportSchema renders the lossless supported Naatre subset as deterministic
// SDL. Unsupported Naatre constructs reject the entire direction report.
func ExportSchema(ctx context.Context, document schema.Document, config ExportConfig) ([]byte, interopadapter.FidelityReport, error) {
	report := newReport(config.AdapterID, config.SchemaIdentity, config.WireVersion, interopadapter.SchemaExport)
	limits, err := normalizeLimits(config.Limits)
	if err != nil || !validReportIdentities(config.AdapterID, config.SchemaIdentity, config.WireVersion) ||
		config.SchemaIdentity == config.WireVersion {
		return nil, report, rejectReport(&report, CodeSchemaInvalid, "identity", "GraphQL export identities and limits are invalid")
	}
	if err := contextErr(ctx); err != nil {
		return nil, report, rejectReport(&report, CodeCancelled, "cancellation", publicMessage(CodeCancelled))
	}
	types := document.Types()
	operations := document.Operations()
	members := document.Members()
	if len(types) > limits.MaxTypes || len(operations) > limits.MaxOperations || len(members) > limits.MaxFields {
		return nil, report, rejectReport(&report, CodeResourceExhausted, "limits", publicMessage(CodeResourceExhausted))
	}
	output, exportErr := renderSchema(types, operations, members)
	if exportErr != nil {
		code := CodeOf(exportErr)
		return nil, report, rejectReport(&report, code, "mapping", publicMessage(code))
	}
	if len(output) > limits.MaxDocumentBytes {
		return nil, report, rejectReport(&report, CodeResourceExhausted, "limits", publicMessage(CodeResourceExhausted))
	}
	report.Status = "ready"
	for _, operation := range operations {
		report.Operations = append(report.Operations, interopadapter.OperationReport{
			ExternalName: operation.Name, NaatreName: operation.Name, Approved: true,
			Policy: interopadapter.PolicyClaims{
				Effect: runtime.Effect(operation.Effect), Idempotency: runtime.IdempotencyPolicy(operation.Idempotency),
				Cost: operation.Cost, RetrySafe: operation.RetrySafe, Cacheable: operation.Cacheable,
				Batching: runtime.Batching(operation.Batching), Transaction: runtime.TransactionParticipation(operation.Transaction),
			},
		})
	}
	return output, report, nil
}

func renderSchema(types []schema.TypeDeclaration, operations []schema.OperationDescriptor, members []schema.MemberDescriptor) ([]byte, error) {
	typeByID := make(map[schema.TypeID]schema.TypeDeclaration, len(types))
	nameByID := builtinTypeNames()
	for _, value := range types {
		typeByID[value.ID] = value
		nameByID[value.ID] = value.Name
	}
	memberByOwner := make(map[schema.TypeID][]schema.MemberDescriptor)
	for _, member := range members {
		memberByOwner[member.Owner] = append(memberByOwner[member.Owner], member)
	}
	var builder strings.Builder
	for _, value := range types {
		if value.Kind == schema.ListType || strings.HasPrefix(string(value.ID), "graphql.input.") {
			continue
		}
		if !validUserGraphQLName(value.Name) {
			return nil, adapterError(CodeUnsupported, errors.New("naatre type name has no GraphQL representation"))
		}
		switch value.Kind {
		case schema.ObjectType:
			fmt.Fprintf(&builder, "type %s {\n", value.Name)
			owned := memberByOwner[value.ID]
			sort.Slice(owned, func(i, j int) bool { return owned[i].Name < owned[j].Name })
			for _, member := range owned {
				line, err := renderCallable(member.Name, member.Input, member.Output, member.OutputNullable, typeByID, nameByID)
				if err != nil {
					return nil, err
				}
				builder.WriteString("  " + line + "\n")
			}
			builder.WriteString("}\n\n")
		case schema.InputObjectType:
			fmt.Fprintf(&builder, "input %s {\n", value.Name)
			for _, field := range value.Fields {
				if !validUserGraphQLName(field.Name) {
					return nil, adapterError(CodeUnsupported, errors.New("naatre field name has no GraphQL representation"))
				}
				rendered, err := renderType(field.Type, field.Nullable, typeByID, nameByID)
				if err != nil {
					return nil, err
				}
				fmt.Fprintf(&builder, "  %s: %s\n", field.Name, rendered)
			}
			builder.WriteString("}\n\n")
		case schema.EnumType:
			fmt.Fprintf(&builder, "enum %s {\n", value.Name)
			for _, member := range value.EnumMembers {
				if !validUserGraphQLName(member.Name) || member.Name == "true" || member.Name == "false" || member.Name == "null" {
					return nil, adapterError(CodeUnsupported, errors.New("naatre enum member has no GraphQL representation"))
				}
				builder.WriteString("  " + member.Name + "\n")
			}
			builder.WriteString("}\n\n")
		case schema.UnionType:
			variants := make([]string, 0, len(value.VariantMembers))
			for _, member := range value.VariantMembers {
				name, ok := nameByID[member.Type]
				if !ok {
					return nil, adapterError(CodeSchemaInvalid, errors.New("naatre union references unknown type"))
				}
				variants = append(variants, name)
			}
			fmt.Fprintf(&builder, "union %s = %s\n\n", value.Name, strings.Join(variants, " | "))
		case schema.ScalarType:
			fmt.Fprintf(&builder, "scalar %s\n\n", value.Name)
		case schema.ListType, schema.MapType, schema.InterfaceType, schema.OneOfType:
			return nil, adapterError(CodeUnsupported, errors.New("naatre type has no supported GraphQL representation"))
		}
	}
	groups := map[protocol.OperationKind][]schema.OperationDescriptor{}
	for _, operation := range operations {
		groups[operation.Kind] = append(groups[operation.Kind], operation)
	}
	for _, kind := range []protocol.OperationKind{protocol.Query, protocol.Mutation, protocol.Subscription} {
		values := groups[kind]
		if len(values) == 0 {
			continue
		}
		sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
		rootName := strings.ToUpper(string(kind[:1])) + string(kind[1:])
		fmt.Fprintf(&builder, "type %s {\n", rootName)
		for _, operation := range values {
			line, err := renderCallable(operation.Name, operation.Input, operation.Output, operation.OutputNullable, typeByID, nameByID)
			if err != nil {
				return nil, err
			}
			builder.WriteString("  " + line + "\n")
		}
		builder.WriteString("}\n\n")
	}
	return []byte(strings.TrimSpace(builder.String()) + "\n"), nil
}

func renderCallable(name string, input, output schema.TypeID, nullable bool, types map[schema.TypeID]schema.TypeDeclaration, names map[schema.TypeID]string) (string, error) {
	if !validUserGraphQLName(name) {
		return "", adapterError(CodeUnsupported, errors.New("naatre callable name has no GraphQL representation"))
	}
	var builder strings.Builder
	builder.WriteString(name)
	if input != "" {
		inputType, ok := types[input]
		if ok && inputType.Kind == schema.InputObjectType {
			builder.WriteString("(")
			for index, field := range inputType.Fields {
				if !validUserGraphQLName(field.Name) {
					return "", adapterError(CodeUnsupported, errors.New("naatre argument name has no GraphQL representation"))
				}
				if index > 0 {
					builder.WriteString(", ")
				}
				rendered, err := renderType(field.Type, field.Nullable, types, names)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&builder, "%s: %s", field.Name, rendered)
			}
			builder.WriteString(")")
		} else {
			rendered, err := renderType(input, false, types, names)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&builder, "(input: %s)", rendered)
		}
	}
	rendered, err := renderType(output, nullable, types, names)
	if err != nil {
		return "", err
	}
	return builder.String() + ": " + rendered, nil
}

func renderType(identifier schema.TypeID, nullable bool, types map[schema.TypeID]schema.TypeDeclaration, names map[schema.TypeID]string) (string, error) {
	if value, ok := types[identifier]; ok && value.Kind == schema.ListType {
		element, err := renderType(value.Element, value.ElementNullable, types, names)
		if err != nil {
			return "", err
		}
		result := "[" + element + "]"
		if !nullable {
			result += "!"
		}
		return result, nil
	}
	name, ok := names[identifier]
	if !ok || !validUserGraphQLName(name) {
		return "", adapterError(CodeUnsupported, errors.New("naatre scalar or type has no GraphQL mapping"))
	}
	if !nullable {
		name += "!"
	}
	return name, nil
}

// MarshalReport returns canonical machine-readable fidelity evidence.
func MarshalReport(report interopadapter.FidelityReport) ([]byte, error) {
	return interopadapter.MarshalFidelityReport(report)
}

func cloneJSON(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var cloned any
	if err := decoder.Decode(&cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}
