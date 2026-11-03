//! The `naatre cache` read tier (M13b): a LAN-side cache for immutable
//! by-id object reads, ticket-gated, origin-backed.
//!
//! Surface: `GET /v1/objects/{id}` — the ONLY data route. Refs, logs,
//! changesets, everything else: refused (404 via the router fallback,
//! 405 via axum's method router) and NEVER proxied to the origin. Every
//! request's ticket is verified — signature by key-id against the local
//! verify list, the lifetime cap, expiry — BEFORE any byte is read from
//! the local store or fetched from the origin (verification is LOCAL:
//! the tier's own key list and clock; the origin is not consulted).
//!
//! The scope proof is the M1 `repo_blobs` rule enforced at the tier: the
//! tier holds, per (repo, root-tree), the origin's membership walk (see
//! [`crate::routes::tickets::membership`]) — every blob under the
//! ticket's tree that the repo references and does not tombstone. An id
//! outside that set is refused with the uniform object-not-found 404
//! BEFORE the store is touched and BEFORE any origin fetch: a
//! warm-blob-outside-ticket-scope answer is a refusal, never a serve,
//! and negative answers are NEVER stored (negative caching forbidden —
//! every miss's authority is the origin's, fetched per request).
//!
//! Honesty mechanisms, per the plan's M13b line:
//!
//! * **Tombstone epoch revalidated at serve** (the M10 clause): on every
//!   serve of an in-scope id the tier probes the origin's epoch endpoint;
//!   a changed epoch (a tombstone event since the walk) drops the
//!   snapshot, refetches membership, and — for an id that vanished from
//!   the fresh set — delegates to the origin's own fetch so the client
//!   sees the origin's exact 410 `object_redacted`. The tier never
//!   serves what the origin would refuse: the tombstone never reaches
//!   the tier as content (the mint-time membership walk excludes active
//!   tombstones; the per-serve epoch probe catches later marks; the
//!   origin's 410 gates every fetch; a 410 also DELETES any local copy
//!   — the erasure duty at the tier). HONEST RACE WINDOW: the probe and
//!   the serve are two steps — a tombstone landing at the origin BETWEEN
//!   the epoch probe and the bytes leaving is caught on the NEXT serve
//!   of that id (the epoch will differ then), not this one: a
//!   one-request window of the same class as the origin's own read
//!   concurrency, never an un-revalidated answer.
//! * **Corrupt local store → origin fallback**: the local CAS verifies
//!   every read's hash ([`naatre_store::Cas::get`]); a mismatch is
//!   refetched from the origin, re-verified, and rewritten. A cache
//!   never serves bytes it cannot hash-verify.
//! * **The tier is a bandwidth cache, not an availability tier**: an
//!   origin outage answers 503 `origin_unreachable` (fail-closed — the
//!   epoch revalidation is what makes tombstone honesty possible, so it
//!   is never skipped to serve a hit).
//!
//! Startup is fail-closed: no verify keys, no serve (the host verb
//! loads them before the listener binds).

