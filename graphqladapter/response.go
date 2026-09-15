package graphqladapter

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/valksor/naatre/runtime"
)

func (r *Runtime) validateResponseBounds(response Response) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return adapterError(CodeResponseInvalid, errors.New("GraphQL response cannot be encoded"))
	}
	if len(encoded) > r.limits.MaxResponseBytes || len(response.Errors) > r.limits.MaxErrors {
		return adapterError(CodeResourceExhausted, errors.New("GraphQL response exceeds configured bounds"))
	}
	return nil
}

func failureResponse(code string) Response {
	return Response{Errors: []ResponseError{{
		Message: publicMessage(code), Extensions: map[string]any{"code": code},
	}}}
}

// MapResponse preserves partial data while replacing all untrusted error
// content with stable public failures and bounded response paths.
func MapResponse(response Response, inputLimits Limits) (MappedResponse, error) {
	limits, err := normalizeLimits(inputLimits)
	if err != nil {
		return MappedResponse{}, err
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded) > limits.MaxResponseBytes || len(response.Errors) > limits.MaxErrors {
		return MappedResponse{}, adapterError(CodeResourceExhausted, errors.New("GraphQL response exceeds configured bounds"))
	}
	data, err := cloneJSON(response.Data)
	if err != nil {
		return MappedResponse{}, adapterError(CodeResponseInvalid, err)
	}
	result := MappedResponse{Data: data, Errors: make([]PublicError, 0, len(response.Errors)), Complete: len(response.Errors) == 0}
	for _, upstream := range response.Errors {
		path, pathErr := safePath(upstream.Path, limits.MaxDepth)
		if pathErr != nil {
			return MappedResponse{}, pathErr
		}
		result.Errors = append(result.Errors, PublicError{
			Code: CodeUpstreamFailed, Message: publicMessage(CodeUpstreamFailed), Path: path,
		})
	}
	return result, nil
}

func safePath(input []any, maximum int) ([]any, error) {
	if len(input) > maximum {
		return nil, adapterError(CodeResourceExhausted, errors.New("GraphQL error path exceeds depth limit"))
	}
	result := make([]any, 0, len(input))
	for _, segment := range input {
		switch value := segment.(type) {
		case string:
			if len(value) > 128 || !validUserGraphQLName(value) {
				return nil, adapterError(CodeResponseInvalid, errors.New("GraphQL error path contains invalid name"))
			}
			result = append(result, value)
		case int:
			if value < 0 {
				return nil, adapterError(CodeResponseInvalid, errors.New("GraphQL error path contains negative index"))
			}
			result = append(result, value)
		case float64:
			if value < 0 || value > math.MaxInt || math.Trunc(value) != value {
				return nil, adapterError(CodeResponseInvalid, errors.New("GraphQL error path contains invalid index"))
			}
			result = append(result, int(value))
		default:
			return nil, adapterError(CodeResponseInvalid, errors.New("GraphQL error path contains invalid segment"))
		}
	}
	return result, nil
}

func (r *Runtime) outcomeResponse(executable Executable, operation graphOperation, outcome runtime.Outcome) Response {
	response := Response{Data: outcome.Data}
	paths := make([][]any, 0, len(outcome.Errors))
	for _, failure := range outcome.Errors {
		path, pathErr := safePath(failure.Path, r.limits.MaxDepth)
		if pathErr != nil || !pathMatchesSelection(path, r.schema.model.roots[operation.kind], operation.selections, executable.model.fragments, r.schema.model) {
			path = nil
		}
		paths = append(paths, path)
		code, message := publicRuntimeFailure(failure.Code)
		response.Errors = append(response.Errors, ResponseError{
			Message: message, Path: path, Extensions: map[string]any{"code": code},
		})
	}
	response.Data = applyNonNull(response.Data, paths, operation, executable.model.fragments, r.schema.model)
	return response
}

func pathMatchesSelection(path []any, typeName string, selections []graphSelection, fragments map[string]graphFragment, model *schemaModel) bool {
	if len(path) == 0 || model == nil {
		return false
	}
	currentType := typeName
	currentSelections := selections
	var currentRef *graphTypeRef
	for _, raw := range path {
		switch segment := raw.(type) {
		case string:
			selection, ok := findSelection(currentSelections, segment, fragments)
			if !ok {
				return false
			}
			field, ok := findField(model.types[currentType], selection.name)
			if !ok {
				return false
			}
			currentType = field.typeRef.named()
			currentSelections = selection.selections
			copyRef := field.typeRef
			currentRef = &copyRef
		case int:
			if segment < 0 || currentRef == nil || currentRef.element == nil {
				return false
			}
			copyRef := *currentRef.element
			currentRef = &copyRef
			currentType = currentRef.named()
		default:
			return false
		}
	}
	return true
}

