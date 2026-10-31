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
//! and `present` only asks the filesystem. Batch PUT is effectively one
//! max-size blob per request (the 64 MiB payload dominates the 96 MiB
//! body cap) — use the raw PUT for large blobs.

use std::path::{Path as FsPath, PathBuf};
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

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::routes::repo_id_or_404;
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
/// disk", not "may this repo read it".
#[derive(Serialize, ToSchema)]
pub struct PresentResponse {
    pub missing: Vec<String>,
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
/// hex/base64/hash mismatches, `too_large` over the 64 MiB cap).
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
#[derive(Serialize, ToSchema)]
pub struct BatchGetResponse {
    pub objects: Vec<StoredObject>,
    pub missing: Vec<String>,
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
        (status = 200, description = "Ids absent from the CAS", body = PresentResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo"),
        (status = 422, description = "More than 256 ids")
    )
)]
pub async fn present(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<PresentRequest>,
) -> Result<Json<PresentResponse>, ApiError> {
    let ids = parse_id_batch(&body.0.ids)?;
    repo_id_or_404(&state, &repo).await?;
    let cas = Arc::clone(&state.cas);
    let missing = state
        .blocking(move || {
            let has = cas.has(&ids).map_err(ApiError::log_internal)?;
            let missing: Vec<String> = ids
                .iter()
                .zip(has)
                .filter(|(_, present)| !*present)
                .map(|(id, _)| id.to_hex())
                .collect();
            Ok(missing)
        })
        .await?;
    Ok(Json(PresentResponse { missing }))
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
        (status = 413, description = "Payload over the 64 MiB blob cap")
    )
)]
pub async fn put_raw(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
    payload: axum::body::Bytes,
) -> Result<Response, ApiError> {
    require_role(&auth, Role::Writer)?;
    let id = parse_single_id(&id_hex)?;
    repo_id_or_404(&state, &repo).await?;
    verify_addressed(&id, &payload)?;
    if payload.len() > BLOB_LIMIT {
        return Err(too_large(payload.len()));
    }

    let cas = Arc::clone(&state.cas);
    let data_dir = state.data_dir_display.clone();
    let payload_owned = payload.clone();
    let csize = state
        .blocking(move || put_and_size(cas.as_ref(), &data_dir, &id, &payload_owned))
        .await?;

    let existed = state
        .write(move |store| {
            db::object_row_upsert(store.conn(), &id, payload.len() as i64, csize)
                .map_err(ApiError::log_internal)
        })
        .await?;
    let status = if existed {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    };
    Ok((status, Json(StoredObjectId { id: id.to_hex() })).into_response())
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
        (status = 422, description = "More than 256 entries")
    )
)]
pub async fn create_batch(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<BatchPutRequest>,
) -> Result<Json<BatchPutResponse>, ApiError> {
    require_role(&auth, Role::Writer)?;
    let entries = body.0.objects;
    if entries.len() > BATCH_CAP {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("objects batch is capped at {BATCH_CAP} entries"),
        ));
    }
    repo_id_or_404(&state, &repo).await?;

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

    // Collect the response's stored ids before `verified` moves into the
    // blocking closure.
    let stored: Vec<String> = verified.iter().map(|(id, _)| id.to_hex()).collect();

    // File first: all blobs fsynced in one blocking step…
    let cas = Arc::clone(&state.cas);
    let data_dir = state.data_dir_display.clone();
    let rows = state
        .blocking(move || {
            let mut rows = Vec::with_capacity(verified.len());
            for (id, payload) in &verified {
                let csize = put_and_size(cas.as_ref(), &data_dir, id, payload)?;
                rows.push((*id, payload.len() as i64, csize));
            }
            Ok(rows)
        })
        .await?;

    // …then the rows, in one single-writer transaction.
    if !rows.is_empty() {
        state
            .write(move |store| {
                for (id, size, csize) in &rows {
                    db::object_row_upsert(store.conn(), id, *size, *csize)
                        .map_err(ApiError::log_internal)?;
                }
                Ok(())
            })
            .await?;
    }

    Ok(Json(BatchPutResponse { stored, rejected }))
}

/// Batch fetch (any authenticated principal), membership-enforced: only ids
/// with a `repo_blobs` row for this repo are served; the rest land in
/// `missing`. More than 256 ids → 422; a non-hex id → 400.
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
    Extension(_auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<BatchGetRequest>,
) -> Result<Json<BatchGetResponse>, ApiError> {
    let ids = parse_id_batch(&body.0.ids)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;

    // Membership first: this is the cross-repo isolation gate.
    let ids_for_membership = ids.clone();
    let member = state
        .read(move |conn| {
            ids_for_membership
                .iter()
                .map(|id| db::repo_blob_check(conn, repo_id, id).map_err(ApiError::log_internal))
                .collect::<Result<Vec<bool>, ApiError>>()
        })
        .await?;

    // Fetch the members' bytes off the runtime.
    let cas = Arc::clone(&state.cas);
    let member_ids: Vec<Id> = ids
        .iter()
        .zip(&member)
        .filter_map(|(id, m)| m.then_some(*id))
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
    let mut fetched = fetched.into_iter();
    for (id, is_member) in ids.iter().zip(&member) {
        if !*is_member {
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
    Ok(Json(BatchGetResponse { objects, missing }))
}

/// Raw fetch (any authenticated principal), membership-enforced: no
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
        (status = 404, description = "No such repo; or the object is not a member of this repo; or the file is missing from the CAS")
    )
)]
pub async fn get_raw(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Response, ApiError> {
    let id = parse_single_id(&id_hex)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;

    let is_member = state
        .read(move |conn| db::repo_blob_check(conn, repo_id, &id).map_err(ApiError::log_internal))
        .await?;
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

/// `Cas::put` (file fsynced) plus the stored file's byte size, for the
/// `objects.csize` column.
///
/// The shard layout (`objects/<2hex>/<2hex>/<62hex>` under the data dir) is
/// the documented data-dir contract (see the `naatre_store` crate docs);
/// reading the stored size here keeps `naatre-store` untouched by this
/// slice. A successful put with no file behind it is a broken invariant.
fn put_and_size(
    cas: &dyn BlobStore,
    data_dir: &FsPath,
    id: &Id,
    payload: &[u8],
) -> Result<i64, ApiError> {
    let stored = cas.put(payload).map_err(ApiError::log_internal)?;
    debug_assert_eq!(stored, *id, "cas.put must address the verified payload");
    let path = shard_file(data_dir, id);
    std::fs::metadata(&path)
        .map(|meta| meta.len() as i64)
        .map_err(ApiError::log_internal)
}

/// CAS file path for `id` under the data dir: `objects/ab/cd/<62hex>`.
fn shard_file(data_dir: &FsPath, id: &Id) -> PathBuf {
    let hex = id.to_hex();
    data_dir
        .join("objects")
        .join(&hex[0..2])
        .join(&hex[2..4])
        .join(&hex[4..])
}
