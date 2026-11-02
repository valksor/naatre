//! `/v1/repos/{r}/objects…`: presence, raw PUT, batch PUT, batch fetch
//! (membership-enforced), and raw fetch.
//!
//! Durability invariant: the blob is fsynced into the CAS (`Cas::put`)
//! *before* the `objects` row commits — a crash leaves an orphan object,
//! never a dangling row.
//!
//! Membership: writes record the global `objects` row only. `repo_blobs`
//! membership rows arrive with changesets (M2a); until then batch/raw
//! fetches report freshly-PUT objects as missing (no membership row yet),
//! and `present` only asks the backend. Batch PUT is effectively one
//! max-size blob per request (the 64 MiB payload dominates the 96 MiB
//! body cap) — use the raw PUT for large blobs.

use std::sync::Arc;

use axum::extract::{Path, State};
use axum::http::{HeaderName, HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::{Extension, Json};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use naatre_core::ids::{Id, Kind, object_id};
use naatre_store::{BLOB_LIMIT, BlobStore, StoreError};

use super::{repo_id_or_404, repo_read_gate, repo_write_gate};
use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, CacheTicket};
use crate::db;
use crate::extract::AppJson;
use crate::state::AppState;

/// Max entries per batch request (present / batch PUT / batch fetch).
const BATCH_CAP: usize = 256;

/// Request body of the presence check.
#[derive(Deserialize, ToSchema)]
pub struct PresentRequest {
    /// Object ids, 64 lowercase hex each.
    pub ids: Vec<String>,
}

/// Presence-check result: ids whose object file is absent from the CAS.
/// Membership is NOT consulted — `present` answers "is the content on
/// disk", not "may this repo read it". Tombstoned ids (active or purged)
/// are NEVER reported `missing` (the sync re-upload invitation would
/// resurrect same-content ids): they are reported in `redacted`, whatever
/// the CAS holds.
#[derive(Serialize, ToSchema)]
pub struct PresentResponse {
    pub missing: Vec<String>,
    /// Ids redacted in this repo (the tombstone wins over the CAS answer;
    /// re-uploading them is refused at the PUT boundary).
    pub redacted: Vec<String>,
}

/// One entry of the batch PUT.
#[derive(Deserialize, ToSchema)]
pub struct BatchPutEntry {
    /// 64 lowercase hex id that must equal `BLAKE3(decoded data)`.
    pub id: String,
    /// Standard (padded) base64 of the payload — single line, no
    /// whitespace; large payloads should use the raw PUT instead.
    pub data_b64: String,
}

/// Request body of the batch PUT.
#[derive(Deserialize, ToSchema)]
pub struct BatchPutRequest {
    pub objects: Vec<BatchPutEntry>,
}

/// A batch-PUT rejection: the entry id and its error code (`malformed` for
/// hex/base64/hash mismatches, `too_large` over the 64 MiB cap,
/// `redacted` for a tombstoned id — active or purged).
#[derive(Serialize, ToSchema)]
pub struct RejectedEntry {
    pub id: String,
    pub code: String,
}

/// Batch PUT result.
#[derive(Serialize, ToSchema)]
pub struct BatchPutResponse {
    pub stored: Vec<String>,
    pub rejected: Vec<RejectedEntry>,
}

/// One stored object in a batch fetch.
#[derive(Serialize, ToSchema)]
pub struct StoredObject {
    pub id: String,
    pub data_b64: String,
}

/// Request body of the batch fetch.
#[derive(Deserialize, ToSchema)]
pub struct BatchGetRequest {
    /// Object ids, 64 lowercase hex each.
    pub ids: Vec<String>,
}

/// Batch fetch result. Membership-enforced: an id without a
/// `repo_blobs(repo, id)` row lands in `missing` even when the CAS holds
/// the file (cross-repo isolation). A missing or unreadable file after a
/// membership hit is also reported in `missing` (logged server-side).
/// Tombstoned ids land in `redacted` (M10b — active or purged: the
/// tombstone wins over membership, and the bytes never serve).
#[derive(Serialize, ToSchema)]
pub struct BatchGetResponse {
    pub objects: Vec<StoredObject>,
    pub missing: Vec<String>,
    /// Ids redacted in this repo (the tombstone overrides membership).
    pub redacted: Vec<String>,
}

