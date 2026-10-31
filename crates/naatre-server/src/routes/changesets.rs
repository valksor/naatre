//! `/v1/repos/{r}/changesets`: delta-only create with the full validation
//! matrix, plus by-id fetch and the root-tree pointer read.
//!
//! Scope enforcement lives here (the plan's "writes are path-scoped" cell):
//! every change's path must satisfy the token's `scope_path`, segment-aligned.
//!
//! Validation order (each failure → its code, no rows written on any of
//! them — the whole create commits in one transaction): role → scope →
//! parent ids/hex → unknown parents → core validation (paths, NFC, case
//! folds, author/message char set, empty, caps) → duplicate/unsorted paths
//! → canonical size → blob presence → tree duality.

use std::sync::Arc;

use axum::Extension;
use axum::Json;
use axum::extract::{Path, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use naatre_core::changeset::{self, Changeset, FileChange, Op};
use naatre_core::error::CoreError;
use naatre_core::ids::{Id, Kind, object_id};
use naatre_core::path::PathError;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::routes::repo_id_or_404;
use crate::state::AppState;
use crate::tree_derive::{ApplyError, MemNode};

/// One change of a create request.
#[derive(Deserialize, ToSchema)]
pub struct ChangesetChange {
    /// Repo-relative grammar-valid NFC path.
    pub path: String,
    /// `add` | `mod` | `del`.
    pub op: ChangeOp,
    /// 64 lowercase hex blob id; required for add/mod, forbidden for del.
    pub blob: Option<String>,
    /// Content byte length; required for add/mod, forbidden for del.
    pub size: Option<u64>,
    /// Exec bit; required for add/mod, forbidden for del.
    pub exec: Option<bool>,
}

/// The operation of one change.
#[derive(Deserialize, ToSchema, Clone, Copy)]
#[serde(rename_all = "lowercase")]
pub enum ChangeOp {
    Add,
    Mod,
    Del,
}

/// Request body of changeset create.
#[derive(Deserialize, ToSchema)]
pub struct CreateChangesetRequest {
    /// Parent changeset ids; the public API takes at most one (imports
    /// create 2-parent changesets through the db layer, not this route).
    pub parents: Vec<String>,
    /// Client-supplied author string (`Name <email>`).
    pub author: String,
    /// Commit message.
    pub message: String,
    /// Author timestamp in ns; omitted → the server clock.
    pub timestamp_ns: Option<u64>,
    /// The file changes, sorted by path bytes — the server rejects
    /// unsorted input, it never re-sorts (the id is the client's
    /// responsibility).
    pub changes: Vec<ChangesetChange>,
}

/// Created changeset body (201 + `Location`).
#[derive(Serialize, ToSchema)]
pub struct ChangesetCreated {
    pub id: String,
    pub root_tree_id: String,
    pub generation: u64,
}

/// One change of a changeset fetch response.
#[derive(Serialize, ToSchema)]
pub struct ChangesetChangeOut {
    pub path: String,
    pub op: String,
    /// Hex blob id; null for deletes.
    pub blob: Option<String>,
    pub size: Option<u64>,
    pub exec: bool,
}

/// A changeset fetch response.
#[derive(Serialize, ToSchema)]
pub struct ChangesetResponse {
    pub id: String,
    pub parents: Vec<String>,
    pub author: String,
    pub message: String,
    pub timestamp_ns: u64,
    pub generation: u64,
    pub principal: String,
    pub changes: Vec<ChangesetChangeOut>,
}

/// The root-tree pointer of a changeset.
#[derive(Serialize, ToSchema)]
pub struct ChangesetRootResponse {
    pub root_tree_id: String,
}

/// Creates a changeset (writer and above, path-scope enforced). Runs the
/// full validation matrix, derives the root tree, and commits everything —
/// changeset, parents, file changes, blob membership, derived trees, audit
/// row — in one transaction. Any validation failure leaves zero rows.
#[utoipa::path(
    post,
    path = "/v1/repos/{r}/changesets",
    params(("r" = String, Path, description = "Repo name")),
    request_body = CreateChangesetRequest,
    responses(
        (status = 201, description = "Changeset created with its derived root tree", body = ChangesetCreated),
        (status = 400, description = "Malformed JSON or a parent/blob id that is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the writer role, or a path outside the token's scope (details.path names the first offender)"),
        (status = 404, description = "No such repo"),
        (status = 410, description = "The changeset id — or a referenced blob — is redacted in this repo (`object_redacted`); recreation is refused"),
        (status = 413, description = "Canonical encoding exceeds the 16 MiB cap"),
        (status = 422, description = "Validation failure: unknown_parent / path_not_normalized / validation / empty_changeset / duplicate_path / unsorted_changes / missing blobs / duality")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(repo): Path<String>,
    body: AppJson<CreateChangesetRequest>,
) -> Result<Response, ApiError> {
    let created = create_inner(&state, &auth, &repo, body.0).await?;
    Ok((
        StatusCode::CREATED,
        [(
            header::LOCATION,
            format!("/v1/repos/{repo}/changesets/{}", created.id),
        )],
        Json(created),
    )
        .into_response())
}

async fn create_inner(
    state: &AppState,
    auth: &AuthedPrincipal,
    repo: &str,
    body: CreateChangesetRequest,
) -> Result<ChangesetCreated, ApiError> {
    require_role(auth, Role::Writer)?;

    // Path scope: every change must sit inside the token's scope. Checked
    // before any validation work — authorization does not leak validation
    // detail.
    if let Some(offender) = body
        .changes
        .iter()
        .map(|c| &c.path)
        .find(|p| !crate::authz::scope_allows(auth.scope_path.as_deref(), p))
    {
        return Err(
            ApiError::new(ApiCode::Forbidden, "path is outside this token's scope")
                .with_details(serde_json::json!({ "path": offender })),
        );
    }

    // Public creates take at most one parent (checked before any parent
    // resolution; imports create 2-parent changesets through the db layer).
    if body.parents.len() > 1 {
        return Err(ApiError::new(
            ApiCode::Validation,
            "public changesets take at most one parent",
        ));
    }
    let mut parents = Vec::with_capacity(body.parents.len());
    for hex in &body.parents {
        parents.push(parse_id(hex)?);
    }

    // Repo + parents resolve before any content check (unknown_parent
    // outranks the content validations). Changesets are append-only, so the
    // metas fetched here stay valid through the write below.
    let repo_id = repo_id_or_404(state, repo).await?;
    let parent_metas: Vec<(i64, Option<Vec<u8>>)> = {
        let ids = parents.clone();
        let graphs: Vec<Option<db::CsGraph>> = state
            .read(move |conn| {
                ids.iter()
                    .map(|id| {
                        db::changeset_graph(conn, repo_id, id).map_err(ApiError::log_internal)
                    })
                    .collect()
            })
            .await?;
        let mut metas = Vec::with_capacity(graphs.len());
        let mut missing: Vec<String> = Vec::new();
        for (id, graph) in parents.iter().zip(graphs) {
            match graph {
                Some(g) => metas.push((g.generation, g.root_tree_id)),
                None => missing.push(id.to_hex()),
            }
        }
        if !missing.is_empty() {
            return Err(ApiError::new(
                ApiCode::UnknownParent,
                "parent is not a changeset of this repo",
            )
            .with_details(serde_json::json!({ "missing_parents": missing })));
        }
        metas
    };

    // Parse the change list into core shape; per-entry field rules land
    // here (422 validation) and blob hex rules as 400 malformed.
    let mut changes = Vec::with_capacity(body.changes.len());
    for c in body.changes {
        let (op, blob, size, exec) = match c.op {
            ChangeOp::Del => {
                if c.blob.is_some() || c.size.is_some() || c.exec.is_some() {
                    return Err(ApiError::new(
                        ApiCode::Validation,
                        format!("del of {:?} must not carry blob/size/exec", c.path),
                    ));
                }
                (Op::Del, None, None, None)
            }
            ChangeOp::Add | ChangeOp::Mod => {
                let (Some(blob), Some(size), Some(exec)) = (c.blob, c.size, c.exec) else {
                    return Err(ApiError::new(
                        ApiCode::Validation,
                        format!(
                            "{} of {:?} requires blob, size, and exec",
                            c.op.op_str(),
                            c.path
                        ),
                    ));
                };
                let blob = parse_id(&blob)?;
                (op_of(c.op), Some(blob), Some(size), Some(exec))
            }
        };
        changes.push(FileChange {
            path: c.path,
            op,
            blob,
            size,
            exec,
        });
    }

    let timestamp_ns = body.timestamp_ns.unwrap_or_else(|| state.now_ns());
    let cs = Changeset {
        repo_id: repo_id as u64,
        parents,
        author: body.author,
        message: body.message,
        timestamp_ns,
        changes,
    };

    // Exact duplicate paths first (core validation would otherwise report
    // them as case-fold duplicates); then core semantic validation: path
    // grammar (incl. NFC), case-fold duplicates, author/message char set,
    // empty map, entry/parent caps; then strict sorting — rejected, never
    // re-sorted.
    {
        let mut seen = std::collections::HashSet::with_capacity(cs.changes.len());
        for c in &cs.changes {
            if !seen.insert(c.path.as_str()) {
                return Err(ApiError::new(
                    ApiCode::DuplicatePath,
                    format!("path {:?} appears twice in the request", c.path),
                ));
            }
        }
    }
    changeset::validate(&cs).map_err(map_core)?;
    if !cs
        .changes
        .windows(2)
        .all(|w| w[0].path.as_bytes() < w[1].path.as_bytes())
    {
        return Err(ApiError::new(
            ApiCode::UnsortedChanges,
            "changes are not strictly sorted by path bytes; sort them client-side \
             and re-encode — the id covers the order you send",
        ));
    }

    // Canonical bytes + id. Only EncodeTooLarge can remain (sorting,
    // payloads, and parent count were checked above).
    let canonical = changeset::encode(&cs).map_err(|e| match e {
        CoreError::EncodeTooLarge => ApiError::new(
            ApiCode::TooLarge,
            "canonical encoding exceeds the 16 MiB cap",
        ),
        other => ApiError::log_internal(other),
    })?;
    let cs_id = object_id(Kind::Changeset, &canonical);

    let generation = parent_metas
        .iter()
        .map(|(parent_gen, _)| *parent_gen)
        .max()
        .map_or(0, |g| g + 1);
    let parent_root = parent_metas
        .first()
        .and_then(|(_, root)| root.clone())
        .map(|bytes| Id(bytes.try_into().expect("root tree id is 32 bytes")));

    // Add/Mod blobs, deduplicated, for presence + membership.
    let mut blobs: Vec<Id> = Vec::new();
    for c in &cs.changes {
        if let (Op::Add | Op::Mod, Some(blob)) = (c.op, c.blob)
            && !blobs.contains(&blob)
        {
            blobs.push(blob);
        }
    }

    let root_tree_id = persist(
        state,
        repo_id,
        repo,
        auth,
        &cs,
        &cs_id,
        &canonical,
        generation,
        parent_root,
        blobs,
        timestamp_ns,
    )
    .await?;
    Ok(ChangesetCreated {
        id: cs_id.to_hex(),
        root_tree_id: root_tree_id.to_hex(),
        generation: generation as u64,
    })
}

/// The write phase: blob presence, duality against the parent tree,
/// derivation, and the single atomic commit.
#[allow(clippy::too_many_arguments)]
async fn persist(
    state: &AppState,
    repo_id: i64,
    repo_name: &str,
    auth: &AuthedPrincipal,
    cs: &Changeset,
    cs_id: &Id,
    canonical: &[u8],
    generation: i64,
    parent_root: Option<Id>,
    blobs: Vec<Id>,
    timestamp_ns: u64,
) -> Result<Id, ApiError> {
    let principal = auth.principal.clone();
    let repo = repo_name.to_string();
    let cs = cs.clone();
    let cs_id = *cs_id;
    let canonical = canonical.to_vec();
    let cas = Arc::clone(&state.cas);
    let derive = Arc::clone(&state.derive);
    let clock = Arc::clone(&state.clock);
    let walk_deadline = state.derive_walk_deadline.as_nanos() as u64;

    state
        .write(move |store| {
            let conn = store.conn();

            // The tombstone boundary (inside the write slot, so a
            // concurrent mark cannot slip between probe and insert): a
            // changeset id that is tombstoned in this repo — active or
            // PURGED, the resurrection arm — is refused (same content +
            // parents hash to the same id), and so is a changeset whose
            // Add/Mod blobs are tombstoned here (the tree entry would
            // carry the redacted blob's name, size, and hash).
            if db::redaction_active(conn, repo_id, &cs_id).map_err(ApiError::log_internal)? {
                return Err(ApiError::new(
                    ApiCode::ObjectRedacted,
                    format!(
                        "changeset {} is redacted in this repo; recreating it is refused",
                        cs_id.to_hex()
                    ),
                ));
            }
            if !blobs.is_empty() {
                let redacted_blobs = db::redactions_active_among(conn, repo_id, &blobs)
                    .map_err(ApiError::log_internal)?;
                if !redacted_blobs.is_empty() {
                    let hexes: Vec<String> = redacted_blobs.iter().map(Id::to_hex).collect();
                    return Err(ApiError::new(
                        ApiCode::ObjectRedacted,
                        format!(
                            "blobs are redacted in this repo; referencing them is refused: {}",
                            hexes.join(", ")
                        ),
                    ));
                }
            }

            // Blob presence: a CAS file AND an objects row, listing every
            // missing id in the details.
            let cas_has = cas.has(&blobs).map_err(ApiError::log_internal)?;
            let mut missing: Vec<Id> =
                db::objects_missing(conn, &blobs).map_err(ApiError::log_internal)?;
            for (id, present) in blobs.iter().zip(&cas_has) {
                if !present && !missing.contains(id) {
                    missing.push(*id);
                }
            }
            if !missing.is_empty() {
                let hexes: Vec<String> = missing.iter().map(Id::to_hex).collect();
                return Err(ApiError::new(
                    ApiCode::Validation,
                    "referenced blobs are missing; upload them first",
                )
                .with_details(serde_json::json!({ "missing_blobs": hexes })));
            }

            // Duality: apply the delta to the parent's tree in memory.
            let mut node = MemNode::load(conn, parent_root).map_err(map_derive)?;
            node.apply(&cs.changes).map_err(|e| {
                // The depth cap is grammar (422 validation), not duality —
                // the route's own validation rejected it earlier; apply's
                // check is derivation's own enforcement of the same rule.
                if let ApplyError::TooDeep { path } = &e {
                    return ApiError::new(
                        ApiCode::Validation,
                        format!("{path}: exceeds the 256-component path cap"),
                    );
                }
                let (path, why): (&str, &str) = match &e {
                    ApplyError::FileInPath { path } => (
                        path,
                        "an intermediate component is a file in the parent tree",
                    ),
                    ApplyError::ModNotFile { path } => {
                        (path, "mod target is not a file in the parent tree")
                    }
                    ApplyError::DelNotFile { path } => {
                        (path, "del target is not a file in the parent tree")
                    }
                    ApplyError::TooDeep { .. } => {
                        unreachable!("handled above")
                    }
                };
                ApiError::new(ApiCode::Duality, format!("{path}: {why}"))
            })?;
            let (root_tree_id, _, trees) = node.collect().map_err(map_derive)?;

            db::changeset_insert(
                conn,
                db::ChangesetInsert {
                    repo_id,
                    repo_name: &repo,
                    id: &cs_id,
                    canonical: &canonical,
                    author: &cs.author,
                    message: &cs.message,
                    timestamp_ns: timestamp_ns as i64,
                    generation,
                    principal: &principal,
                    root_tree_id: &root_tree_id,
                    parents: &cs.parents,
                    changes: &cs.changes,
                    blobs: &blobs,
                    trees: &trees,
                    now_ns: timestamp_ns as i64,
                },
            )
            .map_err(ApiError::log_internal)?;
            // Derived data (M6a): enqueue (or eagerly derive) the new
            // changeset for every registered kind, inside the same
            // transaction — a rolled-back create leaves no queue rows.
            crate::derive::enqueue_changeset(
                conn,
                &derive,
                repo_id,
                cs_id,
                &cs.parents,
                crate::derive::EagerEnv {
                    now_ns: timestamp_ns as i64,
                    clock,
                    walk_deadline_ns: walk_deadline,
                },
            )
            .map_err(ApiError::log_internal)?;
            Ok(root_tree_id)
        })
        .await
}

fn op_of(op: ChangeOp) -> Op {
    match op {
        ChangeOp::Add => Op::Add,
        ChangeOp::Mod => Op::Mod,
        ChangeOp::Del => Op::Del,
    }
}

impl ChangeOp {
    fn op_str(&self) -> &'static str {
        match self {
            ChangeOp::Add => "add",
            ChangeOp::Mod => "mod",
            ChangeOp::Del => "del",
        }
    }
}