func publicRuntimeFailure(code string) (string, string) {
	switch code {
	case "VALIDATION_FAILED":
		return code, "operation validation failed"
	case "UNAUTHORIZED":
		return code, "operation is not authorized"
	case "RESOURCE_EXHAUSTED":
		return code, "request deadline or resource limit exceeded"
	case "CANCELLED":
		return code, "request cancelled"
	case "PARTIAL", "FIELD_FAILED", "HANDLER_FAILED", "OUTPUT_COMPLETION":
		return code, "field unavailable"
	default:
		return CodeExecutionFailed, publicMessage(CodeExecutionFailed)
	}
}

type responseLocation struct {
	container any
	key       any
	nonNull   bool
}

func applyNonNull(data any, paths [][]any, operation graphOperation, fragments map[string]graphFragment, model *schemaModel) any {
	cloned, err := cloneJSON(data)
	if err != nil || model == nil {
		return nil
	}
	rootName := model.roots[operation.kind]
	for _, path := range paths {
		locations, ok := locateResponsePath(cloned, path, rootName, operation.selections, fragments, model)
		if !ok || len(locations) == 0 {
			continue
		}
		setLocation(locations[len(locations)-1], nil)
		for index := len(locations) - 1; index >= 0 && locations[index].nonNull; index-- {
			if index == 0 {
				cloned = nil
				break
			}
			setLocation(locations[index-1], nil)
		}
	}
	return cloned
}

func locateResponsePath(data any, path []any, typeName string, selections []graphSelection, fragments map[string]graphFragment, model *schemaModel) ([]responseLocation, bool) {
	current := data
	currentType := typeName
	currentSelections := selections
	var currentRef *graphTypeRef
	locations := make([]responseLocation, 0, len(path))
	for _, raw := range path {
		switch segment := raw.(type) {
		case string:
			selection, ok := findSelection(currentSelections, segment, fragments)
			if !ok {
				return nil, false
			}
			definition := model.types[currentType]
			field, ok := findField(definition, selection.name)
			if !ok {
				return nil, false
			}
			locations = append(locations, responseLocation{container: current, key: segment, nonNull: field.typeRef.nonNull})
			object, ok := current.(map[string]any)
			if !ok {
				return locations, true
			}
			current = object[segment]
			currentType = field.typeRef.named()
			currentSelections = selection.selections
			copyRef := field.typeRef
			currentRef = &copyRef
		case int:
			if currentRef == nil || currentRef.element == nil {
				return nil, false
			}
			list, ok := current.([]any)
			if !ok || segment < 0 || segment >= len(list) {
				return nil, false
			}
			locations = append(locations, responseLocation{container: current, key: segment, nonNull: currentRef.element.nonNull})
			current = list[segment]
			copyRef := *currentRef.element
			currentRef = &copyRef
			currentType = currentRef.named()
		default:
			return nil, false
		}
	}
	return locations, true
}

func findSelection(selections []graphSelection, responseName string, fragments map[string]graphFragment) (graphSelection, bool) {
	for _, selection := range selections {
		if selection.fragmentName != "" {
			if found, ok := findSelection(fragments[selection.fragmentName].selections, responseName, fragments); ok {
				return found, true
			}
			continue
		}
		if selection.typeCondition != "" {
			if found, ok := findSelection(selection.selections, responseName, fragments); ok {
				return found, true
			}
			continue
		}
		name := selection.name
		if selection.alias != "" {
			name = selection.alias
		}
		if name == responseName {
			return selection, true
		}
	}
	return graphSelection{}, false
}

func findField(value *graphType, name string) (graphField, bool) {
	if value == nil {
		return graphField{}, false
	}
	for _, field := range value.fields {
		if field.name == name {
			return field, true
		}
	}
	return graphField{}, false
}

func setLocation(location responseLocation, value any) {
	switch container := location.container.(type) {
	case map[string]any:
		if key, ok := location.key.(string); ok {
			container[key] = value
		}
	case []any:
		if index, ok := location.key.(int); ok && index >= 0 && index < len(container) {
			container[index] = value
		}
	}
}
