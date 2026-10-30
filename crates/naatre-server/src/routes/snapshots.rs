//! `/v1/repos/{r}/snapshots…`: snapshot create (presence-validated),
//! metadata fetch, and per-blob fetch/put — all against the ephemeral
//! namespace, never the history CAS.
//!
//! Create (writer-class, repo-level) commits atomically: the client-built
//! tree rows, their blob-presence validation, and the snapshot row all land
//! in one transaction, so a rejected POST leaves zero rows and lists every
//! missing blob id in `details`. Expiry is enforced at EVERY fetch (metadata
//! and bytes): an expired snapshot answers 410 `snapshot_expired` before
//! anything else about it is served. GETs are owned by the creating
//! principal — another principal's snapshot id is 404, indistinguishable
//! from a nonexistent one.

use std::collections::HashSet;
use std::sync::Arc;

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use axum::http::{HeaderName, HeaderValue, StatusCode, header};
use axum::response::{IntoResponse, Response};
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use naatre_core::ids::{Id, object_id};
use naatre_core::tree::{self, Tree};
use naatre_store::BLOB_LIMIT;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db::{self, TreeEntryRow, TreeWrite};
use crate::extract::AppJson;
use crate::routes::repo_id_or_404;
use crate::snapshots_gc::{SNAPSHOT_DEFAULT_TTL_SECS, SNAPSHOT_MIN_TTL_SECS};
use crate::state::AppState;

/// Byte cap for one canonical tree in the POST body (same bound as core's
/// `changeset::MAX_OBJECT_BYTES`, which is `pub(crate)` — trees are encoded
/// by the same 16 MiB rule as every other canonical object).
const TREE_CANONICAL_CAP: usize = 16 * 1024 * 1024;

/// Max canonical trees per snapshot POST (root + subtrees). The client
/// sends the full closure of its built tree; a depth-`d` fanout-`f` tree has
/// O(f^d) nodes, so the cap bounds hostile bodies, not real snapshots.
const MAX_SNAPSHOT_TREES: usize = 10_000;

/// Request body of snapshot create. `trees` carries the canonical bytes of
/// the referenced tree and every subtree below it (standard base64): the
/// client builds them bottom-up, and the server writes their rows inside the
/// create transaction so the blob-presence walk can traverse them.
#[derive(Deserialize, ToSchema)]
pub struct CreateSnapshotRequest {
    /// Client-generated snapshot id, 64 lowercase hex (CSPRNG).
    pub snap_id: String,
    /// The root tree id, 64 lowercase hex; must be among `trees`.
    pub tree_id: String,
    /// TTL in seconds (default 604800 = 7d; below 60 → 422).
    pub ttl_secs: Option<u64>,
    /// Canonical tree bytes (root + full subtree closure), base64.
    pub trees: Vec<String>,
}

/// Created snapshot body (201 + `Location`; an identical re-POST → 200).
#[derive(Serialize, ToSchema)]
pub struct SnapshotCreated {
    pub id: String,
    pub expires_ns: u64,
}

/// A snapshot's metadata.
#[derive(Serialize, ToSchema)]
pub struct SnapshotResponse {
    pub id: String,
    pub tree_id: String,
    pub created_ns: u64,
    pub expires_ns: u64,
}

/// The id echo of a successful snapshot-blob PUT.
#[derive(Serialize, ToSchema)]
pub struct SnapshotBlobStored {
    pub id: String,
}

