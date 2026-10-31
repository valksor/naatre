-- PG twin of SQLite migration 8: the reachability GC's pending-collection
-- ledger (scope: 'cs' a changeset, 'blob' a CAS blob, 'tree' a trees row;
-- first_ns is when the row was FIRST observed unreachable — INSERT ON
-- CONFLICT DO NOTHING preserves it across passes, so the grace window is
-- measured from the moment the data became garbage and a crash mid-GC is
-- idempotent), the blob index that keeps the sweep's per-batch
-- re-verification and the mark's blob collection indexed, and the upload
-- grace clock: every PUT refreshes objects.touched_ns in the same
-- transaction as the row, so an OLD orphan re-uploaded and re-referenced
-- while a GC pass is in flight re-enters the grace window (content-addressed
-- re-PUTs are CAS no-ops on every backend; the DB clock is the uniform
-- signal).
-- condemned_at is the two-phase delete's mark: the sweep's phase A
-- stamps it (bytes stay), phase B deletes bytes+rows only while it is
-- STILL set — any PUT's row touch clears it.
CREATE TABLE gc_pending(scope TEXT NOT NULL CHECK(scope IN ('cs','blob','tree')), id BYTEA NOT NULL, first_ns BIGINT NOT NULL, condemned_at BIGINT, PRIMARY KEY(scope, id));
CREATE INDEX idx_file_changes_blob ON file_changes(blob_id);
ALTER TABLE objects ADD COLUMN touched_ns BIGINT NOT NULL DEFAULT 0;
