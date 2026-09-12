package protocol

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// DecodeDocument decodes one standalone core.language-1 document into an
// immutable typed AST without performing schema or registry validation.
func DecodeDocument(input []byte, limits Limits) (Document, error) {
	limits = limits.withDefaults()
	root, err := parseJSON(input, limits)
	if err != nil {
		return Document{}, err
	}
	return (languageDecoder{input: input, limits: limits}).document(root, "")
}

type languageDecoder struct {
	input  []byte
	limits Limits
}

var maxStructuralInteger = new(big.Int).SetUint64(9_007_199_254_740_991)

func (d languageDecoder) document(value node, pointer string) (Document, error) {
	if err := expectKindPhase(d.input, value, nodeObject, pointer, "document object", "LANG-001", "validate"); err != nil {
		return Document{}, err
	}
	if err := rejectUnknown(d.input, value, map[string]bool{"operations": true, "fragments": true, "requires": true}, "LANG-001", "validate", pointer); err != nil {
		return Document{}, err
	}
	operationsNode, exists := value.member("operations")
	operationsPointer := joinPointer(pointer, "operations")
	if !exists {
		return Document{}, newDiagnostic(d.input, "MISSING_FIELD", "LANG-001", "validate", "document requires an operations array", operationsPointer, value.start)
	}
	if operationsNode.kind != nodeArray {
		return Document{}, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-001", "validate", "operations must be an array", operationsPointer, operationsNode.start)
	}
	operations, err := d.operations(operationsNode, operationsPointer)
	if err != nil {
		return Document{}, err
	}
	document := Document{operations: operations, source: sourceAt(d.input, pointer, value)}
	if fragmentsNode, ok := value.member("fragments"); ok {
		document.fragments, err = d.fragments(fragmentsNode, joinPointer(pointer, "fragments"))
		if err != nil {
			return Document{}, err
		}
	}
	if requiresNode, ok := value.member("requires"); ok {
		document.requires, err = d.requirements(requiresNode, joinPointer(pointer, "requires"))
		if err != nil {
			return Document{}, err
		}
	}
	document.canonical, err = d.canonicalDocument(value)
	return document, err
}

func (d languageDecoder) operations(value node, pointer string) ([]Operation, error) {
	operations := make([]Operation, 0, len(value.array))
	operationNames := make(map[string]bool, len(value.array))
	for index, operationNode := range value.array {
		operation, err := d.operation(operationNode, joinPointer(pointer, intString(index)))
		if err != nil {
			return nil, err
		}
		if operationNames[operation.name] {
			return nil, newDiagnostic(d.input, "DUPLICATE_NAME", "LANG-005", "validate", "duplicate operation name", operation.source.Pointer, operation.source.Start)
		}
		operationNames[operation.name] = true
		operations = append(operations, operation)
	}
	return operations, nil
}

func (d languageDecoder) fragments(value node, pointer string) ([]Fragment, error) {
	if value.kind != nodeArray {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-001", "validate", "fragments must be an array", pointer, value.start)
	}
	fragments := make([]Fragment, 0, len(value.array))
	fragmentNames := make(map[string]bool, len(value.array))
	for index, fragmentNode := range value.array {
		fragment, err := d.fragment(fragmentNode, joinPointer(pointer, intString(index)))
		if err != nil {
			return nil, err
		}
		if fragmentNames[fragment.name] {
			return nil, newDiagnostic(d.input, "DUPLICATE_NAME", "LANG-005", "validate", "duplicate fragment name", fragment.source.Pointer, fragment.source.Start)
		}
		fragmentNames[fragment.name] = true
		fragments = append(fragments, fragment)
	}
	return fragments, nil
}

func (d languageDecoder) requirements(value node, pointer string) ([]string, error) {
	if value.kind != nodeArray {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-001", "validate", "requires must be an array", pointer, value.start)
	}
	requirements := make([]string, 0, len(value.array))
	seen := make(map[string]bool, len(value.array))
	for index, requirement := range value.array {
		itemPointer := joinPointer(pointer, intString(index))
		name, err := d.requirement(requirement, itemPointer)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, newDiagnostic(d.input, "DUPLICATE_NAME", "LANG-005", "validate", "duplicate required capability", itemPointer, requirement.start)
		}
		seen[name] = true
		requirements = append(requirements, name)
	}
	return requirements, nil
}

