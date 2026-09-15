// Package asyncoperation provides durable Go adapters for the queue-neutral
// asynchronous-operation contract in package runtime.
package asyncoperation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	naatreruntime "github.com/valksor/naatre/runtime"
	_ "modernc.org/sqlite"
)

const (
	// Profile identifies this implementation slice. The normative wire and
	// state-machine authority remains operations.async-1.
	Profile = "operations.async-adapters-go-1"

	CodeInvalidConfig     = "ASYNC_ADAPTER_INVALID_CONFIG"
	CodeInvalidRecord     = "ASYNC_ADAPTER_INVALID_RECORD"
	CodeUnavailable       = "ASYNC_ADAPTER_UNAVAILABLE"
	CodeStoreUnavailable  = "ASYNC_ADAPTER_STORE_UNAVAILABLE"
	CodeLeaseExpired      = "ASYNC_ADAPTER_LEASE_EXPIRED"
	CodeFenceRejected     = "ASYNC_ADAPTER_FENCE_REJECTED"
	CodeResourceExhausted = "ASYNC_ADAPTER_RESOURCE_EXHAUSTED"
	CodeCancelled         = "ASYNC_ADAPTER_CANCELLED"
)

// Error is a bounded public adapter failure. It intentionally retains no
// backend error, statement, DSN, record, binding, payload, or credential.
type Error struct {
	Code    string
	Message string
	match   error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Is permits unavailable and cancellation failures to preserve the runtime's
// generic error identity without exposing an underlying implementation error.
func (e *Error) Is(target error) bool {
	if e == target || e.match == target {
		return true
	}
	other, ok := target.(*Error)
	return ok && e.Code == other.Code
}

// ErrorCode returns the stable public code or an empty string for failures
// outside this adapter boundary.
func ErrorCode(err error) string {
	var public *Error
	if errors.As(err, &public) {
		return public.Code
	}
	return ""
}

// Limits bounds every caller-controlled or retained store operation.
type Limits struct {
	MaxIdentifierBytes int
	MaxBindingBytes    int
	MaxPayloadBytes    int
	MaxRecordBytes     int
	MaxPendingBatch    int
	MaxCollectionBatch int
}

// DefaultLimits returns the supported finite reference profile.
func DefaultLimits() Limits {
	return Limits{
		MaxIdentifierBytes: 128,
		MaxBindingBytes:    4096,
		MaxPayloadBytes:    256 * 1024,
		MaxRecordBytes:     1024 * 1024,
		MaxPendingBatch:    128,
		MaxCollectionBatch: 128,
	}
}

// StoreConfig defines storage-owned time, lease, retention, and resource
// policy. Coordinator retention must not exceed ResultRetention.
type StoreConfig struct {
	Now             func() time.Time
	LeaseDuration   time.Duration
	ResultRetention time.Duration
	Limits          Limits
}

// SQLiteStore implements runtime.AsyncOperationStore with atomic SQLite
// revisions and private monotonically increasing worker fences.
type SQLiteStore struct {
	db     *sql.DB
	config StoreConfig
	owned  bool
}

// OpenSQLite opens a modernc SQLite database, initializes the adapter-owned
// tables, and returns a store whose Close method closes that database.
func OpenSQLite(ctx context.Context, dataSourceName string, config StoreConfig) (*SQLiteStore, error) {
	if strings.TrimSpace(dataSourceName) == "" {
		return nil, adapterError(CodeInvalidConfig, "SQLite data source is required", nil)
	}
	db, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
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

// NewSQLiteStore initializes an application-owned database. Closing the store
// does not close a database supplied through this constructor.
func NewSQLiteStore(ctx context.Context, db *sql.DB, config StoreConfig) (*SQLiteStore, error) {
	if db == nil || config.Now == nil || config.LeaseDuration <= 0 || config.ResultRetention <= 0 || !validLimits(config.Limits) {
		return nil, adapterError(CodeInvalidConfig, "asynchronous operation store configuration is invalid", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	for _, statement := range sqliteSchema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
		}
	}
	return &SQLiteStore{db: db, config: config}, nil
}

// Close closes only a database opened by OpenSQLite.
func (s *SQLiteStore) Close() error {
	if s == nil || !s.owned {
		return nil
	}
	s.owned = false
	if err := s.db.Close(); err != nil {
		return adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	return nil
}

func (*SQLiteStore) Durability() naatreruntime.IdempotencyDurability {
	return naatreruntime.IdempotencyDurable
}

func (s *SQLiteStore) Accept(ctx context.Context, record naatreruntime.AsyncOperationRecord, requireIdempotency bool) (naatreruntime.AsyncOperationRecord, bool, error) {
	encoded, err := s.encodeRecord(record, true)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	if requireIdempotency && (record.Binding.IdempotencyKey == "" || record.Binding.Fingerprint == "") {
		return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeInvalidRecord, "idempotency binding is incomplete", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	var digest []byte
	if requireIdempotency {
		digest = idempotencyDigest(record.Binding)
		if existing, found, loadErr := s.loadByIdempotency(ctx, digest); loadErr != nil {
			return naatreruntime.AsyncOperationRecord{}, false, loadErr
		} else if found {
			return resolveIdempotentAccept(existing, record)
		}
	}
	_, execErr := s.db.ExecContext(ctx, `INSERT INTO naatre_async_operations
		(id, state, revision, record, idempotency_digest, created_at, expires_at, lease_fence, lease_until)
		VALUES (?, ?, ?, ?, ?, ?, NULL, 0, NULL)`,
		record.Handle.ID, string(record.Handle.State), record.Handle.Revision, encoded, digest, record.Handle.CreatedAt.UnixNano())
	if execErr == nil {
		return cloneRecord(record), true, nil
	}
	if requireIdempotency {
		if existing, found, loadErr := s.loadByIdempotency(ctx, digest); loadErr == nil && found {
			return resolveIdempotentAccept(existing, record)
		}
	}
	return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
}

func (s *SQLiteStore) Load(ctx context.Context, id string) (naatreruntime.AsyncOperationRecord, error) {
	if err := s.validateIdentifier(id); err != nil {
		return naatreruntime.AsyncOperationRecord{}, unavailableError()
	}
	record, expiresAt, err := s.loadRecord(ctx, id)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, err
	}
	if record.Handle.State.Terminal() && expiresAt > 0 && !s.config.Now().UTC().Before(time.Unix(0, expiresAt).UTC()) {
		return naatreruntime.AsyncOperationRecord{}, unavailableError()
	}
	return record, nil
}

func (s *SQLiteStore) Pending(ctx context.Context, limit int) ([]naatreruntime.AsyncOperationRecord, error) {
	if limit <= 0 || limit > s.config.Limits.MaxPendingBatch {
		return nil, adapterError(CodeResourceExhausted, "pending discovery limit is outside the configured bound", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM naatre_async_operations
		WHERE state = ? ORDER BY created_at, id LIMIT ?`, string(naatreruntime.AsyncOperationPending), limit)
	if err != nil {
		return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	records := make([]naatreruntime.AsyncOperationRecord, 0, limit)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			_ = rows.Close()
			return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
		}
		record, err := decodeRecord(encoded, s.config.Limits.MaxRecordBytes)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	if err := rows.Close(); err != nil {
		return nil, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	return records, nil
}

func (s *SQLiteStore) CompareAndSwap(ctx context.Context, id string, expected uint64, next naatreruntime.AsyncOperationHandle) (naatreruntime.AsyncOperationRecord, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	current, _, err := s.loadRecord(ctx, id)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	if current.Handle.State.Terminal() || current.Handle.Revision != expected {
		return current, false, nil
	}
	if err := s.validateUpdate(current.Handle, next, expected); err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	oldState := current.Handle.State
	current.Handle = next
	encoded, err := s.encodeRecord(current, false)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, err
	}
	now := s.config.Now().UTC()
	leaseUntil := any(nil)
	leaseFenceExpression := "lease_fence"
	if next.State == naatreruntime.AsyncOperationRunning && oldState == naatreruntime.AsyncOperationPending {
		leaseUntil = now.Add(s.config.LeaseDuration).UnixNano()
		leaseFenceExpression = "lease_fence + 1"
	} else if oldState == naatreruntime.AsyncOperationRunning || oldState == naatreruntime.AsyncOperationCancelling {
		var persistedUntil sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT lease_until FROM naatre_async_operations WHERE id = ?`, id).Scan(&persistedUntil); err != nil {
			return naatreruntime.AsyncOperationRecord{}, false, storeFailure(err)
		}
		if !persistedUntil.Valid || persistedUntil.Int64 <= now.UnixNano() {
			return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeLeaseExpired, "worker lease has expired", nil)
		}
		leaseUntil = persistedUntil.Int64
	}
	if next.State.Terminal() {
		leaseUntil = nil
	}
	query := `UPDATE naatre_async_operations SET state = ?, revision = ?, record = ?, expires_at = ?, lease_fence = ` + leaseFenceExpression + `, lease_until = ? WHERE id = ? AND revision = ? AND state = ?`
	var expiresAt any
	if next.State.Terminal() {
		expiresAt = next.ExpiresAt.UnixNano()
	}
	result, err := s.db.ExecContext(ctx, query, string(next.State), next.Revision, encoded, expiresAt, leaseUntil, id, expected, string(oldState))
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	if changed == 0 {
		winner, _, loadErr := s.loadRecord(ctx, id)
		return winner, false, loadErr
	}
	return cloneRecord(current), true, nil
}

// DeleteExpired removes at most limit terminal records. Active records and
// their bindings are never collected by this operation.
func (s *SQLiteStore) DeleteExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > s.config.Limits.MaxCollectionBatch {
		return 0, adapterError(CodeResourceExhausted, "expiry collection limit is outside the configured bound", nil)
	}
	if err := contextFailure(ctx); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM naatre_async_operations WHERE id IN (
		SELECT id FROM naatre_async_operations WHERE expires_at IS NOT NULL AND expires_at <= ?
		AND state IN (?, ?, ?, ?) ORDER BY expires_at, id LIMIT ?
	)`, before.UTC().UnixNano(), string(naatreruntime.AsyncOperationSucceeded), string(naatreruntime.AsyncOperationFailed),
		string(naatreruntime.AsyncOperationCancelled), string(naatreruntime.AsyncOperationIndeterminate), limit)
	if err != nil {
		return 0, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	return int(count), nil
}

// Lease is private worker ownership metadata. It is never part of a runtime
// handle, progress payload, result, error, or transport response.
type Lease struct {
	HandleID string
	Fence    uint64
	Until    time.Time
}

// CurrentLease returns active worker ownership for adapter composition.
func (s *SQLiteStore) CurrentLease(ctx context.Context, id string) (Lease, error) {
	if err := s.validateIdentifier(id); err != nil {
		return Lease{}, unavailableError()
	}
	var fence uint64
	var until sql.NullInt64
	var state string
	err := s.db.QueryRowContext(ctx, `SELECT state, lease_fence, lease_until FROM naatre_async_operations WHERE id = ?`, id).Scan(&state, &fence, &until)
	if err != nil {
		return Lease{}, storeFailure(err)
	}
	now := s.config.Now().UTC()
	if (state != string(naatreruntime.AsyncOperationRunning) && state != string(naatreruntime.AsyncOperationCancelling)) || !until.Valid || until.Int64 <= now.UnixNano() {
		return Lease{}, adapterError(CodeLeaseExpired, "worker lease has expired", nil)
	}
	return Lease{HandleID: id, Fence: fence, Until: time.Unix(0, until.Int64).UTC()}, nil
}

// RenewLease extends a live lease only for its exact fence.
func (s *SQLiteStore) RenewLease(ctx context.Context, lease Lease) (Lease, error) {
	if err := contextFailure(ctx); err != nil {
		return Lease{}, err
	}
	now := s.config.Now().UTC()
	until := now.Add(s.config.LeaseDuration)
	result, err := s.db.ExecContext(ctx, `UPDATE naatre_async_operations SET lease_until = ?
		WHERE id = ? AND lease_fence = ? AND lease_until > ? AND state IN (?, ?)`,
		until.UnixNano(), lease.HandleID, lease.Fence, now.UnixNano(), string(naatreruntime.AsyncOperationRunning), string(naatreruntime.AsyncOperationCancelling))
	if err != nil {
		return Lease{}, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Lease{}, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	if changed != 1 {
		return Lease{}, adapterError(CodeFenceRejected, "worker fence is no longer current", nil)
	}
	lease.Until = until
	return lease, nil
}

// ExpireLeases converts at most limit expired running/cancelling operations to
// indeterminate. The revision predicate fences concurrent renewals and writes.
func (s *SQLiteStore) ExpireLeases(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 || limit > s.config.Limits.MaxCollectionBatch {
		return 0, adapterError(CodeResourceExhausted, "lease recovery limit is outside the configured bound", nil)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, revision, lease_fence, record FROM naatre_async_operations
		WHERE lease_until IS NOT NULL AND lease_until <= ? AND state IN (?, ?)
		ORDER BY lease_until, id LIMIT ?`, before.UTC().UnixNano(), string(naatreruntime.AsyncOperationRunning), string(naatreruntime.AsyncOperationCancelling), limit)
	if err != nil {
		return 0, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	type expired struct {
		id       string
		revision uint64
		fence    uint64
		record   []byte
	}
	candidates := make([]expired, 0, limit)
	for rows.Next() {
		var candidate expired
		if err := rows.Scan(&candidate.id, &candidate.revision, &candidate.fence, &candidate.record); err != nil {
			_ = rows.Close()
			return 0, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return 0, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	count := 0
	for _, candidate := range candidates {
		record, err := decodeRecord(candidate.record, s.config.Limits.MaxRecordBytes)
		if err != nil {
			return count, err
		}
		now := before.UTC()
		record.Handle.State = naatreruntime.AsyncOperationIndeterminate
		record.Handle.Revision++
		record.Handle.UpdatedAt = now
		record.Handle.FinishedAt = now
		record.Handle.ExpiresAt = now.Add(s.config.ResultRetention)
		record.Handle.Result = nil
		record.Handle.Errors = nil
		record.Handle.ProjectionErrors = nil
		encoded, err := s.encodeRecord(record, false)
		if err != nil {
			return count, err
		}
		result, err := s.db.ExecContext(ctx, `UPDATE naatre_async_operations SET state = ?, revision = ?, record = ?, expires_at = ?, lease_until = NULL
			WHERE id = ? AND revision = ? AND lease_fence = ? AND lease_until <= ? AND state IN (?, ?)`,
			string(record.Handle.State), record.Handle.Revision, encoded, record.Handle.ExpiresAt.UnixNano(), candidate.id, candidate.revision,
			candidate.fence, before.UTC().UnixNano(), string(naatreruntime.AsyncOperationRunning), string(naatreruntime.AsyncOperationCancelling))
		if err != nil {
			return count, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return count, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
		}
		count += int(changed)
	}
	return count, nil
}

func (s *SQLiteStore) validateUpdate(current, next naatreruntime.AsyncOperationHandle, expected uint64) error {
	if next.ID != current.ID || next.Revision != expected+1 || next.CreatedAt != current.CreatedAt || next.StartedAt.Before(current.StartedAt) {
		return adapterError(CodeInvalidRecord, "asynchronous operation update is invalid", nil)
	}
	if next.State != current.State && !naatreruntime.ValidAsyncOperationTransition(current.State, next.State) {
		return adapterError(CodeInvalidRecord, "asynchronous operation transition is invalid", nil)
	}
	if next.State == current.State && next.State != naatreruntime.AsyncOperationRunning && next.State != naatreruntime.AsyncOperationCancelling {
		return adapterError(CodeInvalidRecord, "asynchronous operation metadata update is invalid", nil)
	}
	if next.State.Terminal() {
		if next.FinishedAt.IsZero() || next.ExpiresAt.IsZero() || next.ExpiresAt.Before(next.FinishedAt) || next.ExpiresAt.Sub(next.FinishedAt) > s.config.ResultRetention {
			return adapterError(CodeInvalidRecord, "terminal result retention is outside the configured bound", nil)
		}
	}
	return nil
}

func (s *SQLiteStore) encodeRecord(record naatreruntime.AsyncOperationRecord, accepting bool) ([]byte, error) {
	if err := s.validateIdentifier(record.Handle.ID); err != nil {
		return nil, err
	}
	if len(record.Payload) > s.config.Limits.MaxPayloadBytes || bindingBytes(record.Binding) > s.config.Limits.MaxBindingBytes {
		return nil, adapterError(CodeResourceExhausted, "asynchronous operation record exceeds a configured bound", nil)
	}
	if accepting && (record.Handle.State != naatreruntime.AsyncOperationPending || record.Handle.Revision != 1 || record.Handle.CreatedAt.IsZero() || record.Handle.UpdatedAt.IsZero()) {
		return nil, adapterError(CodeInvalidRecord, "accepted asynchronous operation record is invalid", nil)
	}
	encoded, err := json.Marshal(newStoredRecord(record))
	if err != nil {
		return nil, adapterError(CodeInvalidRecord, "asynchronous operation record cannot be encoded", nil)
	}
	if len(encoded) > s.config.Limits.MaxRecordBytes {
		return nil, adapterError(CodeResourceExhausted, "asynchronous operation record exceeds a configured bound", nil)
	}
	return encoded, nil
}

func (s *SQLiteStore) validateIdentifier(id string) error {
	if id == "" || !utf8.ValidString(id) || len(id) > s.config.Limits.MaxIdentifierBytes || strings.IndexFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return adapterError(CodeInvalidRecord, "asynchronous operation identifier is invalid", nil)
	}
	return nil
}

func (s *SQLiteStore) loadRecord(ctx context.Context, id string) (naatreruntime.AsyncOperationRecord, int64, error) {
	if err := contextFailure(ctx); err != nil {
		return naatreruntime.AsyncOperationRecord{}, 0, err
	}
	var encoded []byte
	var expiresAt sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT record, expires_at FROM naatre_async_operations WHERE id = ?`, id).Scan(&encoded, &expiresAt)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, 0, storeFailure(err)
	}
	record, err := decodeRecord(encoded, s.config.Limits.MaxRecordBytes)
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, 0, err
	}
	return record, expiresAt.Int64, nil
}

func (s *SQLiteStore) loadByIdempotency(ctx context.Context, digest []byte) (naatreruntime.AsyncOperationRecord, bool, error) {
	var encoded []byte
	err := s.db.QueryRowContext(ctx, `SELECT record FROM naatre_async_operations WHERE idempotency_digest = ?`, digest).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return naatreruntime.AsyncOperationRecord{}, false, nil
	}
	if err != nil {
		return naatreruntime.AsyncOperationRecord{}, false, adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
	}
	record, err := decodeRecord(encoded, s.config.Limits.MaxRecordBytes)
	return record, err == nil, err
}

func decodeRecord(encoded []byte, maximum int) (naatreruntime.AsyncOperationRecord, error) {
	if len(encoded) == 0 || len(encoded) > maximum {
		return naatreruntime.AsyncOperationRecord{}, adapterError(CodeStoreUnavailable, "stored asynchronous operation record is invalid", nil)
	}
	var stored storedRecord
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&stored); err != nil {
		return naatreruntime.AsyncOperationRecord{}, adapterError(CodeStoreUnavailable, "stored asynchronous operation record is invalid", nil)
	}
	if !stored.PayloadSet {
		stored.Record.Payload = nil
	}
	if !stored.ProgressSet {
		stored.Record.Handle.Progress.Payload = nil
	}
	return stored.Record, nil
}

func resolveIdempotentAccept(existing, requested naatreruntime.AsyncOperationRecord) (naatreruntime.AsyncOperationRecord, bool, error) {
	if existing.Binding != requested.Binding {
		return naatreruntime.AsyncOperationRecord{}, false, adapterError(naatreruntime.CodeIdempotencyConflict, "idempotency binding conflicts with retained work", nil)
	}
	return cloneRecord(existing), false, nil
}

func idempotencyDigest(binding naatreruntime.AsyncOperationBinding) []byte {
	hash := sha256.New()
	hash.Write([]byte("naatre:async-operation-idempotency:v1\n"))
	for _, value := range []string{binding.Principal, binding.Tenant, binding.Operation, binding.IdempotencyKey} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(size[:])
		hash.Write([]byte(value))
	}
	return hash.Sum(nil)
}

func bindingBytes(binding naatreruntime.AsyncOperationBinding) int {
	return len(binding.Principal) + len(binding.Tenant) + len(binding.AuthorizationRevision) + len(binding.SchemaRevision) + len(binding.Operation) + len(binding.IdempotencyKey) + len(binding.Fingerprint)
}

func cloneRecord(record naatreruntime.AsyncOperationRecord) naatreruntime.AsyncOperationRecord {
	encoded, _ := json.Marshal(newStoredRecord(record))
	var clone storedRecord
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	_ = decoder.Decode(&clone)
	if !clone.PayloadSet {
		clone.Record.Payload = nil
	}
	if !clone.ProgressSet {
		clone.Record.Handle.Progress.Payload = nil
	}
	return clone.Record
}

type storedRecord struct {
	Record      naatreruntime.AsyncOperationRecord `json:"record"`
	PayloadSet  bool                               `json:"payloadSet"`
	ProgressSet bool                               `json:"progressSet"`
}

func newStoredRecord(record naatreruntime.AsyncOperationRecord) storedRecord {
	return storedRecord{Record: record, PayloadSet: record.Payload != nil, ProgressSet: record.Handle.Progress.Payload != nil}
}

func validLimits(limits Limits) bool {
	return limits.MaxIdentifierBytes > 0 && limits.MaxBindingBytes > 0 && limits.MaxPayloadBytes > 0 && limits.MaxRecordBytes >= limits.MaxPayloadBytes &&
		limits.MaxPendingBatch > 0 && limits.MaxCollectionBatch > 0
}

func contextFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return adapterError(CodeCancelled, "asynchronous operation storage request was cancelled", err)
	}
	return nil
}

func storeFailure(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return unavailableError()
	}
	return adapterError(CodeStoreUnavailable, "asynchronous operation storage is unavailable", nil)
}

func unavailableError() error {
	return adapterError(CodeUnavailable, "asynchronous operation is unavailable", naatreruntime.ErrAsyncOperationUnavailable)
}

func adapterError(code, message string, match error) error {
	return &Error{Code: code, Message: message, match: match}
}

var sqliteSchema = []string{
	`CREATE TABLE IF NOT EXISTS naatre_async_operations (
		id TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		revision INTEGER NOT NULL,
		record BLOB NOT NULL,
		idempotency_digest BLOB,
		created_at INTEGER NOT NULL,
		expires_at INTEGER,
		lease_fence INTEGER NOT NULL,
		lease_until INTEGER
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS naatre_async_operations_idempotency
		ON naatre_async_operations(idempotency_digest) WHERE idempotency_digest IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS naatre_async_operations_pending
		ON naatre_async_operations(state, created_at, id)`,
	`CREATE INDEX IF NOT EXISTS naatre_async_operations_expiry
		ON naatre_async_operations(expires_at, id) WHERE expires_at IS NOT NULL`,
	`CREATE INDEX IF NOT EXISTS naatre_async_operations_leases
		ON naatre_async_operations(lease_until, id) WHERE lease_until IS NOT NULL`,
}
