//! Ticket minting (M13b) and the origin's ticket-scoped cache-support
//! endpoints.
//!
//! `POST /v1/repos/{r}/tickets` is THE mint surface: any authenticated
//! principal that may read the repo gets a ticket bound to exactly
//! (principal, repo, one root tree, ≤15 min). The CLIENT story runs on
//! it — a `NAATRE_CACHE_URL` client mints at fetch time for the tree it
//! is materializing and re-mints when the ticket expires — and the
//! ACL-change staleness bound runs on it: the mint checks the repo read
//! gate (a private repo's non-reader gets the uniform 404, never a
//! ticket), so a ticket can only outlive an ACL change for its remaining
//! TTL (the plan's "max ticket lifetime bounds ACL-change staleness").
//! The mint does NOT audit (mints are the hot path of every sync; the
//! audited events are the ACL changes and key rotations those tickets
//! ride on).
//!
//! The two GET endpoints under `/v1/repos/{r}/cache/…` are the origin's
//! documented data plane for the TIER (ticket-authenticated only — a
//! bearer token answers 403; the cache never holds a powerful token,
//! only the client's short tickets):
//!
//! * `GET /v1/repos/{r}/cache/membership?tree=<hex>` — the ticket's
//!   root-tree membership walk: every blob id under the tree that this
//!   repo references (`repo_blobs`, the M1 rule) and is not tombstoned
//!   in it. THE TOMBSTONE GATE AT MINT TIME lives here: a tombstoned
//!   blob never enters a membership set, so it is never in scope, never
//!   warmed, never served — and the response's tombstone epoch lets the
//!   tier detect every later tombstone event (re-fetch membership, and
//!   any post-membership redaction is caught at serve; see
//!   `crate::cache_tier`).
//! * `GET /v1/repos/{r}/cache/epoch` — the repo's tombstone epoch (the
//!   redactions row count; see [`crate::db::tombstone_epoch`]). The
//!   tier's per-store-hit revalidation probe.

use axum::extract::{Path, Query, State};
use axum::http::StatusCode;
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};

use naatre_core::ids::Id;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, CacheTicket, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::routes::repo_read_gate;
use crate::state::AppState;
use crate::ticket;

use ring::signature::KeyPair as _;

/// Request body of `POST /v1/repos/{r}/tickets`.
#[derive(Deserialize, ToSchema)]
pub struct MintTicketRequest {
    /// The root tree to bind the ticket to, 64 lowercase hex — the walk
    /// scope the tier will enforce. Must be a tree THIS repo wrote
    /// (`repo_trees` junction; snapshot trees are never ticketable).
    pub root_tree: String,
    /// Requested lifetime in seconds (default
    /// [`crate::ticket::TICKET_DEFAULT_LIFETIME_SECS`]; anything above
    /// the 15-min cap is refused 422 — the cap is enforced at mint AND
    /// at verify).
    pub ttl_secs: Option<u64>,
}

/// Response of the mint (201).
#[derive(Serialize, ToSchema)]
pub struct TicketCreated {
    /// The ticket string: `<b64url payload>.<b64url signature>.<key-id>`.
    /// Presented as `Authorization: NaatreTicket <ticket>`.
    pub ticket: String,
    /// The signing key's id (the tier's verify list must carry it).
    pub key_id: String,
    /// Expiry, ns since the epoch.
    pub expires_ns: u64,
    pub principal: String,
    pub repo: String,
    /// The bound root tree (echo).
    pub root_tree: String,
    /// The granted lifetime in seconds (echo of the effective value).
    pub lifetime_secs: u64,
}

