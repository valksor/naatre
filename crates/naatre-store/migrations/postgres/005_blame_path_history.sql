-- PG twin of SQLite migration 5: the blame and path_history kinds' served
-- read-surface tables (commit_graph keeps its state in the framework
-- handoff output and needs no table).
CREATE TABLE blame_state(repo_id BIGINT NOT NULL, cs BYTEA NOT NULL, path TEXT NOT NULL, blob_id BYTEA, parent_blob_id BYTEA, PRIMARY KEY(repo_id, cs, path));
CREATE TABLE path_history(repo_id BIGINT NOT NULL, path TEXT NOT NULL, gen BIGINT NOT NULL, cs BYTEA NOT NULL, op BIGINT NOT NULL, PRIMARY KEY(repo_id, path, gen, cs));
