package runtime

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"sort"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

type staticType struct {
	id         schema.TypeID
	nullable   bool
	valid      bool
	possible   map[schema.TypeID]bool
	collection *CollectionMetadata
}

type planNode struct {
	selection      protocol.Selection
	kind           protocol.SelectionKind
	name           string
	definition     Definition
	hasDefinition  bool
	outputName     string
	binding        string
	input          staticType
	output         staticType
	source         protocol.Source
	children       []planNode
	responsePath   []string
	scheduling     Scheduling
	ordinal        int
	parallelPolicy protocol.ParallelPolicy
	optional       bool
	typeConditions []schema.TypeID
	fragmentParams map[string]fragmentParameterBinding
	directives     []plannedDirective
	staticCost     uint64
	cursorID       uint64
}

type fragmentParameterBinding struct {
	expression    protocol.Expression
	hasExpression bool
	defaultValue  json.RawMessage
	hasDefault    bool
	typeID        schema.TypeID
	nullable      bool
}

type plannedVariable struct {
	definition protocol.VariableDefinition
	typeInfo   staticType
	guaranteed bool
	used       bool
}

type plannedBinding struct {
	typeInfo staticType
	source   protocol.Source
}

type bindingDeclaration struct {
	source    protocol.Source
	selection protocol.Source
}

type responseClaim struct {
	source   protocol.Source
	possible map[schema.TypeID]bool
}

type validationScope struct {
	current        staticType
	parent         staticType
	bindings       map[string]plannedBinding
	responseNames  map[string][]responseClaim
	fragmentStack  map[string]protocol.Source
	typeConditions []schema.TypeID
}

type planValidator struct {
	registry  Snapshot
	request   *protocol.Request
	operation protocol.Operation
	fragments map[string]protocol.Fragment
	// usedFragments records every fragment reached by a spread from the
	// operation, so an unreachable declaration is reported rather than silently
	// carried through planning and cost analysis.
	usedFragments             map[string]bool
	variables                 map[string]*plannedVariable
	allBindings               map[string]bindingDeclaration
	context                   []protocol.Source
	issues                    []ValidationIssue
	fragmentSelections        int
	fragmentExpansionExceeded bool
	directiveCost             uint64
	staticCost                uint64
	nextCursorID              uint64
	resourceLimits            ResourceLimits
}

func newPlanValidator(registry Snapshot, request *protocol.Request, operation protocol.Operation, limits ResourceLimits) *planValidator {
	validator := &planValidator{
		registry: registry, request: request, operation: operation,
		fragments: make(map[string]protocol.Fragment), usedFragments: make(map[string]bool),
		variables:      make(map[string]*plannedVariable),
		allBindings:    make(map[string]bindingDeclaration),
		resourceLimits: limits,
	}
	for _, fragment := range request.Document().Fragments() {
		validator.fragments[fragment.Name()] = fragment
	}
	validator.collectBindings(operation.Selections(), make(map[string]bool))
	return validator
}

func (v *planValidator) validate() ([]planNode, []plannedSelection) {
	v.validateCapabilities()
	v.validateVariables()
	scope := validationScope{
		bindings: make(map[string]plannedBinding), responseNames: make(map[string][]responseClaim),
		fragmentStack: make(map[string]protocol.Source),
	}
	nodes := v.validateSequence(v.operation.Selections(), &scope, true)
	annotatePlanNodes(nodes, nil, SequentialScheduling)
	for _, variable := range v.operation.Variables() {
		planned := v.variables[variable.Name()]
		if planned != nil && !planned.used {
			v.add("UNUSED_VARIABLE", "LANG-005", "variable is declared but never used", variable.Source())
		}
	}
	// A fragment reached only from another unreachable fragment is itself
	// unreachable, which falls out of marking at the spread site.
	for _, fragment := range v.request.Document().Fragments() {
		if !v.usedFragments[fragment.Name()] {
			v.add("UNUSED_FRAGMENT", "LANG-240", "fragment is declared but never spread", fragment.Source())
		}
	}
	sort.SliceStable(v.issues, func(left, right int) bool {
		return v.issues[left].Diagnostic.Offset < v.issues[right].Diagnostic.Offset
	})
	executable := make([]plannedSelection, 0, len(nodes))
	for _, node := range nodes {
		if node.kind == protocol.CallSelection && node.hasDefinition {
			executable = append(executable, plannedSelection{definition: node.definition, outputName: node.outputName, source: node.source})
		}
	}
	return nodes, executable
}

