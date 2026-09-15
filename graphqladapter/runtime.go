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
)

// CompileConsumer binds only the resolvers explicitly named by the
// application. Schema import alone never creates an executable registration.
func CompileConsumer(config ConsumerConfig) (*CompiledConsumer, interopadapter.FidelityReport, error) {
	report := newReport("", "", "", interopadapter.RuntimeConsume)
	if config.Schema.model == nil {
		return nil, report, rejectReport(&report, CodeSchemaInvalid, "schema", "GraphQL schema mapping is required")
	}
	if config.Client == nil {
		return nil, report, rejectReport(&report, CodeResolverRequired, "transport", publicMessage(CodeResolverRequired))
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, report, rejectReport(&report, CodeResourceExhausted, "limits", publicMessage(CodeResourceExhausted))
	}
	maxOperations := config.MaxOperations
	if maxOperations == 0 {
		maxOperations = limits.MaxOperations
	}
	maxFanOut := config.MaxFanOut
	if maxFanOut == 0 {
		maxFanOut = min(limits.MaxFields, 4096)
	}
	if len(config.Resolvers) == 0 || len(config.Resolvers) > maxOperations {
		return nil, report, rejectReport(&report, CodeResolverRequired, "resolver-registration", publicMessage(CodeResolverRequired))
	}
	operations := make([]interopadapter.Operation, 0, len(config.Resolvers))
	seen := make(map[string]bool)
	for _, resolver := range config.Resolvers {
		descriptor, exists := config.Schema.operations[resolver.Operation]
		if !exists || seen[resolver.Operation] {
			return nil, report, rejectReport(&report, CodeOperationUnknown, "resolver-registration", publicMessage(CodeOperationUnknown))
		}
		if resolver.Adapt == nil {
			return nil, report, rejectReport(&report, CodeResolverRequired, "partial-failure", "GraphQL partial-result adaptation must be explicit")
		}
		seen[resolver.Operation] = true
		current := resolver
		operations = append(operations, interopadapter.Operation{
			ExternalName: resolver.Operation, Approved: resolver.Approved, Descriptor: descriptor,
			Invoker: interopadapter.InvokerFunc(func(ctx context.Context, backend interopadapter.BackendRequest) (map[string]any, error) {
				request, requestErr := buildUpstreamRequest(config.Schema, current.Operation, backend, limits)
				if requestErr != nil {
					code := CodeOf(requestErr)
					return nil, &runtime.Error{Code: code, Message: publicMessage(code), Cause: requestErr}
				}
				response, executeErr := config.Client.Execute(ctx, request)
				if executeErr != nil {
					if errors.Is(executeErr, context.Canceled) || errors.Is(executeErr, context.DeadlineExceeded) {
						return nil, &runtime.Error{Code: CodeCancelled, Message: publicMessage(CodeCancelled), Cause: executeErr}
					}
					return nil, &runtime.Error{Code: CodeUpstreamFailed, Message: publicMessage(CodeUpstreamFailed), Cause: executeErr}
				}
				mapped, mapErr := MapResponse(response, limits)
				if mapErr != nil {
					code := CodeOf(mapErr)
					return nil, &runtime.Error{Code: code, Message: publicMessage(code), Cause: mapErr}
				}
				adapted, adaptErr := current.Adapt(ctx, mapped)
				if adaptErr != nil {
					return nil, &runtime.Error{Code: CodeExecutionFailed, Message: publicMessage(CodeExecutionFailed), Cause: adaptErr}
				}
				return adapted, nil
			}),
		})
	}
	snapshot, snapshotErr := config.Schema.document.Snapshot()
	if snapshotErr != nil {
		return nil, report, rejectReport(&report, CodeSchemaInvalid, "schema", publicMessage(CodeSchemaInvalid))
	}
	core, coreReport, err := interopadapter.Compile(interopadapter.Config{
		AdapterID: config.Schema.adapterID, Protocol: interopadapter.GraphQL,
		Specification: Specification, Direction: interopadapter.RuntimeConsume,
		SchemaIdentity: config.Schema.schemaIdentity, WireVersion: config.Schema.wireVersion,
		Types: snapshot, Mappings: directionMappings(interopadapter.RuntimeConsume),
		Operations: operations, MaxOperations: maxOperations, MaxFanOut: maxFanOut,
	})
	if err != nil {
		coreReport.Profile = Profile
		return nil, coreReport, adapterError(CodeRegistrationFailed, err)
	}
	coreReport.Profile = Profile
	return &CompiledConsumer{core: core, report: coreReport}, coreReport, nil
}

