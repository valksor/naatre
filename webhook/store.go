// Package webhook provides the durable Go/SQLite delivery and receiver
// adapters for the transport-neutral contracts in package event.
package webhook

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/valksor/naatre/event"
	_ "modernc.org/sqlite"
)

const (
	// Profile identifies this implementation slice. core.events-1 remains the
	// normative envelope, signature, retry, and delivery-state authority.
	Profile = "events.webhook-adapters-go-1"

	CodeInvalidConfig     = "WEBHOOK_ADAPTER_INVALID_CONFIG"
	CodeInvalidRecord     = "WEBHOOK_ADAPTER_INVALID_RECORD"
	CodeStoreUnavailable  = "WEBHOOK_ADAPTER_STORE_UNAVAILABLE"
	CodeResourceExhausted = "WEBHOOK_ADAPTER_RESOURCE_EXHAUSTED"
	CodeFenceRejected     = "WEBHOOK_ADAPTER_FENCE_REJECTED"
	CodeCancelled         = "WEBHOOK_ADAPTER_CANCELLED"
	CodeEndpointRevoked   = "WEBHOOK_ENDPOINT_REVOKED"
	CodeKeyRevoked        = "WEBHOOK_KEY_REVOKED"
	CodeDNSRebinding      = "WEBHOOK_DNS_REBINDING"
	CodeRedirect          = "WEBHOOK_REDIRECT"
	CodeHTTPPermanent     = "WEBHOOK_HTTP_PERMANENT"
	CodeHTTPTransient     = "WEBHOOK_HTTP_TRANSIENT"
	CodeTransport         = "WEBHOOK_TRANSPORT_UNAVAILABLE"
	CodeRetryExhausted    = "WEBHOOK_RETRY_EXHAUSTED"
	CodeSignatureInvalid  = "WEBHOOK_SIGNATURE_INVALID"
	CodeStale             = "WEBHOOK_STALE"
	CodeReplayStore       = "WEBHOOK_REPLAY_STORE_UNAVAILABLE"
	CodeEventReordered    = "WEBHOOK_EVENT_REORDERED"
)