fn parse_id(hex: &str) -> Result<Id, ApiError> {
    Id::from_hex(hex)
        .ok_or_else(|| ApiError::new(ApiCode::Malformed, "id must be 64 lowercase hex characters"))
}

/// Core validation errors map onto the registry: NFC gets its own code,
/// other path errors and char-set failures carry the core message.
fn map_core(e: CoreError) -> ApiError {
    match e {
        CoreError::Path(PathError::NotNormalized) => ApiError::new(
            ApiCode::PathNotNormalized,
            "path is not in NFC normalization form",
        ),
        CoreError::Path(pe) => ApiError::new(ApiCode::Validation, pe.to_string()),
        CoreError::EncodeEmptyChangeset => {
            ApiError::new(ApiCode::EmptyChangeset, "changeset has no entries")
        }
        CoreError::AuthorChar { .. }
        | CoreError::MessageChar { .. }
        | CoreError::EncodeTooManyEntries
        | CoreError::EncodeTooManyParents => ApiError::new(ApiCode::Validation, e.to_string()),
        // Unreachable given the route's prior checks; kept total.
        other => ApiError::log_internal(other),
    }
}

fn map_derive(e: crate::tree_derive::DeriveError) -> ApiError {
    match e {
        crate::tree_derive::DeriveError::Core(CoreError::EncodeTooLarge) => ApiError::new(
            ApiCode::TooLarge,
            "derived tree exceeds the 16 MiB canonical cap",
        ),
        other => ApiError::log_internal(other),
    }
}

