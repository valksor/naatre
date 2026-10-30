//! The OpenAPI document. Grows with each slice's utoipa-annotated routes;
//! served at `/v1/openapi.json` (authenticated) and published as a checked
//! artifact in M5.

use utoipa::OpenApi;

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
        routes::history::diff,
        routes::history::merge_base,
        routes::history::file,
        routes::objects::present,
        routes::objects::put_raw,
        routes::objects::create_batch,
        routes::objects::get_batch,
        routes::objects::get_raw,
        routes::tokens::create,
        routes::tokens::list,
        routes::tokens::revoke,
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
        routes::history::DiffEntry,
        routes::history::DiffResponse,
        routes::history::MergeBaseResponse,
        routes::history::FileResponse,
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
