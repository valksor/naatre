package graphqladapter

import (
	"context"
	"slices"
	"sort"

	"github.com/valksor/naatre/schema"
)

func builtinTypeIDs() map[string]schema.TypeID {
	return map[string]schema.TypeID{
		"Boolean": schema.TypeID(schema.Boolean), "String": schema.TypeID(schema.String),
		"ID": schema.TypeID(schema.ID), "Int": schema.TypeID(schema.Int32),
		"Float": schema.TypeID(schema.Float64),
	}
}

func builtinTypeNames() map[schema.TypeID]string {
	return map[schema.TypeID]string{
		schema.TypeID(schema.Boolean): "Boolean", schema.TypeID(schema.String): "String",
		schema.TypeID(schema.ID): "ID", schema.TypeID(schema.Int32): "Int",
		schema.TypeID(schema.Float64): "Float",
	}
}

func isBuiltinScalar(name string) bool {
	_, ok := builtinTypeIDs()[name]
	return ok
}

func sortedGraphTypeNames(values map[string]*graphType) []string {
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func sortedDescriptorIDs(values map[schema.TypeID]schema.TypeDescriptor) []schema.TypeID {
	result := make([]schema.TypeID, 0, len(values))
	for identifier := range values {
		result = append(result, identifier)
	}
	slices.Sort(result)
	return result
}

func validReportIdentities(values ...string) bool {
	for _, value := range values {
		if !reportIdentityPattern.MatchString(value) {
			return false
		}
	}
	return true
}

func contextErr(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return adapterError(CodeCancelled, err)
		}
	}
	return nil
}

func publicMessage(code string) string {
	switch code {
	case CodeCancelled:
		return "GraphQL operation was cancelled"
	case CodeResourceExhausted:
		return "GraphQL resource limit was exceeded"
	case CodeUnsupported:
		return "GraphQL capability is unsupported"
	case CodePolicyRequired:
		return "GraphQL mapping requires explicit application policy"
	case CodeResolverRequired:
		return "GraphQL operation requires an explicit resolver"
	case CodeOperationUnknown:
		return "GraphQL operation is not registered"
	case CodeResponseInvalid:
		return "GraphQL response is invalid"
	case CodeUpstreamFailed:
		return "GraphQL upstream operation failed"
	case CodeExecutionFailed:
		return "GraphQL execution failed"
	default:
		return "GraphQL document is invalid"
	}
}