/// Fetches one changeset with its parents and file changes (any
/// authenticated principal; ids and path metadata are repo-visible). An
/// actively-tombstoned changeset answers 410 `object_redacted` (M10b —
/// attribution never serves after the mark; the row itself persists until
/// the redaction purge).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/changesets/{id}",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Changeset id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The changeset", body = ChangesetResponse),
        (status = 400, description = "Id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo, or no such changeset in this repo"),
        (status = 410, description = "The changeset is redacted in this repo (`object_redacted`)")
    )
)]
pub async fn get_changeset(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Json<ChangesetResponse>, ApiError> {
    let id = parse_id(&id_hex)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let (gate_hex, gate_repo) = (id_hex.clone(), repo.clone());
    let fetched = state
        .read(move |conn| {
            // The redaction gate first: the tombstone wins over the row
            // lookup (410 even after a concurrent purge removed the row).
            if db::redaction_active(conn, repo_id, &id).map_err(ApiError::log_internal)? {
                return Err(ApiError::new(
                    ApiCode::ObjectRedacted,
                    format!("changeset {gate_hex} is redacted in {gate_repo}"),
                ));
            }
            let Some(row) =
                db::changeset_get(conn, repo_id, &id).map_err(ApiError::log_internal)?
            else {
                return Ok(None);
            };
            let parents = db::changeset_graph(conn, repo_id, &id)
                .map_err(ApiError::log_internal)?
                .map(|g| g.parents)
                .unwrap_or_default();
            let changes =
                db::file_changes_list(conn, repo_id, &id).map_err(ApiError::log_internal)?;
            Ok(Some((row, parents, changes)))
        })
        .await?;
    let Some((row, parents, changes)) = fetched else {
        return Err(ApiError::new(
            ApiCode::NotFound,
            format!("no changeset {id_hex} in {repo:?}"),
        ));
    };
    Ok(Json(ChangesetResponse {
        id: id.to_hex(),
        parents: parents.iter().map(Id::to_hex).collect(),
        author: row.author,
        message: row.message,
        timestamp_ns: row.timestamp_ns as u64,
        generation: row.generation as u64,
        principal: row.principal,
        changes: changes
            .into_iter()
            .map(|c| ChangesetChangeOut {
                path: c.path,
                op: match c.op {
                    0 => "add".into(),
                    1 => "mod".into(),
                    _ => "del".into(),
                },
                blob: c
                    .blob_id
                    .map(|b| Id(b.try_into().expect("blob id is 32 bytes")).to_hex()),
                size: c.size.map(|s| s as u64),
                exec: c.exec,
            })
            .collect(),
    }))
}

/// Fetches a changeset's derived root-tree id. Tombstoned changesets
/// answer 410 `object_redacted` (the same gate as the changeset fetch — a
/// redacted node's subtree pointers do not serve either).
#[utoipa::path(
    get,
    path = "/v1/repos/{r}/changesets/{id}/root",
    params(
        ("r" = String, Path, description = "Repo name"),
        ("id" = String, Path, description = "Changeset id, 64 lowercase hex")
    ),
    responses(
        (status = 200, description = "The root tree id", body = ChangesetRootResponse),
        (status = 400, description = "Id is not 64 lowercase hex"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 404, description = "No such repo, or no such changeset in this repo"),
        (status = 410, description = "The changeset is redacted in this repo (`object_redacted`)")
    )
)]
pub async fn get_root(
    state: State<AppState>,
    Extension(_auth): Extension<AuthedPrincipal>,
    Path((repo, id_hex)): Path<(String, String)>,
) -> Result<Json<ChangesetRootResponse>, ApiError> {
    let id = parse_id(&id_hex)?;
    let repo_id = repo_id_or_404(&state, &repo).await?;
    let (gate_hex, gate_repo) = (id_hex.clone(), repo.clone());
    let root = state
        .read(move |conn| {
            if db::redaction_active(conn, repo_id, &id).map_err(ApiError::log_internal)? {
                return Err(ApiError::new(
                    ApiCode::ObjectRedacted,
                    format!("changeset {gate_hex} is redacted in {gate_repo}"),
                ));
            }
            db::changeset_get(conn, repo_id, &id)
                .map_err(ApiError::log_internal)
                .map(|row| row.and_then(|r| r.root_tree_id))
        })
        .await?;
    root.map(|bytes| Id(bytes.try_into().expect("root tree id is 32 bytes")).to_hex())
        .map(|hex| Json(ChangesetRootResponse { root_tree_id: hex }))
        .ok_or_else(|| {
            ApiError::new(
                ApiCode::NotFound,
                format!("no changeset {id_hex} in {repo:?}"),
            )
        })
}
