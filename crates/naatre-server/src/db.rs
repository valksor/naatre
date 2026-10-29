//! All server-side SQL, as plain synchronous functions over `&Connection`.
//!
//! Callers run these inside [`crate::state::ServerState::read`] /
//! [`crate::state::ServerState::write`] closures (or compose several inside
//! one transaction); no other module in the crate touches SQL. Multi-step
//! mutations (mint + audit, ref CAS + audit) run in a single
//! `unchecked_transaction` here so a failure rolls the whole step back.

use rusqlite::{Connection, OptionalExtension, params};

use naatre_core::ids::Id;

/// One row of `repos`.
pub struct RepoRow {
    pub id: i64,
    pub name: String,
    pub created_ns: i64,
}

/// One row of `refs`.
pub struct RefRow {
    pub name: String,
    pub target: Option<Vec<u8>>,
    pub version: i64,
}

/// The joined `tokens`/`principals` row an auth lookup resolves to.
pub struct AuthRow {
    pub token_id: i64,
    pub principal: String,
    pub role: String,
    pub expires_ns: Option<i64>,
    pub revoked: bool,
    pub scope_path: Option<String>,
}

/// A listed token (token material is never part of any row shape).
pub struct TokenRow {
    pub id: i64,
    pub principal: String,
    pub role: String,
    pub expires_ns: Option<i64>,
    pub scope_path: Option<String>,
    pub revoked: bool,
}

/// One row of `audit_log`.
pub struct AuditRow {
    pub id: i64,
    pub ts_ns: i64,
    pub principal: String,
    pub action: String,
    pub details_json: String,
}

/// Outcome of the compare-and-swap ref move.
pub enum RefCas {
    /// Ref created at version 1.
    Created,
    /// Ref moved to `version`.
    Updated { version: i64 },
    /// CAS lost: the ref's current version/target.
    Conflict {
        current_version: i64,
        current_target: Option<Vec<u8>>,
    },
    /// No such repo.
    RepoMissing,
    /// CAS `expected` was given but the ref does not exist (no version can
    /// match). The caller answers 404 `not_found`.
    RefMissing,
}

/// True when `err` is a UNIQUE-constraint violation.
pub fn is_unique_violation(err: &rusqlite::Error) -> bool {
    matches!(
        err,
        rusqlite::Error::SqliteFailure(ffi, Some(_))
            if ffi.code == rusqlite::ErrorCode::ConstraintViolation
    )
}

// ---- repos ---------------------------------------------------------------

/// Inserts a repo; the caller maps UNIQUE violations to `repo_exists`.
pub fn repos_create(conn: &Connection, name: &str, created_ns: i64) -> rusqlite::Result<i64> {
    conn.execute(
        "INSERT INTO repos(name, created_ns) VALUES (?1, ?2)",
        params![name, created_ns],
    )?;
    Ok(conn.last_insert_rowid())
}

/// Lists repos with `id > after_id`, ascending, up to `limit` rows.
pub fn repos_list(conn: &Connection, after_id: i64, limit: i64) -> rusqlite::Result<Vec<RepoRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT id, name, created_ns FROM repos WHERE id > ?1 ORDER BY id ASC LIMIT ?2",
    )?;
    let rows = stmt.query_map(params![after_id, limit], |row| {
        Ok(RepoRow {
            id: row.get(0)?,
            name: row.get(1)?,
            created_ns: row.get(2)?,
        })
    })?;
    rows.collect()
}

/// The id of repo `name`, or `None`.
pub fn repo_id_by_name(conn: &Connection, name: &str) -> rusqlite::Result<Option<i64>> {
    conn.query_row("SELECT id FROM repos WHERE name = ?1", params![name], |r| {
        r.get(0)
    })
    .optional()
}

/// Repo-scoped counters for `GET /v1/repos/{r}/stats`.
/// Returns `(changesets, objects, bytes, cbytes)`.
pub fn repo_stats(conn: &Connection, repo_id: i64) -> rusqlite::Result<(i64, i64, i64, i64)> {
    let changesets: i64 = conn.query_row(
        "SELECT COUNT(*) FROM changesets WHERE repo_id = ?1",
        params![repo_id],
        |r| r.get(0),
    )?;
    let (objects, bytes, cbytes): (i64, i64, i64) = conn.query_row(
        "SELECT COUNT(*), COALESCE(SUM(o.size), 0), COALESCE(SUM(o.csize), 0) \
         FROM repo_blobs rb JOIN objects o ON o.object_id = rb.object_id \
         WHERE rb.repo_id = ?1",
        params![repo_id],
        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
    )?;
    Ok((changesets, objects, bytes, cbytes))
}

// ---- refs ----------------------------------------------------------------