use std::collections::{HashMap, HashSet};
use std::io;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use axum::Router;
use axum::extract::{Path, State};
use axum::http::{HeaderName, HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::get;

use naatre_core::ids::Id;
use naatre_store::Cas;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::ticket::{TicketClaims, TicketError, VerifyKeys};

/// Graceful-shutdown drain budget (shorter than the origin's 30 s: the
/// tier holds no writes, only in-flight reads).
const DRAIN_TIMEOUT: Duration = Duration::from_secs(10);

/// Shared cache-tier state.
pub struct CacheState {
    /// The origin's verify keys (current + retired) — the whole trust
    /// anchor, loaded fail-closed at startup.
    keys: VerifyKeys,
    /// The origin client (miss fetches, membership, epoch probes).
    origin: OriginClient,
    /// The tier's own content-addressed store (the `objects/` dir of the
    /// cache's data dir — no database, blobs only). `Arc` so blocking
    /// closures can own a handle (the CAS is stateless path arithmetic;
    /// concurrent gets need no lock).
    cas: Arc<Cas>,
    /// The cache data dir (for the rare second CAS handle: the 410
    /// erasure delete).
    data_dir: PathBuf,
    /// Membership snapshots per (repo, root-tree): `{epoch, blobs}`. A
    /// std mutex — locked and released inside synchronous steps, never
    /// across an `.await` (the same discipline as the server's
    /// auth-failure buckets).
    membership: Mutex<HashMap<(String, Id), MembershipSnapshot>>,
}

struct MembershipSnapshot {
    /// The repo's tombstone epoch at walk time.
    epoch: i64,
    blobs: HashSet<Id>,
    /// The walk's fetch time (the bound's clock — see
    /// [`MEMBERSHIP_TTL`] and [`MEMBERSHIP_MAX_ENTRIES`]).
    fetched_ns: u64,
}

/// The membership map's bounds (M13b-fix qa A1 / compliance A2 /
/// senior-dev A1): an entry older than [`MEMBERSHIP_TTL`] is stale
/// regardless of epoch (the tier refetches it on next use), and the map
/// is capped at [`MEMBERSHIP_MAX_ENTRIES`] entries — inserting past the
/// cap evicts the OLDEST-fetched entries first (a multi-repo tier
/// cannot grow its map without bound). Bounded memory; correctness is
/// untouched (an evicted snapshot simply refetches).
const MEMBERSHIP_TTL_NS: u64 = 3600 * 1_000_000_000;
const MEMBERSHIP_MAX_ENTRIES: usize = 256;

/// Evicts stale + oldest entries to keep the map at or under `cap`.
fn bound_membership(map: &mut HashMap<(String, Id), MembershipSnapshot>, now_ns: u64, cap: usize) {
    map.retain(|_, snap| now_ns.saturating_sub(snap.fetched_ns) < MEMBERSHIP_TTL_NS);
    if map.len() <= cap {
        return;
    }
    let mut order: Vec<((String, Id), u64)> =
        map.iter().map(|(k, s)| (k.clone(), s.fetched_ns)).collect();
    order.sort_by_key(|(_, fetched)| *fetched);
    let excess = map.len() - cap;
    for (key, _) in order.into_iter().take(excess) {
        map.remove(&key);
    }
}

impl CacheState {
    /// Builds the tier state. Fails closed on an empty key list (the
    /// caller — the host verb — surfaces the message and exits before
    /// binding).
    pub fn new(
        keys: VerifyKeys,
        origin_url: &str,
        cache_data: &std::path::Path,
    ) -> Result<Arc<CacheState>, String> {
        if keys.keys().is_empty() {
            return Err("no origin verify keys: refusing to start (fail-closed); \
                        pass --key <cache_keys.json>"
                .into());
        }
        let data = naatre_store::DataDir::create(cache_data)
            .map_err(|e| format!("cache data dir {}: {e}", cache_data.display()))?;
        let cas = Cas::new(&data).map_err(|e| format!("cache store: {e}"))?;
        Ok(Arc::new(CacheState {
            keys,
            origin: OriginClient::new(origin_url),
            cas: Arc::new(cas),
            data_dir: cache_data.to_path_buf(),
            membership: Mutex::new(HashMap::new()),
        }))
    }

    fn now_ns(&self) -> u64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0)
    }

    fn lock_membership(
        &self,
    ) -> std::sync::MutexGuard<'_, HashMap<(String, Id), MembershipSnapshot>> {
        self.membership
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
    }
}

/// The tier's router: the by-id object read, an ops health route, and
/// the envelope-shaped fallback.
pub fn cache_router(state: Arc<CacheState>) -> Router {
    let v1 = Router::new()
        .route("/v1/objects/{id}", get(get_object))
        .with_state(Arc::clone(&state));
    Router::new()
        .route("/healthz", get(healthz))
        .merge(v1)
        .fallback(fallback)
        .with_state(state)
}

async fn healthz() -> &'static str {
    "ok"
}

async fn fallback() -> ApiError {
    // Refs/log/anything-else: refused, never proxied. The body names the
    // tier's whole surface so a misrouted client self-corrects.
    ApiError::new(
        ApiCode::NotFound,
        "the cache tier serves GET /v1/objects/{id} only (refs, logs, and \
         writes are never proxied; point them at the origin)",
    )
}

