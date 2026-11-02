-- M13b: the repo read ACLs (the SQLite migration 13 twin). One row grants
-- (or records the absence of) read/write on one repo to one global
-- principal — global-principal state per repo. Postgres never runs the
-- db-per-repo split, so the shared set is the only home on this dialect.
-- A repo with at least one row here is PRIVATE: non-readers meet the
-- uniform 404 on every repo-scoped route and the listing drops it.
-- `read` and `write` are 0/1 flags with the invariant read >= write
-- (write implies read — a writer must see what it writes); the statement
-- layer enforces it at write time. No row for a repo = that repo is
-- PUBLIC (the pre-M13 behavior: any authenticated principal reads).
CREATE TABLE repo_access(
    repo_id BIGINT NOT NULL,
    principal TEXT NOT NULL,
    read BIGINT NOT NULL,
    write BIGINT NOT NULL,
    PRIMARY KEY(repo_id, principal)
);
