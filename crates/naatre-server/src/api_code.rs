//! The closed error-code registry.
//!
//! Every machine-readable error code the API may emit, with its pinned HTTP
//! status. The registry is closed: a new code is a deliberate addition here,
//! and the status mapping is diff-visible (see the table test in
//! `tests/api_code_test.rs`). The registry is published in the OpenAPI
//! document and the docs error-code table as milestones land.

use axum::http::StatusCode;

/// A stable, machine-readable API error code.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ApiCode {
    /// 401 — bearer token is malformed or unknown.
    TokenInvalid,
    /// 401 — bearer token is past its expiry.
    TokenExpired,
    /// 401 — bearer token has been revoked.
    TokenRevoked,
    /// 403 — authenticated but not allowed on this route or path scope.
    Forbidden,
    /// 404 — generic resource or route not found.
    NotFound,
    /// 404 — no repo with that name.
    RepoNotFound,
    /// 409 — a repo with that name already exists.
    RepoExists,
    /// 404 — no object with that id in this repo's membership.
    ObjectNotFound,
    /// 409 — ref move lost the compare-and-swap (stale version).
    RefConflict,
    /// 409 — idempotency key already settled with a different payload.
    IdempotencyKeySettled,
    /// 409 — client working copy is dirty (or has colliding untracked files).
    DirtyWorkingCopy,
    /// 409 — git import graft refused: histories diverged.
    GitHistoryDiverged,
    /// 409 — the land's 3-way base (or the head being landed) is a
    /// redacted changeset: a structured conflict, never a silent hole in
    /// the rebase.
    RedactedBase,
    /// 422 — commit content contains conflict markers.
    ConflictMarkersPresent,
    /// 409 — the server refuses the root of a conflicted node whose
    /// conflict format is higher than the client's `Naatre-Conflict-Format`
    /// header (absent = 0). Clients send their format on every request.
    VersionSkewConflicted,
    /// 400 — request payload is structurally malformed.
    Malformed,
    /// 422 — semantic validation failure (delta shape, char sets, caps).
    Validation,
    /// 422 — path is not NFC-normalized.
    PathNotNormalized,
    /// 422 — changeset create with an empty file map.
    EmptyChangeset,
    /// 422 — ref move is not a fast-forward (new target does not contain the old).
    NotFastForward,
    /// 422 — changeset create references a parent that is not a changeset of
    /// this repo (details list the missing ids).
    UnknownParent,
    /// 422 — path collides with a file/dir in the parent tree (file/dir
    /// duality): add under a file, mod of a directory, del of an absent path.
    Duality,
    /// 422 — changeset paths are not strictly sorted; rejected, never
    /// re-sorted server-side (the id is the client's responsibility).
    UnsortedChanges,
    /// 422 — the same path appears twice in one changeset request.
    DuplicatePath,
    /// 413 — body or blob exceeds the size cap.
    TooLarge,
    /// 410 — snapshot is past its TTL.
    SnapshotExpired,
    /// 410 — history object was collected by GC after the grace window.
    ObjectCollected,
    /// 410 — object was redacted (tombstoned).
    ObjectRedacted,
    /// 503 — single-writer slot is busy; safe to retry with backoff.
    WriterBusy,
    /// 503 — derived data is still being computed for this read.
    DerivationPending,
    /// 503 — the blame read exceeded a structural cap (walked chain nodes,
    /// streamed blob bytes); deterministic, so retrying cannot help.
    BlameTooLarge,
    /// 503 — the request outlived the server's per-request time budget and
    /// was shed; safe to retry with backoff (writes are idempotent by key).
    RequestTimeout,
    /// 503 — the server is at its concurrent-request limit; shed before any
    /// work started; safe to retry with backoff.
    ServerBusy,
    /// 503 — an upload's record step found the blob's bytes gone: the
    /// reachability GC's sweep removed them between the upload's CAS step
    /// and the record's turn in the write slot (an old, unreferenced re-PUT
    /// past its grace window). The row was never written; a retry re-PUTs
    /// the bytes and lands the row fresh.
    BlobSwept,
    /// 503 — the cache tier could not reach its origin for a step it
    /// refuses to guess about (a miss fetch, an epoch revalidation). The
    /// tier never serves un-revalidated bytes, so an origin outage is a
    /// 503 with `retryable: true`, never a stale answer.
    OriginUnreachable,
    /// 503 — ticket minting is not configured on this origin (no signing
    /// key in the data dir: run `naatre cache-key init`). Not retryable —
    /// the operator must configure the key, retrying cannot help.
    CacheDisabled,
    /// 500 — internal error; details never reach the client.
    Internal,
}

