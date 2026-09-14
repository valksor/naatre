package http

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	nethttp "net/http"
	"strings"

	"github.com/valksor/naatre/schema"
)

const (
	// DiscoveryProfile identifies the authenticated schema discovery HTTP slice.
	DiscoveryProfile = "schema.discovery.http-1"
	// DiscoveryPath returns the current authorized schema document.
	DiscoveryPath = "/v1/schema"
	// DiscoveryDiffPath returns an authorized migration report from a retained revision.
	DiscoveryDiffPath = "/v1/schema/diff"

	DiscoverySchemaMediaType    = "application/vnd.naatre.schema+json;version=1"
	DiscoveryMigrationMediaType = "application/vnd.naatre.schema-migration+json;version=1"
)

// Stable public failure codes. Error causes, credentials, identities, and
// authorization metadata are deliberately absent from the wire problem.
const (
	CodeDiscoveryDisabled         = "DISCOVERY_DISABLED"
	CodeDiscoveryNotFound         = "DISCOVERY_NOT_FOUND"
	CodeDiscoveryMethod           = "DISCOVERY_METHOD_NOT_ALLOWED"
	CodeDiscoveryBadRequest       = "DISCOVERY_BAD_REQUEST"
	CodeDiscoveryUnauthenticated  = "DISCOVERY_UNAUTHENTICATED"
	CodeDiscoveryForbidden        = "DISCOVERY_FORBIDDEN"
	CodeDiscoveryRevisionMissing  = "DISCOVERY_REVISION_NOT_FOUND"
	CodeDiscoveryCancelled        = "DISCOVERY_CANCELLED"
	CodeDiscoveryBusy             = "DISCOVERY_BUSY"
	CodeDiscoveryLimit            = "DISCOVERY_LIMIT_EXCEEDED"
	CodeDiscoveryRevisionConflict = "DISCOVERY_REVISION_CONFLICT"
	CodeDiscoveryInternal         = "DISCOVERY_INTERNAL"
)

var (
	// ErrDiscoveryUnauthenticated lets an authenticator deny a request without
	// exposing its private cause.
	ErrDiscoveryUnauthenticated = &DiscoveryError{code: CodeDiscoveryUnauthenticated, message: "discovery authentication failed"}
	// ErrDiscoveryForbidden lets an authorizer deny a principal without
	// exposing policy metadata.
	ErrDiscoveryForbidden = &DiscoveryError{code: CodeDiscoveryForbidden, message: "discovery authorization failed"}
)

// DiscoveryError is a safe in-process failure with the same stable public code
// used by the HTTP problem response. Its message contains no private cause.
type DiscoveryError struct {
	code    string
	message string
}

func (e *DiscoveryError) Error() string { return e.message }

// Code returns the immutable public failure code.
func (e *DiscoveryError) Code() string { return e.code }

// DiscoveryIdentity contains opaque deployment-owned authentication results.
// The handler passes them only to the authorizer; it never emits or stores
// them.
type DiscoveryIdentity struct {
	Tenant    string
	Principal string
}

// DiscoveryDecision is one deny-by-default visibility decision for one target
// revision. PolicyRevision must identify every deployment policy dimension
// that can alter Visibility.
type DiscoveryDecision struct {
	Visibility     schema.Visibility
	PolicyRevision string
}

// DiscoveryAuthenticator authenticates one request. It must return
// ErrDiscoveryUnauthenticated for rejected credentials and must not attach
// credentials to returned errors.
type DiscoveryAuthenticator func(context.Context, *nethttp.Request) (DiscoveryIdentity, error)

// DiscoveryAuthorizer makes one decision for the immutable target revision.
// The same decision is applied to both sides of a migration report. Returned
// visibility maps transfer ownership to the handler and must not be mutated
// after the callback returns; the handler snapshots them before use.
type DiscoveryAuthorizer func(context.Context, DiscoveryIdentity, string) (DiscoveryDecision, error)

// DiscoverySource supplies immutable current and retained schema revisions.
// Current must select one revision atomically. Lookup must never fetch a
// revision from an untrusted request URI or remote endpoint.
type DiscoverySource interface {
	Current(context.Context) (schema.Document, error)
	Lookup(context.Context, string) (schema.Document, bool, error)
}