// Error is a bounded public failure. It deliberately retains no URL, address,
// body, payload reference, digest, signature, key, secret, SQL, or backend
// error.
type Error struct {
	Code    string
	Message string
	match   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func (e *Error) Is(target error) bool {
	if e == target || e.match == target {
		return true
	}
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// ErrorCode returns a stable public code, or an empty string when err did not
// originate at this adapter boundary.
func ErrorCode(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}

// Limits bounds retained state, scans, receiver allocations, and response
// consumption.
type Limits struct {
	MaxIdentifierBytes int
	MaxPayloadBytes    int
	MaxResponseBytes   int64
	MaxBatchEvents     int
	MaxDispatchBatch   int
	MaxRecoveryBatch   int
}

func DefaultLimits() Limits {
	return Limits{
		MaxIdentifierBytes: 128,
		MaxPayloadBytes:    event.DefaultMaximumBodyBytes,
		MaxResponseBytes:   64 * 1024,
		MaxBatchEvents:     100,
		MaxDispatchBatch:   100,
		MaxRecoveryBatch:   100,
	}
}

// StoreConfig owns durable lease, retry, retention, and resource policy.
type StoreConfig struct {
	Now           func() time.Time
	LeaseDuration time.Duration
	Retention     time.Duration
	Retry         event.RetryPolicy
	Limits        Limits
}

// Delivery is protected outbox state. Applications must authorize access to
// values returned by Load; routine observations use Observation instead.
type Delivery struct {
	Record          event.DeliveryRecord
	EventTypes      []string
	ContentType     string
	ContentEncoding string
	Body            []byte
	CreatedAt       time.Time
	NextAttempt     time.Time
	LeaseUntil      time.Time
	Fence           uint64
	HTTPStatus      int
}

// SQLiteStore is a durable outbox, lease, replay, and ordering store. It is
// suitable for atomic state-plus-event recording only when the application
// state and outbox use the same SQLite transaction.
type SQLiteStore struct {
	db     *sql.DB
	config StoreConfig
	owned  bool
}

func OpenSQLite(ctx context.Context, dataSourceName string, config StoreConfig) (*SQLiteStore, error) {
	if strings.TrimSpace(dataSourceName) == "" {
		return nil, publicError(CodeInvalidConfig, "SQLite data source is required", nil)
	}
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, storeFailure()
	}
	db.SetMaxOpenConns(1)
	store, err := NewSQLiteStore(ctx, db, config)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	store.owned = true
	return store, nil
}

// NewSQLiteStore initializes tables in an application-owned pool. Close does
// not close a pool supplied through this constructor.
func NewSQLiteStore(ctx context.Context, db *sql.DB, config StoreConfig) (*SQLiteStore, error) {
	if db == nil || config.Now == nil || config.LeaseDuration <= 0 || config.Retention <= 0 ||
		!validLimits(config.Limits) || !validRetryPolicy(config.Retry) {
		return nil, publicError(CodeInvalidConfig, "webhook store configuration is invalid", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	for _, statement := range sqliteSchema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, storeFailure()
		}
	}
	return &SQLiteStore{db: db, config: config}, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || !s.owned {
		return nil
	}
	s.owned = false
	if err := s.db.Close(); err != nil {
		return storeFailure()
	}
	return nil
}

// Enqueue records one delivery in its own transaction. created is false only
// for an exact idempotent duplicate.
func (s *SQLiteStore) Enqueue(ctx context.Context, delivery Delivery) (created bool, err error) {
	if s == nil {
		return false, publicError(CodeInvalidConfig, "webhook store is unavailable", nil)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, mapContextOrStore(ctx)
	}
	defer func() { _ = tx.Rollback() }()
	created, err = s.EnqueueTx(ctx, tx, delivery)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, mapContextOrStore(ctx)
	}
	return created, nil
}

// EnqueueTx atomically records exact delivery bytes inside the caller's
// SQLite business transaction. It never commits or rolls back that transaction.
func (s *SQLiteStore) EnqueueTx(ctx context.Context, tx *sql.Tx, delivery Delivery) (bool, error) {
	if s == nil || tx == nil {
		return false, publicError(CodeInvalidConfig, "webhook transaction is unavailable", nil)
	}
	prepared, err := s.prepareDelivery(delivery)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO naatre_webhook_outbox
		(delivery_id, tenant, endpoint_id, endpoint_revision, event_ids, event_types, payload_reference,
		 content_type, content_encoding, body, state, attempt, lease_owner, lease_until, lease_fence,
		 next_attempt, failure_code, http_status, created_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', NULL, 0, ?, '', 0, ?, NULL)`,
		prepared.Record.DeliveryID, prepared.Record.Tenant, prepared.Record.EndpointID, prepared.Record.EndpointRevision,
		prepared.eventIDsJSON, prepared.eventTypesJSON, prepared.Record.PayloadReference, prepared.ContentType,
		prepared.ContentEncoding, prepared.Body, string(event.DeliveryPending), prepared.NextAttempt.UnixNano(), prepared.CreatedAt.UnixNano())
	if err != nil {
		return false, mapContextOrStore(ctx)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, storeFailure()
	}
	if rows == 1 {
		return true, nil
	}
	existing, err := loadDeliveryRow(tx.QueryRowContext(ctx, selectDelivery+` WHERE delivery_id = ?`, prepared.Record.DeliveryID), s.config.Limits)
	if err != nil {
		return false, mapSQLFailure(ctx, err)
	}
	if !sameEnqueue(existing, delivery) {
		return false, publicError(CodeInvalidRecord, "delivery identifier conflicts with retained outbox state", nil)
	}
	return false, nil
}