/// GET /v1/objects/{id} — the tier's whole data surface.
async fn get_object(
    State(state): State<Arc<CacheState>>,
    Path(id_hex): Path<String>,
    req: axum::extract::Request,
) -> Result<Response, ApiError> {
    // 1. The ticket, verified BEFORE any byte: signature (by key-id),
    //    lifetime cap, expiry — against the tier's own key list and
    //    clock.
    let ticket_header = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.split_once(' '))
        .filter(|(scheme, _)| scheme.eq_ignore_ascii_case(crate::ticket::TICKET_SCHEME))
        .map(|(_, value)| value.trim().to_string());
    let Some(ticket_str) = ticket_header else {
        return Err(ApiError::new(
            ApiCode::TokenInvalid,
            "the cache tier takes a NaatreTicket cache ticket",
        ));
    };
    let claims: TicketClaims =
        match crate::ticket::verify_ticket(&ticket_str, &state.keys, state.now_ns()) {
            Ok(claims) => claims,
            Err(TicketError::Expired) => {
                return Err(ApiError::new(
                    ApiCode::TokenExpired,
                    "cache ticket is expired",
                ));
            }
            Err(TicketError::Invalid) => {
                return Err(ApiError::new(
                    ApiCode::TokenInvalid,
                    "invalid cache ticket (signature, key-id, shape, or lifetime above the cap)",
                ));
            }
        };
    let repo = claims.repo.clone();
    let root_tree = Id::from_hex(&claims.root_tree)
        .ok_or_else(|| ApiError::new(ApiCode::TokenInvalid, "ticket binds a malformed tree id"))?;

    // 2. The id.
    let id = Id::from_hex(&id_hex).ok_or_else(|| {
        ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters")
    })?;

    // 3. Scope: the membership walk with the tombstone-epoch
    //    revalidation (the module doc carries the exact flow).
    match scope_check(&state, &repo, &root_tree, &id, &ticket_str).await? {
        ScopeOutcome::OutOfScope => {
            // The uniform refusal: indistinguishable from a non-member at
            // the origin (the cross-repo 404 shape). Nothing stored,
            // nothing fetched — negative caching is forbidden.
            Err(ApiError::new(
                ApiCode::ObjectNotFound,
                format!("object {id_hex} is not in this ticket's scope"),
            ))
        }
        ScopeOutcome::DelegateToOrigin => {
            // An in-scope-yesterday id that vanished from the fresh walk:
            // the origin's own gate names it exactly (410 redacted vs 404
            // gone) and the tier never answers from guesswork.
            let out = fetch_passthrough(&state, &repo, &id, &ticket_str).await;
            // Erasure duty at the tier: a redacted id's local copy
            // (warmed before the mark) is deleted on the 410.
            if out.as_ref().is_ok_and(|r| r.status() == StatusCode::GONE)
                || out
                    .as_ref()
                    .is_err_and(|e| e.code() == ApiCode::ObjectRedacted)
            {
                delete_local(&state, &id).await;
            }
            out
        }
        ScopeOutcome::InScope => {
            // 4. Serve: local (hash-verified) or origin fallback.
            serve_in_scope(&state, &repo, &id, &ticket_str).await
        }
    }
}

enum ScopeOutcome {
    InScope,
    OutOfScope,
    DelegateToOrigin,
}

/// The scope check with the tombstone revalidation. Per-hit cost: one
/// epoch probe whenever a snapshot stands (the documented cost row —
/// this is what makes "a tombstoned id never serves" unconditional).
async fn scope_check(
    state: &Arc<CacheState>,
    repo: &str,
    root_tree: &Id,
    id: &Id,
    ticket: &str,
) -> Result<ScopeOutcome, ApiError> {
    let had_snapshot = state
        .lock_membership()
        .contains_key(&(repo.to_string(), *root_tree));
    if !had_snapshot {
        let mut snap = fetch_membership(state, repo, root_tree, ticket).await?;
        snap.fetched_ns = state.now_ns();
        let in_scope = snap.blobs.contains(id);
        let mut map = state.lock_membership();
        bound_membership(&mut map, state.now_ns(), MEMBERSHIP_MAX_ENTRIES);
        map.insert((repo.to_string(), *root_tree), snap);
        return Ok(if in_scope {
            ScopeOutcome::InScope
        } else {
            ScopeOutcome::OutOfScope
        });
    }
    // A snapshot stands: revalidate the tombstone epoch BEFORE any byte.
    let recorded = state
        .lock_membership()
        .get(&(repo.to_string(), *root_tree))
        .map(|s| s.epoch)
        .expect("checked above");
    let current = state
        .origin
        .epoch(repo, ticket)
        .await
        .map_err(|e| e.into_api_error())?;
    if current == recorded {
        let in_scope = state
            .lock_membership()
            .get(&(repo.to_string(), *root_tree))
            .map(|s| s.blobs.contains(id))
            .expect("snapshot stands");
        return Ok(if in_scope {
            ScopeOutcome::InScope
        } else {
            ScopeOutcome::OutOfScope
        });
    }
    // The epoch moved: a tombstone event since the walk. Fresh walk.
    let mut snap = fetch_membership(state, repo, root_tree, ticket).await?;
    snap.fetched_ns = state.now_ns();
    let in_scope = snap.blobs.contains(id);
    let mut map = state.lock_membership();
    bound_membership(&mut map, state.now_ns(), MEMBERSHIP_MAX_ENTRIES);
    map.insert((repo.to_string(), *root_tree), snap);
    Ok(if in_scope {
        ScopeOutcome::InScope
    } else {
        // Vanished from the fresh set: delegate (the origin names it).
        ScopeOutcome::DelegateToOrigin
    })
}