func (d languageDecoder) requirement(value node, pointer string) (string, error) {
	if value.kind != nodeString {
		return "", newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-001", "validate", "requires entry must be a string", pointer, value.start)
	}
	if !profileIdentifierPattern.MatchString(value.text) {
		return "", newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-002", "validate", "requires entry must be a portable identifier", pointer, value.start)
	}
	return value.text, nil
}

func (d languageDecoder) canonicalDocument(value node) (json.RawMessage, error) {
	canonicalRoot := value
	if err := normalizeDocument(d.input, &canonicalRoot); err != nil {
		return nil, err
	}
	return canonicalizeNode(canonicalRoot)
}

func (d languageDecoder) operation(value node, pointer string) (Operation, error) {
	if err := expectKindPhase(d.input, value, nodeObject, pointer, "operation object", "LANG-002", "validate"); err != nil {
		return Operation{}, err
	}
	if err := rejectUnknown(d.input, value, map[string]bool{"name": true, "kind": true, "variables": true, "select": true}, "LANG-002", "validate", pointer); err != nil {
		return Operation{}, err
	}
	name, err := d.requiredIdentifier(value, "name", pointer, false)
	if err != nil {
		return Operation{}, err
	}
	kindNode, hasKind := value.member("kind")
	if !hasKind {
		return Operation{}, newDiagnostic(d.input, "INVALID_OPERATION_KIND", "CORE-003", "validate", "operation kind must be query, mutation, or subscription", joinPointer(pointer, "kind"), value.start)
	}
	if kindNode.kind != nodeString || (kindNode.text != string(Query) && kindNode.text != string(Mutation) && kindNode.text != string(Subscription)) {
		return Operation{}, newDiagnostic(d.input, "INVALID_OPERATION_KIND", "CORE-003", "validate", "operation kind must be query, mutation, or subscription", joinPointer(pointer, "kind"), kindNode.start)
	}
	selectNode, hasSelect := value.member("select")
	selectPointer := joinPointer(pointer, "select")
	if !hasSelect {
		return Operation{}, newDiagnostic(d.input, "INVALID_SELECTIONS", "LANG-002", "validate", "operation requires a selection array", selectPointer, value.start)
	}
	if selectNode.kind != nodeArray {
		return Operation{}, newDiagnostic(d.input, "INVALID_SELECTIONS", "LANG-002", "validate", "operation requires a selection array", selectPointer, selectNode.start)
	}
	selections, err := d.selections(selectNode, selectPointer, false)
	if err != nil {
		return Operation{}, err
	}
	operation := Operation{name: name, kind: OperationKind(kindNode.text), selections: selections, source: sourceAt(d.input, pointer, value)}
	if variablesNode, ok := value.member("variables"); ok {
		operation.variables, err = d.variables(variablesNode, joinPointer(pointer, "variables"))
		if err != nil {
			return Operation{}, err
		}
	}
	return operation, nil
}

func (d languageDecoder) variables(value node, pointer string) ([]VariableDefinition, error) {
	if value.kind != nodeArray {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-005", "validate", "variables must be an array", pointer, value.start)
	}
	variables := make([]VariableDefinition, 0, len(value.array))
	seen := make(map[string]bool, len(value.array))
	for index, variableNode := range value.array {
		variable, err := d.variable(variableNode, joinPointer(pointer, intString(index)))
		if err != nil {
			return nil, err
		}
		if seen[variable.name] {
			return nil, newDiagnostic(d.input, "DUPLICATE_NAME", "LANG-005", "validate", "duplicate variable name", variable.source.Pointer, variable.source.Start)
		}
		seen[variable.name] = true
		variables = append(variables, variable)
	}
	return variables, nil
}