func (v *planValidator) validateCapabilities() {
	document := v.request.Document()
	requirements := document.Requires()
	slices.Sort(requirements)
	negotiated := v.request.Capabilities()
	for _, requirement := range requirements {
		if requirement == "core.language-1" || slices.Contains(negotiated, requirement) {
			continue
		}
		v.add("UNSUPPORTED_CAPABILITY", "PROTO-007", fmt.Sprintf("required capability %q was not negotiated", requirement), document.Source())
	}
}

func (v *planValidator) validateVariables() {
	for _, definition := range v.operation.Variables() {
		typeID := schema.TypeID(definition.Type())
		descriptor, ok := v.registry.types.Lookup(typeID)
		if !ok || !descriptor.Input {
			v.add("UNKNOWN_TYPE", "TYPE-001", "variable references an unknown or non-input type", definition.Source())
			v.variables[definition.Name()] = &plannedVariable{definition: definition}
			continue
		}
		planned := &plannedVariable{
			definition: definition,
			typeInfo:   staticType{id: typeID, nullable: definition.Nullable(), valid: true},
			guaranteed: definition.Required(),
		}
		value, present := v.request.Variable(definition.Name())
		if present {
			planned.guaranteed = true
			if _, err := schema.CoerceInput(v.registry.types, typeID, value, definition.Nullable()); err != nil {
				v.add("TYPE_MISMATCH", "TYPE-100", fmt.Sprintf("variable %q: %v", definition.Name(), err), definition.Source())
			}
		} else if fallback, hasDefault := definition.Default(); hasDefault {
			planned.guaranteed = true
			if _, err := schema.CoerceInput(v.registry.types, typeID, fallback, definition.Nullable()); err != nil {
				v.add("TYPE_MISMATCH", "TYPE-100", fmt.Sprintf("variable %q default: %v", definition.Name(), err), definition.Source())
			}
		} else if definition.Required() {
			v.add("MISSING_VARIABLE", "LANG-023", "required request variable is missing", definition.Source())
		}
		v.variables[definition.Name()] = planned
	}
}

func (v *planValidator) validateSequence(selections []protocol.Selection, scope *validationScope, root bool) []planNode {
	nodes := make([]planNode, 0, len(selections))
	for _, selection := range selections {
		node := v.validateSelection(selection, scope, root, true)
		nodes = append(nodes, node)
		if binding := selection.Bind(); binding != "" && node.output.valid {
			if previous, exists := scope.bindings[binding]; exists {
				v.addRelated("DUPLICATE_BINDING", "LANG-221", "binding name is already defined in this scope", selectionBindSource(selection), previous.source)
			} else {
				scope.bindings[binding] = plannedBinding{typeInfo: node.output, source: selectionBindSource(selection)}
			}
		}
	}
	return nodes
}