func buildUpstreamRequest(imported ImportedSchema, operationName string, backend interopadapter.BackendRequest, limits Limits) (Request, error) {
	operation, field, ok := imported.rootOperation(operationName)
	if !ok {
		return Request{}, adapterError(CodeOperationUnknown, errors.New("GraphQL operation is absent"))
	}
	generatedName := "Naatre_" + operationName
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s", operation.kind, generatedName)
	if len(field.arguments) != 0 {
		builder.WriteString("(")
		for index, argument := range field.arguments {
			if index > 0 {
				builder.WriteString(", ")
			}
			fmt.Fprintf(&builder, "$%s: %s", argument.name, argument.typeRef.string())
		}
		builder.WriteString(")")
	}
	builder.WriteString(" { " + operationName)
	if len(field.arguments) != 0 {
		builder.WriteString("(")
		for index, argument := range field.arguments {
			if index > 0 {
				builder.WriteString(", ")
			}
			fmt.Fprintf(&builder, "%s: $%s", argument.name, argument.name)
		}
		builder.WriteString(")")
	}
	if len(backend.Projection) != 0 {
		projection, err := renderProjection(backend.Projection, limits)
		if err != nil {
			return Request{}, err
		}
		builder.WriteString(projection)
	}
	builder.WriteString(" }")
	if builder.Len() > limits.MaxDocumentBytes {
		return Request{}, adapterError(CodeResourceExhausted, errors.New("generated GraphQL document exceeds configured bounds"))
	}
	variables := backend.Input
	if len(variables) == 0 {
		variables = json.RawMessage(`{}`)
	}
	if !json.Valid(variables) {
		return Request{}, adapterError(CodeDocumentInvalid, errors.New("naatre input is not valid JSON"))
	}
	return Request{Document: builder.String(), OperationName: generatedName, Variables: append(json.RawMessage(nil), variables...)}, nil
}

