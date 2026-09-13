package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
	"github.com/valksor/naatre/schema"
)

// Principal is authenticated request identity. Claims are copied on context
// insertion and retrieval so callers cannot mutate identity after admission.
type Principal struct {
	Subject               string            `json:"subject"`
	Tenant                string            `json:"tenant,omitempty"`
	Claims                map[string]string `json:"claims,omitempty"`
	AuthorizationRevision string            `json:"authorizationRevision,omitempty"`
}

type principalContextKey struct{}

func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	principal.Claims = maps.Clone(principal.Claims)
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	principal.Claims = maps.Clone(principal.Claims)
	return principal, ok
}

type AuthorizationMode string

const (
	AuthorizationAllowByDefault AuthorizationMode = "allow-by-default"
	AuthorizationDenyByDefault  AuthorizationMode = "deny-by-default"
)

type AuthorizationCacheScope string

const (
	AuthorizationCacheNoStore   AuthorizationCacheScope = "no-store"
	AuthorizationCachePrincipal AuthorizationCacheScope = "principal"
	AuthorizationCacheTenant    AuthorizationCacheScope = "tenant"
)

type AuthorizationDecision struct {
	Allowed               bool
	AuthorizationRevision string
	ExpiresAt             time.Time
	CacheScope            AuthorizationCacheScope
}

// AuthorizationRequest intentionally omits operation arguments. Policies get
// stable descriptor metadata, the safe response path, and an isolated current
// object for dynamic checks without receiving secrets merely for logging.
type AuthorizationRequest struct {
	Principal  Principal
	Operation  protocol.OperationKind
	Descriptor Descriptor
	Path       []any
	Object     any
}

type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) (AuthorizationDecision, error)
}

type AuthorizerFunc func(context.Context, AuthorizationRequest) (AuthorizationDecision, error)

func (f AuthorizerFunc) Authorize(ctx context.Context, request AuthorizationRequest) (AuthorizationDecision, error) {
	return f(ctx, request)
}

type PlanningAuthorizationRequest struct {
	Operation  protocol.OperationKind
	Descriptor Descriptor
	Path       []string
	Source     protocol.Source
}

type PlanningAuthorizer interface {
	AuthorizePlan(PlanningAuthorizationRequest) (AuthorizationDecision, error)
}

type PlanningAuthorizerFunc func(PlanningAuthorizationRequest) (AuthorizationDecision, error)

func (f PlanningAuthorizerFunc) AuthorizePlan(request PlanningAuthorizationRequest) (AuthorizationDecision, error) {
	return f(request)
}

type AuthorizationConfig struct {
	Mode               AuthorizationMode
	Authorizer         Authorizer
	PlanningAuthorizer PlanningAuthorizer
	Now                func() time.Time
}

type InterceptorLevel string

const (
	GlobalInterceptor       InterceptorLevel = "global"
	OperationInterceptor    InterceptorLevel = "operation"
	TypeInterceptor         InterceptorLevel = "type"
	FieldInterceptor        InterceptorLevel = "field"
	HandlerInterceptorLevel InterceptorLevel = "handler"
)

type HandlerInvocation struct {
	Principal  Principal
	Operation  protocol.OperationKind
	Descriptor Descriptor
	Path       []any
}

type HandlerNext func() (any, error)

type HandlerInterceptor interface {
	Intercept(context.Context, HandlerInvocation, HandlerNext) (any, error)
}

type HandlerInterceptorFunc func(context.Context, HandlerInvocation, HandlerNext) (any, error)

func (f HandlerInterceptorFunc) Intercept(ctx context.Context, invocation HandlerInvocation, next HandlerNext) (any, error) {
	return f(ctx, invocation, next)
}

type InterceptorRegistration struct {
	Level       InterceptorLevel
	Operation   protocol.OperationKind
	Owner       schema.TypeID
	Member      string
	Interceptor HandlerInterceptor
}

type registeredInterceptor struct {
	InterceptorRegistration
}

