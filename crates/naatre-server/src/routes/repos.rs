//! `/v1/repos`: cursor-paginated listing, repo creation, and repo stats.

use axum::extract::{Path, Query, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::repo_read_gate;
use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::routes::{ListQuery, parse_limit};
use crate::state::AppState;

/// One repo in a listing page.
#[derive(Serialize, ToSchema)]
pub struct RepoSummary {
    pub id: i64,
    pub name: String,
    pub created_ns: u64,
}

/// A page of the repo list.
///
/// Pagination: request `?limit=<1..=200>` (default 50) and optionally
/// `?cursor=after:<id>` — the `next_cursor` value of the previous page,
/// i.e. `after:<id>` where `<id>` is the last repo id of that page. Follow
/// `next_cursor` until `has_more` is false.
#[derive(Serialize, ToSchema)]
pub struct RepoListResponse {
    pub repos: Vec<RepoSummary>,
    /// `after:<id>` of this page's last repo, or null when done.
    pub next_cursor: Option<String>,
    pub has_more: bool,
}

/// Request body of `POST /v1/repos`.
#[derive(Deserialize, ToSchema)]
pub struct CreateRepoRequest {
    /// Repo name, `^[a-z0-9][a-z0-9-]{0,62}$`.
    pub name: String,
}

/// Created repo body.
#[derive(Serialize, ToSchema)]
pub struct RepoCreated {
    pub id: i64,
    pub name: String,
}

/// Repo-scoped counters (`GET /v1/repos/{r}/stats`).
#[derive(Serialize, ToSchema)]
pub struct RepoStats {
    pub changesets: u64,
    /// Objects referenced by this repo via `repo_blobs`.
    pub objects: u64,
    /// Sum of uncompressed sizes over this repo's objects.
    pub bytes: u64,
    /// Sum of on-disk (compressed) sizes over this repo's objects.
    pub cbytes: u64,
}

/// List repos, cursor-paginated. Any authenticated principal may list;
/// since M13b the page is FILTERED: private repos (at least one
/// `repo_access` row) appear only to admins and granted readers — the
/// same invisibility the repo-scoped routes enforce. The cursor tracks
/// the last RAW row scanned (a filtered-out repo never blocks the
/// walk); a page may therefore come back shorter than `limit` while
/// `has_more` is true when private repos were skipped.
#[utoipa::path(
    get,
    path = "/v1/repos",
    params(ListQuery),
    responses(
        (status = 200, description = "A page of repos visible to this principal", body = RepoListResponse),
        (status = 400, description = "Malformed cursor or limit"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 422, description = "limit out of the 1..=200 range")
    )
)]
pub async fn list(
    State(state): State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Query(q): Query<ListQuery>,
) -> Result<Json<RepoListResponse>, ApiError> {
    let limit = parse_limit(&q)?;
    let after = match q.cursor.as_deref() {
        None => 0,
        Some(cursor) => {
            let id = cursor
                .strip_prefix("after:")
                .ok_or_else(bad_cursor)?
                .parse::<i64>()
                .map_err(|_| bad_cursor())?;
            if id < 0 {
                return Err(bad_cursor());
            }
            id
        }
    };
    let admin = auth.role == crate::auth::Role::Admin;
    let principal = auth.principal.clone();
    let (rows, raw_page_full, last_scanned) = state
        .read(move |conn| {
            let raw = db::repos_list(conn, after, limit + 1).map_err(ApiError::log_internal)?;
            let raw_page_full = raw.len() as i64 > limit;
            let last_scanned = raw.last().map_or(0, |r| r.id);
            if admin {
                return Ok::<_, ApiError>((raw, raw_page_full, last_scanned));
            }
            // The visibility filter, per row: ONE folded probe answers
            // private AND readable (M13b-fix security A8; the page is
            // bounded by limit+1).
            let mut visible = Vec::with_capacity(raw.len());
            for row in raw {
                let (private, readable) = db::access_visibility(conn, row.id, &principal)
                    .map_err(ApiError::log_internal)?;
                if !private || readable {
                    visible.push(row);
                }
            }
            Ok((visible, raw_page_full, last_scanned))
        })
        .await?;
    // has_more tracks the RAW walk (a page of filtered-out private repos
    // must not end the listing). The cursor advances past what was
    // scanned: normally the last RETURNED id (the legacy contract —
    // invisible rows behind it are merely re-scanned and re-filtered),
    // but on an EMPTY visible page with more raw rows ahead it is the
    // last RAW id scanned (after:0 would restart the walk forever).
    let has_more = raw_page_full;
    let mut rows = rows;
    rows.truncate(limit as usize);
    let cursor_id = rows.last().map_or(last_scanned, |r| r.id);
    let next_cursor = has_more.then(|| format!("after:{cursor_id}"));
    Ok(Json(RepoListResponse {
        repos: rows
            .into_iter()
            .map(|r| RepoSummary {
                id: r.id,
                name: r.name,
                created_ns: r.created_ns as u64,
            })
            .collect(),
        next_cursor,
        has_more,
    }))
}

fn bad_cursor() -> ApiError {
    ApiError::new(ApiCode::Malformed, "cursor is not a valid page cursor")
}

