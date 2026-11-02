//! `/v1/repos/{r}/snapshots…`: snapshot create (presence-validated),
//! metadata fetch, and per-blob fetch/put — all against the ephemeral
//! namespace, never the history CAS.
//!
//! Create (writer-class) commits atomically: the client-built tree rows,
//! their blob-presence validation, and the snapshot row all land in one
//! transaction, so a rejected POST leaves zero rows and lists every missing
//! blob id in `details` (list capped, with a truncation flag). Path scopes
//! gate the POST — every file path in the posted closure must sit inside
//! the token's scope, segment-aligned (same rule as changeset create); the
//! objects PUT below stays repo-level, because a blob has no path until a
//! tree references it. Expiry is enforced at EVERY fetch (metadata, bytes,
//! trees, and re-POSTs alike): an expired snapshot answers 410
//! `snapshot_expired` before anything else about it is served, and an
//! idempotent re-POST returns the RECORDED expiry — never a recomputed one
//! (a differing `ttl_secs` is 422). GETs are owned by the creating
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

use super::{repo_read_gate, repo_write_gate};
use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::AuthedPrincipal;
use crate::db::{self, TreeEntryRow, TreeWrite};
use crate::extract::AppJson;
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

/// Max DISTINCT blob ids one snapshot POST may reference (422 beyond,
/// `details.limit`). The presence check is one filesystem stat per
/// referenced blob inside the write slot — without this cap a max-size body
/// could stall every land behind an unbounded walk.
const MAX_SNAPSHOT_BLOBS: usize = 8_192;

/// Cap on the `missing_blobs` list in a rejected POST's details. The full
/// missing set is known server-side; the response names the first 256
/// sorted ids plus a `missing_blobs_truncated` flag.
const MISSING_BLOBS_LIST_CAP: usize = 256;

/// Max snapshot TTL: 366 days. The plan pins only the 1-minute floor; this
/// ceiling is a deliberate server-side safety bound (supervisor judgment,
/// fix3a review, documented in the route contract): it keeps
/// `now_ns + ttl_secs * 1e9` far inside i64 and bounds how long ephemeral
/// data can be held. 422 beyond.
const SNAPSHOT_TTL_MAX_SECS: u64 = 366 * 24 * 60 * 60;

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
    /// TTL in seconds (default 604800 = 7d; below 60 or above 31622400 =
    /// 366 days → 422. The ceiling is a server-side safety bound; the plan
    /// pins only the 1-minute floor).
    pub ttl_secs: Option<u64>,
    /// Canonical tree bytes (root + full subtree closure), base64.
    pub trees: Vec<String>,
}

/// Created snapshot body (201 + `Location`; an identical re-POST → 200
/// carrying the RECORDED expiry from the snapshot row, never a recomputed
/// one).
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

