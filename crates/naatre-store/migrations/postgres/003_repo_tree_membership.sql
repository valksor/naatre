-- PG twin of SQLite migration 3: per-repo tree membership for HISTORY
-- trees, with the one-time backfill (see the SQLite migration's comment
-- for the full derivation). The recursive CTE and the UNION dedup are
-- portable; the index set matches exactly.
CREATE TABLE repo_trees(repo_id BIGINT NOT NULL, tree_id BYTEA NOT NULL, PRIMARY KEY(repo_id, tree_id));
CREATE INDEX idx_repo_trees_tree ON repo_trees(tree_id);
CREATE INDEX idx_changesets_root ON changesets(root_tree_id);
CREATE INDEX idx_tree_entries_object ON tree_entries(object_id);
WITH RECURSIVE reach(repo_id, tree_id) AS (
    SELECT repo_id, root_tree_id FROM changesets WHERE root_tree_id IS NOT NULL
    UNION
    SELECT r.repo_id, te.object_id FROM reach r
    JOIN tree_entries te ON te.tree_id = r.tree_id AND te.kind = 1
)
INSERT INTO repo_trees(repo_id, tree_id)
SELECT DISTINCT repo_id, tree_id FROM reach;