func (d languageDecoder) variable(value node, pointer string) (VariableDefinition, error) {
	if err := expectKindPhase(d.input, value, nodeObject, pointer, "variable declaration", "LANG-005", "validate"); err != nil {
		return VariableDefinition{}, err
	}
	if err := rejectUnknown(d.input, value, map[string]bool{"name": true, "type": true, "required": true, "nullable": true, "default": true}, "LANG-005", "validate", pointer); err != nil {
		return VariableDefinition{}, err
	}
	name, err := d.requiredIdentifier(value, "name", pointer, false)
	if err != nil {
		return VariableDefinition{}, err
	}
	typeID, err := d.requiredIdentifier(value, "type", pointer, true)
	if err != nil {
		return VariableDefinition{}, err
	}
	variable := VariableDefinition{name: name, typeID: typeID, source: sourceAt(d.input, pointer, value)}
	if variable.required, err = d.optionalBoolean(value, "required", pointer); err != nil {
		return VariableDefinition{}, err
	}
	if variable.nullable, err = d.optionalBoolean(value, "nullable", pointer); err != nil {
		return VariableDefinition{}, err
	}
	if defaultNode, ok := value.member("default"); ok {
		variable.defaultRaw, err = d.literal(defaultNode, joinPointer(pointer, "default"))
		if err != nil {
			return VariableDefinition{}, err
		}
		variable.hasDefault = true
	}
	return variable, nil
}

func (d languageDecoder) fragment(value node, pointer string) (Fragment, error) {
	if err := expectKindPhase(d.input, value, nodeObject, pointer, "fragment declaration", "LANG-002", "validate"); err != nil {
		return Fragment{}, err
	}
	if err := rejectUnknown(d.input, value, map[string]bool{"name": true, "on": true, "parameters": true, "select": true}, "LANG-002", "validate", pointer); err != nil {
		return Fragment{}, err
	}
	name, err := d.requiredIdentifier(value, "name", pointer, false)
	if err != nil {
		return Fragment{}, err
	}
	fragment := Fragment{name: name, source: sourceAt(d.input, pointer, value)}
	if onNode, ok := value.member("on"); ok {
		if onNode.kind != nodeString {
			return Fragment{}, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-002", "validate", "fragment type condition must be a string", joinPointer(pointer, "on"), onNode.start)
		}
		if !profileIdentifierPattern.MatchString(onNode.text) {
			return Fragment{}, newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-002", "validate", "fragment type condition must be a portable type identifier", joinPointer(pointer, "on"), onNode.start)
		}
		fragment.on = onNode.text
	}
	// Parameters reuse the variable declaration shape, so duplicate names,
	// defaults, and nullability are decoded by one rule rather than two.
	if parametersNode, ok := value.member("parameters"); ok {
		fragment.parameters, err = d.variables(parametersNode, joinPointer(pointer, "parameters"))
		if err != nil {
			return Fragment{}, err
		}
	}
	selectNode, hasSelect := value.member("select")
	selectPointer := joinPointer(pointer, "select")
	if !hasSelect {
		return Fragment{}, newDiagnostic(d.input, "INVALID_SELECTIONS", "LANG-002", "validate", "fragment requires a selection array", selectPointer, value.start)
	}
	if selectNode.kind != nodeArray {
		return Fragment{}, newDiagnostic(d.input, "INVALID_SELECTIONS", "LANG-002", "validate", "fragment requires a selection array", selectPointer, selectNode.start)
	}
	fragment.selections, err = d.selections(selectNode, selectPointer, false)
	return fragment, err
}

func (d languageDecoder) selections(value node, pointer string, pipelineStep bool) ([]Selection, error) {
	if value.kind != nodeArray {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-003", "validate", "selections must be an array", pointer, value.start)
	}
	result := make([]Selection, 0, len(value.array))
	for index, selectionNode := range value.array {
		selection, err := d.selection(selectionNode, joinPointer(pointer, intString(index)), pipelineStep)
		if err != nil {
			return nil, err
		}
		result = append(result, selection)
	}
	return result, nil
}

type selectionShape struct {
	kind           SelectionKind
	allowed        map[string]bool
	requireName    bool
	requireAlias   bool
	requireSelect  bool
	requireStages  bool
	requireAt      bool
	parallelPolicy bool
}