/// The object id echo of a successful PUT.
#[derive(Serialize, ToSchema)]
pub struct StoredObjectId {
    pub id: String,
}

/// Presence check (any authenticated principal): which of `ids` lacks an
/// object file in the CAS. Any non-64-hex id → 400 `malformed`; more than
/// 256 ids → 422 `validation`.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/objects/present",
    params(("r" = String, Path, description = "Repo name")),
    request_body = PresentRequest,
    responses(
        (status = 200, description = "Ids absent from the CAS; tombstoned ids are reported in `redacted`, never `missing`", body = PresentResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo"),
        (status = 422, description = "More than 256 ids")
    )
)]
pub async fn present(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<PresentRequest>,
) -> Result<Json<PresentResponse>, ApiError> {
    repo_read_gate(&state, &auth, &repo).await?;
    let ids = parse_id_batch(&body.0.ids)?;
    let tombstoned: std::collections::HashSet<Id> = {
        let ids = ids.clone();
        let repo_name = repo.clone();
        state
            .read_repo(repo_name.clone(), move |cx| {
                db::redactions_active_among(cx.db, cx.id, &ids).map_err(ApiError::log_internal)
            })
            .await?
            .into_iter()
            .collect()
    };
    let cas = Arc::clone(&state.cas);
    let (missing, redacted) = state
        .blocking(move || {
            // Tombstoned ids never reach the CAS probe: the tombstone
            // already decided their answer, so the backend is not asked
            // about them.
            let probe: Vec<Id> = ids
                .iter()
                .filter(|id| !tombstoned.contains(id))
                .copied()
                .collect();
            let has = cas.has(&probe).map_err(ApiError::log_internal)?;
            let mut missing = Vec::new();
            let mut redacted = Vec::new();
            let mut has = has.into_iter();
            for id in &ids {
                if tombstoned.contains(id) {
                    // The tombstone wins over the CAS answer: never an
                    // invitation to re-upload.
                    redacted.push(id.to_hex());
                } else if !has.next().unwrap_or(false) {
                    missing.push(id.to_hex());
                }
            }
            Ok((missing, redacted))
        })
        .await?;
    Ok(Json(PresentResponse { missing, redacted }))
}

/// Raw PUT (writer and above, repo-level): body bytes are hash-verified
/// against the URL id, then fsynced into the CAS, then the `objects` row
/// commits. First PUT → 201; identical re-PUT → 200 (idempotent).
#[utoipa::path(
    put,
    path = "/v1/repos/{r}/objects/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "BLAKE3 of the payload, 64 lowercase hex")
    ),
    request_body(content = String, content_type = "application/octet-stream", description = "Raw blob bytes (≤ 64 MiB)"),
    responses(
        (status = 201, description = "Object stored", body = StoredObjectId),
        (status = 200, description = "Object already stored (idempotent re-PUT)", body = StoredObjectId),
        (status = 400, description = "Payload does not hash to the URL id (`mis-addressed`)"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role"),
        (status = 404, description = "No such repo"),
        (status = 410, description = "The id is tombstoned in this repo (`object_redacted`, active or purged) — re-upload refused before any bytes are written"),
        (status = 413, description = "Payload over the 64 MiB blob cap")
    )
)]
pub async fn put_raw(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
    payload: axum::body::Bytes,
) -> Result<Response, ApiError> {
    // The M13b repo write gate replaces the bare role check: public
    // repos keep the writer-and-above matrix (the gate answers the same
    // 403), private repos admit granted writers (uniform 404 for the
    // rest — the write surface is as invisible as the read surface).
    repo_write_gate(&state, &auth, &repo).await?;
    let id = parse_single_id(&id_hex)?;
    verify_addressed(&id, &payload)?;
    if payload.len() > BLOB_LIMIT {
        return Err(too_large(payload.len()));
    }

    // The tombstone boundary probe (before ANY CAS write): a redacted id
    // — active or purged — is refused; same content hashes to the same id,
    // so a re-PUT would resurrect purged content's bytes. The authoritative
    // check re-runs inside the record step's immediate transaction.
    {
        let repo_name = repo.clone();
        let redacted = state
            .read_repo(repo_name.clone(), move |cx| {
                db::redaction_active(cx.db, cx.id, &id).map_err(ApiError::log_internal)
            })
            .await?;
        if redacted {
            return Err(object_redacted(&id_hex, &repo));
        }
    }

    let cas = Arc::clone(&state.cas);
    let payload_owned = payload.clone();
    let csize = state
        .blocking(move || put_and_size(cas.as_ref(), &id, &payload_owned))
        .await?;

    let existed = record_object(&state, &repo, id, payload.len() as i64, csize).await?;
    let status = if existed {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    };
    Ok((status, Json(StoredObjectId { id: id.to_hex() })).into_response())
}

