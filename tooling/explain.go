package tooling

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

const ExplainVersion = "naatre.explain-1"

type ExplainStep struct {
	Sequence     int                    `json:"sequence"`
	Kind         protocol.SelectionKind `json:"kind"`
	Name         string                 `json:"name,omitempty"`
	ResponsePath []string               `json:"responsePath,omitempty"`
	Scheduling   runtime.Scheduling     `json:"scheduling"`
	Barriers     []runtime.Barrier      `json:"barriers"`
	Batch        bool                   `json:"batchOpportunity,omitempty"`
}

type ExplainReport struct {
	Version       string                   `json:"version"`
	Operation     string                   `json:"operation,omitempty"`
	Resolved      *runtime.PlanDescription `json:"resolved,omitempty"`
	Execution     []ExplainStep            `json:"execution"`
	EstimatedCost uint64                   `json:"estimatedCost"`
	Capabilities  []string                 `json:"capabilities"`
	Rejections    []Diagnostic             `json:"rejections"`
}

type ExplainOptions struct {
	// VisibleIDs is an allow-list of portable schema IDs. Nil means the caller
	// already holds an authorized complete offline bundle.
	VisibleIDs map[string]bool
}

// Explain resolves a document from an offline schema bundle without running
// policy, directive, or business callbacks.
func Explain(schemaDocument schema.Document, documentInput []byte, operation string) ExplainReport {
	return ExplainWithOptions(schemaDocument, documentInput, operation, ExplainOptions{})
}

// ExplainWithOptions applies an explicit schema-visibility boundary before
// returning plan metadata.
func ExplainWithOptions(schemaDocument schema.Document, documentInput []byte, operation string, options ExplainOptions) ExplainReport {
	report := ExplainReport{Version: ExplainVersion, Operation: operation, Execution: []ExplainStep{}, Capabilities: []string{}, Rejections: []Diagnostic{}}
	snapshot, err := runtime.NewPlanningSnapshot(schemaDocument)
	if err != nil {
		report.Rejections = []Diagnostic{toolDiagnostic("schema", "TOOL_SCHEMA_INVALID", "", err)}
		return report
	}
	report = ExplainWithSnapshot(snapshot, documentInput, operation)
	if options.VisibleIDs != nil {
		redactVisibilityDiagnostics(schemaDocument, report.Rejections, options.VisibleIDs)
		if report.Resolved != nil {
			applyVisibility(snapshot, report.Resolved, options.VisibleIDs)
			sequence := 0
			report.Execution = explainNodes(snapshot, report.Resolved.Nodes, &sequence)
		}
	}
	return report
}

// ExplainWithSnapshot uses only immutable registry metadata. Runtime Prepare
// is a validation/planning operation and never invokes a registered handler.
func ExplainWithSnapshot(snapshot runtime.Snapshot, documentInput []byte, operation string) ExplainReport {
	report := ExplainReport{Version: ExplainVersion, Operation: operation, Execution: []ExplainStep{}, Capabilities: []string{}, Rejections: []Diagnostic{}}
	portable, err := snapshot.ExportSchema(schema.ExportOptions{Revision: "tooling-explain"})
	if err != nil {
		report.Rejections = []Diagnostic{toolDiagnostic("schema", "TOOL_SCHEMA_INVALID", "", err)}
		return report
	}
	offline, err := runtime.NewPlanningSnapshot(portable)
	if err != nil {
		report.Rejections = []Diagnostic{toolDiagnostic("schema", "TOOL_SCHEMA_INVALID", "", err)}
		return report
	}
	document, err := protocol.DecodeDocument(documentInput, protocol.Limits{})
	if err != nil {
		report.Rejections = diagnosticsFromError(err)
		return report
	}
	request, err := requestForDocument(document, operation)
	if err != nil {
		report.Rejections = diagnosticsFromError(err)
		return report
	}
	plan, err := runtime.Prepare(offline, request)
	if err != nil {
		report.Rejections = diagnosticsFromError(err)
		return report
	}
	description := plan.Description()
	redactPlanDescription(&description)
	report.Operation = description.Operation
	report.Resolved = &description
	report.EstimatedCost = plan.StaticCost()
	report.Capabilities = append([]string{}, description.Requirements...)
	sequence := 0
	report.Execution = explainNodes(offline, description.Nodes, &sequence)
	return report
}

func explainNodes(snapshot runtime.Snapshot, nodes []runtime.PlanNodeDescription, sequence *int) []ExplainStep {
	steps := make([]ExplainStep, 0, len(nodes))
	for _, node := range nodes {
		step := ExplainStep{
			Sequence: *sequence, Kind: node.Kind, Name: node.Name, ResponsePath: slices.Clone(node.ResponsePath),
			Scheduling: node.Scheduling, Barriers: slices.Clone(node.Barriers), Batch: batchOpportunity(snapshot, node),
		}
		*sequence++
		steps = append(steps, step)
		steps = append(steps, explainNodes(snapshot, node.Children, sequence)...)
	}
	return steps
}