/// Creates a snapshot (writer and above, repo-level): validates the posted
/// tree closure, writes the tree rows, checks EVERY referenced blob is
/// present under `ephemeral/<snap-id>/`, and commits the snapshot row — all
/// in one transaction. A missing blob rejects the whole POST with 422 and
/// `details.missing_blobs`, zero rows written.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/snapshots",
    params(("r" = String, Path, description = "Repo name")),
    request_body = CreateSnapshotRequest,
    responses(
        (status = 201, description = "Snapshot created", body = SnapshotCreated),
        (status = 200, description = "Identical re-POST (same snap id, tree, principal) — idempotent", body = SnapshotCreated),
        (status = 400, description = "An id is not 64 lowercase hex, or a tree payload is not valid base64"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role"),
        (status = 404, description = "No such repo"),
        (status = 413, description = "A canonical tree exceeds the 16 MiB cap"),
        (status = 422, description = "ttl_secs below 60; tree closure incomplete; referenced tree not derived in this repo; or referenced blobs missing from ephemeral/<snap-id>/ (details.missing_blobs)")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<CreateSnapshotRequest>,
) -> Result<Response, ApiError> {
    require_role(&auth, Role::Writer)?;
    let snap_id = parse_id(&body.0.snap_id)?;
    let tree_id = parse_id(&body.0.tree_id)?;
    let ttl_secs = body.0.ttl_secs.unwrap_or(SNAPSHOT_DEFAULT_TTL_SECS);
    if ttl_secs < SNAPSHOT_MIN_TTL_SECS {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("ttl_secs must be at least {SNAPSHOT_MIN_TTL_SECS} seconds"),
        ));
    }

    // Decode and validate the tree closure before touching the write slot:
    // shape failures are 400/422 with no store work at all.
    let trees = decode_trees(&body.0.trees)?;
    if !trees.contains_key(&tree_id) {
        return Err(ApiError::new(
            ApiCode::Validation,
            "the referenced tree is not among the posted trees",
        )
        .with_details(serde_json::json!({ "missing_trees": [tree_id.to_hex()] })));
    }
    for (_, tree) in trees.values() {
        for e in &tree.entries {
            if e.kind == tree::EntryKind::Dir && !trees.contains_key(&e.object) {
                return Err(ApiError::new(
                    ApiCode::Validation,
                    format!(
                        "subtree {} is referenced but not posted; send the full closure",
                        e.object.to_hex()
                    ),
                )
                .with_details(serde_json::json!({ "missing_trees": [e.object.to_hex()] })));
            }
        }
    }

    let repo_id = repo_id_or_404(&state, &repo).await?;
    let principal = auth.principal.clone();
    let ephemeral = Arc::clone(&state.ephemeral);
    let now_ns = state.now_ns() as i64;
    let expires_ns = now_ns + (ttl_secs as i64) * 1_000_000_000;

    enum Outcome {
        Created,
        Existing,
    }

    let outcome = state
        .write(move |store| {
            let conn = store.conn();
            // One transaction: tree rows, the presence walk, and the
            // snapshot row commit together or not at all (the plan's "no
            // row" for rejected POSTs covers the tree rows too).
            let tx = conn
                .unchecked_transaction()
                .map_err(ApiError::log_internal)?;
            let writes: Vec<TreeWrite> = trees
                .iter()
                .map(|(id, (canonical, tree))| TreeWrite {
                    id: *id,
                    canonical: canonical.clone(),
                    entries: tree
                        .entries
                        .iter()
                        .map(|e| TreeEntryRow {
                            name: e.name.clone(),
                            kind: match e.kind {
                                tree::EntryKind::File => 0,
                                tree::EntryKind::Dir => 1,
                            },
                            object_id: e.object.0.to_vec(),
                            size: e.size as i64,
                            exec: e.exec,
                        })
                        .collect(),
                })
                .collect();
            db::trees_write(&tx, repo_id, &writes).map_err(ApiError::log_internal)?;

            // Presence walk over the rows just written: every blob the tree
            // references must exist under ephemeral/<snap-id>/.
            let blobs = collect_blob_ids(&tx, &tree_id)?;
            let missing = {
                let snap = snap_id.to_hex();
                let has = ephemeral
                    .has(&snap, &blobs)
                    .map_err(ApiError::log_internal)?;
                let mut missing: Vec<String> = blobs
                    .iter()
                    .zip(has)
                    .filter(|(_, present)| !*present)
                    .map(|(id, _)| id.to_hex())
                    .collect();
                missing.sort();
                missing.dedup();
                missing
            };
            if !missing.is_empty() {
                return Err(ApiError::new(
                    ApiCode::Validation,
                    "referenced blobs are missing from the snapshot namespace; \
                     upload them first",
                )
                .with_details(serde_json::json!({ "missing_blobs": missing })));
            }

            // Idempotent re-POST: same snap id, same tree, same principal →
            // 200 with the recorded expiry. Anything else on a taken id is
            // rejected.
            if let Some(row) =
                db::snapshot_get(&tx, repo_id, &snap_id).map_err(ApiError::log_internal)?
            {
                if row.principal == principal && row.tree_id == tree_id.0 {
                    return Ok(Outcome::Existing);
                }
                return Err(ApiError::new(
                    ApiCode::Validation,
                    "snapshot id is already used by a different snapshot",
                ));
            }
            db::snapshot_insert(
                &tx, repo_id, &snap_id, &tree_id, &principal, now_ns, expires_ns,
            )
            .map_err(ApiError::log_internal)?;
            tx.commit().map_err(ApiError::log_internal)?;
            Ok(Outcome::Created)
        })
        .await?;

    // Opportunistic GC after the commit — never in the POST's failure path.
    if state.snapshot_gc_enabled {
        let gc_state = Arc::clone(&state);
        tokio::spawn(async move {
            if let Err(e) = crate::snapshots_gc::gc_snapshots(&gc_state).await {
                tracing::warn!(error = %e, "opportunistic snapshot GC failed");
            }
        });
    }

    let body = SnapshotCreated {
        id: snap_id.to_hex(),
        expires_ns: expires_ns as u64,
    };
    match outcome {
        Outcome::Created => Ok((
            StatusCode::CREATED,
            [(
                header::LOCATION,
                format!("/v1/repos/{repo}/snapshots/{}", snap_id.to_hex()),
            )],
            Json(body),
        )
            .into_response()),
        Outcome::Existing => Ok((StatusCode::OK, Json(body)).into_response()),
    }
}