// DiscoveryLimits bounds request concurrency and response construction.
// Zero fields receive the finite defaults returned by DefaultDiscoveryLimits.
type DiscoveryLimits struct {
	MaxConcurrent       int
	MaxRevisionBytes    int
	MaxDocumentBytes    int
	MaxResponseBytes    int
	MaxMigrationChanges int
}

// DefaultDiscoveryLimits returns conservative finite transport limits.
func DefaultDiscoveryLimits() DiscoveryLimits {
	return DiscoveryLimits{
		MaxConcurrent:       32,
		MaxRevisionBytes:    128,
		MaxDocumentBytes:    1 << 20,
		MaxResponseBytes:    1 << 20,
		MaxMigrationChanges: 4096,
	}
}

// DiscoveryConfig is opt-in. A zero configuration returns a disabled handler
// and cannot invoke authentication, authorization, or source callbacks.
type DiscoveryConfig struct {
	Enabled      bool
	Source       DiscoverySource
	Authenticate DiscoveryAuthenticator
	Authorize    DiscoveryAuthorizer
	// AuthenticationChallenge is the safe deployment-owned value returned in
	// WWW-Authenticate when Authenticate rejects credentials.
	AuthenticationChallenge string
	Limits                  DiscoveryLimits
}

// MigrationReport is a filtered compatibility report. Diff remains the
// core.schema-1 authority; this envelope only identifies the transport profile.
type MigrationReport struct {
	Profile string            `json:"profile"`
	Diff    schema.SchemaDiff `json:"diff"`
}

type discoveryHandler struct {
	config  DiscoveryConfig
	limits  DiscoveryLimits
	active  chan struct{}
	etagKey [sha256.Size]byte
}

type discoveryProblem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
}

type discoveryResult struct {
	status    int
	code      string
	title     string
	body      []byte
	mediaType string
	etag      string
}

type authorizedDiscovery struct {
	revision string
	filtered schema.Document
	identity DiscoveryIdentity
	decision DiscoveryDecision
}

// NewDiscoveryHandler constructs an authenticated discovery handler. Merely
// constructing or importing it never creates a listener or network binding.
func NewDiscoveryHandler(config DiscoveryConfig) (nethttp.Handler, error) {
	if !config.Enabled {
		return disabledDiscoveryHandler{}, nil
	}
	if config.Source == nil || config.Authenticate == nil || config.Authorize == nil || !validAuthenticationChallenge(config.AuthenticationChallenge) {
		return nil, errors.New("enabled discovery requires source, authenticator, authorizer, and authentication challenge")
	}
	limits, err := resolveDiscoveryLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	var etagKey [sha256.Size]byte
	if _, err := rand.Read(etagKey[:]); err != nil {
		return nil, errors.New("initialize discovery cache identity")
	}
	return &discoveryHandler{config: config, limits: limits, active: make(chan struct{}, limits.MaxConcurrent), etagKey: etagKey}, nil
}

type disabledDiscoveryHandler struct{}

func (disabledDiscoveryHandler) ServeHTTP(response nethttp.ResponseWriter, _ *nethttp.Request) {
	writeDiscoveryProblem(response, nethttp.StatusNotFound, CodeDiscoveryDisabled, "schema discovery is disabled")
}

func resolveDiscoveryLimits(input DiscoveryLimits) (DiscoveryLimits, error) {
	defaults := DefaultDiscoveryLimits()
	if input.MaxConcurrent == 0 {
		input.MaxConcurrent = defaults.MaxConcurrent
	}
	if input.MaxRevisionBytes == 0 {
		input.MaxRevisionBytes = defaults.MaxRevisionBytes
	}
	if input.MaxDocumentBytes == 0 {
		input.MaxDocumentBytes = defaults.MaxDocumentBytes
	}
	if input.MaxResponseBytes == 0 {
		input.MaxResponseBytes = defaults.MaxResponseBytes
	}
	if input.MaxMigrationChanges == 0 {
		input.MaxMigrationChanges = defaults.MaxMigrationChanges
	}
	if input.MaxConcurrent < 1 || input.MaxRevisionBytes < 1 || input.MaxRevisionBytes > defaults.MaxRevisionBytes || input.MaxDocumentBytes < 1 || input.MaxResponseBytes < 1 || input.MaxMigrationChanges < 1 {
		return DiscoveryLimits{}, errors.New("discovery limits are invalid")
	}
	return input, nil
}