func (v *planValidator) validateSelection(selection protocol.Selection, scope *validationScope, root, emitResponse bool) planNode {
	contextLength := len(v.context)
	if selection.Kind() == protocol.FragmentSelection {
		if fragment, ok := v.fragments[selection.Name()]; ok {
			v.context = append(v.context, fragment.Source(), selection.Source())
			defer func() { v.context = v.context[:contextLength] }()
		}
	}
	node := planNode{
		selection: selection, kind: selection.Kind(), name: selection.Name(), binding: selection.Bind(), input: scope.current,
		source: selection.Source(), parallelPolicy: selection.Policy(), optional: selectionIsOptional(selection),
		typeConditions: append([]schema.TypeID(nil), scope.typeConditions...),
	}
	if selection.Kind() == protocol.PageSelection {
		v.nextCursorID++
		node.cursorID = v.nextCursorID
	}
	if emitResponse {
		node.outputName = v.claimResponseName(selection, scope)
	}
	switch selection.Kind() {
	case protocol.CallSelection:
		v.validateCall(selection, scope, root, &node)
	case protocol.FieldSelection:
		v.validateField(selection, scope, &node)
	case protocol.PipelineSelection:
		v.validatePipeline(selection, scope, root, &node)
	case protocol.MapSelection:
		v.validateMap(selection, scope, &node)
	case protocol.IndexSelection:
		v.validateIndex(selection, scope, &node)
	case protocol.SliceSelection, protocol.PageSelection:
		v.validateCollection(selection, scope, &node)
	case protocol.MetaSelection:
		v.validateMeta(selection, scope, &node)
	case protocol.ParallelSelection:
		v.validateParallel(selection, scope, root, &node)
	case protocol.FragmentSelection:
		v.validateFragment(selection, scope, root, &node)
	case protocol.CurrentSelection:
		if !scope.current.valid {
			v.add("INVALID_CURRENT", "LANG-106", "current value is not available in this scope", selection.Source())
		} else if v.requiresExplicitSelection(scope.current) {
			v.add("INVALID_CURRENT", "LANG-106", "composite current value requires an explicit selected-result schema", selectionPayloadSource(selection))
		} else {
			node.output = scope.current
		}
	case protocol.NestSelection, protocol.UnnestSelection:
		if selection.Kind() == protocol.UnnestSelection && !v.isObjectType(scope.current) {
			v.add("INVALID_UNNEST", "LANG-203", "unnest requires a statically known object result", selectionPayloadSource(selection))
			break
		}
		childScope := scope.child(scope.current, true)
		if selection.Kind() == protocol.UnnestSelection {
			childScope.responseNames = scope.responseNames
		}
		node.children = v.validateSequence(selection.Selections(), &childScope, root)
		node.output = staticType{valid: true}
	default:
		v.add("UNKNOWN_SELECTION", "LANG-003", "selection kind is not supported", selection.Source())
	}
	node.directives = v.validateDirectives(selection.Directives(), scope, &node)
	return node
}

func (v *planValidator) validateCall(selection protocol.Selection, scope *validationScope, root bool, node *planNode) {
	definition, ok := v.resolveDefinition(selection.Name(), CallMember, scope.current, root)
	if !ok {
		v.add("UNKNOWN_CALL", "LANG-100", "call is not registered for the static scope", selectionNameSource(selection))
		return
	}
	node.definition, node.hasDefinition = definition, true
	v.validateDefinition(definition, selectionNameSource(selection))
	v.validateArguments(definition.descriptor, selection.Arguments(), selection.Source(), scope)
	node.output = staticTypeForDefinition(definition)
	node.children = v.validateOutputSelections(selection.Selections(), node.output, node.outputName != "", selection.Source(), scope)
}

func (v *planValidator) validateField(selection protocol.Selection, scope *validationScope, node *planNode) {
	definition, ok := v.resolveDefinition(selection.Name(), FieldMember, scope.current, false)
	if !ok {
		code := "UNKNOWN_FIELD"
		source := selectionNameSource(selection)
		if v.isCollectionType(scope.current) {
			code = "INVALID_COLLECTION_SCOPE"
			source = selection.Source()
		} else if !v.isObjectType(scope.current) {
			code = "INVALID_FIELD_SCOPE"
			source = selection.Source()
		}
		v.add(code, "LANG-100", "field is not registered for the static current type", source)
		return
	}
	node.definition, node.hasDefinition = definition, true
	v.validateDefinition(definition, selectionNameSource(selection))
	node.output = staticTypeForDefinition(definition)
	node.children = v.validateOutputSelections(selection.Selections(), node.output, node.outputName != "", selection.Source(), scope)
}