/// The 410 of the tombstone write boundary (`object_redacted`): a redacted
/// id is refused at every write that would recreate its content or point
/// at it.
fn object_redacted(id_hex: &str, repo: &str) -> ApiError {
    ApiError::new(
        ApiCode::ObjectRedacted,
        format!("object {id_hex} is redacted in {repo:?}; re-upload refused"),
    )
}

/// The upload RECORD step — bytes-then-row on both sides of the race with
/// the reachability GC's sweep. The CAS step ran outside the slot; by the
/// time this record takes the single-writer slot, the sweep may have
/// committed a batch that unlinked exactly these bytes (an old, still
/// unreferenced re-PUT past its grace window). So the record VERIFIES the
/// bytes are present first — inside the same slot-held `BEGIN IMMEDIATE`
/// transaction that writes the row (the presence check, the row upsert,
/// and the condemnation-clear commit atomically, and on Postgres under the
/// fixed advisory lock — the destructive-writer section the GC's phase B
/// serializes against cross-process, never just in-process) — and answers
/// a RETRYABLE 503 `blob_swept` when they are gone, instead of re-writing
/// (a raw PUT may be up to 64 MiB; re-writing inside the write slot is a
/// stall the retry avoids — the client re-PUTs, which is idempotent at a
/// content-addressed URL, and the retry's own record sees the fresh
/// bytes). The row is written ONLY after the check, so it can never name
/// absent bytes. The per-upload presence check is a hot-path cost (one
/// backend `has` per PUT inside the slot — a network HEAD on S3); it is
/// the price of the ordering argument and runs on every upload, not only
/// when the sweep raced. The tombstone boundary re-checks in the same
/// transaction (`repo_id`-scoped, purged tombstones included): a redacted
/// id answers 410 `object_redacted` and its row is never written — the
/// authoritative half of put_raw's early probe, closing the probe-to-
/// record race against a concurrent mark. The refusal also condemns the
/// bytes the CAS step already wrote into the GC ledger (scope blob, in a
/// step committed before the refusal's rollback): no row will ever name
/// them, so the sweep's reference re-check — the cross-repo safety — is
/// the path that reclaims them. Returns whether the row already existed.
/// `pub` for the M10a race pin (the fs and S3 suites drive this step
/// after a full sweep).
pub async fn record_object(
    state: &AppState,
    repo: &str,
    id: Id,
    size: i64,
    csize: i64,
) -> Result<bool, ApiError> {
    let cas = Arc::clone(&state.cas);
    let now_i = state.now_ns() as i64;
    let repo_name = repo.to_string();
    let repo_for_slot = repo_name.clone();
    state
        .write_repo(repo_for_slot, move |cx| {
            let repo = &repo_name;
            let repo_id = cx.id;
            if db::redaction_active(cx.db, repo_id, &id).map_err(ApiError::log_internal)? {
                // The ledger condemnation is catalog-side and committed
                // BEFORE the refusal unwinds: the statement commits
                // immediately (its own auto-commit — the old single-DB
                // transaction's early-commit semantics were for the
                // rollback the shared connection would have issued), and
                // the row survives the error — it is the only record
                // naming the bytes the CAS step already wrote. Nothing
                // else has been written at this point (the refusal
                // precedes the presence check and the upsert).
                db::gc_candidates_upsert(&cx.cat(), db::GC_SCOPE_BLOB, &[id], now_i)
                    .map_err(ApiError::log_internal)?;
                return Err(object_redacted(&id.to_hex(), repo));
            }
            if !BlobStore::has(cas.as_ref(), &[id]).map_err(ApiError::log_internal)?[0] {
                return Err(ApiError::new(
                    ApiCode::BlobSwept,
                    "the blob's bytes were removed by gc mid-upload; re-upload it",
                )
                .retryable(true));
            }
            // The record row is catalog-side (the ledger); the coordinated
            // sweep batches hold the repo slot + the sweep flag, so this
            // statement cannot interleave with a delete batch's probe
            // window — the isolation the old shared transaction provided.
            db::object_row_upsert(&cx.cat(), &id, size, csize, now_i)
                .map_err(ApiError::log_internal)
        })
        .await
}