func batchOpportunity(snapshot runtime.Snapshot, node runtime.PlanNodeDescription) bool {
	if node.Kind != protocol.CallSelection && node.Kind != protocol.FieldSelection {
		return false
	}
	var descriptor runtime.Descriptor
	var ok bool
	if node.Current == "" {
		descriptor, ok = snapshot.Root(protocol.Query, node.Name)
		if !ok {
			descriptor, ok = snapshot.Root(protocol.Mutation, node.Name)
		}
		if !ok {
			descriptor, ok = snapshot.Root(protocol.Subscription, node.Name)
		}
	} else {
		kind := runtime.CallMember
		if node.Kind == protocol.FieldSelection {
			kind = runtime.FieldMember
		}
		descriptor, ok = snapshot.Member(node.Current, kind, node.Name)
	}
	return ok && descriptor.Metadata.Batching == runtime.BatchEligible
}

func redactPlanDescription(description *runtime.PlanDescription) {
	redactPlanNodes(description.Nodes)
}

func applyVisibility(snapshot runtime.Snapshot, description *runtime.PlanDescription, visible map[string]bool) {
	hiddenPaths := make(map[string]bool)
	applyNodeVisibility(snapshot, description.Kind, description.Nodes, visible, hiddenPaths)
	applyResultVisibility(&description.Result, nil, hiddenPaths, visible)
}

func applyNodeVisibility(snapshot runtime.Snapshot, operation protocol.OperationKind, nodes []runtime.PlanNodeDescription, visible map[string]bool, hiddenPaths map[string]bool) {
	for index := range nodes {
		node := &nodes[index]
		identifier := ""
		if node.Current == "" && node.Name != "" {
			if descriptor, ok := snapshot.Root(operation, node.Name); ok {
				identifier = descriptor.ID
			}
		} else if node.Name != "" {
			kind := runtime.CallMember
			if node.Kind == protocol.FieldSelection {
				kind = runtime.FieldMember
			}
			if descriptor, ok := snapshot.Member(node.Current, kind, node.Name); ok {
				identifier = descriptor.ID
			}
		}
		if identifier != "" && !visible[identifier] {
			hiddenPaths[pathKey(node.ResponsePath)] = true
			node.Name = Redacted
			node.ResponseName = Redacted
			node.Current = schema.TypeID(Redacted)
			node.Arguments = schema.TypeID(Redacted)
			node.Output = schema.TypeID(Redacted)
		}
		if node.Current != "" && node.Current != schema.TypeID(Redacted) && !visible[string(node.Current)] {
			node.Current = schema.TypeID(Redacted)
		}
		if node.Arguments != "" && node.Arguments != schema.TypeID(Redacted) && !visible[string(node.Arguments)] {
			node.Arguments = schema.TypeID(Redacted)
		}
		if node.Output != "" && node.Output != schema.TypeID(Redacted) && !visible[string(node.Output)] {
			node.Output = schema.TypeID(Redacted)
		}
		applyNodeVisibility(snapshot, operation, node.Children, visible, hiddenPaths)
	}
}

func applyResultVisibility(result *runtime.SelectedResultDescription, path []string, hiddenPaths, visible map[string]bool) {
	if result.Type != "" && !visible[string(result.Type)] {
		result.Type = schema.TypeID(Redacted)
	}
	for index := range result.Fields {
		field := &result.Fields[index]
		fieldPath := append(append([]string(nil), path...), field.Name)
		if hiddenPaths[pathKey(fieldPath)] {
			field.Name = Redacted
			field.Result.Type = schema.TypeID(Redacted)
		}
		applyResultVisibility(&field.Result, fieldPath, hiddenPaths, visible)
	}
	if result.Element != nil {
		applyResultVisibility(result.Element, path, hiddenPaths, visible)
	}
}

func redactVisibilityDiagnostics(document schema.Document, diagnostics []Diagnostic, visible map[string]bool) {
	hidden := make([]string, 0)
	for _, operation := range document.Operations() {
		if !visible[operation.ID] {
			hidden = append(hidden, operation.Name)
		}
	}
	for _, member := range document.Members() {
		if !visible[member.ID] {
			hidden = append(hidden, member.Name)
		}
	}
	for _, declaration := range document.Types() {
		if !visible[string(declaration.ID)] {
			hidden = append(hidden, declaration.Name)
		}
	}
	for index := range diagnostics {
		for _, name := range hidden {
			if name == "" {
				continue
			}
			diagnostics[index].Message = strings.ReplaceAll(diagnostics[index].Message, name, Redacted)
			diagnostics[index].Path = strings.ReplaceAll(diagnostics[index].Path, name, Redacted)
		}
	}
}

func pathKey(path []string) string {
	encoded, _ := json.Marshal(path)
	return string(encoded)
}

func redactPlanNodes(nodes []runtime.PlanNodeDescription) {
	for index := range nodes {
		nodes[index].AuthorizationPolicy = ""
		for inputIndex := range nodes[index].Inputs {
			if len(nodes[index].Inputs[inputIndex].Expression.Literal) != 0 {
				nodes[index].Inputs[inputIndex].Expression.Literal = json.RawMessage(`"<redacted>"`)
			}
		}
		for directiveIndex := range nodes[index].Directives {
			for inputIndex := range nodes[index].Directives[directiveIndex].Inputs {
				if len(nodes[index].Directives[directiveIndex].Inputs[inputIndex].Expression.Literal) != 0 {
					nodes[index].Directives[directiveIndex].Inputs[inputIndex].Expression.Literal = json.RawMessage(`"<redacted>"`)
				}
			}
		}
		redactPlanNodes(nodes[index].Children)
	}
}