var ErrInterceptorNextCalled = errors.New("interceptor next called more than once")

func (r *Registry) ConfigureAuthorization(config AuthorizationConfig) error {
	if config.Mode == "" {
		config.Mode = AuthorizationAllowByDefault
	}
	if config.Mode != AuthorizationAllowByDefault && config.Mode != AuthorizationDenyByDefault {
		return fmt.Errorf("unknown authorization mode %q", config.Mode)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	r.authorization = config
	return nil
}

func (r *Registry) RegisterInterceptor(registration InterceptorRegistration) error {
	if err := validateInterceptorRegistration(registration); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return errors.New("runtime registry is frozen")
	}
	r.interceptors = append(r.interceptors, registeredInterceptor{InterceptorRegistration: registration})
	return nil
}

func validateInterceptorRegistration(registration InterceptorRegistration) error {
	if registration.Interceptor == nil {
		return errors.New("register nil interceptor")
	}
	switch registration.Level {
	case GlobalInterceptor:
		if registration.Operation != "" || registration.Owner != "" || registration.Member != "" {
			return errors.New("global interceptor cannot declare a selector")
		}
	case OperationInterceptor:
		if registration.Operation != protocol.Query && registration.Operation != protocol.Mutation && registration.Operation != protocol.Subscription {
			return errors.New("operation interceptor requires a valid operation kind")
		}
		if registration.Owner != "" || registration.Member != "" {
			return errors.New("operation interceptor cannot declare an owner or member")
		}
	case TypeInterceptor:
		if registration.Operation != "" || registration.Owner == "" || registration.Member != "" {
			return errors.New("type interceptor requires only an owner")
		}
	case FieldInterceptor:
		if registration.Operation != "" || registration.Owner == "" || registration.Member == "" {
			return errors.New("field interceptor requires only an owner and member")
		}
	case HandlerInterceptorLevel:
		if registration.Member == "" {
			return errors.New("handler interceptor requires a member")
		}
		if registration.Owner == "" {
			if registration.Operation != protocol.Query && registration.Operation != protocol.Mutation && registration.Operation != protocol.Subscription {
				return errors.New("root handler interceptor requires an operation kind")
			}
		} else if registration.Operation != "" {
			return errors.New("object handler interceptor cannot declare an operation kind")
		}
	default:
		return fmt.Errorf("unknown interceptor level %q", registration.Level)
	}
	return nil
}

func interceptorHasRegisteredTarget(registration registeredInterceptor, definitions map[string]Definition) bool {
	if registration.Level == GlobalInterceptor {
		return true
	}
	for _, definition := range definitions {
		descriptor := definition.descriptor
		switch registration.Level {
		case GlobalInterceptor:
			return true
		case OperationInterceptor:
			if descriptor.Scope == RootScope && descriptor.Kind == registration.Operation {
				return true
			}
		case TypeInterceptor:
			if descriptor.Scope == ObjectScope && descriptor.Owner == registration.Owner {
				return true
			}
		case FieldInterceptor:
			if descriptor.Scope == ObjectScope && descriptor.Member == FieldMember && descriptor.Owner == registration.Owner && descriptor.Name == registration.Member {
				return true
			}
		case HandlerInterceptorLevel:
			if registration.Owner == "" && descriptor.Scope == RootScope && descriptor.Kind == registration.Operation && descriptor.Name == registration.Member {
				return true
			}
			if registration.Owner != "" && descriptor.Scope == ObjectScope && descriptor.Owner == registration.Owner && descriptor.Name == registration.Member {
				return true
			}
		}
	}
	return false
}