/// Fetches the origin's membership walk for (repo, tree).
async fn fetch_membership(
    state: &Arc<CacheState>,
    repo: &str,
    root_tree: &Id,
    ticket: &str,
) -> Result<MembershipSnapshot, ApiError> {
    let (epoch, blobs) = state
        .origin
        .membership(repo, root_tree, ticket)
        .await
        .map_err(|e| e.into_api_error())?;
    Ok(MembershipSnapshot {
        epoch,
        blobs: blobs.into_iter().collect(),
        fetched_ns: 0, // stamped by the caller (its own clock read)
    })
}

/// Serves an in-scope id: local CAS first (hash-verified by the CAS
/// itself), origin fallback on a missing or corrupt local copy (the
/// corrupt bytes are overwritten by the verified re-fetch — the cache
/// never serves bytes it cannot hash-verify).
async fn serve_in_scope(
    state: &Arc<CacheState>,
    repo: &str,
    id: &Id,
    ticket: &str,
) -> Result<Response, ApiError> {
    let cas = Arc::clone(&state.cas);
    let owned_id = *id;
    let local = tokio::task::spawn_blocking(move || cas.get(&owned_id))
        .await
        .map_err(|join| ApiError::log_internal(format!("cache cas task panicked: {join}")))?;
    match local {
        Ok(bytes) => Ok(bytes_response(&bytes)),
        // ANY local-read failure is the corrupt-store case: the shard is
        // missing, does not hash to its id, fails its zstd frame, or the
        // filesystem errored — every shape means "this copy cannot be
        // trusted", and the answer is the same origin fallback (fetch,
        // hash-verify, rewrite). A cache never serves bytes it cannot
        // verify, and never answers 500 for a recoverable local fault.
        Err(e) => {
            tracing::warn!(
                id = %id.to_hex(),
                error = %e,
                "cache local read failed: refetching from origin"
            );
            let bytes = fetch_and_store(state, repo, id, ticket).await?;
            Ok(bytes_response(&bytes))
        }
    }
}

/// The 410 erasure delete: drops the local copy of a redacted id.
async fn delete_local(state: &Arc<CacheState>, id: &Id) {
    // A second handle over the same dirs (the CAS is path arithmetic;
    // the constructor's ensure_dir calls are existence checks here).
    let data = naatre_store::DataDir::open_any(state.data_dir.clone());
    let cas = match Cas::new(&data) {
        Ok(cas) => Arc::new(cas),
        Err(e) => {
            tracing::warn!(error = %e, "cache erasure delete could not open its store");
            return;
        }
    };
    let owned_id = *id;
    let _ = tokio::task::spawn_blocking(move || cas.delete(&[owned_id])).await;
}

/// One origin fetch + local store. Verifies the hash BEFORE storing (the
/// tier's contract is bytes-that-hash — the same gate the client
/// applies on top).
async fn fetch_and_store(
    state: &Arc<CacheState>,
    repo: &str,
    id: &Id,
    ticket: &str,
) -> Result<Vec<u8>, ApiError> {
    let bytes = state
        .origin
        .object(repo, id, ticket)
        .await
        .map_err(|e| e.into_api_error())?;
    let actual = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, &bytes);
    if actual != *id {
        // The origin handed us bytes that do not address: a bug or an
        // imposter. Serve nothing, store nothing, say internal.
        tracing::error!(id = %id.to_hex(), "origin returned mis-addressed bytes");
        return Err(ApiError::internal());
    }
    let data = naatre_store::DataDir::open_any(state.data_dir.clone());
    let cas = Arc::new(Cas::new(&data).map_err(ApiError::log_internal)?);
    let bytes_for_put = bytes.clone();
    tokio::task::spawn_blocking(move || cas.put(&bytes_for_put))
        .await
        .map_err(|join| ApiError::log_internal(format!("cache cas put panicked: {join}")))?
        .map_err(ApiError::log_internal)?;
    Ok(bytes)
}