/// Batch PUT (writer and above): every entry is hash-verified like the raw
/// PUT; entries that fail verification are reported per-entry, the rest are
/// stored. More than 256 entries → 422 `validation`.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/objects",
    params(("r" = String, Path, description = "Repo name")),
    request_body = BatchPutRequest,
    responses(
        (status = 200, description = "Per-entry results", body = BatchPutResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role"),
        (status = 404, description = "No such repo"),
        (status = 410, description = "A tombstoned id survived the per-entry probe to the record step's re-check (`object_redacted`, active or purged) — the whole record refuses and no rows are written"),
        (status = 422, description = "More than 256 entries")
    )
)]
pub async fn create_batch(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<BatchPutRequest>,
) -> Result<Json<BatchPutResponse>, ApiError> {
    // The write gate (see put_raw): public repos keep the writer matrix,
    // private repos admit granted writers.
    repo_write_gate(&state, &auth, &repo).await?;
    let entries = body.0.objects;
    if entries.len() > BATCH_CAP {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("objects batch is capped at {BATCH_CAP} entries"),
        ));
    }
    // Verify every entry first: rejected entries never touch the CAS.
    let mut verified: Vec<(Id, Vec<u8>)> = Vec::with_capacity(entries.len());
    let mut rejected: Vec<RejectedEntry> = Vec::new();
    for entry in &entries {
        match decode_entry(entry) {
            Ok(pair) => verified.push(pair),
            Err(code) => rejected.push(RejectedEntry {
                id: entry.id.clone(),
                code: code.to_string(),
            }),
        }
    }

    // The tombstone boundary probe (before ANY CAS write): a redacted id —
    // active or purged — is rejected per entry, the batch's other entries
    // proceed. The authoritative re-check runs in [`record_batch`].
    {
        let ids: Vec<Id> = verified.iter().map(|(id, _)| *id).collect();
        let repo_name = repo.clone();
        let tombstoned: std::collections::HashSet<Id> = state
            .read_repo(repo_name.clone(), move |cx| {
                db::redactions_active_among(cx.db, cx.id, &ids).map_err(ApiError::log_internal)
            })
            .await?
            .into_iter()
            .collect();
        if !tombstoned.is_empty() {
            verified.retain(|(id, _)| !tombstoned.contains(id));
            for id in &tombstoned {
                rejected.push(RejectedEntry {
                    id: id.to_hex(),
                    code: "redacted".to_string(),
                });
            }
        }
    }

    // Collect the response's stored ids before `verified` moves into the
    // blocking closure.
    let stored: Vec<String> = verified.iter().map(|(id, _)| id.to_hex()).collect();

    // File first: all blobs fsynced in one blocking step.
    let cas = Arc::clone(&state.cas);
    let rows = state
        .blocking(move || {
            let mut rows = Vec::with_capacity(verified.len());
            for (id, payload) in &verified {
                let csize = put_and_size(cas.as_ref(), id, payload)?;
                rows.push((*id, payload.len() as i64, csize));
            }
            Ok(rows)
        })
        .await?;

    // …then the rows, in one single-writer transaction — the batch form of
    // [`record_object`]'s contract ([`record_batch`]).
    if !rows.is_empty() {
        record_batch(&state, &repo, rows).await?;
    }

    Ok(Json(BatchPutResponse { stored, rejected }))
}

