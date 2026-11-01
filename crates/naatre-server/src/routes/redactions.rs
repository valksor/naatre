//! `/v1/repos/{r}/redactions`: the admin redaction surface (M10b). A POST
//! marks one object (a CAS blob or a changeset) tombstoned in this repo —
//! the universal gate then drops it from every serving path at mark time;
//! the GC's redaction phase reclaims the bytes/rows later and stamps
//! `purged_ns`. The tombstone ROW records path + date only, never author —
//! no reason, no actor: both live ONLY in the audited `redact` action here
//! (the acting admin is the audit row's principal, the reason its
//! details).

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::routes::repo_id_or_404;
use crate::state::AppState;

/// Request body of `POST /v1/repos/{r}/redactions`.
#[derive(Deserialize, ToSchema)]
pub struct CreateRedactionRequest {
    /// The object id to tombstone, 64 lowercase hex (a blob id or a
    /// changeset id — `kind` says which).
    pub id: String,
    /// `blob` or `changeset`.
    pub kind: String,
    /// Why the object is being redacted (free text). Audited ONLY — it
    /// never lands in the tombstone row (free text can carry personal
    /// data; the tombstone records path + date).
    pub reason: String,
}

/// One tombstone as served by the list and the POST responses.
#[derive(Serialize, ToSchema)]
pub struct RedactionEntry {
    pub id: String,
    pub kind: String,
    /// The repo path the object sits at (its first, lexicographically),
    /// null when the object carries none — the tombstone's only descriptive
    /// metadata.
    pub path: Option<String>,
    /// The tombstone date (ns since the epoch). An idempotent re-POST
    /// keeps the EARLIEST mark; re-arming a purged tombstone moves it to
    /// the re-arm's stamp.
    pub marked_ns: u64,
    /// The reclaim date once the GC's redaction phase purged the object;
    /// null while it stands tombstoned-but-unpurged (a normal state).
    pub purged_ns: Option<u64>,
}

/// Mark response (201 on a new tombstone; 200 on an idempotent re-POST —
/// the recorded row, the earliest `marked_ns` stands — or on a RE-ARM of a
/// purged tombstone, `rearmed: true`).
#[derive(Serialize, ToSchema)]
pub struct RedactionMarked {
    pub redaction: RedactionEntry,
    /// True when this call re-armed a PURGED tombstone (new `marked_ns`,
    /// a new `redact` audit row). `purged_ns` cleared when something
    /// remains to purge (the tombstone re-enters the work list); for a
    /// changeset id whose row the purge already deleted, `purged_ns`
    /// re-stamps to this call's time — nothing remains to purge, and the
    /// tombstone reports purged with the write gates still armed. Never
    /// true on the 201 insert.
    pub rearmed: bool,
}

/// List body.
#[derive(Serialize, ToSchema)]
pub struct RedactionListResponse {
    pub redactions: Vec<RedactionEntry>,
    /// `after:<id>` of this page's last entry, or null when done.
    pub next_cursor: Option<String>,
    pub has_more: bool,
}

fn entry_of(row: db::RedactionRow) -> RedactionEntry {
    RedactionEntry {
        id: row.object_id.to_hex(),
        kind: row.kind,
        path: row.path,
        marked_ns: row.marked_ns as u64,
        purged_ns: row.purged_ns.map(|ns| ns as u64),
    }
}