func (h *discoveryHandler) ServeHTTP(response nethttp.ResponseWriter, request *nethttp.Request) {
	wrote := false
	defer func() {
		if recover() != nil && !wrote {
			writeDiscoveryProblem(response, nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
		}
	}()
	result := h.serve(request)
	wrote = true
	if result.code != "" {
		if result.status == nethttp.StatusUnauthorized {
			response.Header().Set("WWW-Authenticate", h.config.AuthenticationChallenge)
		}
		writeDiscoveryProblem(response, result.status, result.code, result.title)
		return
	}
	writeDiscoveryResponse(response, result.status, result.mediaType, result.etag, result.body)
}

func (h *discoveryHandler) serve(request *nethttp.Request) discoveryResult {
	if result, admitted := h.admit(request.Context()); !admitted {
		return result
	}
	defer func() { <-h.active }()
	if result, valid := validateDiscoveryRoute(request); !valid {
		return result
	}
	identity, err := h.config.Authenticate(request.Context(), request)
	if err != nil {
		return discoveryAuthenticationFailure(err)
	}
	if identity.Tenant == "" || identity.Principal == "" {
		return failure(nethttp.StatusUnauthorized, CodeDiscoveryUnauthenticated, "authentication is required")
	}
	authorized, result, ok := h.authorizedSnapshot(request.Context(), identity)
	if !ok {
		return result
	}
	if request.URL.Path == DiscoveryPath {
		return h.schemaResponse(request, authorized)
	}
	return h.migrationResponse(request, authorized)
}

func (h *discoveryHandler) admit(ctx context.Context) (discoveryResult, bool) {
	if err := ctx.Err(); err != nil {
		return discoveryFailure(err), false
	}
	select {
	case h.active <- struct{}{}:
		return discoveryResult{}, true
	default:
		return failure(nethttp.StatusServiceUnavailable, CodeDiscoveryBusy, "schema discovery is busy"), false
	}
}

func validateDiscoveryRoute(request *nethttp.Request) (discoveryResult, bool) {
	if request.URL.Path != DiscoveryPath && request.URL.Path != DiscoveryDiffPath {
		return failure(nethttp.StatusNotFound, CodeDiscoveryNotFound, "discovery resource was not found"), false
	}
	if request.Method != nethttp.MethodGet {
		return failure(nethttp.StatusMethodNotAllowed, CodeDiscoveryMethod, "method is not allowed"), false
	}
	return discoveryResult{}, true
}

func discoveryAuthenticationFailure(err error) discoveryResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return discoveryFailure(err)
	}
	if errors.Is(err, ErrDiscoveryUnauthenticated) {
		return failure(nethttp.StatusUnauthorized, CodeDiscoveryUnauthenticated, "authentication is required")
	}
	return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
}

