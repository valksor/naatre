package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// MaxDirectivePlanCost bounds the total declared and planner-added directive
// cost in one prepared operation. A directive cannot raise this host limit.
const MaxDirectivePlanCost = schema.MaxDirectiveCost

// ErrDirectiveNextCalled reports an execution wrapper that attempted to
// invoke its downstream work more than once.
var ErrDirectiveNextCalled = errors.New("directive next called more than once")

var directiveVersionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_.-]{0,127}$`)

// DirectiveArguments is an immutable, schema-coerced argument set.
type DirectiveArguments struct {
	values map[string]any
}

// Value returns one detached argument value. Built-in scalars use their Go
// scalar representation; portable composite and custom values use
// schema.InputValue.
func (a DirectiveArguments) Value(name string) (any, bool) {
	value, ok := a.values[name]
	if !ok || value == nil {
		return value, ok
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Pointer || reflected.IsNil() {
		return value, true
	}
	detached := reflect.New(reflected.Elem().Type())
	detached.Elem().Set(reflected.Elem())
	return detached.Interface(), true
}

// DirectiveSelection is the closed public view exposed to planners and
// wrappers. It deliberately contains no structural mutation handles.
type DirectiveSelection struct {
	Kind         protocol.SelectionKind
	Name         string
	ResponseName string
	Binding      string
	Input        schema.TypeID
	Output       schema.TypeID
	Effect       Effect
	Cost         uint64
}

// DirectivePlanContext contains only immutable, request-stable planning data.
type DirectivePlanContext struct {
	Operation  protocol.OperationKind
	Descriptor schema.DirectiveDescriptor
	Selection  DirectiveSelection
	Arguments  DirectiveArguments
}

// DirectivePlanDecision is the complete set of supported plan changes.
// Directives can skip an already-validated node or add bounded cost; they
// cannot add fields, effects, aliases, bindings, or authorization policy.
type DirectivePlanDecision struct {
	Skip           bool
	AdditionalCost uint64
}

type DirectivePlanner interface {
	PlanDirective(DirectivePlanContext) (DirectivePlanDecision, error)
}

type DirectivePlannerFunc func(DirectivePlanContext) (DirectivePlanDecision, error)

func (f DirectivePlannerFunc) PlanDirective(input DirectivePlanContext) (DirectivePlanDecision, error) {
	return f(input)
}

// DirectiveExecutionContext is supplied only after authorization, input
// validation, cancellation checks, and resource admission have succeeded.
type DirectiveExecutionContext struct {
	Operation  protocol.OperationKind
	Descriptor schema.DirectiveDescriptor
	Selection  DirectiveSelection
	Arguments  DirectiveArguments
	Path       []any
}

type DirectiveNext func() (any, error)

type DirectiveExecutionWrapper interface {
	WrapDirective(context.Context, DirectiveExecutionContext, DirectiveNext) (any, error)
}

type DirectiveExecutionWrapperFunc func(context.Context, DirectiveExecutionContext, DirectiveNext) (any, error)

func (f DirectiveExecutionWrapperFunc) WrapDirective(ctx context.Context, input DirectiveExecutionContext, next DirectiveNext) (any, error) {
	return f(ctx, input, next)
}

// DirectiveResponseContext exposes only response-safe metadata. It contains no
// mutable response value and therefore cannot rewrite completed application
// data.
type DirectiveResponseContext struct {
	Operation  protocol.OperationKind
	Descriptor schema.DirectiveDescriptor
	Selection  DirectiveSelection
	Arguments  DirectiveArguments
	Path       []any
}

type DirectiveResponseAnnotator interface {
	AnnotateDirective(DirectiveResponseContext) (json.RawMessage, error)
}

type DirectiveResponseAnnotatorFunc func(DirectiveResponseContext) (json.RawMessage, error)

func (f DirectiveResponseAnnotatorFunc) AnnotateDirective(input DirectiveResponseContext) (json.RawMessage, error) {
	return f(input)
}

// DirectiveAnnotation is ordered by directive source position, independent of
// parallel completion order.
type DirectiveAnnotation struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Path    []any           `json:"path"`
	Value   json.RawMessage `json:"value"`
}

// DirectiveDefinition binds a portable, versioned descriptor to bounded host
// callbacks. The callbacks themselves never participate in schema identity.
type DirectiveDefinition struct {
	Descriptor schema.DirectiveDescriptor
	Planner    DirectivePlanner
	Wrapper    DirectiveExecutionWrapper
	Annotator  DirectiveResponseAnnotator
}

type registeredDirective struct {
	DirectiveDefinition
	standard bool
}

type plannedDirective struct {
	definition registeredDirective
	invocation protocol.Directive
	decision   DirectivePlanDecision
}

type evaluatedDirective struct {
	planned   plannedDirective
	arguments DirectiveArguments
}

type recordedDirectiveAnnotation struct {
	annotation  DirectiveAnnotation
	sourceStart int
}

type directiveAnnotationRecorder struct {
	mu     sync.Mutex
	values []recordedDirectiveAnnotation
}

func (r *directiveAnnotationRecorder) record(annotation DirectiveAnnotation, sourceStart int) {
	r.mu.Lock()
	r.values = append(r.values, recordedDirectiveAnnotation{annotation: annotation, sourceStart: sourceStart})
	r.mu.Unlock()
}

func (r *directiveAnnotationRecorder) annotations() []DirectiveAnnotation {
	r.mu.Lock()
	values := slices.Clone(r.values)
	r.mu.Unlock()
	slices.SortStableFunc(values, func(left, right recordedDirectiveAnnotation) int {
		if compared := comparePaths(left.annotation.Path, right.annotation.Path); compared != 0 {
			return compared
		}
		return left.sourceStart - right.sourceStart
	})
	result := make([]DirectiveAnnotation, len(values))
	for index, value := range values {
		result[index] = value.annotation
		result[index].Path = slices.Clone(value.annotation.Path)
		result[index].Value = append(json.RawMessage(nil), value.annotation.Value...)
	}
	return result
}

func builtInDirectives() map[string]registeredDirective {
	locations := []protocol.SelectionKind{
		protocol.FieldSelection, protocol.CallSelection, protocol.PipelineSelection, protocol.MapSelection,
		protocol.IndexSelection, protocol.SliceSelection, protocol.PageSelection, protocol.MetaSelection,
		protocol.ParallelSelection, protocol.FragmentSelection, protocol.CurrentSelection,
		protocol.NestSelection, protocol.UnnestSelection,
	}
	result := make(map[string]registeredDirective, 2)
	for _, name := range []string{"include", "skip"} {
		result[name] = registeredDirective{standard: true, DirectiveDefinition: DirectiveDefinition{Descriptor: schema.DirectiveDescriptor{
			ID: "naatre." + name, Name: name, Version: "1", Capability: "core.language-1",
			Locations: slices.Clone(locations), Arguments: []schema.DirectiveArgumentDescriptor{{
				ID: "naatre." + name + ".if", Name: "if", Type: schema.TypeID(schema.Boolean), Required: true,
			}},
			Phases: []schema.DirectivePhase{schema.DirectiveValidation, schema.DirectiveExecution},
			Effect: string(ReadEffect), Deterministic: true, Compatibility: schema.ChangeBreaking,
		}}}
	}
	return result
}

func cloneRegisteredDirectives(input map[string]registeredDirective) map[string]registeredDirective {
	result := maps.Clone(input)
	for name, definition := range result {
		definition.Descriptor = cloneDirectiveDescriptor(definition.Descriptor)
		result[name] = definition
	}
	return result
}

func cloneDirectiveDescriptor(input schema.DirectiveDescriptor) schema.DirectiveDescriptor {
	result := input
	result.Locations = slices.Clone(input.Locations)
	result.Arguments = slices.Clone(input.Arguments)
	for index := range result.Arguments {
		result.Arguments[index].Default = append(json.RawMessage(nil), input.Arguments[index].Default...)
	}
	result.Phases = slices.Clone(input.Phases)
	result.Capabilities = slices.Clone(input.Capabilities)
	result.Traits = cloneRuntimeTraits(input.Traits)
	result.Source = cloneRuntimeSource(input.Source)
	if input.Deprecation != nil {
		deprecation := *input.Deprecation
		result.Deprecation = &deprecation
	}
	return result
}

func prepareDirectiveDefinition(types schema.Snapshot, definition DirectiveDefinition) (DirectiveDefinition, error) {
	definition.Descriptor = cloneDirectiveDescriptor(definition.Descriptor)
	if err := validateDirectiveDefinition(types, definition); err != nil {
		return DirectiveDefinition{}, err
	}
	document, err := schema.ExportDocument(types, nil, nil, schema.ExportOptions{
		Revision: "directive-registration", Directives: []schema.DirectiveDescriptor{definition.Descriptor},
	})
	if err != nil {
		return DirectiveDefinition{}, fmt.Errorf("directive %q is not portable: %w", definition.Descriptor.ID, err)
	}
	definition.Descriptor = document.Directives()[0]
	return definition, nil
}

func validateDirectiveDefinition(types schema.Snapshot, definition DirectiveDefinition) error {
	descriptor := definition.Descriptor
	if !schemaIdentityPattern.MatchString(descriptor.ID) || !namePattern.MatchString(descriptor.Name) ||
		!directiveVersionPattern.MatchString(descriptor.Version) || !schemaIdentityPattern.MatchString(descriptor.Capability) {
		return fmt.Errorf("invalid directive descriptor %q", descriptor.ID)
	}
	if strings.HasPrefix(descriptor.ID, "naatre.") || strings.HasPrefix(descriptor.Capability, "core.") ||
		descriptor.Name == "include" || descriptor.Name == "skip" {
		return errors.New("the Naatre directive namespace is reserved")
	}
	if descriptor.Effect != string(ReadEffect) && descriptor.Effect != string(WriteEffect) {
		return fmt.Errorf("directive %q has invalid effect %q", descriptor.ID, descriptor.Effect)
	}
	if descriptor.Cost > MaxDirectivePlanCost {
		return fmt.Errorf("directive %q exceeds the directive cost limit", descriptor.ID)
	}
	if descriptor.Compatibility != schema.ChangeBreaking && descriptor.Compatibility != schema.ChangeDangerous &&
		descriptor.Compatibility != schema.ChangeAdditive && descriptor.Compatibility != schema.ChangeBehaviorOnly {
		return fmt.Errorf("directive %q has invalid compatibility %q", descriptor.ID, descriptor.Compatibility)
	}
	metadata := Descriptor{
		Name: descriptor.Name, Deprecation: descriptor.Deprecation, Capabilities: descriptor.Capabilities,
		Traits: descriptor.Traits, Source: descriptor.Source,
	}
	if err := validatePortableDescriptorMetadata(metadata); err != nil {
		return fmt.Errorf("directive %q: %w", descriptor.ID, err)
	}
	if err := validateRuntimeDirectiveLocations(descriptor.Locations); err != nil {
		return fmt.Errorf("directive %q: %w", descriptor.ID, err)
	}
	if err := validateRuntimeDirectivePhases(descriptor, definition); err != nil {
		return fmt.Errorf("directive %q: %w", descriptor.ID, err)
	}
	return validateRuntimeDirectiveArguments(types, descriptor.Arguments)
}

func validateRuntimeDirectiveLocations(locations []protocol.SelectionKind) error {
	if len(locations) == 0 {
		return errors.New("at least one location is required")
	}
	seen := make(map[protocol.SelectionKind]bool, len(locations))
	valid := builtInDirectives()["include"].Descriptor.Locations
	for _, location := range locations {
		if seen[location] || !slices.Contains(valid, location) {
			return fmt.Errorf("invalid or duplicate location %q", location)
		}
		seen[location] = true
	}
	return nil
}

func validateRuntimeDirectivePhases(descriptor schema.DirectiveDescriptor, definition DirectiveDefinition) error {
	seen, err := runtimeDirectivePhaseSet(descriptor.Phases)
	if err != nil {
		return err
	}
	if err := validateRuntimeDirectiveCallbacks(seen, definition); err != nil {
		return err
	}
	return validateRuntimeDirectivePhaseSemantics(descriptor, seen)
}

func runtimeDirectivePhaseSet(phases []schema.DirectivePhase) (map[schema.DirectivePhase]bool, error) {
	if len(phases) == 0 {
		return nil, errors.New("at least one phase is required")
	}
	seen := make(map[schema.DirectivePhase]bool, len(phases))
	for _, phase := range phases {
		if seen[phase] || !isRuntimeDirectivePhase(phase) {
			return nil, fmt.Errorf("invalid or duplicate phase %q", phase)
		}
		seen[phase] = true
	}
	if !seen[schema.DirectiveValidation] {
		return nil, errors.New("validation phase is required")
	}
	return seen, nil
}

func isRuntimeDirectivePhase(phase schema.DirectivePhase) bool {
	switch phase {
	case schema.DirectiveValidation, schema.DirectivePlanning, schema.DirectiveExecution, schema.DirectiveResponse:
		return true
	default:
		return false
	}
}

func validateRuntimeDirectiveCallbacks(seen map[schema.DirectivePhase]bool, definition DirectiveDefinition) error {
	callbacks := []struct {
		phase   schema.DirectivePhase
		present bool
		message string
	}{
		{schema.DirectivePlanning, !nilCallback(definition.Planner), "planning phase and planner callback must be declared together"},
		{schema.DirectiveExecution, !nilCallback(definition.Wrapper), "execution phase and wrapper callback must be declared together"},
		{schema.DirectiveResponse, !nilCallback(definition.Annotator), "response phase and annotator callback must be declared together"},
	}
	for _, callback := range callbacks {
		if seen[callback.phase] != callback.present {
			return errors.New(callback.message)
		}
	}
	return nil
}

func validateRuntimeDirectivePhaseSemantics(descriptor schema.DirectiveDescriptor, seen map[schema.DirectivePhase]bool) error {
	if seen[schema.DirectivePlanning] && !descriptor.Deterministic {
		return errors.New("planning phase requires deterministic behavior")
	}
	if descriptor.Effect == string(WriteEffect) && !seen[schema.DirectiveExecution] {
		return errors.New("write effect requires the execution phase")
	}
	if (seen[schema.DirectiveExecution] || seen[schema.DirectiveResponse]) && slices.ContainsFunc(descriptor.Locations, func(location protocol.SelectionKind) bool {
		return location != protocol.CallSelection && location != protocol.FieldSelection
	}) {
		return errors.New("execution and response callbacks require call or field locations")
	}
	return nil
}

func validateRuntimeDirectiveArguments(types schema.Snapshot, arguments []schema.DirectiveArgumentDescriptor) error {
	ids := make(map[string]bool, len(arguments))
	names := make(map[string]bool, len(arguments))
	for _, argument := range arguments {
		if !schemaIdentityPattern.MatchString(argument.ID) || !namePattern.MatchString(argument.Name) || ids[argument.ID] || names[argument.Name] {
			return fmt.Errorf("invalid or duplicate directive argument %q", argument.ID)
		}
		if argument.Required && len(argument.Default) != 0 {
			return fmt.Errorf("required directive argument %q cannot have a default", argument.Name)
		}
		descriptor, ok := types.Lookup(argument.Type)
		if !ok || !descriptor.Input {
			return fmt.Errorf("directive argument %q references an unknown or non-input type", argument.Name)
		}
		if len(argument.Default) != 0 {
			if _, err := schema.CoerceInput(types, argument.Type, argument.Default, argument.Nullable); err != nil {
				return fmt.Errorf("directive argument %q has invalid default: %w", argument.Name, err)
			}
		}
		ids[argument.ID], names[argument.Name] = true, true
	}
	return nil
}

func nilCallback(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	kind := reflected.Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map ||
		kind == reflect.Pointer || kind == reflect.Slice) && reflected.IsNil()
}

func callDirectivePlanner(planner DirectivePlanner, input DirectivePlanContext) (decision DirectivePlanDecision, err error) {
	return callDirectiveCallback(func() (DirectivePlanDecision, error) {
		return planner.PlanDirective(input)
	}, DirectivePlanDecision{}, errors.New("directive planner panicked"))
}

func callDirectiveWrapper(ctx context.Context, wrapper DirectiveExecutionWrapper, input DirectiveExecutionContext, next DirectiveNext) (output any, err error) {
	return callDirectiveCallback(func() (any, error) {
		return wrapper.WrapDirective(ctx, input, next)
	}, nil, &handlerPanic{})
}

func callDirectiveAnnotator(annotator DirectiveResponseAnnotator, input DirectiveResponseContext) (value json.RawMessage, err error) {
	return callDirectiveCallback(func() (json.RawMessage, error) {
		return annotator.AnnotateDirective(input)
	}, nil, errors.New("directive response annotator panicked"))
}

func callDirectiveCallback[T any](callback func() (T, error), panicValue T, panicErr error) (value T, err error) {
	containPanic(func() {
		value, err = callback()
	}, func() {
		value = panicValue
		err = panicErr
	})
	return value, err
}

func (p *Plan) recordDirectiveAnnotations(ctx context.Context, scope executionScope, node planNode, directives []evaluatedDirective, path []any) error {
	for _, directive := range directives {
		annotator := directive.planned.definition.Annotator
		if annotator == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		input := DirectiveResponseContext{
			Operation: p.kind, Descriptor: cloneDirectiveDescriptor(directive.planned.definition.Descriptor),
			Selection: directiveSelection(node), Arguments: directive.arguments, Path: slices.Clone(path),
		}
		value, err := invokeDirectiveAnnotator(ctx, scope, annotator, input)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		canonical, err := protocol.CanonicalizeJSON(value, protocol.Limits{})
		if err != nil {
			return fmt.Errorf("directive %q returned an invalid annotation: %w", directive.planned.definition.Descriptor.Name, err)
		}
		descriptor := directive.planned.definition.Descriptor
		scope.annotations.record(DirectiveAnnotation{
			ID: descriptor.ID, Name: descriptor.Name, Version: descriptor.Version,
			Path: slices.Clone(path), Value: canonical,
		}, directive.planned.invocation.Source().Start)
	}
	return nil
}

func invokeDirectiveAnnotator(ctx context.Context, scope executionScope, annotator DirectiveResponseAnnotator, input DirectiveResponseContext) (json.RawMessage, error) {
	release, err := acquireExecutionSlot(ctx, scope)
	if err != nil {
		return nil, err
	}
	value, err := callWithinGrace(ctx, release, scope.grace, func() (any, error) {
		return callDirectiveAnnotator(annotator, input)
	})
	if err != nil {
		return nil, err
	}
	annotation, ok := value.(json.RawMessage)
	if !ok {
		return nil, errors.New("directive response annotator returned an invalid value")
	}
	return annotation, nil
}

type directiveHandlerCall struct {
	node   planNode
	source any
	input  any
	path   []any
}

type directiveNextState struct {
	mu         sync.Mutex
	open       bool
	called     bool
	violated   bool
	done       chan struct{}
	downstream DirectiveNext
}

func newDirectiveNextState(downstream DirectiveNext) *directiveNextState {
	return &directiveNextState{open: true, downstream: downstream}
}

func (s *directiveNextState) call() (value any, err error) {
	s.mu.Lock()
	if !s.open || s.called {
		s.violated = true
		s.mu.Unlock()
		return nil, ErrDirectiveNextCalled
	}
	s.called = true
	s.done = make(chan struct{})
	done := s.done
	s.mu.Unlock()
	defer close(done)
	return s.downstream()
}

func (s *directiveNextState) finish() bool {
	s.mu.Lock()
	s.open = false
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	s.mu.Lock()
	violated := s.violated
	s.mu.Unlock()
	return violated
}

func (p *Plan) invokeDirectiveWrappers(ctx context.Context, directives []evaluatedDirective, call directiveHandlerCall) (any, error) {
	chain := DirectiveNext(func() (any, error) { return p.invokeHandler(ctx, call.node, call.source, call.input, call.path) })
	for index := len(directives) - 1; index >= 0; index-- {
		current := directives[index]
		if current.planned.definition.Wrapper == nil {
			continue
		}
		downstream := chain
		chain = func() (any, error) {
			next := newDirectiveNextState(downstream)
			invocation := DirectiveExecutionContext{
				Operation: p.kind, Descriptor: cloneDirectiveDescriptor(current.planned.definition.Descriptor),
				Selection: directiveSelection(call.node), Arguments: current.arguments, Path: slices.Clone(call.path),
			}
			value, err := callDirectiveWrapper(ctx, current.planned.definition.Wrapper, invocation, next.call)
			if next.finish() {
				return nil, ErrDirectiveNextCalled
			}
			return value, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, err := chain()
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	return value, err
}

func directiveSelection(node planNode) DirectiveSelection {
	selection := DirectiveSelection{
		Kind: node.kind, Name: node.name, ResponseName: node.outputName, Binding: node.binding,
		Input: node.input.id, Output: node.output.id,
	}
	if node.hasDefinition {
		selection.Effect = node.definition.descriptor.Metadata.Effect
		selection.Cost = node.definition.descriptor.Metadata.Cost
	}
	return selection
}