/// One ref lookup.
pub fn ref_get(conn: &Connection, repo_id: i64, name: &str) -> rusqlite::Result<Option<RefRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT name, target, version FROM refs WHERE repo_id = ?1 AND name = ?2",
    )?;
    stmt.query_row(params![repo_id, name], |row| {
        Ok(RefRow {
            name: row.get(0)?,
            target: row.get(1)?,
            version: row.get(2)?,
        })
    })
    .optional()
}

/// The parameters of one ref move.
pub struct RefMove<'a> {
    pub repo_id: i64,
    pub repo_name: &'a str,
    pub name: &'a str,
    pub target: &'a Id,
    /// `None` creates the ref (conflict if it exists); `Some(v)` moves only
    /// when the current version is `v`.
    pub expected: Option<i64>,
    pub principal: &'a str,
    pub now_ns: i64,
}

/// Compare-and-swap ref move, atomic with its `ref_move` audit row
/// (successes only; conflicts are answered, not audited).
pub fn ref_cas(conn: &Connection, mv: RefMove<'_>) -> rusqlite::Result<RefCas> {
    let RefMove {
        repo_id,
        repo_name,
        name,
        target,
        expected,
        principal,
        now_ns,
    } = mv;
    let tx = conn.unchecked_transaction()?;
    let exists: Option<i64> = tx
        .query_row(
            "SELECT id FROM repos WHERE id = ?1",
            params![repo_id],
            |r| r.get(0),
        )
        .optional()?;
    if exists.is_none() {
        return Ok(RefCas::RepoMissing);
    }

    let current: Option<RefRow> = ref_get(&tx, repo_id, name)?;
    let outcome = match (expected, current) {
        (None, None) => {
            tx.execute(
                "INSERT INTO refs(repo_id, name, target, version, last_principal) \
                 VALUES (?1, ?2, ?3, 1, ?4)",
                params![repo_id, name, target.0.as_slice(), principal],
            )?;
            RefCas::Created
        }
        (None, Some(current)) => RefCas::Conflict {
            current_version: current.version,
            current_target: current.target,
        },
        (Some(_expected), None) => {
            // CAS against a ref that does not exist: no version can match.
            RefCas::RefMissing
        }
        (Some(expected), Some(current)) => {
            if current.version != expected {
                RefCas::Conflict {
                    current_version: current.version,
                    current_target: current.target,
                }
            } else {
                let new_version = current.version + 1;
                tx.execute(
                    "UPDATE refs SET target = ?1, version = ?2, last_principal = ?3 \
                     WHERE repo_id = ?4 AND name = ?5",
                    params![target.0.as_slice(), new_version, principal, repo_id, name],
                )?;
                RefCas::Updated {
                    version: new_version,
                }
            }
        }
    };

    if matches!(outcome, RefCas::Created | RefCas::Updated { .. }) {
        audit_append(
            &tx,
            now_ns,
            principal,
            "ref_move",
            &serde_json::json!({
                "repo": repo_name,
                "ref": name,
                "target": target.to_hex(),
            })
            .to_string(),
        )?;
    }
    tx.commit()?;
    Ok(outcome)
}

// ---- objects -------------------------------------------------------------

/// Records (or refreshes) the `objects` row for a CAS blob that was already
/// fsynced. Returns `true` when the row already existed (idempotent re-PUT).
pub fn object_row_upsert(
    conn: &Connection,
    object_id: &Id,
    size: i64,
    csize: i64,
) -> rusqlite::Result<bool> {
    let existed: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM objects WHERE object_id = ?1",
            params![object_id.0.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    conn.execute(
        "INSERT INTO objects(object_id, size, csize) VALUES (?1, ?2, ?3) \
         ON CONFLICT(object_id) DO UPDATE SET size = excluded.size, csize = excluded.csize",
        params![object_id.0.as_slice(), size, csize],
    )?;
    Ok(existed.is_some())
}

/// Membership check: does repo `repo_id` reference `object_id` via
/// `repo_blobs`? Every object read is gated on this.
pub fn repo_blob_check(conn: &Connection, repo_id: i64, object_id: &Id) -> rusqlite::Result<bool> {
    let hit: Option<i64> = conn
        .query_row(
            "SELECT 1 FROM repo_blobs WHERE repo_id = ?1 AND object_id = ?2",
            params![repo_id, object_id.0.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    Ok(hit.is_some())
}

// ---- principals & tokens -------------------------------------------------

/// Creates or re-roles a principal.
pub fn principal_upsert(
    conn: &Connection,
    name: &str,
    role: &str,
    created_ns: i64,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO principals(name, role, created_ns) VALUES (?1, ?2, ?3) \
         ON CONFLICT(name) DO UPDATE SET role = excluded.role",
        params![name, role, created_ns],
    )?;
    Ok(())
}

/// Inserts a token row (hash only — token material never persists).
pub fn token_insert(
    conn: &Connection,
    principal: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
) -> rusqlite::Result<i64> {
    conn.execute(
        "INSERT INTO tokens(principal, blake3, expires_ns, scope_path) VALUES (?1, ?2, ?3, ?4)",
        params![principal, blake3, expires_ns, scope_path],
    )?;
    Ok(conn.last_insert_rowid())
}

/// Mints a token row: principal upsert, token insert, and the `token_create`
/// audit row land in one transaction. Returns the new token row id.
pub fn token_create(
    conn: &Connection,
    principal: &str,
    role: &str,
    blake3: &[u8; 32],
    expires_ns: Option<i64>,
    scope_path: Option<&str>,
    now_ns: i64,
) -> rusqlite::Result<i64> {
    let tx = conn.unchecked_transaction()?;
    principal_upsert(&tx, principal, role, now_ns)?;
    let token_id = token_insert(&tx, principal, blake3, expires_ns, scope_path)?;
    audit_append(
        &tx,
        now_ns,
        principal,
        "token_create",
        &serde_json::json!({
            "token_id": token_id,
            "principal": principal,
            "role": role,
        })
        .to_string(),
    )?;
    tx.commit()?;
    Ok(token_id)
}

/// Lists tokens joined with their principal's role, ascending by id.
pub fn tokens_list(conn: &Connection) -> rusqlite::Result<Vec<TokenRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT t.id, t.principal, p.role, t.expires_ns, t.scope_path, t.revoked \
         FROM tokens t JOIN principals p ON p.name = t.principal ORDER BY t.id ASC",
    )?;
    let rows = stmt.query_map([], |row| {
        Ok(TokenRow {
            id: row.get(0)?,
            principal: row.get(1)?,
            role: row.get(2)?,
            expires_ns: row.get(3)?,
            scope_path: row.get(4)?,
            revoked: row.get::<_, i64>(5)? != 0,
        })
    })?;
    rows.collect()
}

