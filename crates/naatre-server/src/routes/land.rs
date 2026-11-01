//! `/v1/repos/{r}/land`: server-side rebase + CAS of `main`, and the
//! per-key land status read. Writer+ per the route matrix (reader → 403,
//! anon → 401); path scopes gate the rebase chain inside the write
//! transaction — the chain's change paths are only known once the
//! transaction has loaded them.
//!
//! In-flight semantics (see [`crate::land_join`]): a concurrent land on the
//! same (repo, principal, key) JOINS the in-flight one instead of
//! contending for the write slot. Same key and same head → the identical
//! settled outcome (200, one settlement). Same key, different head → await
//! it, then the settled rules: the settled-rules 409
//! `idempotency_key_settled` with the settled head in details — UNLESS this
//! request's own head equals the settled head named in the leader's 409
//! (the leader's 409 describes the LEADER's head mismatch, not the
//! joiner's), in which case the joiner replays the settled row itself and
//! gets the identical 200. A different key finds the write slot contended
//! only while another WRITE is in flight and is answered 503
//! `writer_busy` before the transaction; the same key then replays the
//! settled result on retry.
//!
//! The registry is in-memory, held outside the store mutex. The join wait
//! is bounded by the join deadline ([`crate::state::JOIN_DEADLINE`],
//! independent of the shutdown drain timeout; expiry → retryable 503,
//! landing the retry on the settled-row replay). On the production router
//! the identical-key join holds end to end: auth lookups and every read go
//! through the server's read pool ([`crate::state::ServerState::read`]) and
//! never contend for the write slot, so two concurrent identical lands both
//! get their 200 from one settlement — no 503 is possible on this path.
//! The join path's only store touch is the settled-row replay read, which
//! also goes through the pool.

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
use crate::land_join::{InFlightLand, JoinSlot, LandOutcome};
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
    /// unsettled) the same key re-lands. While a land with this key is in
    /// flight, a concurrent request joins it (see the module doc).
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
        (status = 409, description = "ref_conflict: the rebase hit conflicting paths (details.conflicts + details.recipe) — the key stays unsettled; or idempotency_key_settled: the key is settled with a different head; or redacted_base: the base chain crosses a redacted changeset (details.redacted names it)"),
        (status = 410, description = "The rebase body is tombstoned in this repo (`object_redacted`, active or purged): the reproduced changeset id, or a blob id the rebase writes or references — the key stays unsettled and no rows are written"),
        (status = 413, description = "A rebased changeset or merged tree exceeds the 16 MiB canonical cap"),
        (status = 422, description = "The repo has no main ref, or head shares no history with main"),
        (status = 503, description = "writer_busy: the single-writer slot is busy with another write (never a read — reads run on the read pool), or an in-flight same-key join outlived the join deadline; retry with backoff (Retry-After)")
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

    // In-flight join (M4c): a concurrent land on the same (repo, principal,
    // key) is joined instead of contending for the write slot — see the
    // module doc. The registry is in-memory only: no store touch before
    // `state.write`, which would turn the 503 backpressure into a block.
    let lead = match state
        .land_joins
        .begin(&repo, &auth.principal, &body.key, head)
    {
        JoinSlot::Join(in_flight) => {
            return join_in_flight(&state, &repo, &auth.principal, &body.key, head, in_flight)
                .await;
        }
        JoinSlot::Lead(lead) => lead,
    };

    let cmd = LandCommand {
        repo_name: repo.clone(),
        principal: auth.principal.clone(),
        scope: auth.scope_path.clone(),
        head,
        key: body.key.clone(),
        base_hint,
        now_ns: state.now_ns() as i64,
        clock: Arc::clone(&state.clock),
        walk_deadline: state.derive_walk_deadline.as_nanos() as u64,
    };
    let cas = Arc::clone(&state.cas);
    let derive = Arc::clone(&state.derive);
    let result = state
        .write(move |store| land::execute(store, cas.as_ref(), &derive, &cmd))
        .await;
    // Share the outcome with joiners, then deregister (Drop). The write slot
    // is free again by the time this runs: a joiner sees the entry with its
    // outcome set, or finds it gone and replays the settled row.
    lead.publish(match &result {
        Ok(done) => LandOutcome::Done(done.clone()),
        Err(e) => LandOutcome::Err(e.clone()),
    });
    result.map(|done| Json(LandResponse::from(done)))
}