func (v *planValidator) validatePipeline(selection protocol.Selection, scope *validationScope, root bool, node *planNode) {
	pipelineScope := scope.child(scope.current, false)
	stages := selection.Stages()
	node.children = make([]planNode, 0, len(stages))
	for index, stage := range stages {
		stageNode := v.validateSelection(stage, &pipelineScope, root && index == 0 && !pipelineScope.current.valid, false)
		node.children = append(node.children, stageNode)
		if binding := stage.Bind(); binding != "" && stageNode.output.valid {
			if previous, exists := pipelineScope.bindings[binding]; exists {
				v.addRelated("DUPLICATE_BINDING", "LANG-221", "pipeline binding is already defined", selectionBindSource(stage), previous.source)
			} else {
				pipelineScope.bindings[binding] = plannedBinding{typeInfo: stageNode.output, source: selectionBindSource(stage)}
			}
		}
		pipelineScope.parent = pipelineScope.current
		pipelineScope.current = stageNode.output
	}
	node.output = pipelineScope.current
	if node.outputName != "" && len(node.children) != 0 {
		last := node.children[len(node.children)-1]
		if len(last.children) == 0 && v.requiresExplicitSelection(last.output) {
			v.add("INVALID_OBJECT_SELECTION", "LANG-107", "composite pipeline output requires child selections", last.source)
		}
	}
}

func (v *planValidator) validateMap(selection protocol.Selection, scope *validationScope, node *planNode) {
	descriptor, ok := v.collection(scope.current, selection.Source())
	if !ok {
		return
	}
	item := staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true}
	childScope := scope.child(item, true)
	node.children = v.validateSequence(selection.Selections(), &childScope, false)
	node.output = scope.current
}

func (v *planValidator) validateIndex(selection protocol.Selection, scope *validationScope, node *planNode) {
	descriptor, ok := v.collection(scope.current, selection.Source())
	if !ok {
		return
	}
	node.output = staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true}
	node.optional = true
	node.children = v.validateOutputSelections(selection.Selections(), node.output, node.outputName != "", selection.Source(), scope)
}

func (v *planValidator) validateCollection(selection protocol.Selection, scope *validationScope, node *planNode) {
	descriptor, ok := v.collection(scope.current, selection.Source())
	if !ok {
		return
	}
	if selection.Kind() == protocol.PageSelection {
		v.validatePageSelection(selection, scope.current.collection)
	}
	if start, hasStart := selection.Start(); hasStart {
		if end, hasEnd := selection.End(); hasEnd && start.Cmp(end) > 0 {
			v.add("INVALID_SLICE", "LANG-123", "slice start exceeds end", selectionPayloadSource(selection))
		}
	}
	if after, ok := selection.After(); ok {
		v.inferExpression(after, scope)
	}
	if before, ok := selection.Before(); ok {
		v.inferExpression(before, scope)
	}
	node.output = scope.current
	item := staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true}
	node.children = v.validateOutputSelections(selection.Selections(), item, node.outputName != "", selection.Source(), scope)
}

func staticTypeForDefinition(definition Definition) staticType {
	descriptor := definition.descriptor
	return staticType{id: descriptor.Output, nullable: descriptor.OutputNullable, valid: true, collection: descriptor.Metadata.Collection}
}

func (v *planValidator) validatePageSelection(selection protocol.Selection, collection *CollectionMetadata) {
	if !slices.Contains(v.request.Capabilities(), CollectionPageCapability) {
		v.add("UNSUPPORTED_CAPABILITY", "LANG-124", "page selection requires collection.page-1", selectionPayloadSource(selection))
	}
	_, hasAfter := selection.After()
	_, hasBefore := selection.Before()
	if !hasAfter && !hasBefore && !hasSecureCollectionRuntime(collection) {
		v.add("COLLECTION_PAGE_UNAVAILABLE", "LANG-124", "page selection requires a configured secure cursor runtime", selectionPayloadSource(selection))
	}
	limit := v.resourceLimits.MaxCollectionItems
	if collection != nil && collection.MaxPageSize != 0 {
		limit = min(limit, collection.MaxPageSize)
	}
	for _, size := range []*big.Int{selectionPageSize(selection)} {
		if size != nil && (!size.IsUint64() || size.Uint64() > limit) {
			v.add("PAGE_SIZE_LIMIT", "LANG-124", "requested page size exceeds the collection limit", selectionPayloadSource(selection))
		}
	}
}

func hasSecureCollectionRuntime(collection *CollectionMetadata) bool {
	return collection != nil && collection.CursorCodec != nil && collection.CursorScope != nil && collection.Position != nil
}