/// Fetches a snapshot's metadata (any authenticated principal; owned by the
/// creating principal — another principal's id is 404). An expired snapshot
/// answers 410 `snapshot_expired`.
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/snapshots/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Snapshot id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The snapshot's metadata", body = SnapshotResponse),
        (status = 400, description = "Id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo; or the snapshot does not exist in this repo, or belongs to another principal"),
        (status = 410, description = "The snapshot is past its TTL (`snapshot_expired`)")
    )
)]
pub async fn get_metadata(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Json<SnapshotResponse>, ApiError> {
    let snap_id = parse_id(&id_hex)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let row = load_owned_unexpired(&state, repo_id, &snap_id, &auth.principal).await?;
    Ok(Json(SnapshotResponse {
        id: snap_id.to_hex(),
        tree_id: Id(row.tree_id.try_into().expect("tree id is 32 bytes")).to_hex(),
        created_ns: row.created_ns as u64,
        expires_ns: row.expires_ns as u64,
    }))
}

/// Fetches one snapshot blob's raw bytes, hash-verified from
/// `ephemeral/<snap-id>/` (owned by the principal; expiry re-checked FIRST —
/// an expired snapshot serves nothing, not even its bytes).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/snapshots/{id}/objects/{oid}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Snapshot id, 64 lowercase hex"),
        ("oid" = String, Path, description = "Blob id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "Raw blob bytes"),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo; or the snapshot does not exist, belongs to another principal, or lacks the blob"),
        (status = 410, description = "The snapshot is past its TTL (`snapshot_expired`)")
    )
)]
pub async fn get_blob(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex, oid_hex)): Path<(String, String, String)>,
) -> Result<Response, ApiError> {
    let snap_id = parse_id(&id_hex)?;
    let oid = parse_id(&oid_hex)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    load_owned_unexpired(&state, repo_id, &snap_id, &auth.principal).await?;

    let ephemeral = Arc::clone(&state.ephemeral);
    let snap = snap_id.to_hex();
    let bytes = state
        .blocking(move || {
            ephemeral.get_blob(&snap, &oid).map_err(|e| match e {
                naatre_store::StoreError::BlobMissing(hex) => ApiError::new(
                    ApiCode::ObjectNotFound,
                    format!("blob {hex} is missing from the snapshot"),
                ),
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

/// PUTs one blob into `ephemeral/<snap-id>/` (writer and above, repo-level):
/// hash-verified against the URL id like every PUT, capped at 64 MiB. No row
/// is written — the blobs are rowless until the snapshot POST commits; a
/// snapshot that never POSTs is swept by GC after the max TTL.
#[utoipa::path(
    put,
    path = "/v1/repos/{r}/snapshots/{id}/objects/{oid}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Snapshot id, 64 lowercase hex"),
        ("oid" = String, Path, description = "BLAKE3 of the payload, 64 lowercase hex")
    ),
    request_body(content = String, content_type = "application/octet-stream", description = "Raw blob bytes (≤ 64 MiB)"),
    responses(
        (status = 201, description = "Blob stored in the snapshot namespace", body = SnapshotBlobStored),
        (status = 200, description = "Blob already stored (idempotent re-PUT)", body = SnapshotBlobStored),
        (status = 400, description = "An id is not 64 lowercase hex, or the payload does not hash to the URL id (`mis-addressed`)"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role"),
        (status = 404, description = "No such repo"),
        (status = 413, description = "Payload over the 64 MiB blob cap")
    )
)]
pub async fn put_blob(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex, oid_hex)): Path<(String, String, String)>,
    payload: axum::body::Bytes,
) -> Result<Response, ApiError> {
    require_role(&auth, Role::Writer)?;
    let snap_id = parse_id(&id_hex)?;
    let oid = parse_id(&oid_hex)?;
    repo_id_or_404(&state, &repo).await?;
    if object_id(naatre_core::ids::Kind::Blob, &payload) != oid {
        return Err(ApiError::new(
            ApiCode::Malformed,
            "mis-addressed: payload does not hash to the given id",
        ));
    }
    if payload.len() > BLOB_LIMIT {
        return Err(ApiError::new(
            ApiCode::TooLarge,
            format!(
                "blob exceeds the {BLOB_LIMIT} byte cap (got {} bytes)",
                payload.len()
            ),
        ));
    }

    let ephemeral = Arc::clone(&state.ephemeral);
    let snap = snap_id.to_hex();
    let existed = state
        .blocking(move || {
            let existed = ephemeral
                .has(&snap, &[oid])
                .map_err(ApiError::log_internal)?[0];
            ephemeral
                .put_blob(&snap, &payload)
                .map_err(ApiError::log_internal)?;
            Ok::<bool, ApiError>(existed)
        })
        .await?;
    let status = if existed {
        StatusCode::OK
    } else {
        StatusCode::CREATED
    };
    Ok((status, Json(SnapshotBlobStored { id: oid.to_hex() })).into_response())
}