/// Origin fetch passthrough: the origin's exact answer with nothing
/// stored on a non-200.
async fn fetch_passthrough(
    state: &Arc<CacheState>,
    repo: &str,
    id: &Id,
    ticket: &str,
) -> Result<Response, ApiError> {
    match state.origin.object(repo, id, ticket).await {
        Ok(bytes) => {
            let actual = naatre_core::ids::object_id(naatre_core::ids::Kind::Blob, &bytes);
            if actual != *id {
                tracing::error!(id = %id.to_hex(), "origin returned mis-addressed bytes");
                return Err(ApiError::internal());
            }
            Ok(bytes_response(&bytes))
        }
        Err(e) => Err(e.into_api_error()),
    }
}

/// The 200-bytes response shape (the origin's raw-fetch headers).
fn bytes_response(bytes: &[u8]) -> Response {
    let mut response = (StatusCode::OK, bytes.to_vec()).into_response();
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/octet-stream"),
    );
    response.headers_mut().insert(
        HeaderName::from_static("x-content-type-options"),
        HeaderValue::from_static("nosniff"),
    );
    response
}

// ---- the origin client -----------------------------------------------------

/// What the origin answered when the tier asked for something it would
/// not give (or could not be reached at all).
#[derive(Debug)]
pub enum OriginError {
    /// The origin's error envelope (status + code + message verbatim —
    /// the tier passes the origin's own answer through, never invents).
    Envelope {
        status: u16,
        code: String,
        message: String,
    },
    /// No answer (connection refused, timeout, non-JSON).
    Transport(String),
}

impl OriginError {
    /// The tier-side error for "I could not get an authoritative answer":
    /// envelopes pass through with their own meaning; transports are the
    /// fail-closed 503 (the tier is a bandwidth cache, not an
    /// availability tier).
    fn into_api_error(self) -> ApiError {
        match self {
            OriginError::Envelope {
                status: 404,
                message,
                ..
            } => ApiError::new(ApiCode::ObjectNotFound, message),
            OriginError::Envelope {
                status: 410,
                message,
                ..
            } => ApiError::new(ApiCode::ObjectRedacted, message),
            // The origin's credential refusals pass through as the
            // credential codes (a 401 from the origin is a ticket
            // problem — expired or invalid — never an internal error).
            OriginError::Envelope {
                status: 401,
                code,
                message,
            } => {
                if code == "token_expired" {
                    ApiError::new(ApiCode::TokenExpired, message)
                } else {
                    ApiError::new(ApiCode::TokenInvalid, message)
                }
            }
            OriginError::Envelope {
                status: 503,
                message,
                ..
            } => ApiError::new(
                ApiCode::OriginUnreachable,
                format!("origin refused: {message}"),
            )
            .retryable(true),
            OriginError::Envelope {
                status,
                code,
                message,
            } => {
                tracing::error!(status, code = %code, %message, "origin answered an unexpected status");
                ApiError::internal()
            }
            OriginError::Transport(detail) => ApiError::new(
                ApiCode::OriginUnreachable,
                "the origin could not be reached; the tier never serves un-revalidated bytes",
            )
            .retryable(true)
            .with_details(serde_json::json!({ "transport": detail })),
        }
    }
}

/// The blocking origin client (ureq, the CLI's pinned client). Calls run
/// inside `spawn_blocking` at the call sites.
pub struct OriginClient {
    agent: ureq::Agent,
    base: String,
}