/// Marks an object redacted in this repo (admin, audited `redact`: the
/// acting admin is the audit row's principal and the reason its details —
/// the tombstone ROW itself carries path + date only, never author). 201
/// on a new tombstone; an idempotent re-POST answers 200 keeping the
/// earliest `marked_ns` (and writes no second audit row); a re-POST on a
/// PURGED tombstone re-arms it — 200 `rearmed: true`, a fresh
/// `marked_ns`, `purged_ns` cleared, and a NEW `redact` audit row (never
/// a silent no-op). A re-POST naming the OTHER kind is refused (422). The
/// mark ALSO invalidates the target changeset's derived rows (every
/// `derive_state` row goes to the poison terminal state, so
/// re-derivations propagate the hole through the taint machinery) in the
/// same immediate transaction.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/redactions",
    params(("r" = String, Path, description = "Repo name")),
    request_body = CreateRedactionRequest,
    responses(
        (status = 201, description = "Tombstone marked", body = RedactionMarked),
        (status = 200, description = "Already tombstoned (idempotent re-POST; the earliest marked_ns stands), or a purged tombstone re-armed (`rearmed: true`)", body = RedactionMarked),
        (status = 400, description = "The id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo; or the target is not a blob or changeset known to this repo, imported content included"),
        (status = 422, description = "kind is not `blob`/`changeset`, the reason is empty, or the id is already tombstoned with the other kind")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<CreateRedactionRequest>,
) -> Result<axum::response::Response, ApiError> {
    let AppJson(body) = body;
    require_role(&auth, Role::Admin)?;
    let kind = match body.kind.as_str() {
        "blob" => "blob",
        "changeset" => "changeset",
        other => {
            return Err(ApiError::new(
                ApiCode::Validation,
                format!("kind must be `blob` or `changeset`, got {other:?}"),
            ));
        }
    };
    if body.reason.trim().is_empty() {
        return Err(ApiError::new(
            ApiCode::Validation,
            "reason must not be empty",
        ));
    }
    let id = Id::from_hex(&body.id).ok_or_else(|| {
        ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters")
    })?;
    let repo_id = repo_id_or_404(&state, &repo).await?;

    // The target must be a real object of this repo, or an already-
    // tombstoned id (the re-arm target): a changeset row of THIS repo, or
    // a blob the server holds in any form — an objects row, repo_blobs
    // membership or a file_changes row of this repo (an import writes
    // membership without an objects row), a blob entry in any written
    // tree — or an existing tombstone row (active or purged). A repo-
    // scoped blob tombstone may target content another repo also
    // references (the shared-subtree pin) or a PUT-but-unreferenced
    // upload; the GATE stays repo-scoped either way.
    let exists = {
        state
            .read(move |conn| {
                db::redaction_target_present(conn, repo_id, &id, kind)
                    .map_err(ApiError::log_internal)
            })
            .await?
    };
    if !exists {
        return Err(ApiError::new(
            ApiCode::ObjectNotFound,
            format!("no {kind} {id} in this repo"),
        ));
    }

    let now_ns = state.now_ns() as i64;
    let principal = auth.principal.clone();
    let reason = body.reason.clone();
    let marked = state
        .write(move |store| {
            let conn = store.conn();
            // Immediate: the mark is a destructive write (it flips every
            // serving gate and poisons derived rows) — the same write-lock
            // class as the GC's purge phases, taken up front.
            let tx = conn
                .transaction_immediate()
                .map_err(ApiError::log_internal)?;
            let outcome = db::redaction_mark(&tx, repo_id, &id, kind, now_ns)
                .map_err(ApiError::log_internal)?;
            match &outcome {
                db::RedactionMarkOutcome::Inserted(_) => {
                    // Mark-time invalidation of the tombstoned changeset's
                    // derived rows (a no-op for blobs): the read gates stop
                    // serving immediately, and re-derivations propagate the
                    // hole through the taint machinery.
                    db::redaction_invalidate_derived(&tx, repo_id, &id)
                        .map_err(ApiError::log_internal)?;
                    // The audit row rides the 0→1 transition only (the
                    // token_revoke convention): an idempotent re-POST writes
                    // nothing.
                    db::audit_append(
                        &tx,
                        now_ns,
                        &principal,
                        "redact",
                        &serde_json::json!({
                            "repo": repo,
                            "id": id.to_hex(),
                            "kind": kind,
                            "reason": reason,
                        })
                        .to_string(),
                    )
                    .map_err(ApiError::log_internal)?;
                }
                db::RedactionMarkOutcome::Rearmed(_) => {
                    // A re-arm is a real admin action on a purged record —
                    // it rows its own audit entry, never a silent no-op.
                    db::audit_append(
                        &tx,
                        now_ns,
                        &principal,
                        "redact",
                        &serde_json::json!({
                            "repo": repo,
                            "id": id.to_hex(),
                            "kind": kind,
                            "reason": reason,
                            "rearmed": true,
                        })
                        .to_string(),
                    )
                    .map_err(ApiError::log_internal)?;
                }
                db::RedactionMarkOutcome::Existing(_)
                | db::RedactionMarkOutcome::KindMismatch { .. } => {}
            }
            tx.commit().map_err(ApiError::log_internal)?;
            Ok(outcome)
        })
        .await?;

    let (row, status, rearmed) = match marked {
        db::RedactionMarkOutcome::Inserted(row) => (row, StatusCode::CREATED, false),
        db::RedactionMarkOutcome::Existing(row) => (row, StatusCode::OK, false),
        db::RedactionMarkOutcome::Rearmed(row) => (row, StatusCode::OK, true),
        db::RedactionMarkOutcome::KindMismatch { existing } => {
            return Err(ApiError::new(
                ApiCode::Validation,
                format!(
                    "{} is already tombstoned with kind {:?}; the re-POST named {kind:?}",
                    existing.object_id.to_hex(),
                    existing.kind
                ),
            ));
        }
    };
    Ok((
        status,
        Json(RedactionMarked {
            redaction: entry_of(row),
            rearmed,
        }),
    )
        .into_response())
}

