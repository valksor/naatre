-- PG twin of SQLite migration 9: the reachability GC's collectible
-- selection probes (`db::gc_collectible_cs` — "is this ledger candidate
-- still unreferenced?") run per candidate against refs.target, lands.head,
-- lands.base, git_map.cs_id, and changeset_parents.parent. None of those
-- probe columns led an index until now: every probe was a full scan per
-- candidate row. This migration gives each one its index (the
-- changeset_parents probe is repo-less by design — gc_pending carries no
-- repo_id, a collected changeset is gone from EVERY repo — so it gets a
-- parent-only index rather than the (repo_id, parent) PK prefix the
-- repo-scoped walks use).
CREATE INDEX idx_refs_target ON refs(target);
CREATE INDEX idx_lands_head ON lands(head);
CREATE INDEX idx_lands_base ON lands(base);
CREATE INDEX idx_git_map_cs ON git_map(cs_id);
CREATE INDEX idx_cs_parents_parent_id ON changeset_parents(parent);
