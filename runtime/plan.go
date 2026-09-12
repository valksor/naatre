package runtime

import (
	"encoding/json"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// Scheduling identifies whether sibling plan nodes execute in declaration
// order or are eligible for bounded parallel admission.
type Scheduling string

const (
	SequentialScheduling Scheduling = "sequential"
	ParallelScheduling   Scheduling = "parallel"
)

// ParallelMutationCapability is the negotiated profile required in addition
// to per-handler registry permission before a mutation branch may be admitted
// to a parallel group.
const ParallelMutationCapability = "mutation.parallel-1"

// CollectionPageCapability gates cursor pagination and pagination metadata.
const CollectionPageCapability = "collection.page-1"

// FragmentParametersCapability is the negotiated profile required for a
// fragment to declare typed parameters and for a spread to bind arguments to
// them. Without it the construct is a validation failure rather than a silently
// ignored member, since ignoring it would change which values a fragment sees.
const FragmentParametersCapability = "language.fragment-parameters-1"

// Barrier identifies a semantic boundary that optimizers and executors must
// preserve.
type Barrier string

const (
	AuthorizationBarrier Barrier = "authorization"
	CompletionBarrier    Barrier = "completion"
	EffectBarrier        Barrier = "effect"
	TransactionBarrier   Barrier = "transaction"
)

// PlanDescription is the portable, request-neutral shape of a resolved plan.
// It deliberately excludes handlers, correlation IDs, variable values, policy
// identities, and other per-request state.
type PlanDescription struct {
	Operation           string                    `json:"operation"`
	Kind                protocol.OperationKind    `json:"kind"`
	Requirements        []string                  `json:"requirements,omitempty"`
	VariableDefinitions []PlanVariableDescription `json:"variableDefinitions,omitempty"`
	Nodes               []PlanNodeDescription     `json:"nodes"`
	Result              SelectedResultDescription `json:"result"`
}

// PlanNodeDescription identifies one resolved language node without exposing
// its in-process handler.
type PlanNodeDescription struct {
	Kind                protocol.SelectionKind     `json:"kind"`
	Name                string                     `json:"name,omitempty"`
	ResponseName        string                     `json:"responseName,omitempty"`
	Binding             string                     `json:"binding,omitempty"`
	Current             schema.TypeID              `json:"current,omitempty"`
	Arguments           schema.TypeID              `json:"arguments,omitempty"`
	Output              schema.TypeID              `json:"output,omitempty"`
	Nullable            bool                       `json:"nullable,omitempty"`
	Effect              Effect                     `json:"effect,omitempty"`
	AuthorizationPolicy string                     `json:"authorizationPolicy,omitempty"`
	SourcePointer       string                     `json:"sourcePointer"`
	ResponsePath        []string                   `json:"responsePath,omitempty"`
	Scheduling          Scheduling                 `json:"scheduling"`
	Ordinal             int                        `json:"ordinal"`
	ParallelPolicy      protocol.ParallelPolicy    `json:"parallelPolicy,omitempty"`
	Barriers            []Barrier                  `json:"barriers,omitempty"`
	Inputs              []PlanInputDescription     `json:"inputs,omitempty"`
	Directives          []PlanDirectiveDescription `json:"directives,omitempty"`
	At                  string                     `json:"at,omitempty"`
	Start               string                     `json:"start,omitempty"`
	End                 string                     `json:"end,omitempty"`
	First               string                     `json:"first,omitempty"`
	Last                string                     `json:"last,omitempty"`
	After               *PlanExpressionDescription `json:"after,omitempty"`
	Before              *PlanExpressionDescription `json:"before,omitempty"`
	Children            []PlanNodeDescription      `json:"children,omitempty"`
}

// PlanVariableDescription retains document-level variable declarations while
// excluding request-bound values.
type PlanVariableDescription struct {
	Name     string          `json:"name"`
	Type     schema.TypeID   `json:"type"`
	Required bool            `json:"required"`
	Nullable bool            `json:"nullable"`
	Default  json.RawMessage `json:"default,omitempty"`
}

// PlanInputDescription records a stable argument name and expression.
type PlanInputDescription struct {
	Name       string                    `json:"name"`
	Expression PlanExpressionDescription `json:"expression"`
}

// PlanDirectiveDescription records compile-time directive semantics.
type PlanDirectiveDescription struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Version        string                 `json:"version"`
	Capability     string                 `json:"capability"`
	Effect         Effect                 `json:"effect"`
	DeclaredCost   uint64                 `json:"declaredCost"`
	AdditionalCost uint64                 `json:"additionalCost,omitempty"`
	Deterministic  bool                   `json:"deterministic"`
	Skip           bool                   `json:"skip,omitempty"`
	Inputs         []PlanInputDescription `json:"inputs,omitempty"`
}

