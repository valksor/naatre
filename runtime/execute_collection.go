package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"

	"github.com/valksor/naatre/protocol"
)

func (p *Plan) executeMap(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	items, ok := collectionItems(scope.current)
	if !ok {
		return invalidCollectionResult(node, path)
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	output := make([]any, len(items))
	var failures []ExecutionError
	failed := false
	for index, item := range items {
		itemValue := collectionItemValue(item, descriptor, index)
		if itemValue.status == valueNull {
			output[index] = nil
			continue
		}
		itemScope := childExecutionScope(scope, itemValue, scope.current)
		children := p.executeSequence(ctx, node.children, itemScope, appendPathSegments(path, index))
		failures = append(failures, children.errors...)
		if children.failed && len(children.data) == 0 {
			output[index] = nil
		} else {
			output[index] = children.data
		}
		failed = failed || children.failed
		if containsExecutionCode(children.errors, "CANCELLED") {
			break
		}
	}
	value := executionValue{value: output, typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: output, emit: node.outputName != "", failed: failed, errors: failures}
}

func (p *Plan) executeIndex(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	items, ok := collectionItems(scope.current)
	if !ok {
		return invalidCollectionResult(node, path)
	}
	position, _ := node.selection.At()
	index, ok := exactIndex(position)
	if !ok || index >= len(items) {
		requested := 0
		if position != nil && position.IsInt64() {
			requested = int(position.Int64())
		}
		failure := nodeExecutionError("INDEX_OUT_OF_RANGE", "collection index is out of range", node, appendPathSegments(path, requested), errors.New("runtime collection is shorter than requested index"))
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	value := collectionItemValue(items[index], descriptor, index)
	presentation := value.value
	var failures []ExecutionError
	failed := false
	if len(node.children) != 0 && value.status == valueAvailable {
		childScope := childExecutionScope(scope, value, scope.current)
		children := p.executeSequence(ctx, node.children, childScope, path)
		presentation, failures, failed = children.data, children.errors, children.failed
	}
	return nodeResult{value: value, data: presentation, emit: node.outputName != "" && value.status != valueUnavailable, failed: failed, errors: failures}
}

func (p *Plan) executeCollectionWindow(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	items, ok := collectionItems(scope.current)
	if !ok {
		return invalidCollectionResult(node, path)
	}
	start, end := 0, len(items)
	if node.kind == protocol.SliceSelection {
		if value, exists := node.selection.Start(); exists {
			start = clampedBound(value, len(items))
		}
		if value, exists := node.selection.End(); exists {
			end = clampedBound(value, len(items))
		}
	} else {
		var cursorErr error
		start, end, cursorErr = p.pageBounds(node, scope, len(items))
		if cursorErr != nil {
			failure := nodeExecutionError("INVALID_CURSOR", "collection cursor is invalid", node, path, cursorErr)
			return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
		}
	}
	if start > end {
		failure := nodeExecutionError("INVALID_SLICE", "collection slice is invalid", node, path, errors.New("runtime slice start exceeds end"))
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}
	window := items[start:end]
	if len(node.children) == 0 {
		copied := append([]any(nil), window...)
		value := executionValue{value: copied, typeInfo: node.output, status: valueAvailable}
		return nodeResult{value: value, data: copied, emit: node.outputName != ""}
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	output := make([]any, len(window))
	var failures []ExecutionError
	failed := false
	for offset, item := range window {
		itemValue := collectionItemValue(item, descriptor, start+offset)
		if itemValue.status == valueNull {
			output[offset] = nil
			continue
		}
		childScope := childExecutionScope(scope, itemValue, scope.current)
		children := p.executeSequence(ctx, node.children, childScope, appendPathSegments(path, offset))
		failures = append(failures, children.errors...)
		if children.failed && len(children.data) == 0 {
			output[offset] = nil
		} else {
			output[offset] = children.data
		}
		failed = failed || children.failed
		if containsExecutionCode(children.errors, "CANCELLED") {
			break
		}
	}
	value := executionValue{value: output, typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: output, emit: node.outputName != "", failed: failed, errors: failures}
}

func (p *Plan) executeMetadata(node planNode, scope executionScope, path []any) nodeResult {
	items, ok := collectionItems(scope.current)
	if !ok {
		return invalidCollectionResult(node, path)
	}
	value := executionValue{value: int32(len(items)), typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: value.value, emit: node.outputName != ""}
}

func (p *Plan) pageBounds(node planNode, scope executionScope, length int) (int, int, error) {
	start, end := 0, length
	if after, ok := node.selection.After(); ok {
		cursor, err := p.cursorIndex(after, scope)
		if err != nil {
			return 0, 0, err
		}
		start = min(cursor+1, length)
	}
	if before, ok := node.selection.Before(); ok {
		cursor, err := p.cursorIndex(before, scope)
		if err != nil {
			return 0, 0, err
		}
		end = min(cursor, length)
	}
	if first, ok := node.selection.First(); ok {
		end = min(end, start+clampedBound(first, length))
	}
	if last, ok := node.selection.Last(); ok {
		start = max(start, end-clampedBound(last, length))
	}
	return start, end, nil
}

func (p *Plan) cursorIndex(expression protocol.Expression, scope executionScope) (int, error) {
	raw, _, err := p.evaluateExpression(expression, scope)
	if err != nil {
		return 0, err
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value, parseErr := strconv.ParseInt(text, 10, 64)
		if parseErr != nil || value < 0 || value > 9007199254740991 {
			return 0, errors.New("cursor is not a non-negative collection offset")
		}
		return int(value), nil
	}
	var number int64
	if err := json.Unmarshal(raw, &number); err != nil || number < 0 || number > 9007199254740991 {
		return 0, errors.New("cursor is not a non-negative collection offset")
	}
	return int(number), nil
}

func exactIndex(value *big.Int) (int, bool) {
	if value == nil || !value.IsInt64() {
		return 0, false
	}
	index := value.Int64()
	if index < 0 || int64(int(index)) != index {
		return 0, false
	}
	return int(index), true
}

func clampedBound(value *big.Int, length int) int {
	if value == nil || !value.IsInt64() || value.Sign() < 0 {
		return 0
	}
	if value.Cmp(big.NewInt(int64(length))) > 0 {
		return length
	}
	return int(value.Int64())
}
