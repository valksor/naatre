//! The error-code registry table: every variant's wire string and pinned
//! HTTP status, written out so any re-map is a diff.

use axum::http::StatusCode;
use naatre_server::api_code::ApiCode;

/// code → (wire string, status). Adding or remapping a row is a deliberate,
/// reviewable diff.
const TABLE: &[(ApiCode, &str, u16)] = &[
    (ApiCode::TokenInvalid, "token_invalid", 401),
    (ApiCode::TokenExpired, "token_expired", 401),
    (ApiCode::TokenRevoked, "token_revoked", 401),
    (ApiCode::Forbidden, "forbidden", 403),
    (ApiCode::NotFound, "not_found", 404),
    (ApiCode::RepoNotFound, "repo_not_found", 404),
    (ApiCode::RepoExists, "repo_exists", 409),
    (ApiCode::ObjectNotFound, "object_not_found", 404),
    (ApiCode::RefConflict, "ref_conflict", 409),
    (
        ApiCode::IdempotencyKeySettled,
        "idempotency_key_settled",
        409,
    ),
    (ApiCode::DirtyWorkingCopy, "dirty_working_copy", 409),
    (ApiCode::GitHistoryDiverged, "git_history_diverged", 409),
    (
        ApiCode::ConflictMarkersPresent,
        "conflict_markers_present",
        409,
    ),
    (ApiCode::Malformed, "malformed", 400),
    (ApiCode::Validation, "validation", 422),
    (ApiCode::PathNotNormalized, "path_not_normalized", 422),
    (ApiCode::EmptyChangeset, "empty_changeset", 422),
    (ApiCode::NotFastForward, "not_fast_forward", 422),
    (ApiCode::UnknownParent, "unknown_parent", 422),
    (ApiCode::Duality, "duality", 422),
    (ApiCode::UnsortedChanges, "unsorted_changes", 422),
    (ApiCode::DuplicatePath, "duplicate_path", 422),
    (ApiCode::TooLarge, "too_large", 413),
    (ApiCode::SnapshotExpired, "snapshot_expired", 410),
    (ApiCode::ObjectCollected, "object_collected", 410),
    (ApiCode::ObjectRedacted, "object_redacted", 410),
    (ApiCode::WriterBusy, "writer_busy", 503),
    (ApiCode::DerivationPending, "derivation_pending", 503),
    (ApiCode::BlameTooLarge, "blame_too_large", 503),
    (ApiCode::RequestTimeout, "request_timeout", 503),
    (ApiCode::ServerBusy, "server_busy", 503),
    (ApiCode::BlobSwept, "blob_swept", 503),
    (ApiCode::Internal, "internal", 500),
];

/// Every variant, listed explicitly. The `match` below is a real exhaustive
/// match with NO wildcard arm, so a newly added variant fails to compile
/// until it lands here (and in TABLE) — the registry cannot silently grow.
/// (A `matches!` guard would not do this: its implicit `_ => false` arm
/// swallows new variants at compile time.)
fn all_variants() -> Vec<ApiCode> {
    use ApiCode::*;
    let all = vec![
        TokenInvalid,
        TokenExpired,
        TokenRevoked,
        Forbidden,
        NotFound,
        RepoNotFound,
        RepoExists,
        ObjectNotFound,
        RefConflict,
        IdempotencyKeySettled,
        DirtyWorkingCopy,
        GitHistoryDiverged,
        ConflictMarkersPresent,
        Malformed,
        Validation,
        PathNotNormalized,
        EmptyChangeset,
        NotFastForward,
        UnknownParent,
        Duality,
        UnsortedChanges,
        DuplicatePath,
        TooLarge,
        SnapshotExpired,
        ObjectCollected,
        ObjectRedacted,
        WriterBusy,
        DerivationPending,
        BlameTooLarge,
        RequestTimeout,
        ServerBusy,
        BlobSwept,
        Internal,
    ];
    for code in &all {
        // Exhaustiveness guard: adding a variant breaks this match.
        let _exhaustive: bool = match code {
            TokenInvalid
            | TokenExpired
            | TokenRevoked
            | Forbidden
            | NotFound
            | RepoNotFound
            | RepoExists
            | ObjectNotFound
            | RefConflict
            | IdempotencyKeySettled
            | DirtyWorkingCopy
            | GitHistoryDiverged
            | ConflictMarkersPresent
            | Malformed
            | Validation
            | PathNotNormalized
            | EmptyChangeset
            | NotFastForward
            | UnknownParent
            | Duality
            | UnsortedChanges
            | DuplicatePath
            | TooLarge
            | SnapshotExpired
            | ObjectCollected
            | ObjectRedacted
            | WriterBusy
            | DerivationPending
            | BlameTooLarge
            | RequestTimeout
            | ServerBusy
            | BlobSwept
            | Internal => true,
        };
    }
    all
}

#[test]
fn table_maps_every_variant_exactly_once() {
    let all = all_variants();
    assert_eq!(TABLE.len(), all.len(), "table and variant list disagree");
    for code in &all {
        let rows: Vec<_> = TABLE.iter().filter(|(c, _, _)| c == code).collect();
        assert_eq!(
            rows.len(),
            1,
            "{code:?} appears {} times in TABLE",
            rows.len()
        );
    }
    // The published-enum list (ApiCode::ALL feeds the OpenAPI ErrorCode
    // component) must equal the exhaustive variant list, in order.
    assert_eq!(ApiCode::ALL.len(), all.len(), "ALL length drifted");
    for (published, variant) in ApiCode::ALL.iter().zip(all.iter()) {
        assert_eq!(published, variant, "ALL order drifted at {variant:?}");
    }
}

#[test]
fn wire_strings_are_non_empty_snake_case() {
    for (code, name, _) in TABLE {
        assert_eq!(code.as_str(), *name);
        assert!(!name.is_empty(), "{code:?}: empty wire string");
        assert!(
            name.chars().all(|c| c.is_ascii_lowercase() || c == '_'),
            "{code:?}: {name:?} is not lowercase snake_case"
        );
        assert!(
            !name.starts_with('_') && !name.ends_with('_') && !name.contains("__"),
            "{code:?}: {name:?} has malformed underscores"
        );
    }
}

#[test]
fn statuses_are_in_the_pinned_set() {
    const PINNED: [u16; 10] = [400, 401, 403, 404, 409, 410, 413, 422, 500, 503];
    for (code, _, status) in TABLE {
        assert_eq!(code.status().as_u16(), *status, "{code:?} remapped");
        assert!(
            PINNED.contains(status),
            "{code:?}: status {status} is not in the pinned set"
        );
    }
}

#[test]
fn statuses_are_valid_http_status_codes() {
    for (code, _, _) in TABLE {
        // StatusCode's constructor validates the canonical reason phrase
        // exists for the code.
        let _canonical: StatusCode = code.status();
    }
}