func selectionPageSize(selection protocol.Selection) *big.Int {
	if size, ok := selection.First(); ok {
		return size
	}
	size, _ := selection.Last()
	return size
}

func (v *planValidator) validateMeta(selection protocol.Selection, scope *validationScope, node *planNode) {
	if _, ok := v.collection(scope.current, selection.Source()); !ok {
		return
	}
	if selection.Name() == "totalCount" &&
		!slices.Contains(v.request.Capabilities(), CollectionPageCapability) {
		v.add("UNSUPPORTED_CAPABILITY", "LANG-125", "pagination metadata requires collection.page-1", selectionNameSource(selection))
		return
	}
	if selection.Name() != "count" {
		if selection.Name() != "totalCount" {
			v.add("UNKNOWN_METADATA", "LANG-125", "collection metadata is not available", selection.Source())
			return
		}
		if scope.current.collection == nil {
			v.add("UNKNOWN_METADATA", "LANG-125", "totalCount is not advertised by the collection", selection.Source())
			return
		}
		node.staticCost = scope.current.collection.TotalCountCost
	}
	node.output = staticType{id: schema.TypeID(schema.Int32), valid: true}
}

func (v *planValidator) validateParallel(selection protocol.Selection, scope *validationScope, root bool, node *planNode) {
	for _, branch := range selection.Selections() {
		branchScope := scope.child(scope.current, false)
		branchScope.parent = scope.parent
		branchScope.responseNames = scope.responseNames
		branchNode := v.validateSelection(branch, &branchScope, root, true)
		v.validateParallelBranch(branchNode)
		node.children = append(node.children, branchNode)
	}
	node.output = staticType{valid: true}
}

func (v *planValidator) validateParallelBranch(node planNode) {
	if node.kind == protocol.ParallelSelection {
		return
	}
	if node.hasDefinition {
		metadata := node.definition.descriptor.Metadata
		if metadata.ThreadSafety != ThreadSafe {
			v.add("PARALLEL_THREAD_UNSAFE", "LANG-301", "parallel branch reaches a serial-only handler", node.source)
		}
		if metadata.Transaction == TransactionRequired {
			v.add("PARALLEL_TRANSACTION", "LANG-301", "parallel branch reaches a transaction-required handler", node.source)
		}
		if metadata.Effect == WriteEffect {
			switch {
			case !metadata.ParallelMutation:
				v.add("PARALLEL_MUTATION_NOT_ALLOWED", "LANG-303", "parallel mutation lacks explicit registry permission", node.source)
			case !slices.Contains(v.request.Capabilities(), ParallelMutationCapability):
				v.add("UNSUPPORTED_CAPABILITY", "LANG-303", "parallel mutation profile was not negotiated", node.source)
			}
		}
	}
	for _, child := range node.children {
		v.validateParallelBranch(child)
	}
}

func (v *planValidator) validateFragment(selection protocol.Selection, scope *validationScope, root bool, node *planNode) {
	if v.fragmentExpansionExceeded {
		return
	}
	var fragment protocol.Fragment
	fragmentSelections := selection.Selections()
	condition := selection.TypeCondition()
	declarationSource := selection.Source()
	if selection.Name() != "" {
		var ok bool
		fragment, ok = v.fragments[selection.Name()]
		if !ok {
			v.add("UNKNOWN_FRAGMENT", "LANG-240", "fragment is not declared", selection.Source())
			return
		}
		v.usedFragments[fragment.Name()] = true
		if firstUse, active := scope.fragmentStack[fragment.Name()]; active {
			v.addRelated("FRAGMENT_CYCLE", "LANG-240", "fragment cycle detected", selection.Source(), fragment.Source(), firstUse)
			return
		}
		fragmentSelections = fragment.Selections()
		condition = fragment.TypeCondition()
		declarationSource = fragment.Source()
	}
	if uint64(len(scope.fragmentStack)) >= v.resourceLimits.MaxFragmentExpansionDepth ||
		uint64(v.fragmentSelections+len(fragmentSelections)) > v.resourceLimits.MaxFragmentSelections {
		v.fragmentExpansionExceeded = true
		v.addRelated("FRAGMENT_EXPANSION_LIMIT", "LANG-240", "fragment expansion exceeds the portable planning budget", selection.Source(), declarationSource)
		return
	}
	v.fragmentSelections += len(fragmentSelections)
	previousCurrent := scope.current
	previousConditions := scope.typeConditions
	if condition != "" {
		conditionID := schema.TypeID(condition)
		narrowed, compatible := v.narrowTypeCondition(scope.current, conditionID)
		if !compatible {
			v.addRelated("IMPOSSIBLE_TYPE_CONDITION", "LANG-240", "fragment type condition cannot match the static current type", selection.Source(), declarationSource)
			return
		}
		scope.current = narrowed
		scope.typeConditions = append(append([]schema.TypeID(nil), scope.typeConditions...), conditionID)
	}
	restoreParameters := func() {}
	if selection.Name() != "" {
		restoreParameters = v.bindFragmentParameters(fragment, selection, scope, node)
		scope.fragmentStack[fragment.Name()] = selection.Source()
	}
	node.children = v.validateSequence(fragmentSelections, scope, root)
	if selection.Name() != "" {
		delete(scope.fragmentStack, fragment.Name())
	}
	restoreParameters()
	scope.current = previousCurrent
	scope.typeConditions = previousConditions
	node.output = staticType{valid: true}
}