var responseSelectionShapes = map[string]selectionShape{
	"$field":    {kind: FieldSelection, allowed: selectionMembers("name", "as", "select"), requireName: true},
	"$call":     {kind: CallSelection, allowed: selectionMembers("name", "as", "args", "select"), requireName: true},
	"$pipeline": {kind: PipelineSelection, allowed: selectionMembers("as", "stages"), requireAlias: true, requireStages: true},
	"$map":      {kind: MapSelection, allowed: selectionMembers("as", "select"), requireAlias: true, requireSelect: true},
	"$index":    {kind: IndexSelection, allowed: selectionMembers("as", "at", "select"), requireAlias: true, requireAt: true},
	"$slice":    {kind: SliceSelection, allowed: selectionMembers("as", "start", "end", "select"), requireAlias: true},
	"$page":     {kind: PageSelection, allowed: selectionMembers("as", "first", "after", "last", "before", "select"), requireAlias: true},
	"$meta":     {kind: MetaSelection, allowed: selectionMembers("name", "as"), requireName: true},
	"$parallel": {kind: ParallelSelection, allowed: allowedMembers("policy", "directives", "select"), requireSelect: true, parallelPolicy: true},
	"$fragment": {kind: FragmentSelection, allowed: allowedMembers("name", "on", "directives", "args", "select")},
	"$current":  {kind: CurrentSelection, allowed: selectionMembers("as"), requireAlias: true},
	"$nest":     {kind: NestSelection, allowed: selectionMembers("as", "select"), requireAlias: true, requireSelect: true},
	"$unnest":   {kind: UnnestSelection, allowed: allowedMembers("directives", "select"), requireSelect: true},
}

var pipelineSelectionShapes = map[string]selectionShape{
	"$field":   {kind: FieldSelection, allowed: selectionMembers("name", "select"), requireName: true},
	"$call":    {kind: CallSelection, allowed: selectionMembers("name", "args", "select"), requireName: true},
	"$map":     {kind: MapSelection, allowed: selectionMembers("select"), requireSelect: true},
	"$index":   {kind: IndexSelection, allowed: selectionMembers("at", "select"), requireAt: true},
	"$slice":   {kind: SliceSelection, allowed: selectionMembers("start", "end", "select")},
	"$page":    {kind: PageSelection, allowed: selectionMembers("first", "after", "last", "before", "select")},
	"$meta":    {kind: MetaSelection, allowed: selectionMembers("name"), requireName: true},
	"$current": {kind: CurrentSelection, allowed: selectionMembers()},
}

func allowedMembers(names ...string) map[string]bool {
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	return allowed
}

func selectionMembers(names ...string) map[string]bool {
	return allowedMembers(append(names, "bind", "directives")...)
}

func shapeForSelection(tag string, pipelineStep bool) (selectionShape, bool) {
	if pipelineStep {
		shape, ok := pipelineSelectionShapes[tag]
		return shape, ok
	}
	shape, ok := responseSelectionShapes[tag]
	return shape, ok
}

func (d languageDecoder) selection(value node, pointer string, pipelineStep bool) (Selection, error) {
	if value.kind != nodeObject || len(value.object) != 1 {
		return Selection{}, newDiagnostic(d.input, "INVALID_SELECTION", "LANG-003", "validate", "selection must be a one-key tagged object", pointer, value.start)
	}
	tag := value.object[0].name
	payload := value.object[0].value
	shape, ok := shapeForSelection(tag, pipelineStep)
	if !ok {
		return Selection{}, newDiagnostic(d.input, "UNKNOWN_SELECTION", "LANG-003", "validate", "unknown selection or pipeline-step tag", joinPointer(pointer, tag), value.object[0].start)
	}
	payloadPointer := joinPointer(pointer, tag)
	if err := expectKindPhase(d.input, payload, nodeObject, payloadPointer, "selection payload object", "LANG-003", "validate"); err != nil {
		return Selection{}, err
	}
	if err := rejectUnknown(d.input, payload, shape.allowed, "LANG-004", "validate", payloadPointer); err != nil {
		return Selection{}, err
	}
	selection := Selection{
		kind: shape.kind, source: sourceAt(d.input, pointer, value),
		payloadSource: sourceAt(d.input, payloadPointer, payload),
	}
	if err := d.selectionNames(payload, payloadPointer, shape, &selection); err != nil {
		return Selection{}, err
	}
	if selection.kind == FragmentSelection {
		if err := d.fragmentSelection(payload, payloadPointer, &selection); err != nil {
			return Selection{}, err
		}
	}
	if err := d.selectionDecorators(payload, payloadPointer, &selection); err != nil {
		return Selection{}, err
	}
	if err := d.selectionChildren(payload, payloadPointer, shape, &selection); err != nil {
		return Selection{}, err
	}
	if err := d.selectionBounds(payload, payloadPointer, shape, &selection); err != nil {
		return Selection{}, err
	}
	return selection, nil
}