func authorizePlannedNodes(config AuthorizationConfig, operation protocol.OperationKind, nodes []planNode) []ValidationIssue {
	if config.PlanningAuthorizer == nil {
		return nil
	}
	var issues []ValidationIssue
	var walk func([]planNode)
	walk = func(current []planNode) {
		for _, node := range current {
			if node.hasDefinition {
				decision, err := callPlanningAuthorizer(config.PlanningAuthorizer, PlanningAuthorizationRequest{
					Operation: operation, Descriptor: node.definition.descriptor,
					Path: slices.Clone(node.responsePath), Source: node.source,
				})
				if err != nil || !decision.Allowed {
					issues = append(issues, newValidationIssue("POLICY_DENIED", "SEC-120", "operation is not permitted", node.source))
				}
			}
			walk(node.children)
		}
	}
	walk(nodes)
	return issues
}

func (p *Plan) authorizeNode(ctx context.Context, node planNode, object any, path []any) *ExecutionError {
	_, failure := p.authorizeNodeDecision(ctx, node, object, path)
	return failure
}

func (p *Plan) authorizeNodeDecision(ctx context.Context, node planNode, object any, path []any) (AuthorizationDecision, *ExecutionError) {
	if err := ctx.Err(); err != nil {
		failure := nodeExecutionError(CodeCancelled, "request cancelled", node, path, err)
		return AuthorizationDecision{}, &failure
	}
	allowed := p.authorization.Mode != AuthorizationDenyByDefault
	decision := AuthorizationDecision{Allowed: allowed, CacheScope: AuthorizationCacheNoStore}
	if p.authorization.Authorizer != nil {
		var err error
		policyObject, copyErr := isolateAuthorizationObject(object, p.resourceLimits)
		if copyErr != nil {
			code, message := CodeInternal, "internal execution error"
			if errors.Is(copyErr, errResourceBudget) {
				code, message = CodeResourceExhausted, "authorization object budget exhausted"
			}
			failure := nodeExecutionError(code, message, node, path, copyErr)
			return AuthorizationDecision{}, &failure
		}
		principal, _ := PrincipalFromContext(ctx)
		decision, err = callAuthorizer(ctx, p.authorization.Authorizer, AuthorizationRequest{
			Principal: principal, Operation: p.kind, Descriptor: node.definition.descriptor,
			Path: slices.Clone(path), Object: policyObject,
		})
		if err != nil {
			return p.denyAuthorization(ctx, node, path, err)
		}
		if contextErr := ctx.Err(); contextErr != nil {
			failure := nodeExecutionError(CodeCancelled, "request cancelled", node, path, contextErr)
			return AuthorizationDecision{}, &failure
		}
		now, clockErr := authorizationNow(p.authorization)
		if clockErr != nil {
			return p.denyAuthorization(ctx, node, path, clockErr)
		}
		allowed = authorizationDecisionAllows(decision, principal, now)
	}
	if !allowed {
		return p.denyAuthorization(ctx, node, path, errors.New("authorization denied"))
	}
	return decision, nil
}

func (p *Plan) denyAuthorization(ctx context.Context, node planNode, path []any, cause error) (AuthorizationDecision, *ExecutionError) {
	failure := nodeExecutionError(CodeUnauthorized, "access denied", node, path, cause)
	if p.kind == protocol.Mutation {
		p.auditMutation(ctx, MutationAuditEvent{
			Stage: MutationDenied, Operation: p.operationName, Group: transactionGroupFromContext(ctx), Code: failure.Code,
		})
	}
	return AuthorizationDecision{}, &failure
}

func callPlanningAuthorizer(authorizer PlanningAuthorizer, request PlanningAuthorizationRequest) (decision AuthorizationDecision, err error) {
	containPanic(func() {
		decision, err = authorizer.AuthorizePlan(request)
	}, func() {
		decision = AuthorizationDecision{}
		err = errors.New("planning authorization callback panicked")
	})
	return decision, err
}

func callAuthorizer(ctx context.Context, authorizer Authorizer, request AuthorizationRequest) (decision AuthorizationDecision, err error) {
	containPanic(func() {
		decision, err = authorizer.Authorize(ctx, request)
	}, func() {
		decision = AuthorizationDecision{}
		err = errors.New("authorization callback panicked")
	})
	return decision, err
}