/// Mints a cache ticket (any principal with read on the repo — the READ
/// gate runs first, so a private repo's non-reader gets the uniform 404
/// and no ticket; that is the invisibility-through-the-cache contract).
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/tickets",
    params(("r" = String, Path, description = "Repo name")),
    request_body = MintTicketRequest,
    responses(
        (status = 201, description = "Ticket minted", body = TicketCreated),
        (status = 400, description = "The root tree is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "A cache ticket is not a minting credential (bearer required)"),
        (status = 404, description = "No such repo — INCLUDING a private repo this principal cannot read (uniform 404, no ticket); or the root tree is not a tree of this repo"),
        (status = 422, description = "ttl_secs above the 15-minute cap"),
        (status = 503, description = "Ticket minting is not configured on this origin (`cache_disabled`: run `naatre cache-key init`)")
    )
)]
pub async fn mint(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<MintTicketRequest>,
) -> Result<(StatusCode, Json<TicketCreated>), ApiError> {
    // A ticket is never a minting credential (no self-perpetuating
    // tickets); bearers only.
    require_role(&auth, Role::Reader)?;
    // The READ gate: private-repo non-readers get the uniform 404 and no
    // ticket; the TTL then bounds how long any EARLIER ticket survives
    // an ACL change.
    repo_read_gate(&state, &auth, &repo).await?;
    let root_tree = Id::from_hex(&body.0.root_tree).ok_or_else(|| {
        ApiError::new(
            ApiCode::Malformed,
            "root_tree must be 64 lowercase hex characters",
        )
    })?;
    let ttl_secs = body
        .0
        .ttl_secs
        .unwrap_or(ticket::TICKET_DEFAULT_LIFETIME_SECS);
    if ttl_secs > ticket::TICKET_MAX_LIFETIME_SECS {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!(
                "ttl_secs above the {}s cap (tickets never outlive {} minutes)",
                ticket::TICKET_MAX_LIFETIME_SECS,
                ticket::TICKET_MAX_LIFETIME_SECS / 60
            ),
        ));
    }
    // The tree must be one this repo wrote (history trees carry junction
    // rows; a foreign or snapshot tree id is not ticketable) — uniform
    // 404: a non-member tree names nothing about the repo.
    let repo_for_tree = repo.clone();
    let is_tree = state
        .read_repo(repo_for_tree, move |cx| {
            db::repo_tree_member(cx.db, cx.id, &root_tree).map_err(ApiError::log_internal)
        })
        .await?;
    if !is_tree {
        return Err(ApiError::new(
            ApiCode::ObjectNotFound,
            format!("tree {} is not a tree of {:?}", root_tree.to_hex(), repo),
        ));
    }
    // The signing key: loaded per mint (rotation takes effect without a
    // restart; a data dir without keys answers the honest 503).
    let data_dir = state.data_dir_display.clone();
    let signing = {
        tokio::task::spawn_blocking(move || {
            ticket::load_signing_key(&data_dir).map_err(|_| {
                ApiError::new(
                    ApiCode::CacheDisabled,
                    "ticket minting is not configured on this origin (run `naatre cache-key init`)",
                )
            })
        })
        .await
        .map_err(|join| ApiError::log_internal(format!("key load task panicked: {join}")))??
    };
    let now_ns = state.now_ns();
    let mut tid = [0u8; 16];
    getrandom::fill(&mut tid).map_err(|_| ApiError::internal())?;
    let tid: String = tid.iter().map(|b| format!("{b:02x}")).collect();
    let claims = ticket::TicketClaims {
        v: 1,
        tid,
        principal: auth.principal.clone(),
        repo: repo.clone(),
        root_tree: root_tree.to_hex(),
        iat: now_ns,
        exp: now_ns.saturating_add(ttl_secs.saturating_mul(1_000_000_000)),
    };
    let key_id = ticket::key_id(signing.public_key().as_ref());
    let t = ticket::mint_ticket(&claims, &signing);
    Ok((
        StatusCode::CREATED,
        Json(TicketCreated {
            ticket: t,
            key_id,
            expires_ns: claims.exp,
            principal: claims.principal,
            repo: claims.repo,
            root_tree: claims.root_tree,
            lifetime_secs: ttl_secs,
        }),
    ))
}

/// Query params of the membership endpoint.
#[derive(Deserialize, IntoParams)]
pub struct MembershipQuery {
    /// The ticket's root tree (must MATCH the ticket's binding — the walk
    /// is only ever served for the ticket's own scope).
    pub tree: String,
}

/// Membership response: the walk's blob ids and the repo's tombstone
/// epoch at walk time.
#[derive(Serialize, ToSchema)]
pub struct MembershipResponse {
    /// The tombstone epoch (redactions row count) at walk time — the
    /// tier stores it with the set and re-probes it per store-hit serve.
    pub epoch: i64,
    /// Blob ids, 64 lowercase hex each: the ticket's in-scope set.
    pub blobs: Vec<String>,
}

