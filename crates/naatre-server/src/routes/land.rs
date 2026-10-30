//! `/v1/repos/{r}/land`: server-side rebase + CAS of `main`, and the
//! per-key land status read. Writer+ per the route matrix (reader → 403,
//! anon → 401); path scopes gate the rebase chain inside the write
//! transaction — the chain's change paths are only known once the
//! transaction has loaded them.
//!
//! In-flight semantics (see [`crate::land`], module doc): the single-writer
//! slot answers a concurrent land 503 `writer_busy` before the transaction;
//! the same key then replays the settled result on retry.

use std::sync::Arc;

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::land::{self, LandCommand, LandDone};
use crate::routes::repo_id_or_404;
use crate::state::AppState;

/// Request body of land.
#[derive(Deserialize, ToSchema)]
pub struct LandRequest {
    /// Head changeset id (64 lowercase hex) to land onto `main`.
    pub head: String,
    /// Idempotency key, scoped to (repo, principal). A settled key with the
    /// same head replays its recorded result; with a different head → 409
    /// `idempotency_key_settled`. After a conflict (which leaves the key
    /// unsettled) the same key re-lands.
    pub key: String,
    /// Advisory base hint: ignored for the rebase (the server always
    /// rebases onto head's merge-base with main); when present and not
    /// head's parent, the response carries a note.
    pub base: Option<String>,
}

/// Response of a successful land.
#[derive(Serialize, ToSchema)]
pub struct LandResponse {
    /// The changeset main points at after the land (the rebased head when
    /// the chain was rewritten).
    pub landed: String,
    /// The `main` ref target after the land (== `landed`).
    pub main: String,
    /// How many changesets were rebased (rewritten with new ids).
    pub rebased: u64,
    /// Present when the advisory `base` was not head's parent.
    pub note: Option<String>,
}

/// The recorded result of a settled land.
#[derive(Serialize, ToSchema)]
pub struct LandStatusResult {
    pub landed: String,
    pub rebased: u64,
}

/// Response of the land status read.
#[derive(Serialize, ToSchema)]
pub struct LandStatusResponse {
    pub key: String,
    /// `done` (a committed pending row is unobservable — single writer).
    pub state: String,
    /// The head the key was landed with.
    pub head: Option<String>,
    pub result: Option<LandStatusResult>,
}

/// Lands `head` onto `main` (writer+): server-side rebase of head's
/// first-parent chain onto main inside one write transaction, then a CAS
/// ref move. Idempotent by (repo, principal, key).
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/land",
    params(("r" = String, Path, description = "Repo name")),
    request_body = LandRequest,
    responses(
        (status = 200, description = "Land settled (a fresh land, an idempotent replay, or an already-contained head)", body = LandResponse),
        (status = 400, description = "head/base is not 64 lowercase hex, or the body is not readable JSON"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Without the writer role, or a rebase-chain path outside the token's scope (details.path names it)"),
        (status = 404, description = "No such repo, or head is not a changeset of this repo"),
        (status = 409, description = "ref_conflict: the rebase hit conflicting paths (details.conflicts + details.recipe) — the key stays unsettled; or idempotency_key_settled: the key is settled with a different head"),
        (status = 413, description = "A rebased changeset or merged tree exceeds the 16 MiB canonical cap"),
        (status = 422, description = "The repo has no main ref, or head shares no history with main"),
        (status = 503, description = "writer_busy: the single-writer slot is busy; retry with backoff (Retry-After)")
    )
)]
pub async fn land(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<LandRequest>,
) -> Result<Json<LandResponse>, ApiError> {
    require_role(&auth, Role::Writer)?;
    let body = body.0;

    let head = parse_id(&body.head)?;
    let base_hint = match body.base.as_deref() {
        Some(hex) => Some(parse_id(hex)?),
        None => None,
    };
    if body.key.is_empty() {
        return Err(ApiError::new(
            ApiCode::Validation,
            "land key must not be empty",
        ));
    }
    if body.key.len() > 256 {
        return Err(ApiError::new(
            ApiCode::Validation,
            "land key must be at most 256 bytes",
        ));
    }

    let cmd = LandCommand {
        repo_name: repo.clone(),
        principal: auth.principal.clone(),
        scope: auth.scope_path.clone(),
        head,
        key: body.key.clone(),
        base_hint,
        now_ns: state.now_ns() as i64,
    };
    let cas = Arc::clone(&state.cas);
    let done = state
        .write(move |store| land::execute(store, &cas, &cmd))
        .await?;
    Ok(Json(LandResponse::from(done)))
}

/// The land status for one key, scoped to the authenticated principal
/// (writer+; keys are per-principal).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/land/{key}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("key" = String, Path, description = "Land idempotency key")
    ),
    responses(
        (status = 200, description = "The land status (done + recorded result)", body = LandStatusResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Without the writer role"),
        (status = 404, description = "No such repo, or no land for this principal + key")
    )
)]
pub async fn land_status(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, key)): Path<(String, String)>,
) -> Result<Json<LandStatusResponse>, ApiError> {
    require_role(&auth, Role::Writer)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let principal = auth.principal.clone();
    let key_for_lookup = key.clone();
    let row = state
        .read(move |store| {
            db::land_get(store.conn(), repo_id, &principal, &key_for_lookup)
                .map_err(ApiError::log_internal)
        })
        .await?;
    let Some(row) = row else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!("no land status for key {key:?}"),
        ));
    };
    let (result, head_hex) = (
        row.result_json
            .as_deref()
            .and_then(parse_result)
            .map(|(landed, rebased)| LandStatusResult { landed, rebased }),
        row.head.as_deref().and_then(id_hex),
    );
    Ok(Json(LandStatusResponse {
        key,
        state: row.state,
        head: head_hex,
        result,
    }))
}

impl From<LandDone> for LandResponse {
    fn from(done: LandDone) -> Self {
        LandResponse {
            landed: done.landed.to_hex(),
            main: done.main.to_hex(),
            rebased: done.rebased,
            note: done.note,
        }
    }
}

fn parse_id(hex: &str) -> Result<Id, ApiError> {
    Id::from_hex(hex)
        .ok_or_else(|| ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters"))
}

/// `(landed, rebased)` out of a recorded result JSON.
fn parse_result(json: &str) -> Option<(String, u64)> {
    let value: serde_json::Value = serde_json::from_str(json).ok()?;
    let landed = value["landed"].as_str()?.to_string();
    let rebased = value["rebased"].as_u64()?;
    Some((landed, rebased))
}

fn id_hex(bytes: &[u8]) -> Option<String> {
    <[u8; 32]>::try_from(bytes).ok().map(|a| Id(a).to_hex())
}