func (h *discoveryHandler) authorizedSnapshot(ctx context.Context, identity DiscoveryIdentity) (authorizedDiscovery, discoveryResult, bool) {
	current, err := h.config.Source.Current(ctx)
	if err != nil {
		return authorizedDiscovery{}, discoveryFailure(err), false
	}
	if err := ctx.Err(); err != nil {
		return authorizedDiscovery{}, discoveryFailure(err), false
	}
	revision := current.Revision()
	if !validDiscoveryRevision(revision, h.limits.MaxRevisionBytes) {
		return authorizedDiscovery{}, failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed"), false
	}
	decision, err := h.config.Authorize(ctx, identity, revision)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return authorizedDiscovery{}, discoveryFailure(err), false
		}
		if errors.Is(err, ErrDiscoveryForbidden) {
			return authorizedDiscovery{}, failure(nethttp.StatusForbidden, CodeDiscoveryForbidden, "schema discovery is forbidden"), false
		}
		return authorizedDiscovery{}, failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed"), false
	}
	if decision.PolicyRevision == "" {
		return authorizedDiscovery{}, failure(nethttp.StatusForbidden, CodeDiscoveryForbidden, "schema discovery is forbidden"), false
	}
	decision.Visibility = cloneDiscoveryVisibility(decision.Visibility)
	if err := ctx.Err(); err != nil {
		return authorizedDiscovery{}, discoveryFailure(err), false
	}
	if result, ok := h.documentWithinLimit(current); !ok {
		return authorizedDiscovery{}, result, false
	}
	filteredCurrent, err := current.Filter(decision.Visibility)
	if err != nil {
		return authorizedDiscovery{}, failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed"), false
	}
	if err := ctx.Err(); err != nil {
		return authorizedDiscovery{}, discoveryFailure(err), false
	}
	return authorizedDiscovery{revision: revision, filtered: filteredCurrent, identity: identity, decision: decision}, discoveryResult{}, true
}

func (h *discoveryHandler) schemaResponse(request *nethttp.Request, authorized authorizedDiscovery) discoveryResult {
	if request.URL.RawQuery != "" {
		return failure(nethttp.StatusBadRequest, CodeDiscoveryBadRequest, "invalid discovery request")
	}
	body, err := authorized.filtered.CanonicalJSON()
	if err != nil {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
	}
	if err := request.Context().Err(); err != nil {
		return discoveryFailure(err)
	}
	return h.success(body, DiscoverySchemaMediaType, authorized)
}

func (h *discoveryHandler) migrationResponse(request *nethttp.Request, authorized authorizedDiscovery) discoveryResult {
	from, ok := h.diffRevision(request)
	if !ok {
		return failure(nethttp.StatusBadRequest, CodeDiscoveryBadRequest, "invalid discovery request")
	}
	before, found, err := h.config.Source.Lookup(request.Context(), from)
	if err != nil {
		return discoveryFailure(err)
	}
	if err := request.Context().Err(); err != nil {
		return discoveryFailure(err)
	}
	if !found {
		return failure(nethttp.StatusNotFound, CodeDiscoveryRevisionMissing, "schema revision was not found")
	}
	if result, ok := h.documentWithinLimit(before); !ok {
		return result
	}
	if before.Revision() != from {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
	}
	filteredBefore, err := before.Filter(authorized.decision.Visibility)
	if err != nil {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
	}
	if err := request.Context().Err(); err != nil {
		return discoveryFailure(err)
	}
	diff := schema.DiffDocuments(filteredBefore, authorized.filtered)
	if err := request.Context().Err(); err != nil {
		return discoveryFailure(err)
	}
	if len(diff.Changes) > h.limits.MaxMigrationChanges {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryLimit, "schema discovery limit exceeded")
	}
	body, err := json.Marshal(MigrationReport{Profile: DiscoveryProfile, Diff: diff})
	if err != nil {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
	}
	return h.success(body, DiscoveryMigrationMediaType, authorized)
}

func (h *discoveryHandler) diffRevision(request *nethttp.Request) (string, bool) {
	query := request.URL.Query()
	values, found := query["from"]
	if !found || len(values) != 1 || len(query) != 1 || !validDiscoveryRevision(values[0], h.limits.MaxRevisionBytes) {
		return "", false
	}
	return values[0], true
}

func (h *discoveryHandler) success(body []byte, mediaType string, authorized authorizedDiscovery) discoveryResult {
	if len(body) > h.limits.MaxResponseBytes {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryLimit, "schema discovery limit exceeded")
	}
	etag := discoveryETag(body, authorized, h.etagKey)
	return discoveryResult{status: nethttp.StatusOK, body: body, mediaType: mediaType, etag: etag}
}

func (h *discoveryHandler) documentWithinLimit(document schema.Document) (discoveryResult, bool) {
	size, err := document.CanonicalJSONSize()
	if err != nil {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed"), false
	}
	if size > h.limits.MaxDocumentBytes {
		return failure(nethttp.StatusInternalServerError, CodeDiscoveryLimit, "schema discovery limit exceeded"), false
	}
	return discoveryResult{}, true
}