func (d languageDecoder) selectionNames(payload node, pointer string, shape selectionShape, selection *Selection) error {
	var err error
	if shape.requireName {
		selection.name, err = d.requiredIdentifier(payload, "name", pointer, false)
		if err != nil {
			return err
		}
		if nameNode, exists := payload.member("name"); exists {
			selection.nameSource = sourceAt(d.input, joinPointer(pointer, "name"), nameNode)
		}
	}
	if aliasNode, exists := payload.member("as"); exists {
		if aliasNode.kind != nodeString {
			return newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-002", "validate", "response alias must be a string", joinPointer(pointer, "as"), aliasNode.start)
		}
		if !identifierPattern.MatchString(aliasNode.text) {
			return newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-002", "validate", "response alias must be a portable identifier", joinPointer(pointer, "as"), aliasNode.start)
		}
		selection.alias = aliasNode.text
		selection.aliasSource = sourceAt(d.input, joinPointer(pointer, "as"), aliasNode)
	} else if shape.requireAlias {
		return newDiagnostic(d.input, "MISSING_FIELD", "LANG-008", "validate", "selection requires a response alias", joinPointer(pointer, "as"), payload.start)
	}
	if bindNode, exists := payload.member("bind"); exists {
		if bindNode.kind != nodeString {
			return newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-220", "validate", "binding must be a string", joinPointer(pointer, "bind"), bindNode.start)
		}
		if !identifierPattern.MatchString(bindNode.text) {
			return newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-220", "validate", "binding must be a portable identifier", joinPointer(pointer, "bind"), bindNode.start)
		}
		selection.bind = bindNode.text
		selection.bindSource = sourceAt(d.input, joinPointer(pointer, "bind"), bindNode)
	}
	return nil
}

func (d languageDecoder) fragmentSelection(payload node, pointer string, selection *Selection) error {
	if nameNode, hasName := payload.member("name"); hasName {
		name, err := d.requiredIdentifier(payload, "name", pointer, false)
		if err != nil {
			return err
		}
		selection.name = name
		selection.nameSource = sourceAt(d.input, joinPointer(pointer, "name"), nameNode)
	}
	onNode, hasOn := payload.member("on")
	_, hasSelect := payload.member("select")
	_, hasArguments := payload.member("args")
	if selection.name != "" {
		if hasOn || hasSelect {
			return newDiagnostic(d.input, "INVALID_FRAGMENT", "LANG-240", "validate", "named fragment spreads cannot declare an inline type condition or selections", pointer, payload.start)
		}
		return nil
	}
	if !hasOn || !hasSelect {
		return newDiagnostic(d.input, "INVALID_FRAGMENT", "LANG-240", "validate", "inline fragments require on and select", pointer, payload.start)
	}
	if hasArguments {
		return newDiagnostic(d.input, "INVALID_FRAGMENT", "LANG-244", "validate", "inline fragments cannot bind fragment arguments", joinPointer(pointer, "args"), payload.start)
	}
	if onNode.kind != nodeString {
		return newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-240", "validate", "inline fragment type condition must be a string", joinPointer(pointer, "on"), onNode.start)
	}
	if !profileIdentifierPattern.MatchString(onNode.text) {
		return newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-240", "validate", "inline fragment type condition must be a portable type identifier", joinPointer(pointer, "on"), onNode.start)
	}
	selection.typeCondition = onNode.text
	return nil
}

func (d languageDecoder) selectionDecorators(payload node, pointer string, selection *Selection) error {
	var err error
	if argsNode, exists := payload.member("args"); exists {
		selection.arguments, err = d.arguments(argsNode, joinPointer(pointer, "args"))
		if err != nil {
			return err
		}
	}
	if directivesNode, exists := payload.member("directives"); exists {
		selection.directives, err = d.directives(directivesNode, joinPointer(pointer, "directives"))
		if err != nil {
			return err
		}
	}
	return nil
}

