//! The OpenAPI document. Grows with each slice's utoipa-annotated routes;
//! served at `/v1/openapi.json` (authenticated) and published as a checked
//! artifact in M5.

use utoipa::OpenApi;

use crate::api_code::ApiCode;
use crate::routes;

/// The naatre API document.
#[derive(OpenApi)]
#[openapi(
    paths(
        routes::health::healthz,
        routes::version::version,
        routes::repos::list,
        routes::repos::create,
        routes::repos::stats,
        routes::refs::get_ref,
        routes::refs::cas_ref,
        routes::changesets::create,
        routes::changesets::get_changeset,
        routes::changesets::get_root,
        routes::trees::get_tree,
        routes::history::log,
        routes::history::blame,
        routes::history::diff,
        routes::history::merge_base,
        routes::history::file,
        routes::snapshots::create,
        routes::snapshots::get_metadata,
        routes::snapshots::get_blob,
        routes::snapshots::put_blob,
        routes::snapshots::get_snapshot_tree,
        routes::land::land,
        routes::land::land_status,
        routes::objects::present,
        routes::objects::put_raw,
        routes::objects::create_batch,
        routes::objects::get_batch,
        routes::objects::get_raw,
        routes::tokens::create,
        routes::tokens::list,
        routes::tokens::revoke,
        routes::principals::delete,
        routes::redactions::create,
        routes::redactions::list,
        routes::whoami::whoami,
        routes::audit::list,
        routes::openapi::serve,
    ),
    components(schemas(
        routes::health::HealthResponse,
        routes::version::VersionResponse,
        routes::repos::RepoSummary,
        routes::repos::RepoListResponse,
        routes::repos::CreateRepoRequest,
        routes::repos::RepoCreated,
        routes::repos::RepoStats,
        routes::refs::RefResponse,
        routes::refs::CreateRefRequest,
        routes::refs::ExpectedRef,
        routes::changesets::ChangeOp,
        routes::changesets::ChangesetChange,
        routes::changesets::CreateChangesetRequest,
        routes::changesets::ChangesetCreated,
        routes::changesets::ChangesetChangeOut,
        routes::changesets::ChangesetResponse,
        routes::changesets::ChangesetRootResponse,
        routes::trees::TreeEntryOut,
        routes::trees::TreeResponse,
        routes::history::LogEntry,
        routes::history::LogResponse,
        routes::history::PathLogResponse,
        routes::history::BlameLine,
        routes::history::BlameResponse,
        routes::history::DiffEntry,
        routes::history::DiffResponse,
        routes::history::MergeBaseResponse,
        routes::history::FileResponse,
        routes::snapshots::CreateSnapshotRequest,
        routes::snapshots::SnapshotCreated,
        routes::snapshots::SnapshotResponse,
        routes::snapshots::SnapshotBlobStored,
        routes::land::LandRequest,
        routes::land::LandResponse,
        routes::land::LandStatusResponse,
        routes::land::LandStatusResult,
        routes::objects::PresentRequest,
        routes::objects::PresentResponse,
        routes::objects::BatchPutEntry,
        routes::objects::BatchPutRequest,
        routes::objects::RejectedEntry,
        routes::objects::BatchPutResponse,
        routes::objects::StoredObject,
        routes::objects::BatchGetRequest,
        routes::objects::BatchGetResponse,
        routes::objects::StoredObjectId,
        routes::tokens::CreateTokenRequest,
        routes::tokens::TokenCreated,
        routes::tokens::TokenSummary,
        routes::tokens::TokenListResponse,
        routes::tokens::RevokedResponse,
        routes::principals::PrincipalDeleted,
        routes::redactions::CreateRedactionRequest,
        routes::redactions::RedactionEntry,
        routes::redactions::RedactionMarked,
        routes::redactions::RedactionListResponse,
        routes::whoami::WhoamiResponse,
        routes::audit::AuditEntry,
        routes::audit::AuditListResponse,
    )),
    info(
        title = "naatre",
        description = "naatre VCS server API.\n\n\
            Conventions: every error uses the envelope \
            `{error: {code, message, retryable, details, request_id}}` with codes from the \
            closed registry; creates return 201 + Location; a re-PUT of an existing blob is an \
            idempotent 200; a busy single writer answers 503 + Retry-After with `retryable: true`.\n\n\
            Pagination: `/v1/repos` pages ascend with `cursor=after:<id>`; `/v1/audit` pages \
            descend with `cursor=before:<id>`. `next_cursor` of a page feeds the next request \
            until `has_more` is false.",
        license(name = "GPL-3.0-only")
    )
)]
pub struct ApiDoc;

impl ApiDoc {
    /// The full contract as one JSON value: the utoipa-rendered document
    /// plus the error-registry components (`ErrorCode` enum and
    /// `ErrorEnvelope`), built at render time from the closed [`ApiCode`]
    /// registry so they cannot drift from the wire strings.
    ///
    /// `GET /v1/openapi.json` (`routes::openapi`) and the checked
    /// `docs/openapi.json` artifact (`tests/openapi_artifact_test.rs`, which
    /// CI regenerates and diffs) both render through this function — one
    /// source, two consumers.
    pub fn document_json() -> serde_json::Value {
        let mut doc = serde_json::to_value(Self::openapi()).expect("ApiDoc serializes to JSON");
        let doc_obj = doc
            .as_object_mut()
            .expect("the OpenAPI document serializes as an object");
        let components = doc_obj
            .entry("components")
            .or_insert_with(|| serde_json::json!({}));
        let schemas = components
            .as_object_mut()
            .expect("components serializes as an object")
            .entry("schemas")
            .or_insert_with(|| serde_json::json!({}));
        let map = schemas
            .as_object_mut()
            .expect("schemas serializes as an object");
        let codes: Vec<&str> = ApiCode::ALL.iter().map(|c| c.as_str()).collect();
        map.insert(
            "ErrorCode".to_string(),
            serde_json::json!({
                "type": "string",
                "description": "The closed error-code registry: every value `error.code` \
                               may take. Each code's pinned HTTP status and recovery `details` \
                               are documented in the README error-code table.",
                "enum": codes,
            }),
        );
        map.insert(
            "ErrorEnvelope".to_string(),
            serde_json::json!({
                "description": "The envelope every error response uses.",
                "type": "object",
                "properties": {
                    "error": {
                        "type": "object",
                        "properties": {
                            "code": {
                                "$ref": "#/components/schemas/ErrorCode"
                            },
                            "message": { "type": "string" },
                            "retryable": { "type": "boolean" },
                            "details": {
                                "type": "object",
                                "additionalProperties": true,
                                "description": "Structured recovery data; recoverable codes \
                                               carry it (e.g. `ref_conflict` carries the \
                                               current ref version, land conflicts carry the \
                                               resolution recipe)."
                            },
                            "request_id": { "type": "string" }
                        },
                        "required": [
                            "code", "message", "retryable", "details", "request_id"
                        ]
                    }
                },
                "required": ["error"]
            }),
        );
        doc
    }
}