// ---- helpers --------------------------------------------------------------

/// Parses one 64-hex id path/body field; anything else is 400 `malformed`.
fn parse_id(hex: &str) -> Result<Id, ApiError> {
    Id::from_hex(hex)
        .ok_or_else(|| ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters"))
}

/// Decodes + validates the POSTed canonical trees into an id-keyed map of
/// (canonical bytes, parsed tree). Duplicate ids collapse silently
/// (identical bytes, identical id) — the last decode wins, byte-identical.
fn decode_trees(
    encoded: &[String],
) -> Result<std::collections::HashMap<Id, (Vec<u8>, Tree)>, ApiError> {
    if encoded.len() > MAX_SNAPSHOT_TREES {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("snapshots take at most {MAX_SNAPSHOT_TREES} trees per POST"),
        ));
    }
    let mut out = std::collections::HashMap::with_capacity(encoded.len());
    for b64 in encoded {
        let bytes = STANDARD
            .decode(b64.as_bytes())
            .map_err(|_| ApiError::new(ApiCode::Malformed, "tree payload is not valid base64"))?;
        if bytes.len() > TREE_CANONICAL_CAP {
            return Err(ApiError::new(
                ApiCode::TooLarge,
                "canonical tree exceeds the 16 MiB cap",
            ));
        }
        let tree = tree::decode(&bytes).map_err(|_| {
            ApiError::new(
                ApiCode::Malformed,
                "tree payload is not a valid canonical tree",
            )
        })?;
        tree::validate(&tree).map_err(|e| ApiError::new(ApiCode::Validation, e.to_string()))?;
        out.insert(
            object_id(naatre_core::ids::Kind::Tree, &bytes),
            (bytes, tree),
        );
    }
    Ok(out)
}

/// Collects every file blob id reachable from `root` over the `tree_entries`
/// rows (the rows are in the caller's transaction, so the walk sees exactly
/// what this POST is about to commit).
fn collect_blob_ids(conn: &rusqlite::Connection, root: &Id) -> Result<Vec<Id>, ApiError> {
    let mut blobs = Vec::new();
    let mut seen = HashSet::new();
    let mut stack = vec![*root];
    while let Some(tree_id) = stack.pop() {
        if !seen.insert(tree_id) {
            continue;
        }
        for e in db::tree_entries_load(conn, &tree_id).map_err(ApiError::log_internal)? {
            let id = Id(e.object_id.try_into().expect("object id is 32 bytes"));
            match e.kind {
                0 => blobs.push(id),
                _ => stack.push(id),
            }
        }
    }
    Ok(blobs)
}

/// The shared GET gate: the row must exist in this repo, belong to the
/// requesting principal (otherwise 404 — indistinguishable from absent),
/// and be unexpired (410 `snapshot_expired` before anything else serves).
async fn load_owned_unexpired(
    state: &AppState,
    repo_id: i64,
    snap_id: &Id,
    principal: &str,
) -> Result<db::SnapshotRow, ApiError> {
    let id = *snap_id;
    let owner = principal.to_string();
    let row = state
        .read(move |conn| db::snapshot_get(conn, repo_id, &id).map_err(ApiError::log_internal))
        .await?;
    let Some(row) = row.filter(|row| row.principal == owner) else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!(
                "no snapshot {} in this repo for this principal",
                snap_id.to_hex()
            ),
        ));
    };
    if row.expires_ns <= state.now_ns() as i64 {
        return Err(ApiError::new(
            ApiCode::SnapshotExpired,
            format!("snapshot {} is past its TTL", snap_id.to_hex()),
        ));
    }
    Ok(row)
}