func renderProjection(paths []string, limits Limits) (string, error) {
	if len(paths) > limits.MaxFields {
		return "", adapterError(CodeResourceExhausted, errors.New("GraphQL projection exceeds configured bounds"))
	}
	type projectionNode map[string]projectionNode
	tree := make(projectionNode)
	for _, path := range paths {
		current := tree
		parts := strings.Split(path, ".")
		if len(parts) > limits.MaxDepth {
			return "", adapterError(CodeResourceExhausted, errors.New("GraphQL projection depth exceeds configured bounds"))
		}
		for _, part := range parts {
			if !validUserGraphQLName(part) {
				return "", adapterError(CodeDocumentInvalid, errors.New("GraphQL projection contains an invalid field name"))
			}
			next, ok := current[part]
			if !ok {
				next = make(projectionNode)
				current[part] = next
			}
			current = next
		}
	}
	var render func(projectionNode) string
	render = func(node projectionNode) string {
		names := make([]string, 0, len(node))
		for name := range node {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, name := range names {
			children := node[name]
			if len(children) == 0 {
				parts = append(parts, name)
			} else {
				parts = append(parts, name+render(children))
			}
		}
		return " { " + strings.Join(parts, " ") + " }"
	}
	return render(tree), nil
}

// NewRuntime creates the in-process runtime-expose adapter. Authentication and
// wire transport remain application-owned; Execute must retain Naatre registry
// authorization and completion.
func NewRuntime(config RuntimeConfig) (*Runtime, error) {
	if config.Schema.model == nil || config.Execute == nil {
		return nil, adapterError(CodeRegistrationFailed, errors.New("GraphQL runtime requires schema and executor"))
	}
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	return &Runtime{schema: config.Schema, execute: config.Execute, subscribe: config.Subscribe, limits: limits}, nil
}

// Execute maps one GraphQL query or mutation through the existing runtime.
func (r *Runtime) Execute(ctx context.Context, source []byte, operationName string, variables json.RawMessage) (Response, error) {
	request, executable, operation, err := r.request(ctx, source, operationName, variables)
	if err != nil {
		return Response{}, err
	}
	if operation.kind == protocol.Subscription {
		return Response{}, adapterError(CodeUnsupported, errors.New("subscription requires Subscribe"))
	}
	outcome := r.execute(ctx, request)
	response := r.outcomeResponse(executable, operation, outcome)
	if err := r.validateResponseBounds(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// Subscribe maps ordered Naatre subscription outcomes until the source closes
// or the caller context is cancelled.
func (r *Runtime) Subscribe(ctx context.Context, source []byte, operationName string, variables json.RawMessage) (<-chan Response, error) {
	request, executable, operation, err := r.request(ctx, source, operationName, variables)
	if err != nil {
		return nil, err
	}
	if operation.kind != protocol.Subscription || r.subscribe == nil {
		return nil, adapterError(CodeUnsupported, errors.New("GraphQL subscription runtime is unavailable"))
	}
	upstream, err := r.subscribe(ctx, request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, adapterError(CodeCancelled, err)
		}
		return nil, adapterError(CodeUpstreamFailed, err)
	}
	if upstream == nil {
		return nil, adapterError(CodeResponseInvalid, errors.New("subscription returned a nil stream"))
	}
	output := make(chan Response)
	go func() {
		defer close(output)
		for {
			select {
			case <-ctx.Done():
				return
			case outcome, ok := <-upstream:
				if !ok {
					return
				}
				response := r.outcomeResponse(executable, operation, outcome)
				if err := r.validateResponseBounds(response); err != nil {
					response = failureResponse(CodeOf(err))
				}
				select {
				case <-ctx.Done():
					return
				case output <- response:
				}
			}
		}
	}()
	return output, nil
}

func (r *Runtime) request(ctx context.Context, source []byte, operationName string, variables json.RawMessage) (*protocol.Request, Executable, graphOperation, error) {
	executable, err := ImportOperation(ctx, source, r.schema, r.limits)
	if err != nil {
		return nil, Executable{}, graphOperation{}, err
	}
	operation, ok := executable.operation(operationName)
	if !ok {
		return nil, Executable{}, graphOperation{}, adapterError(CodeOperationUnknown, errors.New("GraphQL operation selection is ambiguous or absent"))
	}
	if len(variables) == 0 {
		variables = json.RawMessage(`{}`)
	}
	if !json.Valid(variables) {
		return nil, Executable{}, graphOperation{}, adapterError(CodeDocumentInvalid, errors.New("GraphQL variables are invalid JSON"))
	}
	wire := map[string]any{
		"version": "1", "operation": operation.name,
		"document":  json.RawMessage(executable.document.CanonicalJSON()),
		"variables": json.RawMessage(variables), "capabilities": []string{Profile},
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, Executable{}, graphOperation{}, adapterError(CodeDocumentInvalid, err)
	}
	request, err := protocol.DecodeRequest(encoded, protocol.DecodeOptions{Capabilities: map[string]bool{Profile: true}})
	if err != nil {
		return nil, Executable{}, graphOperation{}, adapterError(CodeDocumentInvalid, err)
	}
	return request, executable, operation, nil
}

func (e Executable) operation(name string) (graphOperation, bool) {
	if e.model == nil {
		return graphOperation{}, false
	}
	if name == "" && len(e.model.operations) == 1 {
		return e.model.operations[0], true
	}
	for _, operation := range e.model.operations {
		if operation.name == name {
			return operation, true
		}
	}
	return graphOperation{}, false
}

func (s ImportedSchema) rootOperation(name string) (graphOperation, graphField, bool) {
	if s.model == nil {
		return graphOperation{}, graphField{}, false
	}
	descriptor, ok := s.operations[name]
	if !ok {
		return graphOperation{}, graphField{}, false
	}
	rootName := s.model.roots[descriptor.Kind]
	root := s.model.types[rootName]
	if root == nil {
		return graphOperation{}, graphField{}, false
	}
	for _, field := range root.fields {
		if field.name == name {
			return graphOperation{name: name, kind: descriptor.Kind}, field, true
		}
	}
	return graphOperation{}, graphField{}, false
}