/// Revokes token `id`. Returns `None` when no such token exists, and
/// `Some(first_revocation)` — `true` only on the 0→1 transition, so an
/// idempotent re-revoke answers 200 without writing a second audit row.
pub fn token_revoke(conn: &Connection, id: i64, now_ns: i64) -> rusqlite::Result<Option<bool>> {
    let tx = conn.unchecked_transaction()?;
    let exists: Option<String> = tx
        .query_row(
            "SELECT principal FROM tokens WHERE id = ?1",
            params![id],
            |r| r.get(0),
        )
        .optional()?;
    let Some(principal) = exists else {
        return Ok(None);
    };
    let changed = tx.execute(
        "UPDATE tokens SET revoked = 1 WHERE id = ?1 AND revoked = 0",
        params![id],
    )?;
    let first_revocation = changed == 1;
    if first_revocation {
        audit_append(
            &tx,
            now_ns,
            &principal,
            "token_revoke",
            &serde_json::json!({ "token_id": id }).to_string(),
        )?;
    }
    tx.commit()?;
    Ok(Some(first_revocation))
}

/// Token lookup by stored BLAKE3 hash, joined with the principal's role.
pub fn auth_lookup(conn: &Connection, blake3: &[u8; 32]) -> rusqlite::Result<Option<AuthRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT t.id, t.principal, p.role, t.expires_ns, t.revoked, t.scope_path \
         FROM tokens t JOIN principals p ON p.name = t.principal WHERE t.blake3 = ?1",
    )?;
    stmt.query_row(params![blake3.as_slice()], |row| {
        Ok(AuthRow {
            token_id: row.get(0)?,
            principal: row.get(1)?,
            role: row.get(2)?,
            expires_ns: row.get(3)?,
            revoked: row.get::<_, i64>(4)? != 0,
            scope_path: row.get(5)?,
        })
    })
    .optional()
}

// ---- audit ---------------------------------------------------------------

/// Appends one audit row.
pub fn audit_append(
    conn: &Connection,
    ts_ns: i64,
    principal: &str,
    action: &str,
    details_json: &str,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT INTO audit_log(ts_ns, principal, action, details_json) VALUES (?1, ?2, ?3, ?4)",
        params![ts_ns, principal, action, details_json],
    )?;
    Ok(())
}

/// Lists audit rows with `id < before_id`, descending (id order matches ts
/// order — ids are assigned at insert), up to `limit` rows.
pub fn audit_list(
    conn: &Connection,
    before_id: i64,
    limit: i64,
) -> rusqlite::Result<Vec<AuditRow>> {
    let mut stmt = conn.prepare_cached(
        "SELECT id, ts_ns, principal, action, details_json FROM audit_log \
         WHERE id < ?1 ORDER BY id DESC LIMIT ?2",
    )?;
    let rows = stmt.query_map(params![before_id, limit], |row| {
        Ok(AuditRow {
            id: row.get(0)?,
            ts_ns: row.get(1)?,
            principal: row.get(2)?,
            action: row.get(3)?,
            details_json: row.get(4)?,
        })
    })?;
    rows.collect()
}