// bindFragmentParameters validates a spread's arguments against the fragment's
// declared parameters and installs them for the fragment's selections, where
// they shadow an operation variable of the same name. The returned function
// restores the shadowed variables, so shadowing lasts exactly as long as the
// expansion.
func (v *planValidator) bindFragmentParameters(fragment protocol.Fragment, selection protocol.Selection, scope *validationScope, node *planNode) func() {
	parameters, arguments := fragment.Parameters(), selection.Arguments()
	if len(parameters) == 0 && len(arguments) == 0 {
		return func() {}
	}
	if !slices.Contains(v.request.Capabilities(), FragmentParametersCapability) {
		v.addRelated("UNSUPPORTED_CAPABILITY", "LANG-244", "fragment parameters require a negotiated capability", selection.Source(), fragment.Source())
		return func() {}
	}
	declared := make(map[string]protocol.VariableDefinition, len(parameters))
	for _, parameter := range parameters {
		declared[parameter.Name()] = parameter
	}
	for _, name := range sortedStringKeys(arguments) {
		if _, ok := declared[name]; !ok {
			v.addRelated("UNKNOWN_ARGUMENT", "LANG-244", fmt.Sprintf("fragment has no parameter %q", name), arguments[name].Source(), fragment.Source())
		}
	}
	shadowed := make(map[string]*plannedVariable, len(parameters))
	node.fragmentParams = make(map[string]fragmentParameterBinding, len(parameters))
	for _, parameter := range parameters {
		shadowed[parameter.Name()] = v.variables[parameter.Name()]
		planned, binding := v.bindFragmentParameter(parameter, arguments, selection, fragment, scope)
		v.variables[parameter.Name()] = planned
		node.fragmentParams[parameter.Name()] = binding
	}
	return func() {
		for name, previous := range shadowed {
			if previous == nil {
				delete(v.variables, name)
				continue
			}
			v.variables[name] = previous
		}
	}
}