/// Creates a repo (admin, audited). 201 + `Location: /v1/repos/{name}`.
#[utoipa::path(
    post,
    path = "/v1/repos",
    request_body = CreateRepoRequest,
    responses(
        (status = 201, description = "Repo created", body = RepoCreated),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 409, description = "A repo with that name already exists (`repo_exists`)"),
        (status = 422, description = "Name does not match ^[a-z0-9][a-z0-9-]{0,62}$")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    body: AppJson<CreateRepoRequest>,
) -> Response {
    match create_inner(&state, &auth, body.0).await {
        Ok((id, name)) => (
            StatusCode::CREATED,
            [(header::LOCATION, format!("/v1/repos/{name}"))],
            Json(RepoCreated { id, name }),
        )
            .into_response(),
        Err(e) => e.into_response(),
    }
}

async fn create_inner(
    state: &AppState,
    auth: &AuthedPrincipal,
    body: CreateRepoRequest,
) -> Result<(i64, String), ApiError> {
    require_role(auth, Role::Admin)?;
    validate_repo_name(&body.name)?;
    let data_dir = state.data_dir_display.display().to_string();
    let name = body.name.clone();
    let now_ns = state.now_ns() as i64;
    let principal = auth.principal.clone();
    // In db-per-repo mode the create also materializes the repo's own
    // SQLite file, INSIDE the registry transaction and before it commits
    // (the registry row and the file appear together — the catalog's
    // serialization point; a crash before the commit leaves an orphan
    // file no row names, never an unbacked row). The catalog's `db_file`
    // column is NOT NULL and names the auto-assigned id, so the split
    // insert is [`db::repos_create_split`] (placeholder + real name in
    // one transaction) — a plain two-column insert 500s on the split
    // schema.
    let split = state.layout == naatre_store::Layout::DbPerRepo;
    let id = state
        .write(move |store| {
            let conn = store.conn();
            let tx = conn.transaction().map_err(ApiError::log_internal)?;
            if db::repo_id_by_name(&tx, &name)
                .map_err(ApiError::log_internal)?
                .is_some()
            {
                return Err(ApiError::new(
                    ApiCode::RepoExists,
                    format!("repo {name:?} already exists"),
                ));
            }
            let (id, _db_file) = if split {
                db::repos_create_split(&tx, &name, now_ns).map_err(ApiError::log_internal)?
            } else {
                (
                    db::repos_create(&tx, &name, now_ns).map_err(ApiError::log_internal)?,
                    String::new(),
                )
            };
            if split {
                let path = naatre_store::layout::repo_db_path(std::path::Path::new(&data_dir), id);
                if let Some(parent) = path.parent() {
                    std::fs::create_dir_all(parent).map_err(|e| {
                        ApiError::log_internal(std::io::Error::other(format!("repo db dir: {e}")))
                    })?;
                }
                // The file (migrations run idempotently on open) exists
                // BEFORE the registry row commits.
                naatre_store::store::open_repo_db(&path).map_err(ApiError::log_internal)?;
            }
            db::audit_append(
                &tx,
                now_ns,
                &principal,
                "repo_create",
                &serde_json::json!({ "repo": name }).to_string(),
            )
            .map_err(ApiError::log_internal)?;
            tx.commit().map_err(ApiError::log_internal)?;
            Ok(id)
        })
        .await?;
    Ok((id, body.name))
}

/// Validates against `^[a-z0-9][a-z0-9-]{0,62}$` without a regex dep.
fn validate_repo_name(name: &str) -> Result<(), ApiError> {
    let ok = !name.is_empty()
        && name.len() <= 63
        && name.chars().enumerate().all(|(i, c)| {
            let alnum = c.is_ascii_lowercase() || c.is_ascii_digit();
            if i == 0 { alnum } else { alnum || c == '-' }
        });
    if ok {
        Ok(())
    } else {
        Err(ApiError::new(
            ApiCode::Validation,
            "repo name must match ^[a-z0-9][a-z0-9-]{0,62}$",
        ))
    }
}

/// Repo-scoped counters (admin): changeset count and this repo's object
/// membership totals (the fuller disk-usage view grows later).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/stats",
    params(("r" = String, Path, description = "Repo name")),
    responses(
        (status = 200, description = "Repo counters", body = RepoStats),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No such repo")
    )
)]
pub async fn stats(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(name): Path<String>,
) -> Result<Json<RepoStats>, ApiError> {
    // The M13b read gate first (a private repo's non-reader gets the
    // uniform 404); the admin matrix follows unchanged.
    repo_read_gate(&state, &auth, &name).await?;
    require_role(&auth, Role::Admin)?;
    // The counters split across the two connections: the changeset count
    // and the tombstone exclusion are repo-side; the `objects` sums are
    // catalog-side (chunked IN-lists over this repo's membership ids).
    let repo_for_stats = name.clone();
    let (changesets, objects, bytes, cbytes) = state
        .read_repo(repo_for_stats, move |cx| {
            let repo_id = cx.id;
            let changesets: i64 =
                db::repo_changeset_count(cx.db, repo_id).map_err(ApiError::log_internal)?;
            let excluded: std::collections::HashSet<naatre_core::ids::Id> =
                db::redaction_ids_repo(cx.db, Some(cx.id))
                    .map_err(ApiError::log_internal)?
                    .into_iter()
                    .collect();
            let members: Vec<naatre_core::ids::Id> = db::gc_repo_blobs(cx.db, repo_id)
                .map_err(ApiError::log_internal)?
                .into_iter()
                .filter(|id| !excluded.contains(id))
                .collect();
            let (objects, bytes, cbytes) =
                db::repo_object_sums(&cx.cat(), &members).map_err(ApiError::log_internal)?;
            Ok((changesets, objects, bytes, cbytes))
        })
        .await?;
    Ok(Json(RepoStats {
        changesets: changesets as u64,
        objects: objects as u64,
        bytes: bytes as u64,
        cbytes: cbytes as u64,
    }))
}