/// The batch form of [`record_object`]: verify EVERY entry's bytes first —
/// in the same slot-held `BEGIN IMMEDIATE` transaction that writes the
/// rows (one transaction for the whole batch: the checks, every upsert,
/// and their condemnation-clears commit atomically) — and answer one
/// retryable 503 `blob_swept` naming the swept ids instead of recording
/// any of them (the batch is all-or-nothing on the row side, so no entry's
/// row can name absent bytes; the client re-PUTs the named ids). The
/// tombstone boundary re-checks in the same transaction (`repo_id`-scoped,
/// purged included): any redacted id fails the whole record with 410
/// `object_redacted` naming them — the authoritative half of the route's
/// per-entry probe, closing the probe-to-record race against a concurrent
/// mark. The refusal also condemns EVERY id's already-written bytes into
/// the GC ledger (scope blob, committed before the refusal's rollback —
/// the batch form of [`record_object]'s orphan rule: the clean siblings'
/// rows die with the same rollback, so their bytes orphan identically).
/// `pub` for the M10a race pin.
pub async fn record_batch(
    state: &AppState,
    repo: &str,
    rows: Vec<(Id, i64, i64)>,
) -> Result<(), ApiError> {
    let cas = Arc::clone(&state.cas);
    let now_i = state.now_ns() as i64;
    let repo_name = repo.to_string();
    let repo_for_slot = repo_name.clone();
    state
        .write_repo(repo_for_slot, move |cx| {
            let repo = &repo_name;
            let repo_id = cx.id;
            let ids: Vec<Id> = rows.iter().map(|(id, _, _)| *id).collect();
            let redacted = db::redactions_active_among(cx.db, repo_id, &ids)
                .map_err(ApiError::log_internal)?;
            if !redacted.is_empty() {
                let hexes: Vec<String> = redacted.iter().map(Id::to_hex).collect();
                // Committed before the refusal unwinds (its own
                // auto-commit — the condemnation must survive the error).
                // EVERY written id is condemned, not only the redacted
                // ones: the whole batch's bytes are already in the CAS and
                // no row survives the refusal for any of them. A later
                // legitimate record self-heals — the row upsert clears the
                // condemnation, and the sweep's re-verification spares
                // re-referenced candidates.
                db::gc_candidates_upsert(&cx.cat(), db::GC_SCOPE_BLOB, &ids, now_i)
                    .map_err(ApiError::log_internal)?;
                return Err(object_redacted(&hexes.join(", "), repo));
            }
            let present = BlobStore::has(cas.as_ref(), &ids).map_err(ApiError::log_internal)?;
            let swept: Vec<String> = rows
                .iter()
                .zip(&present)
                .filter(|(_, ok)| !**ok)
                .map(|(row, _)| row.0.to_hex())
                .collect();
            if !swept.is_empty() {
                return Err(ApiError::new(
                    ApiCode::BlobSwept,
                    "blob bytes were removed by gc mid-upload; re-upload them",
                )
                .retryable(true)
                .with_details(serde_json::json!({ "missing_blobs": swept })));
            }
            // The record rows are catalog-side (the ledger); the coordinated
            // sweep batches hold the repo slot + the sweep flag, so these
            // statements cannot interleave with a delete batch's probe
            // window.
            let cat = cx.cat();
            for (id, size, csize) in &rows {
                db::object_row_upsert(&cat, id, *size, *csize, now_i)
                    .map_err(ApiError::log_internal)?;
            }
            Ok(())
        })
        .await
}

