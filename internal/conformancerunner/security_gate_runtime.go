package conformancerunner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valksor/naatre/protocol"
	naatreruntime "github.com/valksor/naatre/runtime"
	"github.com/valksor/naatre/schema"
)

// The security gate drives the real runtime authorization and resource decision
// paths for every vector instead of re-deriving the fixture's own declared
// fields. A genuine regression in runtime/authorization.go (deny-by-default,
// expiry, or policy-revision enforcement) or runtime/resource.go (aggregate
// runtime-work budgeting) makes the observed outcome diverge from the fixture's
// expectation and fails release.stable-1.

const (
	securityGateTenant     = "tenant-a"
	securityGateRevision   = "policy-r1"
	securityGateStaleRev   = "policy-r0"
	securityGateWorkCost   = 1000
	securityGateAuthPolicy = "security-gate"
)

var securityGateClock = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// runSecurityGateVector executes one vector through the real runtime and reports
// how many protected handlers actually started and which stable public code the
// runtime produced. It never consults the vector's Expected outcome.
func runSecurityGateVector(vector securityGateVector) (starts int, code string, safe bool, err error) {
	types, err := schema.NewCatalog().Freeze()
	if err != nil {
		return 0, "", false, err
	}
	registry := naatreruntime.NewRegistry(types)
	var handlerStarts atomic.Int64
	descriptor := naatreruntime.Descriptor{
		Name: "work", Scope: naatreruntime.RootScope, Kind: protocol.Query, Member: naatreruntime.CallMember,
		Input: schema.TypeID(schema.String), Output: schema.TypeID(schema.String),
		Metadata: naatreruntime.Metadata{
			Effect: naatreruntime.ReadEffect, Deterministic: true, Cacheable: false, RetrySafe: true,
			ThreadSafety: naatreruntime.ThreadSafe, Batching: naatreruntime.BatchIneligible,
			Transaction: naatreruntime.TransactionNone, AuthorizationPolicy: securityGateAuthPolicy,
			Cost: securityGateWorkCost,
		},
	}
	if regErr := registry.Register(naatreruntime.BindInvocation[string](descriptor, func(context.Context, naatreruntime.Invocation) (string, error) {
		handlerStarts.Add(1)
		return "ok", nil
	})); regErr != nil {
		return 0, "", false, regErr
	}
	if cfgErr := registry.ConfigureAuthorization(naatreruntime.AuthorizationConfig{
		Mode:       naatreruntime.AuthorizationDenyByDefault,
		Now:        func() time.Time { return securityGateClock },
		Authorizer: securityGateAuthorizer(vector),
	}); cfgErr != nil {
		return 0, "", false, cfgErr
	}
	snapshot, err := registry.Freeze()
	if err != nil {
		return 0, "", false, err
	}

	branches := 1
	if vector.Budget.Attempts > vector.Budget.Limit {
		branches = vector.Budget.Attempts
	}
	request, err := protocol.DecodeRequest(securityGateDocument(branches), protocol.DecodeOptions{})
	if err != nil {
		return 0, "", false, err
	}
	options := naatreruntime.PrepareOptions{}
	if vector.Budget.Attempts > vector.Budget.Limit {
		// The real runtime-work meter must admit exactly Limit protected starts
		// and reject the next one; the +500 buffer covers the plan's fixed node
		// work, which is far below one handler's cost.
		options.Limits = naatreruntime.ResourceLimits{
			MaxRuntimeWork: uint64(vector.Budget.Limit)*securityGateWorkCost + 500,
			MaxConcurrency: 1,
		}
	}
	plan, err := naatreruntime.PrepareWithOptions(snapshot, request, options)
	if err != nil {
		return 0, "", false, err
	}

	ctx := naatreruntime.WithPrincipal(context.Background(), naatreruntime.Principal{
		Subject: "principal-7", Tenant: securityGateTenant, AuthorizationRevision: securityGateRevision,
	})
	if vector.Cancelled {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		ctx = cancelled
	}
	outcome := plan.Execute(ctx)
	return int(handlerStarts.Load()), securityGateOutcomeCode(outcome), securityGateOutcomeSafe(outcome), nil
}

// securityGateAuthorizer translates a vector's declared authorization and
// identity into a real AuthorizationDecision. Tenant and presence checks are
// policy decisions; expiry and policy-revision are left for the runtime itself
// to enforce so a regression in that enforcement is observable here.
func securityGateAuthorizer(vector securityGateVector) naatreruntime.Authorizer {
	return naatreruntime.AuthorizerFunc(func(context.Context, naatreruntime.AuthorizationRequest) (naatreruntime.AuthorizationDecision, error) {
		if !vector.Authorization.Present || !vector.Authorization.Allowed || !vector.Identity.TenantMatches {
			return naatreruntime.AuthorizationDecision{Allowed: false}, nil
		}
		decision := naatreruntime.AuthorizationDecision{Allowed: true, CacheScope: naatreruntime.AuthorizationCacheNoStore}
		if vector.Identity.Expired {
			decision.ExpiresAt = securityGateClock.Add(-time.Hour)
		}
		if vector.Identity.CurrentRevision && !vector.Identity.Revoked {
			decision.AuthorizationRevision = securityGateRevision
		} else {
			decision.AuthorizationRevision = securityGateStaleRev
		}
		return decision, nil
	})
}

func securityGateDocument(branches int) []byte {
	selections := make([]any, branches)
	for index := range selections {
		selections[index] = map[string]any{"$call": map[string]any{"name": "work", "as": fmt.Sprintf("b%d", index)}}
	}
	envelope, _ := json.Marshal(map[string]any{
		"version": "1",
		"document": map[string]any{"operations": []any{map[string]any{
			"name": "Q", "kind": "query",
			"select": []any{map[string]any{"$parallel": map[string]any{"select": selections}}},
		}}},
	})
	return envelope
}

func securityGateOutcomeCode(outcome naatreruntime.Outcome) string {
	for _, failure := range outcome.Errors {
		switch failure.Code {
		case naatreruntime.CodeResourceExhausted:
			return naatreruntime.CodeResourceExhausted
		case naatreruntime.CodeCancelled:
			return naatreruntime.CodeCancelled
		case naatreruntime.CodeUnauthorized:
			return naatreruntime.CodeUnauthorized
		}
	}
	if len(outcome.Errors) > 0 {
		return outcome.Errors[0].Code
	}
	return ""
}

// securityGateOutcomeSafe reports whether the runtime kept protected failure
// metadata out of the public envelope: every error must be generic with no
// structured details.
func securityGateOutcomeSafe(outcome naatreruntime.Outcome) bool {
	for _, failure := range outcome.Errors {
		if failure.Details != nil {
			return false
		}
	}
	return true
}

func securityGateCanonicalMessage(code string) string {
	switch code {
	case naatreruntime.CodeUnauthorized:
		return "request is not authorized"
	case naatreruntime.CodeCancelled:
		return "request cancelled"
	case naatreruntime.CodeResourceExhausted:
		return "resource budget exhausted"
	default:
		return ""
	}
}

// leaksProtectedIdentifier reports whether any protected identifier appears in
// the public code or message the runtime emitted.
func leaksProtectedIdentifier(profile securityGateSurface, vector securityGateVector, code, message string) bool {
	protected := append([]string(nil), profile.ProtectedMetadata...)
	protected = append(protected, vector.HiddenInputs...)
	for _, hidden := range protected {
		if hidden == "" {
			continue
		}
		if strings.Contains(code, hidden) || strings.Contains(message, hidden) {
			return true
		}
	}
	return false
}