func (d languageDecoder) selectionChildren(payload node, pointer string, shape selectionShape, selection *Selection) error {
	var err error
	if selectNode, exists := payload.member("select"); exists {
		selection.children, err = d.selections(selectNode, joinPointer(pointer, "select"), false)
		if err != nil {
			return err
		}
	} else if shape.requireSelect {
		return newDiagnostic(d.input, "MISSING_FIELD", "LANG-010", "validate", "selection requires select", joinPointer(pointer, "select"), payload.start)
	}
	if stagesNode, exists := payload.member("stages"); exists {
		stagesPointer := joinPointer(pointer, "stages")
		if stagesNode.kind != nodeArray || len(stagesNode.array) == 0 {
			return newDiagnostic(d.input, "INVALID_SELECTIONS", "LANG-009", "validate", "pipeline requires non-empty stages", stagesPointer, stagesNode.start)
		}
		selection.stages, err = d.selections(stagesNode, stagesPointer, true)
		if err != nil {
			return err
		}
	} else if shape.requireStages {
		return newDiagnostic(d.input, "MISSING_FIELD", "LANG-009", "validate", "pipeline requires stages", joinPointer(pointer, "stages"), payload.start)
	}
	return nil
}

func (d languageDecoder) selectionBounds(payload node, pointer string, shape selectionShape, selection *Selection) error {
	if err := d.integerBounds(payload, pointer, shape, selection); err != nil {
		return err
	}
	if err := d.cursorBounds(payload, pointer, selection); err != nil {
		return err
	}
	if err := d.validatePage(payload, pointer, shape, selection); err != nil {
		return err
	}
	return d.parallelPolicy(payload, pointer, shape, selection)
}

func (d languageDecoder) integerBounds(payload node, pointer string, shape selectionShape, selection *Selection) error {
	var err error
	if selection.at, err = d.optionalNonNegativeInteger(payload, "at", pointer, true); err != nil {
		return err
	}
	if shape.requireAt && selection.at == nil {
		return newDiagnostic(d.input, "MISSING_FIELD", "LANG-010", "validate", "index requires at", joinPointer(pointer, "at"), payload.start)
	}
	if selection.start, err = d.optionalNonNegativeInteger(payload, "start", pointer, true); err != nil {
		return err
	}
	if selection.end, err = d.optionalNonNegativeInteger(payload, "end", pointer, true); err != nil {
		return err
	}
	if selection.first, err = d.optionalNonNegativeInteger(payload, "first", pointer, false); err != nil {
		return err
	}
	if selection.last, err = d.optionalNonNegativeInteger(payload, "last", pointer, false); err != nil {
		return err
	}
	return nil
}

func (d languageDecoder) cursorBounds(payload node, pointer string, selection *Selection) error {
	if afterNode, ok := payload.member("after"); ok {
		after, decodeErr := d.expression(afterNode, joinPointer(pointer, "after"))
		if decodeErr != nil {
			return decodeErr
		}
		selection.after = &after
	}
	if beforeNode, ok := payload.member("before"); ok {
		before, decodeErr := d.expression(beforeNode, joinPointer(pointer, "before"))
		if decodeErr != nil {
			return decodeErr
		}
		selection.before = &before
	}
	return nil
}

func (d languageDecoder) validatePage(payload node, pointer string, shape selectionShape, selection *Selection) error {
	hasFirst := selection.first != nil
	hasLast := selection.last != nil
	pageMembers := hasFirst || hasLast || selection.after != nil || selection.before != nil
	if pageMembers {
		forward := hasFirst && !hasLast && selection.before == nil
		backward := hasLast && !hasFirst && selection.after == nil
		if !forward && !backward {
			return newDiagnostic(d.input, "INVALID_PAGE", "LANG-010", "validate", "page requires exactly one direction and its matching cursor", pointer, payload.start)
		}
	}
	if shape.kind == PageSelection && !hasFirst && !hasLast {
		return newDiagnostic(d.input, "MISSING_FIELD", "LANG-010", "validate", "page requires first or last", pointer, payload.start)
	}
	return nil
}

func (d languageDecoder) parallelPolicy(payload node, pointer string, shape selectionShape, selection *Selection) error {
	if !shape.parallelPolicy {
		return nil
	}
	selection.policy = CollectParallel
	policyNode, ok := payload.member("policy")
	if !ok {
		return nil
	}
	if policyNode.kind != nodeString || (policyNode.text != string(CollectParallel) && policyNode.text != string(FailFastParallel)) {
		return newDiagnostic(d.input, "INVALID_POLICY", "LANG-011", "validate", "parallel policy must be collect or fail-fast", joinPointer(pointer, "policy"), policyNode.start)
	}
	selection.policy = ParallelPolicy(policyNode.text)
	return nil
}