/// Batch fetch (any authenticated principal), membership-enforced: only ids
/// with a `repo_blobs` row for this repo are served; the rest land in
/// `missing`. Tombstoned ids — active or purged (M10b) — are gated out of `objects` and
/// reported in `redacted` — never served, never silently `missing`. More
/// than 256 ids → 422; a non-hex id → 400.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/objects/get",
    params(("r" = String, Path, description = "Repo name")),
    request_body = BatchGetRequest,
    responses(
        (status = 200, description = "Stored objects and missing ids", body = BatchGetResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo"),
        (status = 422, description = "More than 256 ids")
    )
)]
pub async fn get_batch(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<BatchGetRequest>,
) -> Result<Json<BatchGetResponse>, ApiError> {
    repo_read_gate(&state, &auth, &repo).await?;
    let ids = parse_id_batch(&body.0.ids)?;

    // The redaction gate first (the tombstone wins over membership), then
    // membership: this is the cross-repo isolation gate.
    let ids_for_gates = ids.clone();
    let repo_name = repo.clone();
    let (redacted_ids, member) = state
        .read_repo(repo_name.clone(), move |cx| {
            let redacted = db::redactions_active_among(cx.db, cx.id, &ids_for_gates)
                .map_err(ApiError::log_internal)?;
            let member = ids_for_gates
                .iter()
                .map(|id| db::repo_blob_check(cx.db, cx.id, id).map_err(ApiError::log_internal))
                .collect::<Result<Vec<bool>, ApiError>>()?;
            Ok((redacted, member))
        })
        .await?;
    let redacted_set: std::collections::HashSet<Id> = redacted_ids.iter().copied().collect();

    // Fetch the members' bytes off the runtime (redacted ids excluded even
    // when a membership row exists).
    let cas = Arc::clone(&state.cas);
    let member_ids: Vec<Id> = ids
        .iter()
        .zip(&member)
        .filter_map(|(id, m)| (*m && !redacted_set.contains(id)).then_some(*id))
        .collect();
    let fetched = state
        .blocking(move || {
            let mut out = Vec::with_capacity(member_ids.len());
            for id in &member_ids {
                // A membership hit whose file is gone or unreadable is
                // reported as missing; non-missing failures are logged.
                match cas.get(id) {
                    Ok(bytes) => out.push((id.to_hex(), Some(bytes))),
                    Err(StoreError::BlobMissing(hex)) => out.push((hex, None)),
                    Err(other) => {
                        tracing::error!(error = %other, id = %id.to_hex(), "object unreadable");
                        out.push((id.to_hex(), None));
                    }
                }
            }
            Ok(out)
        })
        .await?;

    // Reassemble in request order.
    let mut objects = Vec::new();
    let mut missing = Vec::new();
    let mut redacted: Vec<String> = redacted_ids.iter().map(Id::to_hex).collect();
    let mut fetched = fetched.into_iter();
    for (id, is_member) in ids.iter().zip(&member) {
        if redacted_set.contains(id) {
            if !redacted.contains(&id.to_hex()) {
                redacted.push(id.to_hex());
            }
        } else if !*is_member {
            missing.push(id.to_hex());
        } else if let Some((hex, Some(bytes))) = fetched.next() {
            objects.push(StoredObject {
                id: hex,
                data_b64: STANDARD.encode(bytes),
            });
        } else {
            missing.push(id.to_hex());
        }
    }
    Ok(Json(BatchGetResponse {
        objects,
        missing,
        redacted,
    }))
}

/// Raw fetch (any authenticated principal), redaction-gated then
/// membership-enforced: a tombstoned id — active or purged — answers 410
/// `object_redacted` (M10b — the tombstone wins over everything); no
/// `repo_blobs` row for this repo → 404 `object_not_found` (a cross-repo
/// id is indistinguishable from a nonexistent one). Serves the raw
/// decompressed bytes as `application/octet-stream` with
/// `X-Content-Type-Options: nosniff`.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/objects/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Object id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "Raw object bytes"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo; or the object is not a member of this repo; or the file is missing from the CAS"),
        (status = 410, description = "The object is redacted in this repo (`object_redacted`)")
    )
)]
pub async fn get_raw(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    cache_ticket: Option<Extension<CacheTicket>>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Response, ApiError> {
    let id = parse_single_id(&id_hex)?;

    // M13b: the fetch credential decides the repo gate. A BEARER caller
    // takes the repo READ gate (uniform 404 for a private repo's
    // non-reader). The CACHE TIER's ticket arm takes the binding check
    // only: the ticket names its repo (a mismatch is the uniform 404),
    // the repo must exist, and the ACL check stays at the MINT (the
    // TTL bound is the plan's staleness story — a ticket minted before
    // a repo goes private cannot outlive it, and the tier's per-serve
    // epoch probe plus fresh walks keep the tombstone gate live). The
    // redaction/membership gates below run for BOTH arms unchanged.
    match &cache_ticket {
        Some(t) if t.repo == repo => {
            repo_id_or_404(&state, &repo).await?;
            // Defense-in-depth (M13b-fix security A1): the ticket's
            // root tree must STILL be a tree this repo wrote — the
            // mint-time junction check re-run at the fetch (a repo can
            // only lose history via redaction/GC, and a tree the repo
            // no longer holds has no business serving through a ticket).
            let tree = t.root_tree;
            let known = state
                .read_repo(repo.clone(), move |cx| {
                    db::repo_tree_member(cx.db, cx.id, &tree).map_err(ApiError::log_internal)
                })
                .await?;
            if !known {
                return Err(ApiError::new(
                    ApiCode::ObjectNotFound,
                    "the ticket's root tree is not a tree of this repo",
                ));
            }
        }
        Some(_) => {
            return Err(ApiError::new(
                ApiCode::RepoNotFound,
                format!("no repo named {repo:?} on this server"),
            ));
        }
        None => repo_read_gate(&state, &auth, &repo).await?,
    }

    // The redaction gate first (the tombstone wins over membership), then
    // the cross-repo isolation gate.
    let repo_name = repo.clone();
    let (is_redacted, is_member) = state
        .read_repo(repo_name.clone(), move |cx| {
            Ok::<_, ApiError>((
                db::redaction_active(cx.db, cx.id, &id).map_err(ApiError::log_internal)?,
                db::repo_blob_check(cx.db, cx.id, &id).map_err(ApiError::log_internal)?,
            ))
        })
        .await?;
    if is_redacted {
        return Err(ApiError::new(
            ApiCode::ObjectRedacted,
            format!("object {id_hex} is redacted in {repo:?}"),
        ));
    }
    if !is_member {
        return Err(ApiError::new(
            ApiCode::ObjectNotFound,
            format!("object {id_hex} is not a member of {repo:?}"),
        ));
    }

    let cas = Arc::clone(&state.cas);
    let bytes = state
        .blocking(move || {
            cas.get(&id).map_err(|e| match e {
                StoreError::BlobMissing(_) => ApiError::new(
                    ApiCode::ObjectNotFound,
                    format!("object {id_hex} is missing"),
                ),
                // A file whose bytes do not hash to its id is store
                // corruption: an internal error, never a silent 404.
                other => ApiError::log_internal(other),
            })
        })
        .await?;

    let mut response = (StatusCode::OK, bytes).into_response();
    response.headers_mut().insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/octet-stream"),
    );
    response.headers_mut().insert(
        HeaderName::from_static("x-content-type-options"),
        HeaderValue::from_static("nosniff"),
    );
    Ok(response)
}