// PlanExpressionDescription retains expression identity and static literal
// data, but never the value of a request variable or completed result.
type PlanExpressionDescription struct {
	Kind      protocol.ExpressionKind `json:"kind"`
	Reference string                  `json:"reference,omitempty"`
	Literal   json.RawMessage         `json:"literal,omitempty"`
}

// SelectedResultDescription is the language-neutral response shape derived
// during planning. Synthetic objects and collection elements are represented
// recursively rather than inferred from runtime values.
type SelectedResultDescription struct {
	Kind     schema.TypeKind            `json:"kind"`
	Type     schema.TypeID              `json:"type,omitempty"`
	Nullable bool                       `json:"nullable,omitempty"`
	Fields   []SelectedFieldDescription `json:"fields,omitempty"`
	Element  *SelectedResultDescription `json:"element,omitempty"`
}

// SelectedFieldDescription records one response name and its selected type.
type SelectedFieldDescription struct {
	Name           string                    `json:"name"`
	Required       bool                      `json:"required"`
	TypeConditions []schema.TypeID           `json:"typeConditions,omitempty"`
	SourcePointer  string                    `json:"sourcePointer"`
	Result         SelectedResultDescription `json:"result"`
}

// Description returns an isolated language-neutral description of the plan.
func (p *Plan) Description() PlanDescription {
	if p == nil {
		return PlanDescription{}
	}
	return PlanDescription{
		Operation: p.operationName, Kind: p.kind,
		Requirements:        append([]string(nil), p.requirements...),
		VariableDefinitions: describeVariables(p.variables),
		Nodes:               describePlanNodes(p.nodes), Result: selectedObject(p.nodes, p.types),
	}
}

func describePlanNodes(nodes []planNode) []PlanNodeDescription {
	if nodes == nil {
		return nil
	}
	descriptions := make([]PlanNodeDescription, len(nodes))
	for index, node := range nodes {
		effect := Effect("")
		arguments := schema.TypeID("")
		authorizationPolicy := ""
		if node.hasDefinition {
			effect = node.definition.descriptor.Metadata.Effect
			arguments = node.definition.descriptor.Input
			authorizationPolicy = node.definition.descriptor.Metadata.AuthorizationPolicy
		}
		barriers := []Barrier{CompletionBarrier}
		if authorizationPolicy != "" {
			barriers = append(barriers, AuthorizationBarrier)
		}
		if effect == WriteEffect {
			barriers = append(barriers, EffectBarrier)
		}
		if node.hasDefinition && node.definition.descriptor.Metadata.Transaction != TransactionNone {
			barriers = append(barriers, TransactionBarrier)
		}
		descriptions[index] = PlanNodeDescription{
			Kind: node.kind, Name: node.name, ResponseName: node.outputName, Binding: node.binding,
			Current: node.input.id, Arguments: arguments, Output: node.output.id, Nullable: node.output.nullable,
			Effect: effect, AuthorizationPolicy: authorizationPolicy,
			SourcePointer: node.source.Pointer, ResponsePath: append([]string(nil), node.responsePath...),
			Scheduling: node.scheduling, Ordinal: node.ordinal, ParallelPolicy: node.parallelPolicy,
			Barriers: barriers, Inputs: describeInputs(node.selection.Arguments()),
			Directives: describeDirectives(node.directives), Children: describePlanNodes(node.children),
		}
		describeBounds(node.selection, &descriptions[index])
	}
	return descriptions
}

func selectedObject(nodes []planNode, types schema.Snapshot) SelectedResultDescription {
	return SelectedResultDescription{Kind: schema.ObjectType, Fields: selectedFields(nodes, types, false)}
}

func selectedFields(nodes []planNode, types schema.Snapshot, inheritedOptional bool) []SelectedFieldDescription {
	var fields []SelectedFieldDescription
	for _, node := range nodes {
		if node.outputName == "" {
			switch node.kind {
			case protocol.FragmentSelection, protocol.ParallelSelection, protocol.UnnestSelection:
				fields = append(fields, selectedFields(node.children, types, inheritedOptional || node.optional)...)
			case protocol.FieldSelection, protocol.CallSelection, protocol.PipelineSelection,
				protocol.MapSelection, protocol.IndexSelection, protocol.SliceSelection,
				protocol.PageSelection, protocol.MetaSelection, protocol.CurrentSelection,
				protocol.NestSelection:
			}
			continue
		}
		fields = append(fields, SelectedFieldDescription{
			Name: node.outputName, Required: !inheritedOptional && !node.optional,
			TypeConditions: append([]schema.TypeID(nil), node.typeConditions...),
			SourcePointer:  node.source.Pointer, Result: selectedNode(node, types),
		})
	}
	return fields
}

