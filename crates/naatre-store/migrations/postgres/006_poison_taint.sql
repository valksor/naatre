-- PG twin of SQLite migration 6: poison taint on the queue rows.
ALTER TABLE derive_state ADD COLUMN tainted BIGINT NOT NULL DEFAULT 0;
ALTER TABLE derive_state ADD COLUMN poison_cs BYTEA;
