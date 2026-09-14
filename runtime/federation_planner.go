package runtime

import (
	"context"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// GoFederationCoordinatorProfile identifies the executable Go coordinator
// evidence. It is not a wire capability and does not replace
// core.federation-1, which remains the normative federation authority.
const GoFederationCoordinatorProfile = "runtime.go.federation-coordinator-1"

// FederationTraceContext is the bounded cross-process trace state made
// available to a transport adapter. Baggage and arbitrary metadata are
// intentionally excluded from this authority boundary.
type FederationTraceContext struct {
	TraceParent string
}

// FederationTraceContextProvider extracts an explicitly approved trace parent
// from the current request context. Implementations must not encode identity,
// credentials, or application metadata into the returned value.
type FederationTraceContextProvider interface {
	TraceContext(context.Context) (FederationTraceContext, error)
}

type FederationTraceContextProviderFunc func(context.Context) (FederationTraceContext, error)

func (f FederationTraceContextProviderFunc) TraceContext(ctx context.Context) (FederationTraceContext, error) {
	return f(ctx)
}

// FederationEntityFetch is one explicit entity route selected for a
// distributed plan. Dependencies name other response keys in the same request;
// the planner verifies that they exactly match the composed manifest graph.
type FederationEntityFetch struct {
	ResponseKey  string
	ServiceID    string
	Type         schema.TypeID
	Input        any
	Path         []any
	MaxAttempts  uint32
	Dependencies []string
}

type FederationPlannerConfig struct {
	Composition schema.FederationComposition
	Limits      FederationLimits
}

// FederationPlanner converts explicit entity fetches into an immutable,
// dependency-ordered plan pinned to one admitted composition revision.
type FederationPlanner struct {
	composition schema.FederationComposition
	limits      FederationLimits
	routes      map[string]schema.FederationEntityRoute
}

func NewFederationPlanner(config FederationPlannerConfig) (*FederationPlanner, error) {
	if _, err := config.Composition.CanonicalJSON(); err != nil ||
		config.Limits.MaxCalls == 0 || config.Limits.MaxCost == 0 ||
		config.Limits.MaxConcurrency == 0 || config.Limits.MaxAttempts == 0 {
		return nil, errors.New("federation planner requires a composition and finite limits")
	}
	if config.Limits.MaxConcurrency > uint64(maxPortableConcurrency) {
		return nil, errors.New("federation concurrency exceeds the portable ceiling")
	}
	routes := make(map[string]schema.FederationEntityRoute)
	for _, route := range config.Composition.EntityRoutes() {
		routes[federationEntityRouteKey(route.ServiceID, route.Type)] = route
	}
	return &FederationPlanner{composition: config.Composition, limits: config.Limits, routes: routes}, nil
}

type plannedFederationEntityFetch struct {
	route schema.FederationEntityRoute
	call  FederationCall
}

func (p *FederationPlanner) PlanEntityFetches(schemaRevision string, fetches []FederationEntityFetch) (FederationPlan, error) {
	if p == nil {
		return FederationPlan{}, federationExecutionFailure(CodeFederationPlanInvalid, "federation planner is not initialized", nil, nil)
	}
	if schemaRevision == "" || schemaRevision != p.composition.Schema().Revision() {
		return FederationPlan{}, federationExecutionFailure(CodeFederationSchemaMismatch, "federation composition revision is unavailable", nil, nil)
	}
	if len(fetches) == 0 || uint64(len(fetches)) > p.limits.MaxCalls {
		return FederationPlan{}, federationExecutionFailure(CodeResourceExhausted, "federation entity fan-out exceeds the call bound", nil, errResourceBudget)
	}

	orderedFetches := slices.Clone(fetches)
	sort.Slice(orderedFetches, func(i, j int) bool { return orderedFetches[i].ResponseKey < orderedFetches[j].ResponseKey })
	planned, failure := p.planOrderedEntityFetches(orderedFetches)
	if failure != nil {
		return FederationPlan{}, failure
	}

	for responseKey, entry := range planned {
		dependencies, failure := validatePlannedEntityDependencies(entry, fetchesForResponseKey(orderedFetches, responseKey), planned)
		if failure != nil {
			return FederationPlan{}, failure
		}
		entry.call.DependsOn = dependencies
		planned[responseKey] = entry
	}
	calls, failure := orderFederationEntityCalls(planned)
	if failure != nil {
		return FederationPlan{}, failure
	}
	return FederationPlan{SchemaRevision: schemaRevision, Calls: calls}, nil
}

func (p *FederationPlanner) planOrderedEntityFetches(orderedFetches []FederationEntityFetch) (map[string]plannedFederationEntityFetch, *ExecutionError) {
	planned := make(map[string]plannedFederationEntityFetch, len(orderedFetches))
	var totalCost uint64
	for _, fetch := range orderedFetches {
		entry, cost, failure := p.planEntityFetch(fetch)
		if failure != nil {
			return nil, failure
		}
		if _, exists := planned[entry.call.ResponseKey]; exists {
			return nil, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity plan contains a duplicate response key", fetch.Path, nil)
		}
		var ok bool
		totalCost, ok = checkedResourceAdd(totalCost, cost)
		if !ok || totalCost > p.limits.MaxCost {
			return nil, federationExecutionFailure(CodeResourceExhausted, "federation entity plan exceeds the cost bound", fetch.Path, errResourceBudget)
		}
		planned[entry.call.ResponseKey] = entry
	}
	return planned, nil
}

func (p *FederationPlanner) planEntityFetch(fetch FederationEntityFetch) (plannedFederationEntityFetch, uint64, *ExecutionError) {
	if fetch.ResponseKey == "" || fetch.ServiceID == "" || fetch.Type == "" || len(fetch.Path) == 0 || !validFederationRemotePath(fetch.Path) {
		return plannedFederationEntityFetch{}, 0, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity fetch is incomplete", fetch.Path, nil)
	}
	route, ok := p.routes[federationEntityRouteKey(fetch.ServiceID, fetch.Type)]
	if !ok {
		return plannedFederationEntityFetch{}, 0, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity route is unavailable", fetch.Path, nil)
	}
	operation, ok := p.composition.Operation(route.OperationID)
	if !ok || operation.Kind != protocol.Query || operation.Effect != string(ReadEffect) ||
		fetch.MaxAttempts == 0 || fetch.MaxAttempts > p.limits.MaxAttempts || (fetch.MaxAttempts > 1 && !operation.RetrySafe) {
		return plannedFederationEntityFetch{}, 0, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity retry policy is invalid", fetch.Path, nil)
	}
	input, err := cloneFederationValue(fetch.Input)
	if err != nil {
		return plannedFederationEntityFetch{}, 0, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity input is not portable", fetch.Path, err)
	}
	cost, ok := checkedResourceMultiply(federationOperationCost(operation), uint64(fetch.MaxAttempts))
	if !ok {
		return plannedFederationEntityFetch{}, 0, federationExecutionFailure(CodeResourceExhausted, "federation entity plan exceeds the cost bound", fetch.Path, errResourceBudget)
	}
	return plannedFederationEntityFetch{
		route: route,
		call: FederationCall{
			ResponseKey: fetch.ResponseKey, ServiceID: route.ServiceID, OperationID: route.OperationID,
			Input: input, Path: clonePath(fetch.Path), MaxAttempts: fetch.MaxAttempts,
		},
	}, cost, nil
}

func fetchesForResponseKey(fetches []FederationEntityFetch, responseKey string) FederationEntityFetch {
	index, _ := slices.BinarySearchFunc(fetches, responseKey, func(fetch FederationEntityFetch, target string) int {
		if fetch.ResponseKey < target {
			return -1
		}
		if fetch.ResponseKey > target {
			return 1
		}
		return 0
	})
	return fetches[index]
}

func validatePlannedEntityDependencies(
	entry plannedFederationEntityFetch,
	fetch FederationEntityFetch,
	planned map[string]plannedFederationEntityFetch,
) ([]string, *ExecutionError) {
	required := make(map[string]bool, len(entry.route.Requires))
	for _, dependency := range entry.route.Requires {
		required[federationEntityRouteKey(dependency.ServiceID, dependency.Type)] = true
	}
	dependencies := slices.Clone(fetch.Dependencies)
	sort.Strings(dependencies)
	for index, responseKey := range dependencies {
		if responseKey == "" || (index > 0 && dependencies[index-1] == responseKey) {
			return nil, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity dependencies are invalid", fetch.Path, nil)
		}
		dependency, ok := planned[responseKey]
		key := federationEntityRouteKey(dependency.route.ServiceID, dependency.route.Type)
		if !ok || !required[key] {
			return nil, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity dependency is unavailable", fetch.Path, nil)
		}
		delete(required, key)
	}
	if len(required) != 0 {
		return nil, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity dependency is missing", fetch.Path, nil)
	}
	return dependencies, nil
}

func orderFederationEntityCalls(planned map[string]plannedFederationEntityFetch) ([]FederationCall, *ExecutionError) {
	indegree, dependents, ready := indexFederationEntityDependencies(planned)
	ordered := make([]FederationCall, 0, len(planned))
	for len(ready) != 0 {
		sort.Strings(ready)
		current := ready
		ready = nil
		for _, responseKey := range current {
			ordered = append(ordered, planned[responseKey].call)
			for _, dependent := range dependents[responseKey] {
				indegree[dependent]--
				if indegree[dependent] == 0 {
					ready = append(ready, dependent)
				}
			}
		}
	}
	if len(ordered) != len(planned) {
		return nil, federationExecutionFailure(CodeFederationPlanInvalid, "federation entity dependency graph is invalid", nil, nil)
	}
	return ordered, nil
}

func indexFederationEntityDependencies(planned map[string]plannedFederationEntityFetch) (map[string]int, map[string][]string, []string) {
	indegree := make(map[string]int, len(planned))
	dependents := make(map[string][]string, len(planned))
	ready := make([]string, 0, len(planned))
	for responseKey, entry := range planned {
		indegree[responseKey] = len(entry.call.DependsOn)
		if len(entry.call.DependsOn) == 0 {
			ready = append(ready, responseKey)
		}
		for _, dependency := range entry.call.DependsOn {
			dependents[dependency] = append(dependents[dependency], responseKey)
		}
	}
	return indegree, dependents, ready
}

func federationEntityRouteKey(serviceID string, typeID schema.TypeID) string {
	return serviceID + "\x00" + string(typeID)
}

type federationDependencyExecution struct {
	indices   map[string]int
	settled   []bool
	results   []federationCallResult
	remaining int
}

func executeFederationDependencies(ctx context.Context, coordinator *ReferenceFederationCoordinator, requestID string, plan FederationPlan) []federationCallResult {
	state := newFederationDependencyExecution(plan.Calls)
	for state.remaining != 0 {
		before := state.remaining
		ready := state.readyCalls(plan.Calls)
		state.executeReady(ctx, coordinator, requestID, plan, ready)
		if state.remaining == before {
			break
		}
	}
	return state.results
}

func newFederationDependencyExecution(calls []FederationCall) *federationDependencyExecution {
	indices := make(map[string]int, len(calls))
	for index, call := range calls {
		indices[call.ResponseKey] = index
	}
	return &federationDependencyExecution{
		indices: indices, settled: make([]bool, len(calls)),
		results: make([]federationCallResult, len(calls)), remaining: len(calls),
	}
}

func (s *federationDependencyExecution) readyCalls(calls []FederationCall) []int {
	ready := make([]int, 0, s.remaining)
	for index, call := range calls {
		if s.settled[index] {
			continue
		}
		settled, failed := s.dependencyStatus(call.DependsOn)
		if !settled {
			continue
		}
		if failed {
			s.setDependencyFailure(index, call.Path)
			continue
		}
		ready = append(ready, index)
	}
	return ready
}

func (s *federationDependencyExecution) dependencyStatus(dependencies []string) (settled, failed bool) {
	for _, responseKey := range dependencies {
		index := s.indices[responseKey]
		if !s.settled[index] {
			return false, false
		}
		failed = failed || len(s.results[index].failures) != 0
	}
	return true, failed
}

func (s *federationDependencyExecution) setDependencyFailure(index int, path []any) {
	failure := makeExecutionError(CodeFederationUnavailable, "required federated entity is unavailable", clonePath(path), protocol.Source{}, nil)
	s.results[index] = federationCallResult{failures: []ExecutionError{failure}}
	s.settled[index] = true
	s.remaining--
}

func (s *federationDependencyExecution) executeReady(
	ctx context.Context,
	coordinator *ReferenceFederationCoordinator,
	requestID string,
	plan FederationPlan,
	ready []int,
) {
	if len(ready) == 0 {
		return
	}
	results := coordinator.dispatchFederationCalls(ctx, requestID, plan, ready)
	for range ready {
		result := <-results
		s.results[result.index] = result.result
		s.settled[result.index] = true
		s.remaining--
	}
}

type federationDependencyGraph struct {
	indegree   []int
	dependents [][]int
}

func buildFederationDependencyGraph(calls []FederationCall, maxEdges uint64) (federationDependencyGraph, *ExecutionError) {
	indices := make(map[string]int, len(calls))
	for index, call := range calls {
		indices[call.ResponseKey] = index
	}
	graph := federationDependencyGraph{indegree: make([]int, len(calls)), dependents: make([][]int, len(calls))}
	var edges uint64
	for index, call := range calls {
		if failure := graph.addCall(index, call, indices, maxEdges, &edges); failure != nil {
			return federationDependencyGraph{}, failure
		}
	}
	return graph, nil
}

func (g *federationDependencyGraph) addCall(index int, call FederationCall, indices map[string]int, maxEdges uint64, edges *uint64) *ExecutionError {
	seen := make(map[string]bool, len(call.DependsOn))
	for _, responseKey := range call.DependsOn {
		dependencyIndex, ok := indices[responseKey]
		if !ok || responseKey == call.ResponseKey || seen[responseKey] {
			return federationPlanFailure("federation plan contains an invalid dependency", call.Path)
		}
		seen[responseKey] = true
		g.indegree[index]++
		g.dependents[dependencyIndex] = append(g.dependents[dependencyIndex], index)
		*edges++
		if *edges > maxEdges {
			return federationBudgetFailure(call.Path)
		}
	}
	return nil
}

func (g federationDependencyGraph) acyclic() bool {
	ready := make([]int, 0, len(g.indegree))
	for index, count := range g.indegree {
		if count == 0 {
			ready = append(ready, index)
		}
	}
	visited := 0
	for len(ready) != 0 {
		current := ready
		ready = nil
		for _, index := range current {
			visited++
			ready = append(ready, g.releasedDependents(index)...)
		}
	}
	return visited == len(g.indegree)
}

func (g *federationDependencyGraph) releasedDependents(index int) []int {
	ready := make([]int, 0, len(g.dependents[index]))
	for _, dependent := range g.dependents[index] {
		g.indegree[dependent]--
		if g.indegree[dependent] == 0 {
			ready = append(ready, dependent)
		}
	}
	return ready
}

// FederationEndpointBinding binds one operator-owned opaque endpoint reference
// to a transport adapter. Composition never dereferences the reference.
type FederationEndpointBinding struct {
	Reference string
	Invoker   FederationInvoker
}

type federationEndpointRegistry struct {
	serviceEndpoints map[string]string
	invokers         map[string]FederationInvoker
}

func newFederationEndpointRegistry(composition schema.FederationComposition, bindings []FederationEndpointBinding) (*federationEndpointRegistry, error) {
	required := make(map[string]bool)
	services := make(map[string]string)
	for _, serviceID := range composition.Services() {
		service, ok := composition.Service(serviceID)
		if !ok {
			return nil, errors.New("federation composition contains an unavailable service")
		}
		required[service.EndpointReference] = true
		services[service.ID] = service.EndpointReference
	}
	invokers := make(map[string]FederationInvoker, len(bindings))
	for _, binding := range bindings {
		if !required[binding.Reference] || binding.Invoker == nil || invokers[binding.Reference] != nil {
			return nil, errors.New("federation endpoint bindings must exactly match the composition")
		}
		invokers[binding.Reference] = binding.Invoker
	}
	if len(invokers) != len(required) {
		return nil, errors.New("federation endpoint bindings must exactly match the composition")
	}
	return &federationEndpointRegistry{serviceEndpoints: services, invokers: invokers}, nil
}

func (r *federationEndpointRegistry) Invoke(ctx context.Context, invocation FederationInvocation) (FederationRemoteResult, error) {
	if r == nil || r.serviceEndpoints[invocation.ServiceID] != invocation.EndpointReference {
		return FederationRemoteResult{}, errors.New("federation endpoint is unavailable")
	}
	invoker := r.invokers[invocation.EndpointReference]
	if invoker == nil {
		return FederationRemoteResult{}, errors.New("federation endpoint is unavailable")
	}
	return invoker.Invoke(ctx, invocation)
}

type FederationCoordinatorConfig struct {
	Composition     schema.FederationComposition
	Delegations     *FederationDelegationIssuer
	Endpoints       []FederationEndpointBinding
	TraceContext    FederationTraceContextProvider
	Limits          FederationLimits
	MaximumDuration time.Duration
	AbandonGrace    time.Duration
}

// FederationCoordinator is the Go runtime integration boundary for planning
// and executing entity fetches through operator-bound transport adapters.
type FederationCoordinator struct {
	planner  *FederationPlanner
	executor *ReferenceFederationCoordinator
}

func NewFederationCoordinator(config FederationCoordinatorConfig) (*FederationCoordinator, error) {
	planner, err := NewFederationPlanner(FederationPlannerConfig{Composition: config.Composition, Limits: config.Limits})
	if err != nil {
		return nil, err
	}
	endpoints, err := newFederationEndpointRegistry(config.Composition, config.Endpoints)
	if err != nil {
		return nil, err
	}
	executor, err := NewReferenceFederationCoordinator(ReferenceFederationConfig{
		Composition: config.Composition, Delegations: config.Delegations, Invoker: endpoints,
		TraceContext: config.TraceContext, Limits: config.Limits,
		MaximumDuration: config.MaximumDuration, AbandonGrace: config.AbandonGrace,
	})
	if err != nil {
		return nil, err
	}
	return &FederationCoordinator{planner: planner, executor: executor}, nil
}

func (c *FederationCoordinator) PlanEntityFetches(schemaRevision string, fetches []FederationEntityFetch) (FederationPlan, error) {
	if c == nil {
		return FederationPlan{}, federationExecutionFailure(CodeFederationPlanInvalid, "federation coordinator is not initialized", nil, nil)
	}
	return c.planner.PlanEntityFetches(schemaRevision, fetches)
}

func (c *FederationCoordinator) ExecuteEntityFetches(ctx context.Context, requestID, schemaRevision string, fetches []FederationEntityFetch) Outcome {
	if c == nil {
		return federationFailure(CodeFederationPlanInvalid, "federation coordinator is not initialized", nil, nil)
	}
	plan, err := c.planner.PlanEntityFetches(schemaRevision, fetches)
	if err != nil {
		var failure *ExecutionError
		if errors.As(err, &failure) {
			return Outcome{Data: map[string]any{}, Errors: []ExecutionError{*failure}, Effects: EffectNotApplicable}
		}
		return federationFailure(CodeFederationPlanInvalid, "federation entity plan could not be created", nil, err)
	}
	return c.executor.Execute(ctx, requestID, plan)
}
