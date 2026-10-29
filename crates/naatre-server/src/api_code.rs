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
    /// 422 — commit content contains conflict markers.
    ConflictMarkersPresent,
    /// 400 — request payload is structurally malformed.
    Malformed,
    /// 422 — semantic validation failure (delta shape, char sets, caps).
    Validation,
    /// 422 — path is not NFC-normalized.
    PathNotNormalized,
    /// 422 — changeset create with an empty file map.
    EmptyChangeset,
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
    /// 500 — internal error; details never reach the client.
    Internal,
}

impl ApiCode {
    /// The wire form of the code (`error.code` in the envelope).
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::TokenInvalid => "token_invalid",
            Self::TokenExpired => "token_expired",
            Self::TokenRevoked => "token_revoked",
            Self::Forbidden => "forbidden",
            Self::NotFound => "not_found",
            Self::RepoNotFound => "repo_not_found",
            Self::ObjectNotFound => "object_not_found",
            Self::RefConflict => "ref_conflict",
            Self::IdempotencyKeySettled => "idempotency_key_settled",
            Self::DirtyWorkingCopy => "dirty_working_copy",
            Self::GitHistoryDiverged => "git_history_diverged",
            Self::ConflictMarkersPresent => "conflict_markers_present",
            Self::Malformed => "malformed",
            Self::Validation => "validation",
            Self::PathNotNormalized => "path_not_normalized",
            Self::EmptyChangeset => "empty_changeset",
            Self::TooLarge => "too_large",
            Self::SnapshotExpired => "snapshot_expired",
            Self::ObjectCollected => "object_collected",
            Self::ObjectRedacted => "object_redacted",
            Self::WriterBusy => "writer_busy",
            Self::DerivationPending => "derivation_pending",
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
            | Self::IdempotencyKeySettled
            | Self::DirtyWorkingCopy
            | Self::GitHistoryDiverged
            | Self::ConflictMarkersPresent => StatusCode::CONFLICT,
            Self::Malformed => StatusCode::BAD_REQUEST,
            Self::Validation | Self::PathNotNormalized | Self::EmptyChangeset => {
                StatusCode::UNPROCESSABLE_ENTITY
            }
            Self::TooLarge => StatusCode::PAYLOAD_TOO_LARGE,
            Self::SnapshotExpired | Self::ObjectCollected | Self::ObjectRedacted => {
                StatusCode::GONE
            }
            Self::WriterBusy | Self::DerivationPending => StatusCode::SERVICE_UNAVAILABLE,
            Self::Internal => StatusCode::INTERNAL_SERVER_ERROR,
        }
    }
}
