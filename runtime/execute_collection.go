package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

func (p *Plan) executeMap(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	items, admissionFailure := admittedCollection(scope, node, path)
	if admissionFailure != nil {
		return *admissionFailure
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	output, failures, failed := p.executeCollectionItems(ctx, node, scope, collectionExecution{
		items: items, descriptor: descriptor, path: path,
	})
	value := executionValue{value: output, typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: output, emit: node.outputName != "", failed: failed, errors: failures}
}

func (p *Plan) executeIndex(ctx context.Context, node planNode, scope executionScope, path []any) nodeResult {
	items, admissionFailure := admittedCollection(scope, node, path)
	if admissionFailure != nil {
		return *admissionFailure
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
	value := collectionItemValue(scope.current, items[index], descriptor, index)
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
	items, admissionFailure := admittedCollection(scope, node, path)
	if admissionFailure != nil {
		return *admissionFailure
	}
	if node.kind == protocol.PageSelection {
		return p.executeSecureCollectionPage(ctx, node, scope, items, path)
	}
	start, end := 0, len(items)
	if node.kind == protocol.SliceSelection {
		if value, exists := node.selection.Start(); exists {
			start = clampedBound(value, len(items))
		}
		if value, exists := node.selection.End(); exists {
			end = clampedBound(value, len(items))
		}
	}
	if start > end {
		failure := nodeExecutionError("INVALID_SLICE", "collection slice is invalid", node, path, errors.New("runtime slice start exceeds end"))
		return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
	}
	window := items[start:end]
	if !scope.resources.chargeCollection(uint64(len(window))) {
		return resourceExhaustedResult(node, path, "collection item budget exhausted")
	}
	if len(node.children) == 0 {
		copied := append([]any(nil), window...)
		value := executionValue{value: copied, typeInfo: node.output, status: valueAvailable}
		return nodeResult{value: value, data: copied, emit: node.outputName != ""}
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	output, failures, failed := p.executeCollectionItems(ctx, node, scope, collectionExecution{
		items: window, descriptor: descriptor, sourceIndexBase: start, path: path,
	})
	value := executionValue{value: output, typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: output, emit: node.outputName != "", failed: failed, errors: failures}
}

func (p *Plan) preflightCollectionCursors(ctx context.Context, nodes []planNode, scope executionScope) []ExecutionError {
	var failures []ExecutionError
	var walk func([]planNode, []any)
	walk = func(current []planNode, path []any) {
		for _, node := range current {
			nodePath := pathForNode(path, node)
			if node.kind == protocol.PageSelection {
				cursorScope, err := p.validatePreparedCursor(ctx, node, scope)
				if err != nil {
					failures = append(failures, nodeExecutionError(CodeInvalidCursor, "collection cursor is invalid", node, nodePath, err))
				} else {
					scope.cursorScopes[node.cursorID] = cursorScope
				}
			}
			walk(node.children, nodePath)
		}
	}
	walk(nodes, nil)
	return failures
}

func (p *Plan) validatePreparedCursor(ctx context.Context, node planNode, scope executionScope) (CursorScope, error) {
	metadata := node.input.collection
	if !hasSecureCollectionRuntime(metadata) {
		return CursorScope{}, ErrInvalidCursor
	}
	cursorScope, err := metadata.CursorScope(ctx)
	if err != nil {
		return CursorScope{}, ErrInvalidCursor
	}
	var expression protocol.Expression
	var direction CursorDirection
	if after, ok := node.selection.After(); ok {
		expression, direction = after, CursorForward
	} else if before, ok := node.selection.Before(); ok {
		expression, direction = before, CursorBackward
	} else {
		return cursorScope, nil
	}
	cursor, err := p.cursorText(expression, scope)
	if err != nil {
		return CursorScope{}, ErrInvalidCursor
	}
	_, err = metadata.CursorCodec.Decode(cursor, cursorScope, direction)
	if err != nil {
		return CursorScope{}, err
	}
	return cursorScope, nil
}

func (p *Plan) executeSecureCollectionPage(ctx context.Context, node planNode, scope executionScope, items []any, path []any) nodeResult {
	metadata := node.input.collection
	cursorScope, ok := scope.cursorScopes[node.cursorID]
	if !ok {
		return invalidCursorResult(node, path, ErrInvalidCursor)
	}
	entries, err := collectionPageEntries(items, metadata)
	if err != nil {
		return invalidCursorResult(node, path, err)
	}
	request, err := p.collectionPageRequest(node, scope, cursorScope)
	if err != nil {
		return invalidCursorResult(node, path, err)
	}
	page, err := Paginate(metadata.CursorCodec, entries, request)
	if err != nil {
		return invalidCursorResult(node, path, err)
	}
	if !scope.resources.chargeCollection(uint64(len(page.Items))) {
		return resourceExhaustedResult(node, path, "collection item budget exhausted")
	}
	window := make([]any, len(page.Items))
	for index, item := range page.Items {
		window[index] = item.value
	}
	descriptor, _ := p.types.Lookup(node.input.id)
	sourceIndexBase := 0
	if len(page.Items) != 0 {
		sourceIndexBase = page.Items[0].index
	}
	output, failures, failed := p.executeCollectionItems(ctx, node, scope, collectionExecution{
		items: window, descriptor: descriptor, sourceIndexBase: sourceIndexBase, path: path,
	})
	presentation := presentCollectionPage(page, output)
	value := executionValue{value: output, typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: presentation, emit: node.outputName != "", failed: failed, errors: failures}
}

type indexedCollectionItem struct {
	value any
	index int
}

func collectionPageEntries(items []any, metadata *CollectionMetadata) ([]PageEntry[indexedCollectionItem], error) {
	entries := make([]PageEntry[indexedCollectionItem], 0, len(items))
	for index, item := range items {
		position, err := metadata.Position(item)
		if err != nil {
			return nil, err
		}
		var edge map[string]any
		if metadata.EdgeMetadata != nil {
			edge, err = metadata.EdgeMetadata(item)
			if err != nil {
				return nil, err
			}
		}
		entries = append(entries, PageEntry[indexedCollectionItem]{
			Item: indexedCollectionItem{value: item, index: index}, Position: position, EdgeMetadata: edge,
		})
	}
	return entries, nil
}

func presentCollectionPage(page CollectionPage[indexedCollectionItem], output []any) map[string]any {
	presentation := map[string]any{"items": output, "pageInfo": page.PageInfo}
	if len(page.Edges) == 0 {
		return presentation
	}
	edges := make([]any, len(page.Edges))
	for index, edge := range page.Edges {
		edges[index] = map[string]any{"cursor": edge.Cursor, "item": output[index], "metadata": edge.Metadata}
	}
	presentation["edges"] = edges
	return presentation
}

func (p *Plan) collectionPageRequest(node planNode, scope executionScope, cursorScope CursorScope) (PageRequest, error) {
	request := PageRequest{Scope: cursorScope}
	if first, ok := node.selection.First(); ok && first.IsUint64() {
		value := first.Uint64()
		request.First = &value
	}
	if last, ok := node.selection.Last(); ok && last.IsUint64() {
		value := last.Uint64()
		request.Last = &value
	}
	if after, ok := node.selection.After(); ok {
		cursor, err := p.cursorText(after, scope)
		if err != nil {
			return PageRequest{}, err
		}
		request.After = cursor
	}
	if before, ok := node.selection.Before(); ok {
		cursor, err := p.cursorText(before, scope)
		if err != nil {
			return PageRequest{}, err
		}
		request.Before = cursor
	}
	return request, nil
}

func (p *Plan) cursorText(expression protocol.Expression, scope executionScope) (string, error) {
	raw, _, err := p.evaluateExpression(expression, scope)
	if err != nil {
		return "", err
	}
	var cursor string
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor == "" {
		return "", ErrInvalidCursor
	}
	return cursor, nil
}

func invalidCursorResult(node planNode, path []any, cause error) nodeResult {
	failure := nodeExecutionError(CodeInvalidCursor, "collection cursor is invalid", node, path, cause)
	return nodeResult{value: executionValue{typeInfo: node.output, status: valueUnavailable}, failed: true, errors: []ExecutionError{failure}}
}

func admittedCollection(scope executionScope, node planNode, path []any) ([]any, *nodeResult) {
	items, ok := collectionItems(scope.current)
	if !ok {
		failure := invalidCollectionResult(node, path)
		return nil, &failure
	}
	if !scope.resources.chargeCollection(uint64(len(items))) {
		failure := resourceExhaustedResult(node, path, "collection item budget exhausted")
		return nil, &failure
	}
	return items, nil
}

type collectionExecution struct {
	items           []any
	descriptor      schema.TypeDescriptor
	sourceIndexBase int
	path            []any
}

func (p *Plan) executeCollectionItems(ctx context.Context, node planNode, scope executionScope, execution collectionExecution) ([]any, []ExecutionError, bool) {
	if p.hasBatchCollectionChild(node.children) {
		return p.executeBatchedCollectionItems(ctx, node, scope, execution)
	}
	return p.executeCollectionItemsSequential(ctx, node, scope, execution)
}

func (p *Plan) executeCollectionItemsSequential(ctx context.Context, node planNode, scope executionScope, execution collectionExecution) ([]any, []ExecutionError, bool) {
	output := make([]any, len(execution.items))
	var failures []ExecutionError
	failed := false
	for offset, item := range execution.items {
		itemValue := collectionItemValue(scope.current, item, execution.descriptor, execution.sourceIndexBase+offset)
		// A failed element is a null placeholder, so later indexes do not shift
		// and no member handler runs for it: one indexed error, never two.
		if itemValue.status == valueNull || itemValue.status == valueUnavailable {
			continue
		}
		itemScope := childExecutionScope(scope, itemValue, scope.current)
		itemPath := appendPathSegments(execution.path, offset)
		children := p.executeSequence(ctx, node.children, itemScope, itemPath)
		var errorBudgetExhausted bool
		failures, errorBudgetExhausted = appendCollectionErrors(
			failures, children.errors, p.resourceLimits.MaxErrors, node, offset+1 < len(execution.items),
		)
		if errorBudgetExhausted {
			return output, failures, true
		}
		if children.failed && len(children.data) == 0 {
			output[offset] = nil
		} else {
			output[offset] = children.data
		}
		failed = failed || children.failed
		if executionShouldStop(children.errors) {
			break
		}
	}
	return output, failures, failed
}

func (p *Plan) executeMetadata(node planNode, scope executionScope, path []any) nodeResult {
	items, ok := collectionItems(scope.current)
	if !ok {
		return invalidCollectionResult(node, path)
	}
	value := executionValue{value: int32(len(items)), typeInfo: node.output, status: valueAvailable}
	return nodeResult{value: value, data: value.value, emit: node.outputName != ""}
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
