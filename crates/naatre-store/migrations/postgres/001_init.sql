-- naatre Postgres schema, migration 1 (initial) — the PG twin of the
-- SQLite migration 1. Translation rules for the whole set:
--   * BLOB → bytea, INTEGER → BIGINT (ns timestamps and sizes do not fit
--     INTEGER; flag columns stay BIGINT 0/1 to keep the statement
--     surface's value domain uniform),
--   * SQLite `id INTEGER PRIMARY KEY` (a rowid alias) →
--     `id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`,
--   * rowid-ORDERING surfaces (the derive queue pick, the backfill
--     cursor) get an explicit `seq BIGINT GENERATED ALWAYS AS IDENTITY`
--     on the two tables whose rowid the code reads: `changesets` (unique,
--     the cursor's second coordinate) and `derive_state` (insertion
--     order). PRAGMA journal_mode/WAL and synchronous=FULL have no
--     Postgres analog (durability is COMMIT semantics + the server's
--     single-writer slot).
CREATE TABLE repos(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_ns BIGINT NOT NULL);
CREATE TABLE changesets(repo_id BIGINT NOT NULL, id BYTEA PRIMARY KEY, canonical BYTEA NOT NULL, author TEXT NOT NULL, message TEXT NOT NULL, timestamp_ns BIGINT NOT NULL, generation BIGINT NOT NULL, principal TEXT NOT NULL, root_tree_id BYTEA, seq BIGINT GENERATED ALWAYS AS IDENTITY, UNIQUE(seq));
CREATE TABLE changeset_parents(repo_id BIGINT NOT NULL, cs BYTEA NOT NULL, parent BYTEA NOT NULL, ord BIGINT NOT NULL, PRIMARY KEY(repo_id, cs, ord));
CREATE INDEX idx_cs_parents_parent ON changeset_parents(repo_id, parent);
CREATE TABLE file_changes(repo_id BIGINT NOT NULL, cs BYTEA NOT NULL, path TEXT NOT NULL, op BIGINT NOT NULL, blob_id BYTEA, size BIGINT, exec BIGINT, PRIMARY KEY(repo_id, cs, path));
CREATE TABLE repo_blobs(repo_id BIGINT NOT NULL, object_id BYTEA NOT NULL, PRIMARY KEY(repo_id, object_id));
CREATE TABLE refs(repo_id BIGINT NOT NULL, name TEXT NOT NULL, target BYTEA, version BIGINT NOT NULL DEFAULT 1, last_principal TEXT, PRIMARY KEY(repo_id, name));
CREATE TABLE trees(id BYTEA PRIMARY KEY, repo_id BIGINT NOT NULL, canonical BYTEA NOT NULL);
CREATE TABLE tree_entries(tree_id BYTEA NOT NULL, name TEXT NOT NULL, kind BIGINT NOT NULL, object_id BYTEA NOT NULL, size BIGINT NOT NULL, exec BIGINT NOT NULL DEFAULT 0, PRIMARY KEY(tree_id, name));
CREATE TABLE objects(object_id BYTEA PRIMARY KEY, size BIGINT NOT NULL, csize BIGINT NOT NULL);
CREATE TABLE principals(name TEXT PRIMARY KEY, role TEXT NOT NULL CHECK(role IN ('admin','writer','reader')), created_ns BIGINT NOT NULL);
CREATE TABLE tokens(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, principal TEXT NOT NULL, blake3 BYTEA UNIQUE NOT NULL, expires_ns BIGINT, revoked BIGINT NOT NULL DEFAULT 0);
CREATE TABLE lands(repo_id BIGINT NOT NULL, principal TEXT NOT NULL, key TEXT NOT NULL, state TEXT NOT NULL, head BYTEA, base BYTEA, result_json TEXT, PRIMARY KEY(repo_id, principal, key));
CREATE TABLE snapshots(id BYTEA PRIMARY KEY, repo_id BIGINT NOT NULL, tree_id BYTEA NOT NULL, principal TEXT NOT NULL, created_ns BIGINT NOT NULL, expires_ns BIGINT NOT NULL);
CREATE INDEX idx_snapshots_expiry ON snapshots(expires_ns);
CREATE TABLE audit_log(id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, ts_ns BIGINT NOT NULL, principal TEXT NOT NULL, action TEXT NOT NULL, details_json TEXT NOT NULL);
CREATE TABLE git_map(repo_id BIGINT NOT NULL, git_oid BYTEA NOT NULL, cs_id BYTEA NOT NULL, PRIMARY KEY(repo_id, git_oid));