// bindFragmentParameter resolves one parameter from its spread-site argument,
// its default, or its absence, and type-checks the binding at the spread site.
func (v *planValidator) bindFragmentParameter(parameter protocol.VariableDefinition, arguments map[string]protocol.Expression,
	selection protocol.Selection, fragment protocol.Fragment, scope *validationScope,
) (*plannedVariable, fragmentParameterBinding) {
	typeID := schema.TypeID(parameter.Type())
	binding := fragmentParameterBinding{typeID: typeID, nullable: parameter.Nullable()}
	descriptor, known := v.registry.types.Lookup(typeID)
	if !known || !descriptor.Input {
		v.add("UNKNOWN_TYPE", "LANG-244", "fragment parameter references an unknown or non-input type", parameter.Source())
		return &plannedVariable{definition: parameter}, binding
	}
	planned := &plannedVariable{
		definition: parameter,
		typeInfo:   staticType{id: typeID, nullable: parameter.Nullable(), valid: true},
	}
	argument, bound := arguments[parameter.Name()]
	fallback, hasDefault := parameter.Default()
	if hasDefault {
		planned.guaranteed = true
		binding.defaultValue = append(json.RawMessage(nil), fallback...)
		binding.hasDefault = true
		if _, err := schema.CoerceInput(v.registry.types, typeID, fallback, parameter.Nullable()); err != nil {
			v.add("TYPE_MISMATCH", "LANG-244", fmt.Sprintf("fragment parameter %q default: %v", parameter.Name(), err), parameter.Source())
		}
	}
	switch {
	case bound:
		planned.guaranteed = true
		binding.expression = argument
		binding.hasExpression = true
		// The argument is coerced against the parameter type at the spread
		// site, so a fragment never observes a value its declaration forbids.
		v.validateExpression(argument, planned.typeInfo, parameter.Required(), scope)
	case parameter.Required() && !hasDefault:
		v.addRelated("MISSING_ARGUMENT", "LANG-244", fmt.Sprintf("fragment requires argument %q", parameter.Name()), selection.Source(), fragment.Source())
		return planned, binding
	}
	return planned, binding
}

func (v *planValidator) validateOutputSelections(selections []protocol.Selection, output staticType, emitted bool, source protocol.Source, parent *validationScope) []planNode {
	if len(selections) == 0 {
		if emitted && v.requiresExplicitSelection(output) {
			v.add("INVALID_OBJECT_SELECTION", "LANG-107", "composite output requires child selections", source)
		}
		return nil
	}
	childScope := parent.child(output, true)
	return v.validateSequence(selections, &childScope, false)
}

func (v *planValidator) requiresExplicitSelection(output staticType) bool {
	return v.isObjectType(output)
}

func (v *planValidator) validateDefinition(definition Definition, source protocol.Source) {
	descriptor := definition.descriptor
	if v.operation.Kind() == protocol.Query && descriptor.Metadata.Effect != ReadEffect {
		v.add("EFFECT_NOT_ALLOWED", "LANG-303", "query transitively reaches a write", source)
		return
	}
	if descriptor.Scope != RootScope {
		return
	}
	switch v.operation.Kind() {
	case protocol.Query:
		if descriptor.Kind != protocol.Query {
			v.add("INVALID_OPERATION_KIND", "CORE-016", "query selects a non-query root", source)
		}
	case protocol.Mutation:
		if descriptor.Kind == protocol.Subscription {
			v.add("INVALID_OPERATION_KIND", "CORE-017", "mutation selects a subscription root", source)
		}
	case protocol.Subscription:
		if descriptor.Kind != protocol.Subscription {
			v.add("INVALID_SUBSCRIPTION", "CORE-018", "subscription selection is not a subscription root", source)
		}
	}
}

func (v *planValidator) resolveDefinition(name string, member MemberKind, current staticType, root bool) (Definition, bool) {
	if root && !current.valid && member == CallMember {
		definition, ok := v.registry.definitions["root:"+name]
		return definition, ok
	}
	if !current.valid {
		return Definition{}, false
	}
	definition, ok := v.registry.definitions["object:"+string(current.id)+":"+string(member)+":"+name]
	return definition, ok
}

func (v *planValidator) collection(current staticType, source protocol.Source) (schema.TypeDescriptor, bool) {
	descriptor, ok := v.registry.types.Lookup(current.id)
	if !current.valid || !ok || descriptor.Kind != schema.ListType {
		v.add("INVALID_COLLECTION_SCOPE", "LANG-126", "collection selection requires a list current type", source)
		return schema.TypeDescriptor{}, false
	}
	return descriptor, true
}

func (v *planValidator) isObjectType(current staticType) bool {
	descriptor, ok := v.registry.types.Lookup(current.id)
	return current.valid && ok && (descriptor.Kind == schema.ObjectType || descriptor.Kind == schema.InterfaceType || descriptor.Kind == schema.UnionType)
}

func (v *planValidator) isCollectionType(current staticType) bool {
	descriptor, ok := v.registry.types.Lookup(current.id)
	return current.valid && ok && descriptor.Kind == schema.ListType
}