// ---- helpers -------------------------------------------------------------

/// Parses one id path param; anything but 64 lowercase hex is 400.
fn parse_single_id(hex: &str) -> Result<Id, ApiError> {
    Id::from_hex(hex)
        .ok_or_else(|| ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters"))
}

/// Parses a batch of ids with the batch cap: over-cap → 422 `validation`,
/// any non-hex id → 400 `malformed`.
fn parse_id_batch(ids: &[String]) -> Result<Vec<Id>, ApiError> {
    if ids.len() > BATCH_CAP {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("id batch is capped at {BATCH_CAP} ids"),
        ));
    }
    ids.iter().map(|s| parse_single_id(s)).collect()
}

/// Addressing invariant: the URL id must be exactly `BLAKE3(payload)`.
fn verify_addressed(id: &Id, payload: &[u8]) -> Result<(), ApiError> {
    if object_id(Kind::Blob, payload) != *id {
        return Err(ApiError::new(
            ApiCode::Malformed,
            "mis-addressed: payload does not hash to the given id",
        ));
    }
    Ok(())
}

fn too_large(got: usize) -> ApiError {
    ApiError::new(
        ApiCode::TooLarge,
        format!("blob exceeds the {BLOB_LIMIT} byte cap (got {got} bytes)"),
    )
}

/// Decodes and fully verifies one batch-PUT entry: hex id, standard base64
/// payload, hash match, size cap. `Err` is the rejected-entry code string.
fn decode_entry(entry: &BatchPutEntry) -> Result<(Id, Vec<u8>), &'static str> {
    let id = Id::from_hex(&entry.id).ok_or("malformed")?;
    let payload = STANDARD
        .decode(entry.data_b64.as_bytes())
        .map_err(|_| "malformed")?;
    verify_addressed(&id, &payload).map_err(|_| "malformed")?;
    if payload.len() > BLOB_LIMIT {
        return Err("too_large");
    }
    Ok((id, payload))
}

/// `BlobStore::put` plus nothing else: the STORED (compressed) size comes
/// back in the backend's own [`PutInfo`] — the route never re-stats a
/// local shard for the `objects.csize` column. Statting the fs shard here
/// made every history PUT behind `blob = "s3"` a 500 (the blob landed in
/// the bucket, the row never upserted, the object invisible forever): the
/// M9b matrix exercised `S3Store` behind the seam, never these routes.
fn put_and_size(cas: &dyn BlobStore, id: &Id, payload: &[u8]) -> Result<i64, ApiError> {
    let stored = cas.put(payload).map_err(ApiError::log_internal)?;
    debug_assert_eq!(stored.id, *id, "cas.put must address the verified payload");
    Ok(stored.csize as i64)
}
