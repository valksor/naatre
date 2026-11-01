-- PG twin of SQLite migration 10: the redaction tombstones (M10b). A row
-- marks one object (a CAS blob or a changeset) redacted IN ONE REPO —
-- repo-scoped by the plan's pin: two repos sharing a subtree redact
-- independently, and the other repo's reads keep working (the gate is the
-- (repo_id, object_id) PRIMARY KEY probe every serving path takes). The
-- tombstone metadata records PATH + DATE ONLY, never author — and no
-- reason, no actor: free text can carry personal data and an actor column
-- would duplicate the accountability record, so both live ONLY in the
-- audited `redact` action in audit_log. `path` is the repo path the object
-- sits at (its first, lexicographically — the deterministic one), NULL when
-- the object carries none; `marked_ns` is the tombstone date. `purged_ns`
-- is stamped when the GC's redaction phase reclaims the bytes/rows: until
-- then the tombstoned-but-unpurged state is normal (attribution is gated at
-- every serving path; the purge only reclaims space), and after it the row
-- stays as the tombstone record.
CREATE TABLE redactions(
    repo_id BIGINT NOT NULL,
    object_id BYTEA NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('blob','changeset')),
    path TEXT,
    marked_ns BIGINT NOT NULL,
    purged_ns BIGINT,
    PRIMARY KEY(repo_id, object_id)
);