/// The tier's origin agent (F1): rustls with the EXPLICIT ring crypto
/// provider (aws-lc-rs also sits in the graph, so a process default would
/// be ambiguous — the same rule the store's TLS follows) over the PLATFORM
/// roots, and `max_redirects(0)` — a 3xx comes back as the response, never
/// followed, so `Authorization` can never cross hosts. The tier has no
/// `ca_file` flag: an origin whose certificate chains to a private CA is
/// unreachable over https from here — front it with a publicly-rooted cert
/// or a trusted proxy. There is deliberately no "skip verification" option.
fn origin_agent() -> ureq::Agent {
    let roots: Vec<ureq::tls::Certificate<'static>> = rustls_native_certs::load_native_certs()
        .certs
        .iter()
        .map(|c| ureq::tls::Certificate::from_der(c.as_ref()).to_owned())
        .collect();
    let tls = ureq::tls::TlsConfig::builder()
        .provider(ureq::tls::TlsProvider::Rustls)
        .unversioned_rustls_crypto_provider(Arc::new(rustls::crypto::ring::default_provider()))
        .root_certs(roots.into())
        .build();
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_connect(Some(Duration::from_secs(5)))
            .timeout_global(Some(Duration::from_secs(120)))
            .max_redirects(0)
            .tls_config(tls)
            .build(),
    )
}

impl OriginClient {
    pub fn new(base: &str) -> OriginClient {
        OriginClient {
            agent: origin_agent(),
            base: base.trim_end_matches('/').to_string(),
        }
    }

    fn auth_header(ticket: &str) -> String {
        format!("{} {}", crate::ticket::TICKET_SCHEME, ticket)
    }