func (d languageDecoder) arguments(value node, pointer string) (map[string]Expression, error) {
	if value.kind != nodeObject {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-004", "validate", "arguments must be an object", pointer, value.start)
	}
	arguments := make(map[string]Expression, len(value.object))
	for _, argument := range value.object {
		argumentPointer := joinPointer(pointer, argument.name)
		if !identifierPattern.MatchString(argument.name) {
			return nil, newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-002", "validate", "argument name must be a portable identifier", argumentPointer, argument.start)
		}
		expression, err := d.expression(argument.value, argumentPointer)
		if err != nil {
			return nil, err
		}
		arguments[argument.name] = expression
	}
	return arguments, nil
}

func (d languageDecoder) expression(value node, pointer string) (Expression, error) {
	if value.kind != nodeObject || len(value.object) != 1 {
		return Expression{}, newDiagnostic(d.input, "INVALID_EXPRESSION", "LANG-004", "validate", "expression must be a one-key tagged object", pointer, value.start)
	}
	tag := value.object[0].name
	payload := value.object[0].value
	expression := Expression{
		source:        sourceAt(d.input, pointer, value),
		payloadSource: sourceAt(d.input, joinPointer(pointer, tag), payload),
	}
	switch tag {
	case "$literal":
		expression.kind = LiteralExpression
		var err error
		expression.literal, err = d.literal(payload, joinPointer(pointer, tag))
		if err != nil {
			return Expression{}, err
		}
	case "$var", "$result":
		if payload.kind != nodeString || !identifierPattern.MatchString(payload.text) {
			return Expression{}, newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-004", "validate", "expression reference must be a portable identifier", joinPointer(pointer, tag), payload.start)
		}
		expression.name = payload.text
		if tag == "$var" {
			expression.kind = VariableExpression
		} else {
			expression.kind = ResultExpression
		}
	case "$parent", "$current":
		if payload.kind != nodeBool || payload.text != "true" {
			return Expression{}, newDiagnostic(d.input, "INVALID_EXPRESSION", "LANG-004", "validate", "context expression payload must be true", joinPointer(pointer, tag), payload.start)
		}
		if tag == "$parent" {
			expression.kind = ParentExpression
		} else {
			expression.kind = CurrentExpression
		}
	default:
		return Expression{}, newDiagnostic(d.input, "UNKNOWN_EXPRESSION", "LANG-004", "validate", "unknown expression tag", joinPointer(pointer, tag), value.object[0].start)
	}
	return expression, nil
}

func (d languageDecoder) directives(value node, pointer string) ([]Directive, error) {
	if value.kind != nodeArray {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-006", "validate", "directives must be an array", pointer, value.start)
	}
	directives := make([]Directive, 0, len(value.array))
	seen := make(map[string]bool, len(value.array))
	for index, directiveNode := range value.array {
		directivePointer := joinPointer(pointer, intString(index))
		if directiveNode.kind != nodeObject {
			return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-006", "validate", "directive must be an object", directivePointer, directiveNode.start)
		}
		if err := rejectUnknown(d.input, directiveNode, map[string]bool{"name": true, "arguments": true}, "LANG-006", "validate", directivePointer); err != nil {
			return nil, err
		}
		name, err := d.requiredIdentifier(directiveNode, "name", directivePointer, false)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, newDiagnostic(d.input, "DUPLICATE_NAME", "LANG-005", "validate", "duplicate directive name", directivePointer, directiveNode.start)
		}
		seen[name] = true
		directive := Directive{name: name, source: sourceAt(d.input, directivePointer, directiveNode)}
		if argumentsNode, ok := directiveNode.member("arguments"); ok {
			directive.arguments, err = d.arguments(argumentsNode, joinPointer(directivePointer, "arguments"))
			if err != nil {
				return nil, err
			}
		}
		directives = append(directives, directive)
	}
	return directives, nil
}

func (d languageDecoder) requiredIdentifier(parent node, name, pointer string, profile bool) (string, error) {
	value, exists := parent.member(name)
	if !exists {
		return "", newDiagnostic(d.input, "MISSING_FIELD", "LANG-002", "validate", name+" requires a string identifier", joinPointer(pointer, name), parent.start)
	}
	if value.kind != nodeString {
		return "", newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-002", "validate", name+" must be a string identifier", joinPointer(pointer, name), value.start)
	}
	valid := identifierPattern.MatchString(value.text)
	if profile {
		valid = profileIdentifierPattern.MatchString(value.text)
	}
	if !valid {
		return "", newDiagnostic(d.input, "INVALID_IDENTIFIER", "LANG-002", "validate", name+" must be a portable identifier", joinPointer(pointer, name), value.start)
	}
	return value.text, nil
}

