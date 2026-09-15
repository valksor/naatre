package mcpadapter

import (
	"errors"
	"math"
	"sync"
)

type SessionID string
type MCPRequestID string
type ProgressToken string
type ResourceURI string
type NaatreRequestID string
type OperationID string
type IdempotencyID string

type RequestIDs struct {
	MCP           MCPRequestID
	Progress      ProgressToken
	NaatreRequest NaatreRequestID
	Operation     OperationID
	Idempotency   IdempotencyID
}

type SessionConfig struct {
	ID                SessionID
	Principal         string
	Tenant            string
	MaxProgressEvents int
	MaxResponseBytes  int
}

type Session struct {
	mu                sync.Mutex
	id                SessionID
	principal         string
	tenant            string
	maxProgressEvents int
	maxResponseBytes  int
	progress          map[ProgressToken]*RequestState
}

type RequestState struct {
	mu               sync.Mutex
	session          *Session
	ids              RequestIDs
	maxResponseBytes int
	progressCount    int
	lastProgress     float64
	cache            map[string]any
	loaders          map[string]any
	finalized        bool
	outcome          Outcome
}

func NewSession(config SessionConfig) (*Session, error) {
	if !boundedIdentity(string(config.ID)) || !boundedIdentity(config.Principal) || !boundedIdentity(config.Tenant) || config.MaxProgressEvents < 1 || config.MaxProgressEvents > 1<<20 || config.MaxResponseBytes < 1 {
		return nil, adapterError("MCP_SESSION_INVALID", errors.New("session identity and limits must be explicit and bounded"))
	}
	return &Session{id: config.ID, principal: config.Principal, tenant: config.Tenant, maxProgressEvents: config.MaxProgressEvents, maxResponseBytes: config.MaxResponseBytes, progress: make(map[ProgressToken]*RequestState)}, nil
}

func (s *Session) ID() SessionID     { return s.id }
func (s *Session) Principal() string { return s.principal }
func (s *Session) Tenant() string    { return s.tenant }

func (s *Session) Begin(ids RequestIDs) (*RequestState, error) {
	if s == nil || !validRequestIDs(ids) {
		return nil, adapterError("MCP_REQUEST_ID_INVALID", errors.New("request identifiers must be distinct, non-empty, and bounded"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progress[ids.Progress] != nil {
		return nil, adapterError("MCP_PROGRESS_TOKEN_REUSED", errors.New("progress token is already bound in this session"))
	}
	state := &RequestState{session: s, ids: ids, maxResponseBytes: s.maxResponseBytes, cache: make(map[string]any), loaders: make(map[string]any)}
	s.progress[ids.Progress] = state
	return state, nil
}

func (s *Session) RecordProgress(token ProgressToken, value float64, _ string) error {
	if s == nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return adapterError("MCP_PROGRESS_INVALID", errors.New("progress value must be finite and non-negative"))
	}
	s.mu.Lock()
	state := s.progress[token]
	s.mu.Unlock()
	if state == nil {
		return adapterError("MCP_PROGRESS_TOKEN_UNKNOWN", errors.New("progress token is not bound to this session"))
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.finalized {
		return adapterError("MCP_REQUEST_FINALIZED", errors.New("request is already finalized"))
	}
	if state.progressCount >= s.maxProgressEvents {
		return adapterError("MCP_PROGRESS_LIMIT", errors.New("progress event limit exceeded"))
	}
	if state.progressCount > 0 && value < state.lastProgress {
		return adapterError("MCP_PROGRESS_REGRESSION", errors.New("progress value cannot decrease"))
	}
	state.progressCount++
	state.lastProgress = value
	return nil
}

func (s *Session) ProgressCount(token ProgressToken) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	state := s.progress[token]
	s.mu.Unlock()
	if state == nil {
		return 0
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.progressCount
}

func (r *RequestState) PutCache(key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finalized {
		r.cache[key] = value
	}
}

func (r *RequestState) Cache(key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return nil, false
	}
	value, ok := r.cache[key]
	return value, ok
}

func (r *RequestState) PutLoader(key string, value any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finalized {
		r.loaders[key] = value
	}
}

func (r *RequestState) Loader(key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return nil, false
	}
	value, ok := r.loaders[key]
	return value, ok
}

type Cause string

const (
	Completed    Cause = "completed"
	Cancelled    Cause = "cancellation"
	Disconnected Cause = "disconnect"
	TimedOut     Cause = "timeout"
	ProcessDied  Cause = "process-death"
)

type Outcome struct {
	Complete bool   `json:"complete"`
	Code     string `json:"code"`
	Retry    bool   `json:"retry"`
}

// Finalize computes the stable terminal outcome, releases the session token
// binding, and clears all request-local state as one session-owned operation.
func (s *Session) Finalize(r *RequestState, cause Cause, responseBytes int) Outcome {
	if s == nil || r == nil {
		return Outcome{Code: "MCP_REQUEST_UNKNOWN"}
	}
	s.mu.Lock()
	r.mu.Lock()
	defer r.mu.Unlock()
	defer s.mu.Unlock()
	if r.session != s {
		return Outcome{Code: "MCP_REQUEST_UNKNOWN"}
	}
	if r.finalized {
		return r.outcome
	}
	if s.progress[r.ids.Progress] != r {
		return Outcome{Code: "MCP_REQUEST_UNKNOWN"}
	}
	r.outcome = lifecycleOutcome(r.maxResponseBytes, cause, responseBytes)
	r.finalized = true
	delete(s.progress, r.ids.Progress)
	r.ids = RequestIDs{}
	r.progressCount = 0
	r.lastProgress = 0
	clear(r.cache)
	clear(r.loaders)
	r.cache = nil
	r.loaders = nil
	return r.outcome
}

func lifecycleOutcome(maxResponseBytes int, cause Cause, responseBytes int) Outcome {
	if responseBytes < 0 || responseBytes > maxResponseBytes {
		return Outcome{Code: "RESPONSE_TOO_LARGE"}
	}
	switch cause {
	case Completed:
		return Outcome{Complete: true}
	case Cancelled:
		return Outcome{Code: "CANCELLED"}
	case Disconnected:
		return Outcome{Code: "MCP_DISCONNECTED"}
	case TimedOut:
		return Outcome{Code: "DEADLINE_EXCEEDED"}
	case ProcessDied:
		return Outcome{Code: "MCP_PROCESS_DIED"}
	default:
		return Outcome{Code: "MCP_LIFECYCLE_INVALID"}
	}
}

func validRequestIDs(ids RequestIDs) bool {
	values := []string{string(ids.MCP), string(ids.Progress), string(ids.NaatreRequest), string(ids.Operation), string(ids.Idempotency)}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !boundedIdentity(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func boundedIdentity(value string) bool { return len(value) > 0 && len(value) <= 128 }
