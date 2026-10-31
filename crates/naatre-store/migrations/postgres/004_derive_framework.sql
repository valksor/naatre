-- PG twin of SQLite migration 4: the generic derived-data framework's
-- tables. `derive_kinds` is seeded EMPTY (kinds register in code);
-- `derive_state` carries `seq` (the rowid analog) — the queue pick's
-- insertion order and the backfill cursor read it.
CREATE TABLE derive_kinds(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE derive_state(kind BIGINT NOT NULL, repo_id BIGINT NOT NULL, cs BYTEA NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','deriving','done','poison')), lease_expires_ns BIGINT, attempts BIGINT NOT NULL DEFAULT 0, output BYTEA, seq BIGINT GENERATED ALWAYS AS IDENTITY, PRIMARY KEY(kind, repo_id, cs));
CREATE INDEX idx_derive_state_kind_state ON derive_state(kind, state);
CREATE TABLE derive_reports(kind BIGINT NOT NULL, repo_id BIGINT NOT NULL, cs BYTEA NOT NULL, reason TEXT NOT NULL, ts_ns BIGINT NOT NULL);
CREATE INDEX idx_derive_reports_node ON derive_reports(kind, repo_id, cs);