/// Awaits an in-flight land on the same (repo, principal, key) and applies
/// the settled rules to THIS request's head: the identical outcome for the
/// same head; for a different one the settled-rules 409 — unless the
/// leader's outcome was ITSELF a settled-409 whose settled head equals this
/// request's head, in which case this request replays the settled row and
/// gets the identical 200 (a leader's 409 describes the leader's head
/// mismatch, not the joiner's); otherwise the leader's own error when its
/// land did not settle. The wait is bounded by the join deadline
/// (`state.join_deadline`); on expiry the request is answered 503
/// `writer_busy`, so the retry lands on the settled-row replay. The only
/// store access is the fallback's settled-row replay read (via the pool).
async fn join_in_flight(
    state: &AppState,
    repo: &str,
    principal: &str,
    key: &str,
    head: Id,
    in_flight: Arc<InFlightLand>,
) -> Result<Json<LandResponse>, ApiError> {
    // The watch Ref (an RwLockReadGuard, !Send) must not live past this
    // block: the settled-head fallback below awaits while holding only the
    // cloned outcome.
    let outcome = {
        let mut rx = in_flight.subscribe();
        let waited = tokio::time::timeout(state.join_deadline, rx.wait_for(|o| o.is_some())).await;
        match waited {
            Ok(Ok(slot)) => slot
                .as_ref()
                .expect("wait_for resolves only on Some")
                .clone(),
            // The wait expired (or the sender is gone with the entry already
            // deregistered): retryable 503 per the single-writer convention.
            _ => {
                return Err(ApiError::new(
                    ApiCode::WriterBusy,
                    "land for this key is still in flight",
                )
                .retryable(true));
            }
        }
    };
    match outcome {
        LandOutcome::Done(done) if in_flight.head == head => Ok(Json(LandResponse::from(done))),
        LandOutcome::Done(_) => Err(settled_with_other_head(repo, key, in_flight.head)),
        LandOutcome::Err(e) => {
            if e.code() == ApiCode::IdempotencyKeySettled && settled_head_of(&e) == Some(head) {
                return settled_replay(state, repo, principal, key, head).await;
            }
            Err(e)
        }
    }
}

/// The settled head named in a leader's `idempotency_key_settled` 409, if
/// it parses.
fn settled_head_of(e: &ApiError) -> Option<Id> {
    e.details()
        .get("settled_head")
        .and_then(|v| v.as_str())
        .and_then(Id::from_hex)
}

/// The joiner's own settled-row replay: the leader refused with a 409 about
/// ITS head, but this request's head IS the settled head, so the settled
/// rules hand it the recorded result (200). Read-only, via the pool — the
/// leader's 409 proves the settled row existed (and rows are never
/// deleted), so a missing or mismatching row here is a broken invariant.
async fn settled_replay(
    state: &AppState,
    repo: &str,
    principal: &str,
    key: &str,
    head: Id,
) -> Result<Json<LandResponse>, ApiError> {
    let repo_id = repo_id_or_404(state, repo).await?;
    let (principal, key) = (principal.to_string(), key.to_string());
    let row = state
        .read(move |conn| {
            db::land_get(conn, repo_id, &principal, &key).map_err(ApiError::log_internal)
        })
        .await?;
    let Some(row) =
        row.filter(|row| row.state == "done" && row.head.as_deref() == Some(head.0.as_ref()))
    else {
        return Err(ApiError::internal());
    };
    land::replay(row.result_json).map(|done| Json(LandResponse::from(done)))
}

/// The settled-rules 409 for a joiner whose head differs from the head the
/// key settled with (same shape as the settled-row path in
/// [`crate::land`]).
fn settled_with_other_head(repo: &str, key: &str, settled_head: Id) -> ApiError {
    ApiError::new(
        ApiCode::IdempotencyKeySettled,
        "this land key is settled with a different head",
    )
    .with_details(serde_json::json!({
        "settled_head": settled_head.to_hex(),
        "land_status_pointer": format!("/v1/repos/{repo}/land/{key}"),
    }))
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
        .read(move |conn| {
            db::land_get(conn, repo_id, &principal, &key_for_lookup).map_err(ApiError::log_internal)
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