/// Creates a snapshot (writer and above, path-scope enforced over the
/// closure's file paths): validates the posted tree closure, writes the
/// tree rows, checks every referenced blob is present under
/// `ephemeral/<snap-id>/`, and commits the snapshot row — all in one
/// transaction. A missing blob rejects the whole POST with 422 and a capped
/// `details.missing_blobs` list, zero rows written. An identical re-POST
/// answers 200 with the RECORDED expiry; a differing `ttl_secs` is 422 with
/// that expiry in `details`; re-POSTing an expired id is 410.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/snapshots",
    params(("r" = String, Path, description = "Repo name")),
    request_body = CreateSnapshotRequest,
    responses(
        (status = 201, description = "Snapshot created", body = SnapshotCreated),
        (status = 200, description = "Identical re-POST (same snap id, tree, principal, effective ttl_secs) — idempotent; the RECORDED expiry is returned, never recomputed", body = SnapshotCreated),
        (status = 400, description = "An id is not 64 lowercase hex, or a tree payload is not valid base64"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role, or a file path in the posted closure is outside the token's path scope (details.path names the first offender)"),
        (status = 404, description = "No such repo"),
        (status = 410, description = "The snap id already exists here and is past its TTL (`snapshot_expired`) — a re-POST never resurrects it; or a closure blob is redacted in this repo (`object_redacted`, details-free message names the ids)"),
        (status = 413, description = "A canonical tree exceeds the 16 MiB cap"),
        (status = 422, description = "ttl_secs below 60 or above the 366-day cap; tree closure incomplete; the closure references more than 8192 distinct blobs (details.limit); ttl_secs differs from an existing snapshot's recorded TTL (details.recorded_expires_ns); or referenced blobs missing from ephemeral/<snap-id>/ (details.missing_blobs, capped at 256 with details.missing_blobs_truncated)")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<CreateSnapshotRequest>,
) -> Result<Response, ApiError> {
    // The M13b repo write gate: public repos keep the writer matrix (the
    // same 403), private repos admit granted writers (uniform 404 for
    // the rest).
    repo_write_gate(&state, &auth, &repo).await?;
    let snap_id = parse_id(&body.0.snap_id)?;
    let tree_id = parse_id(&body.0.tree_id)?;
    let ttl_secs = body.0.ttl_secs.unwrap_or(SNAPSHOT_DEFAULT_TTL_SECS);
    if ttl_secs < SNAPSHOT_MIN_TTL_SECS {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("ttl_secs must be at least {SNAPSHOT_MIN_TTL_SECS} seconds"),
        ));
    }
    if ttl_secs > SNAPSHOT_TTL_MAX_SECS {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("ttl_secs must be at most {SNAPSHOT_TTL_MAX_SECS} seconds (366 days)"),
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

    // Two bounded walks over the closure, both run BEFORE the write slot
    // is touched (an in-slot walk would let a max-size body stall every
    // land behind unbounded per-blob stats): the distinct blob ids for the
    // presence check, and the scope verdict — the first file path outside
    // the token's scope.
    let (blobs, over_blob_cap) = walk_closure(&trees, &tree_id);

    // Path scope (the plan's writes row: "path-prefix scope gates these —
    // segment-aligned"), mirroring changeset create: authorization is
    // checked before any content-validation detail is produced.
    if let Some(offender) = violation_in_closure(&trees, &tree_id, auth.scope_path.as_deref()) {
        return Err(
            ApiError::new(ApiCode::Forbidden, "path is outside this token's scope")
                .with_details(serde_json::json!({ "path": offender })),
        );
    }
    if over_blob_cap {
        return Err(ApiError::new(
            ApiCode::Validation,
            format!("snapshots reference at most {MAX_SNAPSHOT_BLOBS} distinct blobs per POST"),
        )
        .with_details(serde_json::json!({ "limit": MAX_SNAPSHOT_BLOBS })));
    }

    let principal = auth.principal.clone();
    let ephemeral = Arc::clone(&state.ephemeral);
    let now_ns = state.now_ns() as i64;
    let ttl_ns = (ttl_secs as i64) * 1_000_000_000;
    let expires_ns = now_ns + ttl_ns;

    enum Outcome {
        Created,
        Existing { recorded_expires_ns: i64 },
    }

    let repo_for_slot = repo.clone();
    let outcome = state
        .write_repo(repo_for_slot, move |cx| {
            let repo_id = cx.id;
            let conn = cx.db;
            // Re-POST resolution first, reads only, no writes on any branch:
            // the RECORDED row is authoritative, so an identical re-POST
            // answers the recorded expiry (never a recomputed one), a
            // differing ttl_secs is 422 with that expiry in details, and an
            // expired snapshot is 410 — expiry is enforced at every fetch,
            // re-POSTs included; nothing resurrects the row.
            if let Some(row) =
                db::snapshot_get(conn, repo_id, &snap_id).map_err(ApiError::log_internal)?
            {
                if row.principal != principal || row.tree_id != tree_id.0 {
                    return Err(ApiError::new(
                        ApiCode::Validation,
                        "snapshot id is already used by a different snapshot",
                    ));
                }
                if row.expires_ns <= now_ns {
                    return Err(ApiError::new(
                        ApiCode::SnapshotExpired,
                        format!("snapshot {} is past its TTL", snap_id.to_hex()),
                    ));
                }
                if row.expires_ns - row.created_ns != ttl_ns {
                    return Err(ApiError::new(
                        ApiCode::Validation,
                        "ttl_secs differs from the snapshot's recorded TTL; re-POST the \
                         original ttl_secs or let the row expire",
                    )
                    .with_details(serde_json::json!({
                        "recorded_expires_ns": row.expires_ns as u64
                    })));
                }
                // The 200 re-POST re-checks no blob presence, safe by the
                // GC rules: an unexpired row's dir is never rowless-swept,
                // and a purge requires expires_ns <= now — a GC-rule change
                // here must re-validate presence.
                return Ok(Outcome::Existing {
                    recorded_expires_ns: row.expires_ns,
                });
            }

            // One transaction: tree rows, the presence check, and the
            // snapshot row commit together or not at all (the plan's "no
            // row" for rejected POSTs covers the tree rows too).
            let tx = conn.transaction().map_err(ApiError::log_internal)?;
            // The tombstone boundary (inside the write slot): a closure
            // blob tombstoned in this repo — active or purged — refuses
            // the snapshot (the tree entries would carry the redacted
            // blob's name, size, and hash).
            let redacted = db::redactions_active_among(&tx, repo_id, &blobs)
                .map_err(ApiError::log_internal)?;
            if !redacted.is_empty() {
                let hexes: Vec<String> = redacted.iter().map(Id::to_hex).collect();
                return Err(ApiError::new(
                    ApiCode::ObjectRedacted,
                    format!(
                        "blobs are redacted in this repo; snapshots may not reference them: {}",
                        hexes.join(", ")
                    ),
                ));
            }
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
            // Snapshot trees carry NO repo_trees membership (own-principal
            // data, TTL'd): they are served only through the owner-gated
            // snapshot routes below, never through the bare-id tree route.
            db::trees_write_snapshot(&tx, &writes).map_err(ApiError::log_internal)?;

            // Presence check over the closure walked above: every distinct
            // blob the tree references must exist under
            // `ephemeral/<snap-id>/`. The referenced set is already capped
            // pre-slot; the reported list is capped here.
            let (missing, truncated) = {
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
                let truncated = missing.len() > MISSING_BLOBS_LIST_CAP;
                missing.truncate(MISSING_BLOBS_LIST_CAP);
                (missing, truncated)
            };
            if !missing.is_empty() {
                return Err(ApiError::new(
                    ApiCode::Validation,
                    "referenced blobs are missing from the snapshot namespace; \
                     upload them first",
                )
                .with_details(serde_json::json!({
                    "missing_blobs": missing,
                    "missing_blobs_truncated": truncated,
                })));
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

    let recorded = match outcome {
        Outcome::Created => expires_ns,
        Outcome::Existing {
            recorded_expires_ns,
        } => recorded_expires_ns,
    };
    let body = SnapshotCreated {
        id: snap_id.to_hex(),
        expires_ns: recorded as u64,
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
        Outcome::Existing { .. } => Ok((StatusCode::OK, Json(body)).into_response()),
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
    repo_read_gate(&state, &auth, &repo).await?;
    let snap_id = parse_id(&id_hex)?;
    let row = load_owned_unexpired(&state, &repo, &snap_id, &auth.principal).await?;
    Ok(Json(SnapshotResponse {
        id: snap_id.to_hex(),
        tree_id: Id(row.tree_id.try_into().expect("tree id is 32 bytes")).to_hex(),
        created_ns: row.created_ns as u64,
        expires_ns: row.expires_ns as u64,
    }))
}

/// Fetches one snapshot blob's raw bytes, hash-verified from
/// `ephemeral/<snap-id>/` (owned by the principal; the M10b redaction gate
/// FIRST — a tombstoned blob id makes every snapshot holding it
/// unrestorable immediately, redaction overriding the TTL root — then the
/// expiry re-check: an expired snapshot serves nothing, not even its
/// bytes).
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
        (status = 410, description = "The blob is redacted in this repo (`object_redacted`), or the snapshot is past its TTL (`snapshot_expired`)")
    )
)]
pub async fn get_blob(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex, oid_hex)): Path<(String, String, String)>,
) -> Result<Response, ApiError> {
    repo_read_gate(&state, &auth, &repo).await?;
    let snap_id = parse_id(&id_hex)?;
    let oid = parse_id(&oid_hex)?;
    // The redaction gate BEFORE the TTL check: a tombstoned blob id is
    // unrestorable now, regardless of the snapshot's remaining TTL
    // ("redaction overrides the TTL root"). Content-addressing makes the
    // ephemeral copy byte-identical to the gated history blob — same id,
    // same content, same tombstone.
    {
        let oid_gate = oid;
        let redacted = state
            .read_repo(repo.clone(), move |cx| {
                let repo_id = cx.id;
                let conn = cx.db;
                db::redaction_active(conn, repo_id, &oid_gate).map_err(ApiError::log_internal)
            })
            .await?;
        if redacted {
            return Err(ApiError::new(
                ApiCode::ObjectRedacted,
                format!("blob {} is redacted in {repo}", oid.to_hex()),
            ));
        }
    }
    load_owned_unexpired(&state, &repo, &snap_id, &auth.principal).await?;

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

/// Lists one of a snapshot's trees' entries. Snapshot trees carry NO
/// `repo_trees` membership (own-principal, TTL'd data), so the bare-id
/// repo tree route 404s them; this owner-gated route is their only server
/// surface — same gate as the blob fetch: the snapshot must exist for this
/// principal in this repo and be unexpired (expiry re-checked FIRST), and
/// the requested tree must sit in the snapshot root's closure (404
/// otherwise — the route serves the snapshot's own subtree, never an
/// arbitrary tree id that merely exists on the server).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/snapshots/{id}/trees/{tid}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Snapshot id, 64 lowercase hex"),
        ("tid" = String, Path, description = "Tree id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The tree's entries. Entries whose blob is redacted in this repo serve with `redacted: true` and null object_id/size/exec; a dir entry with a redacted descendant serves size-null", body = crate::routes::trees::TreeResponse),
        (status = 400, description = "An id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo; or the snapshot does not exist for this principal, or the tree is not part of the snapshot's closure"),
        (status = 410, description = "The snapshot is past its TTL (`snapshot_expired`)")
    )
)]
pub async fn get_snapshot_tree(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex, tid_hex)): Path<(String, String, String)>,
) -> Result<Json<crate::routes::trees::TreeResponse>, ApiError> {
    repo_read_gate(&state, &auth, &repo).await?;
    let snap_id = parse_id(&id_hex)?;
    let tree_id = parse_id(&tid_hex)?;
    let row = load_owned_unexpired(&state, &repo, &snap_id, &auth.principal).await?;

    // Closure membership first (404 before any entries are served): only
    // the snapshot root itself and the trees reachable from it over dir
    // entries qualify. Entries whose blob is tombstoned in this repo mask
    // exactly as the repo tree route does — the plan's universal gate
    // covers every serving path, owner-scoped ones included (the shared
    // [`crate::routes::trees::mask_tree_entries`], dir-aggregate rule
    // included).
    let root = Id(row.tree_id.try_into().expect("tree id is 32 bytes"));
    let entries = state
        .read_repo(repo.clone(), move |cx| {
            let repo_id = cx.id;
            let conn = cx.db;
            if !db::tree_in_closure(conn, &root, &tree_id).map_err(ApiError::log_internal)? {
                return Ok(None);
            }
            let rows = db::tree_entries_load(conn, &tree_id).map_err(ApiError::log_internal)?;
            Ok::<_, ApiError>(Some(crate::routes::trees::mask_tree_entries(
                conn, repo_id, rows,
            )?))
        })
        .await?;
    let Some(entries) = entries else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!(
                "tree {} is not part of snapshot {}",
                tree_id.to_hex(),
                snap_id.to_hex()
            ),
        ));
    };
    Ok(Json(crate::routes::trees::TreeResponse { entries }))
}