func (d languageDecoder) optionalBoolean(parent node, name, pointer string) (bool, error) {
	value, exists := parent.member(name)
	if !exists {
		return false, nil
	}
	if value.kind != nodeBool {
		return false, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-005", "validate", name+" must be Boolean", joinPointer(pointer, name), value.start)
	}
	return value.text == "true", nil
}

func (d languageDecoder) optionalNonNegativeInteger(parent node, name, pointer string, allowZero bool) (*big.Int, error) {
	value, exists := parent.member(name)
	if !exists {
		return nil, nil
	}
	boundPointer := joinPointer(pointer, name)
	if value.kind != nodeNumber {
		return nil, newDiagnostic(d.input, "TYPE_MISMATCH", "LANG-010", "validate", name+" must be an integer", boundPointer, value.start)
	}
	maxDigits := min(d.limits.MaxNumberBytes, d.limits.MaxBytes)
	normalized, limited, ok := normalizeBoundInteger(value.text, maxDigits)
	if limited {
		return nil, newDiagnostic(d.input, "LIMIT_NUMBER", "PROTO-201", "decode", name+" magnitude exceeds numeric limit", boundPointer, value.start)
	}
	parsed, parsedOK := new(big.Int).SetString(normalized, 10)
	if !ok || !parsedOK || (!allowZero && parsed.Sign() == 0) {
		return nil, newDiagnostic(d.input, invalidBoundCode(name), "LANG-010", "validate", name+" must be a non-negative integer", boundPointer, value.start)
	}
	if parsed.Cmp(maxStructuralInteger) > 0 {
		return nil, newDiagnostic(d.input, invalidBoundCode(name), "LANG-010", "validate", name+" exceeds the maximum safe integer", boundPointer, value.start)
	}
	return parsed, nil
}

func (d languageDecoder) literal(value node, pointer string) (json.RawMessage, error) {
	if value.end-value.start > d.limits.MaxLiteralBytes {
		return nil, newDiagnostic(d.input, "LIMIT_LITERAL", "PROTO-201", "decode", "literal exceeds byte limit", pointer, value.start)
	}
	return append(json.RawMessage(nil), d.input[value.start:value.end]...), nil
}

func invalidBoundCode(name string) string {
	switch name {
	case "at":
		return "INVALID_INDEX"
	case "start", "end":
		return "INVALID_SLICE"
	case "first", "last":
		return "INVALID_PAGE"
	default:
		return "INVALID_INTEGER"
	}
}

func normalizeBoundInteger(raw string, maxDigits int) (normalized string, limited bool, valid bool) {
	negative := strings.HasPrefix(raw, "-")
	unsigned := strings.TrimPrefix(raw, "-")
	mantissa, exponentText, hasExponent := strings.Cut(unsigned, "e")
	if !hasExponent {
		mantissa, exponentText, hasExponent = strings.Cut(unsigned, "E")
	}
	whole, fraction, hasFraction := strings.Cut(mantissa, ".")
	digits := whole
	if hasFraction {
		digits += fraction
	}
	if strings.Trim(digits, "0") == "" {
		return "0", false, true
	}
	exponent := int64(0)
	if hasExponent {
		parsed, err := strconv.ParseInt(exponentText, 10, 32)
		if err != nil {
			return "", true, false
		}
		exponent = parsed
	}
	if negative {
		return "", false, false
	}
	decimalPosition := int64(len(whole)) + exponent
	if decimalPosition <= 0 {
		return "", false, false
	}
	if decimalPosition < int64(len(digits)) {
		if strings.Trim(digits[decimalPosition:], "0") != "" {
			return "", false, false
		}
		digits = digits[:decimalPosition]
	} else {
		zeroCount := decimalPosition - int64(len(digits))
		if zeroCount > int64(maxDigits) {
			return "", true, false
		}
		digits += strings.Repeat("0", int(zeroCount))
	}
	digits = strings.TrimLeft(digits, "0")
	if len(digits) > maxDigits {
		return "", true, false
	}
	return digits, false, true
}