    /// GET …/cache/membership?tree= → (epoch, blob ids).
    pub async fn membership(
        &self,
        repo: &str,
        tree: &Id,
        ticket: &str,
    ) -> Result<(i64, Vec<Id>), OriginError> {
        let url = format!(
            "{}/v1/repos/{}/cache/membership",
            self.base,
            uri_segment(repo)
        );
        let auth = Self::auth_header(ticket);
        let tree_hex = tree.to_hex();
        let agent = self.agent.clone();
        let res = tokio::task::spawn_blocking(move || {
            agent
                .get(&url)
                .query("tree", &tree_hex)
                .header("Authorization", &auth)
                .call()
        })
        .await
        .map_err(|join| OriginError::Transport(format!("origin task panicked: {join}")))?
        .map_err(|e| OriginError::Transport(format!("membership fetch: {e}")))?;
        let status = res.status().as_u16();
        let mut body = res.into_body();
        let value: serde_json::Value = body
            .read_json()
            .map_err(|e| OriginError::Transport(format!("membership body: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(envelope_of(status, &value));
        }
        let epoch = value["epoch"]
            .as_i64()
            .ok_or_else(|| OriginError::Transport("membership response has no epoch".into()))?;
        let blobs = value["blobs"]
            .as_array()
            .ok_or_else(|| OriginError::Transport("membership response has no blobs array".into()))?
            .iter()
            .map(|v| {
                v.as_str().and_then(Id::from_hex).ok_or_else(|| {
                    OriginError::Transport("membership blob id is not 64-hex".into())
                })
            })
            .collect::<Result<Vec<Id>, OriginError>>()?;
        Ok((epoch, blobs))
    }

    /// GET …/cache/epoch → the repo's tombstone epoch.
    pub async fn epoch(&self, repo: &str, ticket: &str) -> Result<i64, OriginError> {
        let url = format!("{}/v1/repos/{}/cache/epoch", self.base, uri_segment(repo));
        let auth = Self::auth_header(ticket);
        let agent = self.agent.clone();
        let res = tokio::task::spawn_blocking(move || {
            agent.get(&url).header("Authorization", &auth).call()
        })
        .await
        .map_err(|join| OriginError::Transport(format!("origin task panicked: {join}")))?
        .map_err(|e| OriginError::Transport(format!("epoch probe: {e}")))?;
        let status = res.status().as_u16();
        let mut body = res.into_body();
        let value: serde_json::Value = body
            .read_json()
            .map_err(|e| OriginError::Transport(format!("epoch body: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(envelope_of(status, &value));
        }
        value["epoch"]
            .as_i64()
            .ok_or_else(|| OriginError::Transport("epoch response has no epoch".into()))
    }

    /// GET /v1/repos/{r}/objects/{id} with the ticket — the miss fetch
    /// through the origin's own gates (tombstone 410, membership 404).
    /// 200 → the raw bytes; anything else → the origin's envelope.
    pub async fn object(&self, repo: &str, id: &Id, ticket: &str) -> Result<Vec<u8>, OriginError> {
        let (status, bytes) = self.object_inner(repo, id, ticket).await?;
        if (200..300).contains(&status) {
            Ok(bytes)
        } else {
            Err(envelope_from_bytes(status, &bytes))
        }
    }

    async fn object_inner(
        &self,
        repo: &str,
        id: &Id,
        ticket: &str,
    ) -> Result<(u16, Vec<u8>), OriginError> {
        let url = format!(
            "{}/v1/repos/{}/objects/{}",
            self.base,
            uri_segment(repo),
            id.to_hex()
        );
        let auth = Self::auth_header(ticket);
        let agent = self.agent.clone();
        let res = tokio::task::spawn_blocking(move || {
            agent.get(&url).header("Authorization", &auth).call()
        })
        .await
        .map_err(|join| OriginError::Transport(format!("origin task panicked: {join}")))?
        .map_err(|e| OriginError::Transport(format!("object fetch: {e}")))?;
        let status = res.status().as_u16();
        let mut body = res.into_body();
        let bytes = body
            .read_to_vec()
            .map_err(|e| OriginError::Transport(format!("object body: {e}")))?;
        Ok((status, bytes))
    }
}

/// The origin's envelope from a JSON body (status + code + message).
fn envelope_of(status: u16, value: &serde_json::Value) -> OriginError {
    OriginError::Envelope {
        status,
        code: value["error"]["code"]
            .as_str()
            .unwrap_or("internal")
            .to_string(),
        message: value["error"]["message"]
            .as_str()
            .unwrap_or("the origin returned an error")
            .to_string(),
    }
}

fn envelope_from_bytes(status: u16, bytes: &[u8]) -> OriginError {
    match serde_json::from_slice::<serde_json::Value>(bytes) {
        Ok(v) if v["error"]["code"].is_string() => envelope_of(status, &v),
        _ => OriginError::Envelope {
            status,
            code: "internal".into(),
            message: "the origin returned a non-envelope body".into(),
        },
    }
}

// ---- run -------------------------------------------------------------------

/// Serves the tier on `listener` until `shutdown` resolves, then drains
/// (the origin's serve-loop shape: the one-shot shutdown fanned out
/// through a watch channel, the drain deadline racing the graceful
/// hook).
pub async fn run(
    listener: tokio::net::TcpListener,
    state: Arc<CacheState>,
    shutdown: impl std::future::Future<Output = ()> + Send + 'static,
) -> io::Result<()> {
    let addr = listener.local_addr()?;
    tracing::info!(
        name = "naatre-cache",
        version = env!("CARGO_PKG_VERSION"),
        "starting naatre cache tier"
    );
    tracing::info!(bind = %addr, "listening");
    tracing::info!(keys = state.keys.keys().len(), "origin verify keys loaded");
    let app = cache_router(state);

    let (signal_tx, signal_rx) = tokio::sync::watch::channel(false);
    tokio::spawn(async move {
        shutdown.await;
        let _ = signal_tx.send(true);
    });

    let graceful = {
        let mut rx = signal_rx.clone();
        async move {
            let _ = rx.changed().await;
        }
    };
    let deadline = {
        let mut rx = signal_rx;
        async move {
            if rx.changed().await.is_ok() {
                tokio::time::sleep(DRAIN_TIMEOUT).await;
            }
        }
    };

    let mut server = std::pin::pin!(
        axum::serve(listener, app)
            .with_graceful_shutdown(graceful)
            .into_future()
    );
    tokio::select! {
        result = &mut server => result,
        _ = deadline => {
            tracing::warn!("cache drain timeout exceeded; forcing shutdown");
            Ok(())
        }
    }
}

/// Percent-encodes one URL path segment (the CLI's helper lives in
/// naatre-cli; the tier carries its own six-line copy to stay dep-clean).
fn uri_segment(segment: &str) -> String {
    let mut out = String::new();
    for b in segment.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(b as char);
            }
            other => out.push_str(&format!("%{other:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The origin's URL segment encoding (repo names are
    /// `^[a-z0-9][a-z0-9-]{0,62}$`, but the encoder never guesses).
    #[test]
    fn uri_segment_encodes_outside_the_unreserved_set() {
        assert_eq!(uri_segment("demo"), "demo");
        assert_eq!(uri_segment("a b/c"), "a%20b%2Fc");
    }
}