func discoveryFailure(err error) discoveryResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failure(nethttp.StatusRequestTimeout, CodeDiscoveryCancelled, "schema discovery was cancelled")
	}
	return failure(nethttp.StatusInternalServerError, CodeDiscoveryInternal, "schema discovery failed")
}

func failure(status int, code, title string) discoveryResult {
	return discoveryResult{status: status, code: code, title: title}
}

func validDiscoveryRevision(value string, maximum int) bool {
	if len(value) == 0 || len(value) > maximum || !isDiscoveryIdentifierStart(value[0]) {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !isDiscoveryIdentifierPart(value[index]) {
			return false
		}
	}
	return true
}

func isDiscoveryIdentifierStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isDiscoveryIdentifierPart(value byte) bool {
	return isDiscoveryIdentifierStart(value) || value >= '0' && value <= '9' || value == '.' || value == '-'
}

func validAuthenticationChallenge(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	scheme := value
	if separator := strings.IndexByte(value, ' '); separator >= 0 {
		scheme = value[:separator]
		if strings.TrimSpace(value[separator+1:]) == "" {
			return false
		}
	}
	if scheme == "" {
		return false
	}
	for index := range len(scheme) {
		if !isHTTPTokenByte(scheme[index]) {
			return false
		}
	}
	return true
}

func isHTTPTokenByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func cloneDiscoveryVisibility(input schema.Visibility) schema.Visibility {
	return schema.Visibility{
		Types:            maps.Clone(input.Types),
		Fields:           maps.Clone(input.Fields),
		EnumValues:       maps.Clone(input.EnumValues),
		Variants:         maps.Clone(input.Variants),
		Operations:       maps.Clone(input.Operations),
		Members:          maps.Clone(input.Members),
		Directives:       maps.Clone(input.Directives),
		Extensions:       maps.Clone(input.Extensions),
		CollectionFields: maps.Clone(input.CollectionFields),
		Retired:          maps.Clone(input.Retired),
	}
}

func discoveryETag(body []byte, authorized authorizedDiscovery, key [sha256.Size]byte) string {
	digest := hmac.New(sha256.New, key[:])
	for _, value := range []string{
		DiscoveryProfile,
		authorized.revision,
		authorized.identity.Tenant,
		authorized.identity.Principal,
		authorized.decision.PolicyRevision,
	} {
		writeDiscoveryETagComponent(digest, []byte(value))
	}
	writeDiscoveryETagComponent(digest, body)
	return `"` + hex.EncodeToString(digest.Sum(nil)) + `"`
}

func writeDiscoveryETagComponent(digest interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

func writeDiscoveryResponse(response nethttp.ResponseWriter, status int, mediaType, etag string, body []byte) {
	setDiscoveryHeaders(response.Header())
	response.Header().Set("Content-Type", mediaType)
	response.Header().Set("ETag", etag)
	response.WriteHeader(status)
	_, _ = response.Write(body)
}

func writeDiscoveryProblem(response nethttp.ResponseWriter, status int, code, title string) {
	setDiscoveryHeaders(response.Header())
	response.Header().Set("Content-Type", "application/problem+json")
	if status == nethttp.StatusMethodNotAllowed {
		response.Header().Set("Allow", nethttp.MethodGet)
	}
	problem := discoveryProblem{
		Type:  "urn:naatre:problem:" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		Title: title, Status: status, Code: code,
	}
	body, err := json.Marshal(problem)
	if err != nil {
		body = []byte(`{"type":"urn:naatre:problem:discovery-internal","title":"schema discovery failed","status":500,"code":"DISCOVERY_INTERNAL"}`)
		status = nethttp.StatusInternalServerError
	}
	response.WriteHeader(status)
	_, _ = response.Write(body)
}

func setDiscoveryHeaders(header nethttp.Header) {
	header.Set("Cache-Control", "private, no-store")
	header.Set("Vary", "Authorization, Cookie")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}