func selectedNode(node planNode, types schema.Snapshot) SelectedResultDescription {
	switch node.kind {
	case protocol.NestSelection, protocol.ParallelSelection, protocol.FragmentSelection, protocol.UnnestSelection:
		return selectedObject(node.children, types)
	case protocol.MapSelection:
		result := selectedStaticType(node.output, types)
		result.Element = selectedCollectionElement(node.children, result.Element, types)
		return result
	case protocol.SliceSelection, protocol.PageSelection:
		result := selectedStaticType(node.output, types)
		if len(node.children) != 0 {
			result.Element = selectedCollectionElement(node.children, result.Element, types)
		}
		return result
	case protocol.IndexSelection:
		if len(node.children) != 0 {
			result := selectedObject(node.children, types)
			result.Type = node.output.id
			result.Nullable = node.output.nullable
			return result
		}
	case protocol.PipelineSelection:
		if len(node.children) != 0 {
			return selectedNode(node.children[len(node.children)-1], types)
		}
	case protocol.FieldSelection, protocol.CallSelection, protocol.MetaSelection, protocol.CurrentSelection:
	}
	if len(node.children) != 0 {
		result := selectedObject(node.children, types)
		result.Type = node.output.id
		result.Nullable = node.output.nullable
		return result
	}
	return selectedStaticType(node.output, types)
}

func selectedCollectionElement(nodes []planNode, declared *SelectedResultDescription, types schema.Snapshot) *SelectedResultDescription {
	item := selectedObject(nodes, types)
	if declared != nil {
		item.Type = declared.Type
		item.Nullable = declared.Nullable
	}
	return &item
}

func selectedStaticType(value staticType, types schema.Snapshot) SelectedResultDescription {
	return selectedStaticTypeSeen(value, types, make(map[schema.TypeID]bool))
}

func selectedStaticTypeSeen(value staticType, types schema.Snapshot, visiting map[schema.TypeID]bool) SelectedResultDescription {
	result := SelectedResultDescription{Type: value.id, Nullable: value.nullable}
	descriptor, ok := types.Lookup(value.id)
	if !value.valid || !ok {
		return result
	}
	result.Kind = descriptor.Kind
	if descriptor.Kind == schema.ListType || descriptor.Kind == schema.MapType {
		if visiting[value.id] {
			return result
		}
		visiting[value.id] = true
		element := selectedStaticTypeSeen(staticType{id: descriptor.Element, nullable: descriptor.ElementNullable, valid: true}, types, visiting)
		delete(visiting, value.id)
		result.Element = &element
	}
	return result
}

func describeVariables(definitions []protocol.VariableDefinition) []PlanVariableDescription {
	if len(definitions) == 0 {
		return nil
	}
	descriptions := make([]PlanVariableDescription, len(definitions))
	for index, definition := range definitions {
		fallback, hasDefault := definition.Default()
		if !hasDefault {
			fallback = nil
		}
		descriptions[index] = PlanVariableDescription{
			Name: definition.Name(), Type: schema.TypeID(definition.Type()), Required: definition.Required(),
			Nullable: definition.Nullable(), Default: append(json.RawMessage(nil), fallback...),
		}
	}
	return descriptions
}

func describeInputs(arguments map[string]protocol.Expression) []PlanInputDescription {
	if len(arguments) == 0 {
		return nil
	}
	names := sortedStringKeys(arguments)
	inputs := make([]PlanInputDescription, len(names))
	for index, name := range names {
		inputs[index] = PlanInputDescription{Name: name, Expression: describeExpression(arguments[name])}
	}
	return inputs
}

func describeDirectives(directives []plannedDirective) []PlanDirectiveDescription {
	if len(directives) == 0 {
		return nil
	}
	descriptions := make([]PlanDirectiveDescription, len(directives))
	for index, directive := range directives {
		descriptor := directive.definition.Descriptor
		descriptions[index] = PlanDirectiveDescription{
			ID: descriptor.ID, Name: descriptor.Name, Version: descriptor.Version, Capability: descriptor.Capability,
			Effect: Effect(descriptor.Effect), DeclaredCost: descriptor.Cost, AdditionalCost: directive.decision.AdditionalCost,
			Deterministic: descriptor.Deterministic, Skip: directive.decision.Skip,
			Inputs: describeInputs(directive.invocation.Arguments()),
		}
	}
	return descriptions
}

func describeExpression(expression protocol.Expression) PlanExpressionDescription {
	description := PlanExpressionDescription{Kind: expression.Kind(), Reference: expression.Name()}
	if literal, ok := expression.Literal(); ok {
		description.Literal = append(json.RawMessage(nil), literal...)
	}
	return description
}

func describeBounds(selection protocol.Selection, description *PlanNodeDescription) {
	if value, ok := selection.At(); ok {
		description.At = value.String()
	}
	if value, ok := selection.Start(); ok {
		description.Start = value.String()
	}
	if value, ok := selection.End(); ok {
		description.End = value.String()
	}
	if value, ok := selection.First(); ok {
		description.First = value.String()
	}
	if value, ok := selection.Last(); ok {
		description.Last = value.String()
	}
	if value, ok := selection.After(); ok {
		after := describeExpression(value)
		description.After = &after
	}
	if value, ok := selection.Before(); ok {
		before := describeExpression(value)
		description.Before = &before
	}
}