func authorizationNow(config AuthorizationConfig) (now time.Time, err error) {
	containPanic(func() {
		if config.Now == nil {
			now = time.Now()
			return
		}
		now = config.Now()
	}, func() {
		now = time.Time{}
		err = errors.New("authorization clock panicked")
	})
	return now, err
}

func containPanic(run, onPanic func()) {
	defer func() {
		if recover() != nil {
			onPanic()
		}
	}()
	run()
}

func observeSafely[Event any](observer func(Event), event Event) {
	if observer == nil {
		return
	}
	containPanic(func() { observer(event) }, func() {})
}

func authorizationDecisionAllows(decision AuthorizationDecision, principal Principal, now time.Time) bool {
	if !decision.Allowed {
		return false
	}
	if !decision.ExpiresAt.IsZero() && !now.Before(decision.ExpiresAt) {
		return false
	}
	if decision.AuthorizationRevision != "" && decision.AuthorizationRevision != principal.AuthorizationRevision {
		return false
	}
	switch decision.CacheScope {
	case "", AuthorizationCacheNoStore:
		return true
	case AuthorizationCachePrincipal:
		return principal.Subject != ""
	case AuthorizationCacheTenant:
		return principal.Tenant != ""
	default:
		return false
	}
}

func isolateAuthorizationObject(object any, limits ResourceLimits) (any, error) {
	if object == nil {
		return nil, nil
	}
	return copyOutput(reflect.ValueOf(object), 0, &copyState{active: make(map[copyReference]bool), limits: limits})
}

func (p *Plan) invokeHandler(ctx context.Context, node planNode, source, input any, path []any) (any, error) {
	principal, _ := PrincipalFromContext(ctx)
	invocation := HandlerInvocation{
		Principal: principal, Operation: p.kind, Descriptor: node.definition.descriptor, Path: slices.Clone(path),
	}
	chain := HandlerNext(func() (any, error) { return node.definition.call(ctx, source, input) })
	interceptors := p.interceptorsFor(node)
	for index := len(interceptors) - 1; index >= 0; index-- {
		interceptor := interceptors[index]
		downstream := chain
		chain = func() (any, error) {
			var called atomic.Bool
			next := func() (any, error) {
				if !called.CompareAndSwap(false, true) {
					return nil, ErrInterceptorNextCalled
				}
				return downstream()
			}
			return callInterceptor(ctx, interceptor, invocation, next)
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

func callInterceptor(ctx context.Context, interceptor HandlerInterceptor, invocation HandlerInvocation, next HandlerNext) (output any, err error) {
	containPanic(func() {
		output, err = interceptor.Intercept(ctx, invocation, next)
	}, func() {
		output = nil
		err = &handlerPanic{}
	})
	return output, err
}

func (p *Plan) interceptorsFor(node planNode) []HandlerInterceptor {
	levels := [...]InterceptorLevel{GlobalInterceptor, OperationInterceptor, TypeInterceptor, FieldInterceptor, HandlerInterceptorLevel}
	var result []HandlerInterceptor
	for _, level := range levels {
		for _, registration := range p.interceptors {
			if registration.Level == level && p.interceptorMatches(registration, node) {
				result = append(result, registration.Interceptor)
			}
		}
	}
	return result
}

func (p *Plan) interceptorMatches(registration registeredInterceptor, node planNode) bool {
	descriptor := node.definition.descriptor
	switch registration.Level {
	case GlobalInterceptor:
		return true
	case OperationInterceptor:
		return registration.Operation == p.kind
	case TypeInterceptor:
		return registration.Owner == descriptor.Owner
	case FieldInterceptor:
		return descriptor.Member == FieldMember && registration.Owner == descriptor.Owner && registration.Member == descriptor.Name
	case HandlerInterceptorLevel:
		if registration.Owner == "" {
			return descriptor.Scope == RootScope && registration.Operation == p.kind && registration.Member == descriptor.Name
		}
		return descriptor.Scope == ObjectScope && registration.Owner == descriptor.Owner && registration.Member == descriptor.Name
	default:
		return false
	}
}