impl ApiCode {
    /// Every variant, in registry order. `tests/api_code_test.rs`
    /// cross-checks this list against its compile-time-exhaustive variant
    /// list (a `match` with no wildcard arm) and the wire-string table, so
    /// the published enum cannot silently drift from the registry.
    pub const ALL: [ApiCode; 37] = [
        ApiCode::TokenInvalid,
        ApiCode::TokenExpired,
        ApiCode::TokenRevoked,
        ApiCode::Forbidden,
        ApiCode::NotFound,
        ApiCode::RepoNotFound,
        ApiCode::RepoExists,
        ApiCode::ObjectNotFound,
        ApiCode::RefConflict,
        ApiCode::IdempotencyKeySettled,
        ApiCode::DirtyWorkingCopy,
        ApiCode::GitHistoryDiverged,
        ApiCode::RedactedBase,
        ApiCode::ConflictMarkersPresent,
        ApiCode::VersionSkewConflicted,
        ApiCode::Malformed,
        ApiCode::Validation,
        ApiCode::PathNotNormalized,
        ApiCode::EmptyChangeset,
        ApiCode::NotFastForward,
        ApiCode::UnknownParent,
        ApiCode::Duality,
        ApiCode::UnsortedChanges,
        ApiCode::DuplicatePath,
        ApiCode::TooLarge,
        ApiCode::SnapshotExpired,
        ApiCode::ObjectCollected,
        ApiCode::ObjectRedacted,
        ApiCode::WriterBusy,
        ApiCode::DerivationPending,
        ApiCode::BlameTooLarge,
        ApiCode::RequestTimeout,
        ApiCode::ServerBusy,
        ApiCode::BlobSwept,
        ApiCode::OriginUnreachable,
        ApiCode::CacheDisabled,
        ApiCode::Internal,
    ];

    /// The wire form of the code (`error.code` in the envelope).
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TokenInvalid => "token_invalid",
            Self::TokenExpired => "token_expired",
            Self::TokenRevoked => "token_revoked",
            Self::Forbidden => "forbidden",
            Self::NotFound => "not_found",
            Self::RepoNotFound => "repo_not_found",
            Self::RepoExists => "repo_exists",
            Self::ObjectNotFound => "object_not_found",
            Self::RefConflict => "ref_conflict",
            Self::IdempotencyKeySettled => "idempotency_key_settled",
            Self::DirtyWorkingCopy => "dirty_working_copy",
            Self::GitHistoryDiverged => "git_history_diverged",
            Self::RedactedBase => "redacted_base",
            Self::ConflictMarkersPresent => "conflict_markers_present",
            Self::VersionSkewConflicted => "version_skew_conflicted",
            Self::Malformed => "malformed",
            Self::Validation => "validation",
            Self::PathNotNormalized => "path_not_normalized",
            Self::EmptyChangeset => "empty_changeset",
            Self::NotFastForward => "not_fast_forward",
            Self::UnknownParent => "unknown_parent",
            Self::Duality => "duality",
            Self::UnsortedChanges => "unsorted_changes",
            Self::DuplicatePath => "duplicate_path",
            Self::TooLarge => "too_large",
            Self::SnapshotExpired => "snapshot_expired",
            Self::ObjectCollected => "object_collected",
            Self::ObjectRedacted => "object_redacted",
            Self::WriterBusy => "writer_busy",
            Self::DerivationPending => "derivation_pending",
            Self::BlameTooLarge => "blame_too_large",
            Self::RequestTimeout => "request_timeout",
            Self::ServerBusy => "server_busy",
            Self::BlobSwept => "blob_swept",
            Self::OriginUnreachable => "origin_unreachable",
            Self::CacheDisabled => "cache_disabled",
            Self::Internal => "internal",
        }
    }

    /// The HTTP status pinned to this code.
    pub fn status(self) -> StatusCode {
        match self {
            Self::TokenInvalid | Self::TokenExpired | Self::TokenRevoked => {
                StatusCode::UNAUTHORIZED
            }
            Self::Forbidden => StatusCode::FORBIDDEN,
            Self::NotFound | Self::RepoNotFound | Self::ObjectNotFound => StatusCode::NOT_FOUND,
            Self::RefConflict
            | Self::RepoExists
            | Self::IdempotencyKeySettled
            | Self::DirtyWorkingCopy
            | Self::GitHistoryDiverged
            | Self::RedactedBase
            | Self::ConflictMarkersPresent
            | Self::VersionSkewConflicted => StatusCode::CONFLICT,
            Self::Malformed => StatusCode::BAD_REQUEST,
            Self::Validation
            | Self::PathNotNormalized
            | Self::EmptyChangeset
            | Self::NotFastForward
            | Self::UnknownParent
            | Self::Duality
            | Self::UnsortedChanges
            | Self::DuplicatePath => StatusCode::UNPROCESSABLE_ENTITY,
            Self::TooLarge => StatusCode::PAYLOAD_TOO_LARGE,
            Self::SnapshotExpired | Self::ObjectCollected | Self::ObjectRedacted => {
                StatusCode::GONE
            }
            Self::WriterBusy
            | Self::DerivationPending
            | Self::BlameTooLarge
            | Self::RequestTimeout
            | Self::ServerBusy
            | Self::BlobSwept
            | Self::OriginUnreachable
            | Self::CacheDisabled => StatusCode::SERVICE_UNAVAILABLE,
            Self::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}