// Claim leases the oldest due delivery using a monotonically increasing fence.
func (s *SQLiteStore) Claim(ctx context.Context, worker string) (Delivery, bool, error) {
	if s == nil || !validText(worker, s.config.Limits.MaxIdentifierBytes) {
		return Delivery{}, false, publicError(CodeInvalidRecord, "worker identity is invalid", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return Delivery{}, false, err
	}
	now := s.config.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, false, mapContextOrStore(ctx)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT delivery_id FROM naatre_webhook_outbox
		WHERE state = ? AND next_attempt <= ? ORDER BY next_attempt, created_at, delivery_id LIMIT 1`,
		string(event.DeliveryPending), now.UnixNano()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, nil
	}
	if err != nil {
		return Delivery{}, false, mapContextOrStore(ctx)
	}
	leaseUntil := now.Add(s.config.LeaseDuration)
	result, err := tx.ExecContext(ctx, `UPDATE naatre_webhook_outbox SET state = ?, attempt = attempt + 1,
		lease_owner = ?, lease_until = ?, lease_fence = lease_fence + 1, failure_code = '', http_status = 0
		WHERE delivery_id = ? AND state = ? AND next_attempt <= ?`, string(event.DeliveryInFlight), worker,
		leaseUntil.UnixNano(), id, string(event.DeliveryPending), now.UnixNano())
	if err != nil {
		return Delivery{}, false, mapContextOrStore(ctx)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return Delivery{}, false, storeFailure()
	}
	delivery, err := loadDeliveryRow(tx.QueryRowContext(ctx, selectDelivery+` WHERE delivery_id = ?`, id), s.config.Limits)
	if err != nil {
		return Delivery{}, false, mapSQLFailure(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return Delivery{}, false, mapContextOrStore(ctx)
	}
	return delivery, true, nil
}

// Succeed durably records a successful attempt when the private fence still
// owns the lease.
func (s *SQLiteStore) Succeed(ctx context.Context, deliveryID string, fence uint64, status int) error {
	return s.finish(ctx, deliveryID, fence, event.DeliverySucceeded, "", status, time.Time{})
}

// Fail records either a bounded retry or a recoverable dead letter. The last
// retryable attempt always uses CodeRetryExhausted as its durable public code.
func (s *SQLiteStore) Fail(ctx context.Context, delivery Delivery, code string, status int, retryable bool, retryAfter time.Duration) error {
	if !stableFailureCode(code) || retryAfter < 0 {
		return publicError(CodeInvalidRecord, "delivery outcome is invalid", nil)
	}
	if !retryable {
		return s.finish(ctx, delivery.Record.DeliveryID, delivery.Fence, event.DeliveryDeadLettered, code, status, time.Time{})
	}
	if delivery.Record.Attempt >= s.config.Retry.MaximumAttempts {
		return s.finish(ctx, delivery.Record.DeliveryID, delivery.Fence, event.DeliveryDeadLettered, CodeRetryExhausted, status, time.Time{})
	}
	delay, err := s.config.Retry.DelayBefore(delivery.Record.Attempt + 1)
	if err != nil {
		return publicError(CodeInvalidConfig, "webhook retry policy is invalid", nil)
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return s.finish(ctx, delivery.Record.DeliveryID, delivery.Fence, event.DeliveryPending, code, status, s.config.Now().UTC().Add(delay))
}

func (s *SQLiteStore) finish(ctx context.Context, id string, fence uint64, state event.DeliveryState, code string, status int, next time.Time) error {
	if s == nil || !validText(id, s.config.Limits.MaxIdentifierBytes) || fence == 0 || status < 0 || status > 999 {
		return publicError(CodeInvalidRecord, "delivery completion is invalid", nil)
	}
	completed := any(nil)
	if state == event.DeliverySucceeded || state == event.DeliveryDeadLettered {
		completed = s.config.Now().UTC().UnixNano()
	}
	nextValue := int64(0)
	if !next.IsZero() {
		nextValue = next.UTC().UnixNano()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE naatre_webhook_outbox SET state = ?, lease_owner = '', lease_until = NULL,
		next_attempt = ?, failure_code = ?, http_status = ?, completed_at = ?
		WHERE delivery_id = ? AND state = ? AND lease_fence = ?`, string(state), nextValue, code, status, completed,
		id, string(event.DeliveryInFlight), fence)
	if err != nil {
		return mapContextOrStore(ctx)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return storeFailure()
	}
	if rows != 1 {
		return publicError(CodeFenceRejected, "delivery lease is no longer owned", nil)
	}
	return nil
}

