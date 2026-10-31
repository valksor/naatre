-- PG twin of SQLite migration 7: the boot backfill's batch cursor seeks
-- `changesets` in (generation, rowid) order — on SQLite the single-column
-- index is internally (generation, rowid); Postgres gets the composite
-- form explicitly so the row-value range seek and the ORDER BY stay
-- indexed.
CREATE INDEX idx_changesets_generation ON changesets(generation);
CREATE INDEX idx_changesets_generation_seq ON changesets(generation, seq);