/// Query params of `GET /v1/repos/{r}/redactions`.
#[derive(Deserialize, IntoParams)]
pub struct RedactionListQuery {
    /// Page size (1–200; default 50).
    pub limit: Option<String>,
    /// Opaque `after:<id>` cursor from the previous page.
    pub cursor: Option<String>,
}

/// Lists the repo's tombstones (admin), oldest id first, purged rows
/// included (their `purged_ns` shows the reclaim date).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/redactions",
    params(("r" = String, Path, description = "Repo name"), RedactionListQuery),
    responses(
        (status = 200, description = "One page of tombstones (purged state included)", body = RedactionListResponse),
        (status = 400, description = "Non-integer limit, or a malformed cursor"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo"),
        (status = 422, description = "limit out of the 1..=200 range")
    )
)]
pub async fn list(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    Query(q): Query<RedactionListQuery>,
) -> Result<Json<RedactionListResponse>, ApiError> {
    require_role(&auth, Role::Admin)?;
    let limit = super::parse_limit(&crate::routes::ListQuery {
        limit: q.limit.clone(),
        cursor: None,
    })?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let after = match q.cursor.as_deref() {
        None => None,
        Some(raw) => {
            let hex = raw.strip_prefix("after:").ok_or_else(|| {
                ApiError::new(ApiCode::Malformed, "cursor is not a valid page cursor")
            })?;
            let id = Id::from_hex(hex).ok_or_else(|| {
                ApiError::new(ApiCode::Malformed, "cursor is not a valid page cursor")
            })?;
            Some(id)
        }
    };
    let rows = state
        .read(move |conn| {
            db::redactions_page(conn, repo_id, after.as_ref(), limit + 1)
                .map_err(ApiError::log_internal)
        })
        .await?;
    let has_more = rows.len() as i64 > limit;
    let mut rows = rows;
    rows.truncate(limit as usize);
    let next_cursor = has_more.then(|| {
        format!(
            "after:{}",
            rows.last().expect("non-empty page").object_id.to_hex()
        )
    });
    Ok(Json(RedactionListResponse {
        redactions: rows.into_iter().map(entry_of).collect(),
        next_cursor,
        has_more,
    }))
}