/// PUTs one blob into `ephemeral/<snap-id>/` (writer and above, repo-level):
/// hash-verified against the URL id like every PUT, capped at 64 MiB. No row
/// is written — the blobs are rowless until the snapshot POST commits; a
/// snapshot that never POSTs is swept by GC after the default TTL (7 days).
///
/// Repo-level by design (the plan's objects-PUT cell, shared with the
/// history objects PUT): path scopes do NOT gate this route — a blob has no
/// path until a tree references it. The scope is enforced by the snapshot
/// POST over the closure's file paths.
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
        (status = 410, description = "The blob is redacted in this repo (`object_redacted`); upload refused"),
        (status = 413, description = "Payload over the 64 MiB blob cap")
    )
)]
pub async fn put_blob(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex, oid_hex)): Path<(String, String, String)>,
    payload: axum::body::Bytes,
) -> Result<Response, ApiError> {
    // The M13b repo write gate (public repos keep the writer matrix;
    // private repos admit granted writers).
    repo_write_gate(&state, &auth, &repo).await?;
    let snap_id = parse_id(&id_hex)?;
    let oid = parse_id(&oid_hex)?;
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

    // The tombstone boundary (before ANY ephemeral write): a redacted id —
    // active or purged — is refused; same content hashes to the same id,
    // and the snapshot blob fetch gates on the same tombstone, so storing
    // the bytes would only invite the 410.
    {
        let redacted = state
            .read_repo(repo.clone(), move |cx| {
                let repo_id = cx.id;
                let conn = cx.db;
                db::redaction_active(conn, repo_id, &oid).map_err(ApiError::log_internal)
            })
            .await?;
        if redacted {
            return Err(ApiError::new(
                ApiCode::ObjectRedacted,
                format!(
                    "blob {} is redacted in {repo}; upload refused",
                    oid.to_hex()
                ),
            ));
        }
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

/// One depth-first walk over the decoded closure from `root`: collects
/// every distinct blob id for the presence check. Collection stops storing
/// at [`MAX_SNAPSHOT_BLOBS`] — the second return value reports whether the
/// cap was passed — so a hostile body cannot balloon the write slot's work.
/// The seen-set bounds the walk to distinct trees: a tree visited once has
/// already contributed every blob id it references, so shared subtrees
/// need no second visit. Path scope is NOT collected here —
/// [`violation_in_closure`] owns it, because enumerating every occurrence
/// of a shared subtree is exponential in the closure's sharing depth.
/// Closure completeness has already been validated, so every dir entry
/// resolves in `trees`, and content-addressed trees cannot cycle (an
/// entry's id covers the bytes that name it). Running synchronously on the
/// handler thread is acceptable under the current caps (96 MiB body / 10k
/// trees); move it to `spawn_blocking` if the caps grow.
fn walk_closure(
    trees: &std::collections::HashMap<Id, (Vec<u8>, Tree)>,
    root: &Id,
) -> (Vec<Id>, bool) {
    let mut blobs: Vec<Id> = Vec::new();
    let mut seen_blobs = HashSet::new();
    let mut seen_trees = HashSet::new();
    let mut over_cap = false;
    let mut stack = vec![*root];
    while let Some(tree_id) = stack.pop() {
        if !seen_trees.insert(tree_id) {
            continue;
        }
        let Some((_, tree)) = trees.get(&tree_id) else {
            continue; // unreachable: closure completeness was validated above
        };
        for e in &tree.entries {
            match e.kind {
                tree::EntryKind::File => {
                    if seen_blobs.insert(e.object) {
                        if blobs.len() == MAX_SNAPSHOT_BLOBS {
                            over_cap = true;
                        } else {
                            blobs.push(e.object);
                        }
                    }
                }
                tree::EntryKind::Dir => stack.push(e.object),
            }
        }
    }
    (blobs, over_cap)
}

/// The POST scope gate: the first file path in the closure rooted at
/// `root` that escapes `scope` (403 `details.path`), or `None` when every
/// path sits inside. This deliberately does NOT enumerate occurrences: a
/// content-addressed subtree may be mounted at many prefixes (root
/// `[app -> T, docs -> T]`), and a tree pointing two names at the next
/// compounds those per level — a ~40-tree diamond spans 2^40 paths, so
/// visiting every `(tree, prefix)` occurrence would hang the handler
/// thread on a ~1 KB body. Instead each occurrence is classified by its
/// dir prefix against the scope, and each class is solved once:
///
/// - covered ([`crate::authz::scope_allows`] admits the dir prefix
///   itself): every path below is in scope by construction — the subtree
///   is never visited;
/// - straddling (the scope path extends the prefix): the only in-scope
///   descent follows the scope's own segment chain, so direct files are
///   checked with `scope_allows`, a dir child on that chain is pushed
///   (at most one child name can extend a prefix to a given scope), and
///   any other dir child is disjoint;
/// - disjoint (prefix and scope diverge): every file below is outside by
///   construction, so the verdict reduces to "does the subtree contain a
///   file at all" — contents-only, memoized per tree id via
///   [`closure_first_file`].
///
/// The memo key is the tree id within ONE class, not a two-state
/// `(tree, inside)` flag: the same tree can occur both straddling and
/// disjoint, and the two contexts can disagree (a file exactly at the
/// scope path is allowed from a straddling occurrence, while any file of
/// a disjoint occurrence is a violation), so a two-state memo would let
/// whichever context evaluates first decide the other — reopening the
/// shared-subtree bypass this walk closes. Straddling contexts need no
/// memo: they follow the scope's own segment chain, which cannot branch
/// and cannot cycle (ids are content-addressed), so each tree occurs in
/// them at most once.
///
/// Cost: O(distinct trees + entries) — one memoized scan per tree plus a
/// linear chain walk; no new caps.
fn violation_in_closure(
    trees: &std::collections::HashMap<Id, (Vec<u8>, Tree)>,
    root: &Id,
    scope: Option<&str>,
) -> Option<String> {
    // None admits every path.
    let scope = scope?;

    // Class of a dir prefix (trailing `/` included; the root is `""`).
    // The root is never covered under Some(scope): only the degenerate
    // `Some("")` scope equality-matches it, and that scope admits no file
    // path at all, so its closure must reach the per-file checks below.
    fn covered(scope: &str, prefix: &str) -> bool {
        !prefix.is_empty() && crate::authz::scope_allows(Some(scope), prefix)
    }
    // The scope path extends the prefix; the trailing `/` keeps this
    // segment-aligned (`Some("ab")` does not straddle `a/`).
    fn straddles(scope: &str, prefix: &str) -> bool {
        scope.starts_with(prefix)
    }

    let mut memo: std::collections::HashMap<Id, Option<String>> = std::collections::HashMap::new();
    let mut stack: Vec<(Id, String)> = vec![(*root, String::new())];
    while let Some((tree_id, prefix)) = stack.pop() {
        let Some((_, tree)) = trees.get(&tree_id) else {
            continue; // unreachable: closure completeness was validated above
        };
        if covered(scope, &prefix) {
            continue; // every path below this occurrence is inside the scope
        }
        if !straddles(scope, &prefix) {
            // Disjoint: any file below is outside the scope, so the whole
            // question is whether the subtree holds a file at all.
            if let Some(rel) = closure_first_file(trees, &mut memo, &tree_id) {
                return Some(format!("{prefix}{rel}"));
            }
            continue;
        }
        for e in &tree.entries {
            let path = format!("{prefix}{}", e.name);
            match e.kind {
                tree::EntryKind::File => {
                    if !crate::authz::scope_allows(Some(scope), &path) {
                        return Some(path);
                    }
                }
                tree::EntryKind::Dir => {
                    let child = format!("{path}/");
                    if covered(scope, &child) {
                        continue; // the child's whole subtree sits inside
                    }
                    if straddles(scope, &child) {
                        stack.push((e.object, child));
                    } else if let Some(rel) = closure_first_file(trees, &mut memo, &e.object) {
                        return Some(format!("{child}{rel}"));
                    }
                }
            }
        }
    }
    None
}

/// The relative path of the first file in the subtree rooted at `root`
/// (entries in stored order, dirs descended depth-first), or `None` when
/// the subtree holds no file at all. Purely contents-derived, so `memo`
/// caches the verdict per tree id and each tree's entries are scanned at
/// most once across all calls.
fn closure_first_file(
    trees: &std::collections::HashMap<Id, (Vec<u8>, Tree)>,
    memo: &mut std::collections::HashMap<Id, Option<String>>,
    root: &Id,
) -> Option<String> {
    enum Frame<'a> {
        /// Resolve `id`'s verdict (memo hit pops for free).
        Enter(Id),
        /// Scan `tree`'s entries from `idx`. Every entry before `idx` is
        /// settled. A dir child whose verdict is still pending is NOT
        /// skipped: the frame parks at its index (`idx` unchanged) and
        /// re-reads that entry once [`Enter`] memoizes the child — a
        /// consuming iterator here would drop the entry's contribution
        /// and memoize a wrong `None` for the whole subtree.
        Scan {
            tree: Id,
            entries: &'a [naatre_core::tree::TreeEntry],
            idx: usize,
        },
    }
    let mut stack = vec![Frame::Enter(*root)];
    while let Some(frame) = stack.pop() {
        match frame {
            Frame::Enter(id) => {
                if memo.contains_key(&id) {
                    continue; // the resuming Scan frame reads the memo itself
                }
                let Some((_, tree)) = trees.get(&id) else {
                    memo.insert(id, None); // unreachable: validated closure
                    continue;
                };
                memo.insert(id, None); // provisional; Scan below finalizes
                stack.push(Frame::Scan {
                    tree: id,
                    entries: &tree.entries,
                    idx: 0,
                });
            }
            Frame::Scan {
                tree,
                entries,
                mut idx,
            } => {
                let mut verdict: Option<String> = None;
                let mut descend: Option<Id> = None;
                while let Some(e) = entries.get(idx) {
                    match e.kind {
                        tree::EntryKind::File => {
                            verdict = Some(e.name.clone());
                            break;
                        }
                        tree::EntryKind::Dir => match memo.get(&e.object) {
                            Some(Some(rel)) => {
                                verdict = Some(format!("{}/{}", e.name, rel));
                                break;
                            }
                            Some(None) => idx += 1, // that subtree holds no file
                            None => {
                                descend = Some(e.object);
                                break; // idx parked on this entry
                            }
                        },
                    }
                }
                match descend {
                    Some(child) => {
                        stack.push(Frame::Scan { tree, entries, idx });
                        stack.push(Frame::Enter(child));
                    }
                    None => {
                        memo.insert(tree, verdict);
                    }
                }
            }
        }
    }
    memo.get(root).cloned().flatten()
}

/// The shared GET gate: the row must exist in this repo, belong to the
/// requesting principal (otherwise 404 — indistinguishable from absent),
/// and be unexpired (410 `snapshot_expired` before anything else serves).
async fn load_owned_unexpired(
    state: &AppState,
    repo: &str,
    snap_id: &Id,
    principal: &str,
) -> Result<db::SnapshotRow, ApiError> {
    let id = *snap_id;
    let owner = principal.to_string();
    let repo = repo.to_string();
    let row = state
        .read_repo(repo, move |cx| {
            db::snapshot_get(cx.db, cx.id, &id).map_err(ApiError::log_internal)
        })
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