// RecoverExpired releases at most limit expired leases. Attempt identity and
// ownership remain unchanged; stale workers retain an unusable old fence.
func (s *SQLiteStore) RecoverExpired(ctx context.Context, limit int) (int, error) {
	if s == nil || limit < 1 || limit > s.config.Limits.MaxRecoveryBatch {
		return 0, publicError(CodeResourceExhausted, "lease recovery limit is outside the configured bound", nil)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE naatre_webhook_outbox SET state = ?, lease_owner = '', lease_until = NULL,
		next_attempt = ?, failure_code = ? WHERE delivery_id IN (
			SELECT delivery_id FROM naatre_webhook_outbox WHERE state = ? AND lease_until <= ?
			ORDER BY lease_until, delivery_id LIMIT ?
		)`, string(event.DeliveryPending), s.config.Now().UTC().UnixNano(), CodeTransport,
		string(event.DeliveryInFlight), s.config.Now().UTC().UnixNano(), limit)
	if err != nil {
		return 0, mapContextOrStore(ctx)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, storeFailure()
	}
	return int(rows), nil
}

// Load returns protected delivery state for an authorized application path.
func (s *SQLiteStore) Load(ctx context.Context, deliveryID string) (Delivery, error) {
	if s == nil || !validText(deliveryID, s.config.Limits.MaxIdentifierBytes) {
		return Delivery{}, publicError(CodeInvalidRecord, "delivery identity is invalid", nil)
	}
	delivery, err := loadDeliveryRow(s.db.QueryRowContext(ctx, selectDelivery+` WHERE delivery_id = ?`, deliveryID), s.config.Limits)
	if err != nil {
		return Delivery{}, mapSQLFailure(ctx, err)
	}
	return delivery, nil
}

// DeadLetters returns a bounded, payload-free operational view.
func (s *SQLiteStore) DeadLetters(ctx context.Context, limit int) ([]event.DeliveryObservation, error) {
	if s == nil || limit < 1 || limit > s.config.Limits.MaxDispatchBatch {
		return nil, publicError(CodeResourceExhausted, "dead-letter listing limit is outside the configured bound", nil)
	}
	rows, err := s.db.QueryContext(ctx, selectDelivery+` WHERE state = ? ORDER BY completed_at, delivery_id LIMIT ?`,
		string(event.DeliveryDeadLettered), limit)
	if err != nil {
		return nil, mapContextOrStore(ctx)
	}
	observations := make([]event.DeliveryObservation, 0, limit)
	for rows.Next() {
		delivery, err := loadDeliveryRow(rows, s.config.Limits)
		if err != nil {
			_ = rows.Close()
			return nil, storeFailure()
		}
		observations = append(observations, Observation(delivery))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, storeFailure()
	}
	if err := rows.Close(); err != nil {
		return nil, storeFailure()
	}
	return observations, nil
}

// DeleteExpired removes only terminal records, bounded by limit.
func (s *SQLiteStore) DeleteExpired(ctx context.Context, limit int) (int, error) {
	if s == nil || limit < 1 || limit > s.config.Limits.MaxRecoveryBatch {
		return 0, publicError(CodeResourceExhausted, "retention limit is outside the configured bound", nil)
	}
	before := s.config.Now().UTC().Add(-s.config.Retention).UnixNano()
	result, err := s.db.ExecContext(ctx, `DELETE FROM naatre_webhook_outbox WHERE delivery_id IN (
		SELECT delivery_id FROM naatre_webhook_outbox WHERE state IN (?, ?) AND completed_at <= ?
		ORDER BY completed_at, delivery_id LIMIT ?
	)`, string(event.DeliverySucceeded), string(event.DeliveryDeadLettered), before, limit)
	if err != nil {
		return 0, mapContextOrStore(ctx)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, storeFailure()
	}
	return int(rows), nil
}

// CheckAndStore implements event.ReplayStore durably and atomically.
func (s *SQLiteStore) CheckAndStore(audience, deliveryID string, now, expires time.Time) (bool, error) {
	if s == nil || !validText(audience, 256) || !validText(deliveryID, s.config.Limits.MaxIdentifierBytes) || now.IsZero() || !expires.After(now) {
		return false, event.ErrReplayStore
	}
	if _, err := s.db.Exec(`DELETE FROM naatre_webhook_replay WHERE rowid IN (
		SELECT rowid FROM naatre_webhook_replay WHERE expires_at <= ? ORDER BY expires_at LIMIT ?
	)`, now.UTC().UnixNano(), s.config.Limits.MaxRecoveryBatch); err != nil {
		return false, event.ErrReplayStore
	}
	result, err := s.db.Exec(`INSERT INTO naatre_webhook_replay (audience, delivery_id, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(audience, delivery_id) DO UPDATE SET expires_at = excluded.expires_at
		WHERE naatre_webhook_replay.expires_at <= ?`, audience, deliveryID, expires.UTC().UnixNano(), now.UTC().UnixNano())
	if err != nil {
		return false, event.ErrReplayStore
	}
	rows, err := result.RowsAffected()
	return rows == 0, err
}

type sequenceValue struct {
	Stream   string
	Sequence uint64
}

func (s *SQLiteStore) recordSequences(ctx context.Context, values []sequenceValue) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapContextOrStore(ctx)
	}
	defer func() { _ = tx.Rollback() }()
	pending := make(map[string]uint64, len(values))
	present := make(map[string]bool, len(values))
	for _, value := range values {
		last, ok := pending[value.Stream]
		if !ok {
			var stored string
			err := tx.QueryRowContext(ctx, `SELECT sequence FROM naatre_webhook_sequences WHERE stream = ?`, value.Stream).Scan(&stored)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return storeFailure()
			}
			if err == nil {
				parsed, parseErr := strconv.ParseUint(stored, 10, 64)
				if parseErr != nil {
					return storeFailure()
				}
				last = parsed
				present[value.Stream] = true
			}
		}
		if present[value.Stream] && value.Sequence <= last {
			return publicError(CodeEventReordered, "authenticated event sequence is not increasing", nil)
		}
		pending[value.Stream] = value.Sequence
		present[value.Stream] = true
	}
	for stream, sequence := range pending {
		if _, err := tx.ExecContext(ctx, `INSERT INTO naatre_webhook_sequences (stream, sequence) VALUES (?, ?)
			ON CONFLICT(stream) DO UPDATE SET sequence = excluded.sequence`, stream, strconv.FormatUint(sequence, 10)); err != nil {
			return storeFailure()
		}
	}
	if err := tx.Commit(); err != nil {
		return mapContextOrStore(ctx)
	}
	return nil
}

// Observation deliberately excludes protected delivery material.
func Observation(delivery Delivery) event.DeliveryObservation {
	return event.DeliveryObservation{
		TenantReference: delivery.Record.Tenant, EndpointReference: delivery.Record.EndpointID,
		DeliveryID: delivery.Record.DeliveryID, EventIDs: append([]string(nil), delivery.Record.EventIDs...),
		EventTypes: append([]string(nil), delivery.EventTypes...), Attempt: delivery.Record.Attempt,
		Outcome: string(delivery.Record.State), FailureCode: delivery.Record.FailureCode,
		HTTPStatus: delivery.HTTPStatus, NextAttempt: delivery.NextAttempt,
	}
}

type preparedDelivery struct {
	Delivery
	eventIDsJSON   []byte
	eventTypesJSON []byte
}

func (s *SQLiteStore) prepareDelivery(delivery Delivery) (preparedDelivery, error) {
	if delivery.Record.State != event.DeliveryPending || delivery.Record.Attempt != 0 || delivery.Record.LeaseOwner != "" ||
		delivery.Record.FailureCode != "" || delivery.Fence != 0 || delivery.HTTPStatus != 0 ||
		!validText(delivery.Record.Tenant, s.config.Limits.MaxIdentifierBytes) ||
		!validText(delivery.Record.EndpointID, s.config.Limits.MaxIdentifierBytes) ||
		!validText(delivery.Record.EndpointRevision, s.config.Limits.MaxIdentifierBytes) ||
		!validText(delivery.Record.DeliveryID, s.config.Limits.MaxIdentifierBytes) ||
		!validText(delivery.Record.PayloadReference, 512) || len(delivery.Record.EventIDs) == 0 ||
		len(delivery.Record.EventIDs) != len(delivery.EventTypes) || len(delivery.Record.EventIDs) > s.config.Limits.MaxBatchEvents ||
		len(delivery.Body) == 0 || len(delivery.Body) > s.config.Limits.MaxPayloadBytes {
		return preparedDelivery{}, publicError(CodeInvalidRecord, "webhook outbox record is invalid", nil)
	}
	if err := validatePayload(delivery, s.config.Limits); err != nil {
		return preparedDelivery{}, err
	}
	ids, err := json.Marshal(delivery.Record.EventIDs)
	if err != nil {
		return preparedDelivery{}, publicError(CodeInvalidRecord, "webhook event identities are invalid", nil)
	}
	types, err := json.Marshal(delivery.EventTypes)
	if err != nil {
		return preparedDelivery{}, publicError(CodeInvalidRecord, "webhook event types are invalid", nil)
	}
	prepared := preparedDelivery{Delivery: cloneDelivery(delivery), eventIDsJSON: ids, eventTypesJSON: types}
	prepared.CreatedAt = s.config.Now().UTC()
	prepared.NextAttempt = prepared.CreatedAt
	return prepared, nil
}

func validatePayload(delivery Delivery, limits Limits) error {
	decoded, err := decodeBody(delivery.Body, delivery.ContentEncoding, limits.MaxPayloadBytes)
	if err != nil {
		return err
	}
	var messages []json.RawMessage
	switch delivery.ContentType {
	case event.JSONContentType:
		messages = []json.RawMessage{decoded}
	case event.BatchJSONContentType:
		if err := json.Unmarshal(decoded, &messages); err != nil || len(messages) == 0 {
			return publicError(CodeInvalidRecord, "webhook batch is invalid", nil)
		}
	default:
		return publicError(CodeInvalidRecord, "webhook content type is unsupported", nil)
	}
	if len(messages) != len(delivery.Record.EventIDs) || len(messages) > limits.MaxBatchEvents {
		return publicError(CodeResourceExhausted, "webhook batch exceeds configured limits", nil)
	}
	for index, raw := range messages {
		var identity struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &identity); err != nil || identity.ID != delivery.Record.EventIDs[index] || identity.Type != delivery.EventTypes[index] ||
			!validText(identity.ID, limits.MaxIdentifierBytes) || !validText(identity.Type, limits.MaxIdentifierBytes) {
			return publicError(CodeInvalidRecord, "webhook payload identity does not match durable metadata", nil)
		}
	}
	return nil
}

func decodeBody(body []byte, encoding string, maximum int) ([]byte, error) {
	switch encoding {
	case event.IdentityEncoding:
		if len(body) > maximum {
			return nil, publicError(CodeResourceExhausted, "webhook payload exceeds configured limits", nil)
		}
		return append([]byte(nil), body...), nil
	case event.GZIPEncoding:
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, publicError(CodeInvalidRecord, "webhook content coding is invalid", nil)
		}
		decoded, readErr := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return nil, publicError(CodeInvalidRecord, "webhook content coding is invalid", nil)
		}
		if len(decoded) > maximum {
			return nil, publicError(CodeResourceExhausted, "decoded webhook payload exceeds configured limits", nil)
		}
		return decoded, nil
	default:
		return nil, publicError(CodeInvalidRecord, "webhook content coding is unsupported", nil)
	}
}

type rowScanner interface{ Scan(...any) error }

func loadDeliveryRow(row rowScanner, limits Limits) (Delivery, error) {
	var delivery Delivery
	var eventIDsJSON, eventTypesJSON []byte
	var state string
	var createdAt, nextAttempt int64
	var leaseUntil, completedAt sql.NullInt64
	var attempt int64
	var fence int64
	err := row.Scan(&delivery.Record.DeliveryID, &delivery.Record.Tenant, &delivery.Record.EndpointID,
		&delivery.Record.EndpointRevision, &eventIDsJSON, &eventTypesJSON, &delivery.Record.PayloadReference,
		&delivery.ContentType, &delivery.ContentEncoding, &delivery.Body, &state, &attempt, &delivery.Record.LeaseOwner,
		&leaseUntil, &fence, &nextAttempt, &delivery.Record.FailureCode, &delivery.HTTPStatus, &createdAt, &completedAt)
	if err != nil {
		return Delivery{}, err
	}
	if len(delivery.Body) > limits.MaxPayloadBytes || attempt < 0 || attempt > int64(^uint32(0)) || fence < 0 ||
		json.Unmarshal(eventIDsJSON, &delivery.Record.EventIDs) != nil || json.Unmarshal(eventTypesJSON, &delivery.EventTypes) != nil {
		return Delivery{}, publicError(CodeStoreUnavailable, "webhook storage returned invalid retained state", nil)
	}
	delivery.Record.State = event.DeliveryState(state)
	delivery.Record.Attempt = uint32(attempt)
	delivery.Fence = uint64(fence)
	delivery.CreatedAt = time.Unix(0, createdAt).UTC()
	delivery.NextAttempt = time.Unix(0, nextAttempt).UTC()
	if leaseUntil.Valid {
		delivery.LeaseUntil = time.Unix(0, leaseUntil.Int64).UTC()
	}
	return cloneDelivery(delivery), nil
}

func sameEnqueue(existing, candidate Delivery) bool {
	return existing.Record.Tenant == candidate.Record.Tenant && existing.Record.EndpointID == candidate.Record.EndpointID &&
		existing.Record.EndpointRevision == candidate.Record.EndpointRevision && existing.Record.PayloadReference == candidate.Record.PayloadReference &&
		equalStrings(existing.Record.EventIDs, candidate.Record.EventIDs) && equalStrings(existing.EventTypes, candidate.EventTypes) &&
		existing.ContentType == candidate.ContentType && existing.ContentEncoding == candidate.ContentEncoding && bytes.Equal(existing.Body, candidate.Body)
}

func cloneDelivery(value Delivery) Delivery {
	value.Record.EventIDs = append([]string(nil), value.Record.EventIDs...)
	value.EventTypes = append([]string(nil), value.EventTypes...)
	value.Body = append([]byte(nil), value.Body...)
	return value
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validLimits(limits Limits) bool {
	return limits.MaxIdentifierBytes > 0 && limits.MaxIdentifierBytes <= 1024 && limits.MaxPayloadBytes > 0 &&
		limits.MaxPayloadBytes <= 64*1024*1024 && limits.MaxResponseBytes > 0 && limits.MaxResponseBytes <= 1024*1024 &&
		limits.MaxBatchEvents > 0 && limits.MaxBatchEvents <= 100 && limits.MaxDispatchBatch > 0 && limits.MaxDispatchBatch <= 1000 &&
		limits.MaxRecoveryBatch > 0 && limits.MaxRecoveryBatch <= 1000
}

func validRetryPolicy(policy event.RetryPolicy) bool {
	_, err := policy.DelayBefore(2)
	return policy.MaximumAttempts >= 2 && err == nil
}

func validText(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func stableFailureCode(code string) bool {
	switch code {
	case CodeCancelled, CodeResourceExhausted, CodeEndpointRevoked, CodeKeyRevoked, CodeDNSRebinding, CodeRedirect, CodeHTTPPermanent,
		CodeHTTPTransient, CodeTransport, CodeRetryExhausted, CodeSignatureInvalid, CodeStale, CodeReplayStore, CodeEventReordered:
		return true
	default:
		return false
	}
}

func publicError(code, message string, match error) error {
	return &Error{Code: code, Message: message, match: match}
}

func contextFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return publicError(CodeCancelled, "webhook operation was cancelled", err)
	}
	return nil
}

func mapContextOrStore(ctx context.Context) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	return storeFailure()
}

func mapSQLFailure(ctx context.Context, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return publicError(CodeInvalidRecord, "webhook delivery was not found", nil)
	}
	return mapContextOrStore(ctx)
}

func storeFailure() error {
	return publicError(CodeStoreUnavailable, "webhook storage is unavailable", nil)
}

const selectDelivery = `SELECT delivery_id, tenant, endpoint_id, endpoint_revision, event_ids, event_types,
	payload_reference, content_type, content_encoding, body, state, attempt, lease_owner, lease_until,
	lease_fence, next_attempt, failure_code, http_status, created_at, completed_at FROM naatre_webhook_outbox`

var sqliteSchema = []string{
	`CREATE TABLE IF NOT EXISTS naatre_webhook_outbox (
		delivery_id TEXT PRIMARY KEY, tenant TEXT NOT NULL, endpoint_id TEXT NOT NULL, endpoint_revision TEXT NOT NULL,
		event_ids BLOB NOT NULL, event_types BLOB NOT NULL, payload_reference TEXT NOT NULL,
		content_type TEXT NOT NULL, content_encoding TEXT NOT NULL, body BLOB NOT NULL,
		state TEXT NOT NULL, attempt INTEGER NOT NULL, lease_owner TEXT NOT NULL, lease_until INTEGER,
		lease_fence INTEGER NOT NULL, next_attempt INTEGER NOT NULL, failure_code TEXT NOT NULL,
		http_status INTEGER NOT NULL, created_at INTEGER NOT NULL, completed_at INTEGER
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS naatre_webhook_due ON naatre_webhook_outbox (state, next_attempt, created_at, delivery_id)`,
	`CREATE TABLE IF NOT EXISTS naatre_webhook_replay (
		audience TEXT NOT NULL, delivery_id TEXT NOT NULL, expires_at INTEGER NOT NULL,
		PRIMARY KEY (audience, delivery_id)
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS naatre_webhook_replay_expiry ON naatre_webhook_replay (expires_at)`,
	`CREATE TABLE IF NOT EXISTS naatre_webhook_sequences (
		stream TEXT PRIMARY KEY, sequence TEXT NOT NULL
	) STRICT`,
}

var _ event.ReplayStore = (*SQLiteStore)(nil)

// Keep fmt imported for a stable opaque stream key without retaining a URL or
// credential. This is private persisted metadata, never a public observation.
func sequenceStream(sender, audience, eventType, orderingKey string) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s", sender, audience, eventType, orderingKey)
}
