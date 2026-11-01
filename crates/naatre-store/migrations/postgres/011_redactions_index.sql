-- PG twin of SQLite migration 11 (fix-m10b2): migration 10 shipped without
-- this index; the fix-m10b commit's attempt to append it to 010 violated
-- the immutability contract and is reverted — the index moves here instead.
-- IF NOT EXISTS because a database migrated by the interim binary already
-- carries the index under version 10. The tombstone probe by object id
-- alone (no repo prefix): the purge phase's per-id completion stamp
-- (kind + object_id) and the reachability sweep's NOT EXISTS tombstone arms
-- probe this way per candidate; without the index each probe scans every
-- tombstone.
CREATE INDEX IF NOT EXISTS redactions_object_id ON redactions(object_id);