/// The ticket's root-tree membership walk (TICKET-ONLY: a bearer token
/// answers 403 — this is the tier's data plane, and the tier holds
/// tickets, not tokens). The walk excludes tombstoned ids — the tombstone
/// gate at mint/scope time: a tombstoned blob never enters any
/// membership set, so it can never be warmed or served from scope.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/cache/membership",
    params(("r" = String, Path, description = "Repo name (must match the ticket)"), MembershipQuery),
    responses(
        (status = 200, description = "The ticket-scoped membership set + the tombstone epoch", body = MembershipResponse),
        (status = 401, description = "Missing, invalid, or expired ticket"),
        (status = 403, description = "Bearer-token callers: this surface is ticket-only"),
        (status = 404, description = "The repo does not match the ticket, or the tree does not match the ticket's root tree")
    )
)]
pub async fn membership(
    state: State<AppState>,
    ticket_auth: Option<Extension<CacheTicket>>,
    Path(repo): Path<String>,
    Query(q): Query<MembershipQuery>,
) -> Result<Json<MembershipResponse>, ApiError> {
    let t = require_ticket(&ticket_auth)?;
    if t.repo != repo {
        return Err(ApiError::new(
            ApiCode::RepoNotFound,
            format!("no repo named {repo:?} on this server"),
        ));
    }
    let tree = Id::from_hex(&q.tree).ok_or_else(|| {
        ApiError::new(
            ApiCode::Malformed,
            "tree must be 64 lowercase hex characters",
        )
    })?;
    if tree != t.root_tree {
        return Err(ApiError::new(
            ApiCode::ObjectNotFound,
            "the requested tree is not this ticket's root tree",
        ));
    }
    let repo_for_walk = repo.clone();
    let (epoch, blobs) = state
        .read_repo(repo_for_walk, move |cx| {
            let epoch = db::tombstone_epoch(cx.db, cx.id).map_err(ApiError::log_internal)?;
            let blobs =
                db::cache_membership(cx.db, cx.id, &tree).map_err(ApiError::log_internal)?;
            Ok((epoch, blobs))
        })
        .await?;
    Ok(Json(MembershipResponse {
        epoch,
        blobs: blobs.iter().map(Id::to_hex).collect(),
    }))
}

/// The repo's tombstone epoch (TICKET-ONLY). The tier's store-hit serve
/// re-probes this BEFORE serving cached bytes: a changed epoch means a
/// tombstone event since the membership walk — the tier drops its
/// snapshot, refetches membership, and the next content fetch goes
/// through the origin's 410 gate. This is the "tombstone epoch
/// revalidated at serve" mechanism (M10's clause carried to the tier).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/cache/epoch",
    params(("r" = String, Path, description = "Repo name (must match the ticket)")),
    responses(
        (status = 200, description = "The repo's tombstone epoch", body = TombstoneEpoch),
        (status = 401, description = "Missing, invalid, or expired ticket"),
        (status = 403, description = "Bearer-token callers: this surface is ticket-only"),
        (status = 404, description = "The repo does not match the ticket")
    )
)]
pub async fn epoch(
    state: State<AppState>,
    ticket_auth: Option<Extension<CacheTicket>>,
    Path(repo): Path<String>,
) -> Result<Json<TombstoneEpoch>, ApiError> {
    let t = require_ticket(&ticket_auth)?;
    if t.repo != repo {
        return Err(ApiError::new(
            ApiCode::RepoNotFound,
            format!("no repo named {repo:?} on this server"),
        ));
    }
    let repo_for_epoch = repo.clone();
    let epoch = state
        .read_repo(repo_for_epoch, move |cx| {
            db::tombstone_epoch(cx.db, cx.id).map_err(ApiError::log_internal)
        })
        .await?;
    Ok(Json(TombstoneEpoch { epoch }))
}

/// The epoch wire shape.
#[derive(Serialize, ToSchema)]
pub struct TombstoneEpoch {
    pub epoch: i64,
}

/// The ticket-only extraction: handlers on the tier's data plane refuse
/// bearer callers with 403 (they authenticate as roles ABOVE the ticket
/// role — this surface is not theirs; the cache presents tickets).
fn require_ticket(t: &Option<Extension<CacheTicket>>) -> Result<&CacheTicket, ApiError> {
    t.as_ref().map(|ext| &ext.0).ok_or_else(|| {
        ApiError::new(
            ApiCode::Forbidden,
            "this surface takes a cache ticket (NaatreTicket), not a bearer token",
        )
    })
}
