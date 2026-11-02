//! Route handlers: the unversioned anon routes (`health`, `version`) and
//! the authenticated `/v1/…` API (`repos`, `refs`, `objects`, `tokens`,
//! `whoami`, `audit`, `openapi`). Every error is the standard envelope;
//! role gates map to 403 `forbidden`, auth to the 401 token codes.

pub mod access;
pub mod audit;
pub mod changesets;
pub mod health;
pub mod history;
pub mod land;
pub mod objects;
pub mod openapi;
pub mod principals;
pub mod redactions;
pub mod refs;
pub mod repos;
pub mod snapshots;
pub mod tickets;
pub mod tokens;
pub mod trees;
pub mod version;
pub mod whoami;

use serde::Deserialize;
use utoipa::IntoParams;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role};
use crate::db;
use crate::state::AppState;
use naatre_store::Db;

/// Shared `limit` + `cursor` query params for the cursor-paginated lists.
///
/// `limit`: page size, default 50, max 200 (larger → 422; non-integer →
/// 400). `cursor`: the opaque `next_cursor` value from the previous page —
/// `after:<id>` for ascending lists (`/v1/repos`), `before:<id>` for the
/// descending audit list.
#[derive(Deserialize, IntoParams)]
pub struct ListQuery {
    /// Page size (1–200; default 50).
    pub limit: Option<String>,
    /// Opaque page cursor from the previous response.
    pub cursor: Option<String>,
}

/// Parses the shared `limit` param: default 50, bounded to 1..=200.
pub(crate) fn parse_limit(q: &ListQuery) -> Result<i64, ApiError> {
    let Some(raw) = q.limit.as_deref() else {
        return Ok(50);
    };
    let n: i64 = raw
        .parse()
        .map_err(|_| ApiError::new(ApiCode::Malformed, "limit must be an integer"))?;
    if n < 1 {
        return Err(ApiError::new(
            ApiCode::Validation,
            "limit must be at least 1",
        ));
    }
    if n > 200 {
        return Err(ApiError::new(
            ApiCode::Validation,
            "limit must be at most 200",
        ));
    }
    Ok(n)
}

/// Resolves a repo name to its id, or 404 `repo_not_found`.
pub(crate) async fn repo_id_or_404(state: &AppState, name: &str) -> Result<i64, ApiError> {
    let name = name.to_string();
    let name_for_lookup = name.clone();
    let found = state
        .read(move |conn| {
            db::repo_id_by_name(conn, &name_for_lookup).map_err(ApiError::log_internal)
        })
        .await?;
    found.ok_or_else(|| {
        ApiError::new(
            ApiCode::RepoNotFound,
            format!("no repo named {name:?} on this server"),
        )
    })
}

/// The exact envelope a non-reader (or anyone at all) gets for a repo
/// they may not see — byte-identical to a missing repo's answer, the
/// uniform-404 contract ("a cross-repo id is indistinguishable from a
/// nonexistent one", carried up to the repo level by M13b).
fn invisible(name: &str) -> ApiError {
    ApiError::new(
        ApiCode::RepoNotFound,
        format!("no repo named {name:?} on this server"),
    )
}

/// The M13b repo READ gate — runs on EVERY repo-scoped route before any
/// repo statement: resolves the repo (missing → the plain 404), then, if
/// the repo is PRIVATE (at least one `repo_access` row — the catalog
/// owns them; one catalog probe), admits only admins and granted
/// readers. Everyone else gets the SAME 404 a missing repo answers —
/// existence is the secret the uniform code keeps. Public repos (no
/// access rows) admit every authenticated principal, the pre-M13
/// behavior; a cache-ticket principal (`Role::Ticket`) ranks below
/// reader and passes only where the route explicitly takes tickets.
pub(crate) async fn repo_read_gate(
    state: &AppState,
    auth: &AuthedPrincipal,
    repo: &str,
) -> Result<(), ApiError> {
    // A cache ticket is a credential for exactly three surfaces (the
    // mint + the two ticket-scoped cache-support routes, plus the by-id
    // object fetch's ticket arm) — never a general reader. This gate is
    // the bearer surface's; tickets answer 403 here (the
    // /v1/objects/{id} ticket arm branches before reaching it).
    if auth.role == Role::Ticket {
        return Err(ApiError::new(
            ApiCode::Forbidden,
            "cache tickets are not credentials on this route",
        ));
    }
    let repo_id = repo_id_or_404(state, repo).await?;
    if auth.role == Role::Admin {
        return Ok(());
    }
    let principal = auth.principal.clone();
    let visible = state
        .read(move |conn| {
            if !db::access_is_private(conn, repo_id).map_err(ApiError::log_internal)? {
                return Ok(true);
            }
            Ok(db::access_row(conn, repo_id, &principal)
                .map_err(ApiError::log_internal)?
                .is_some_and(|row| row.read))
        })
        .await?;
    if visible {
        Ok(())
    } else {
        Err(invisible(repo))
    }
}

/// The write-gate's admission outcome: on a PUBLIC repo the pre-M13 role
/// matrix decides (denied = 403, the repo is no secret); on a PRIVATE
/// repo the grant decides (denied = the uniform 404 — the write surface
/// is as invisible as the read surface).
enum WriteAdmission {
    PublicAllowed,
    PublicDenied,
    PrivateAllowed,
    PrivateDenied,
}

/// The WRITE gate's catalog half, callable INSIDE a write closure (the
/// land route's shape: the gate must run in the detached write task so an
/// aborted leader's first poll still reaches the write spawn — the M4c
/// contract — while an invisible repo still answers the uniform 404 from
/// the leader's published error). Same admission rules as
/// [`repo_write_gate`].
pub(crate) fn repo_write_gate_sync(
    cat: &Db,
    repo_id: i64,
    principal: &str,
    role: Role,
    repo_name: &str,
) -> Result<(), ApiError> {
    // The ticket floor (mirrors repo_read_gate): a cache ticket is a
    // READ-shaped credential for the tier's surfaces — never a write
    // credential, on public or private repos alike. (B1: the private
    // branch below only checks the grant's write flag, so without this
    // floor a ticket under a granted writer would pass.)
    if role == Role::Ticket {
        return Err(ApiError::new(
            ApiCode::Forbidden,
            "cache tickets are not credentials on this route",
        ));
    }
    if role == Role::Admin {
        return Ok(());
    }
    if !db::access_is_private(cat, repo_id).map_err(ApiError::log_internal)? {
        return if role.at_least(Role::Writer) {
            Ok(())
        } else {
            Err(ApiError::new(
                ApiCode::Forbidden,
                "authenticated principal lacks the required role",
            ))
        };
    }
    if db::access_row(cat, repo_id, principal)
        .map_err(ApiError::log_internal)?
        .is_some_and(|row| row.write)
    {
        Ok(())
    } else {
        Err(invisible(repo_name))
    }
}

/// The M13b repo WRITE gate — land/import-class writes and the other
/// repo-scoped writers. Public repos keep the pre-M13 role matrix
/// (writer and above); private repos admit admins and granted writers
/// (a grant's write flag implies read — the statement layer's invariant),
/// everyone else gets the uniform 404.
pub(crate) async fn repo_write_gate(
    state: &AppState,
    auth: &AuthedPrincipal,
    repo: &str,
) -> Result<(), ApiError> {
    // The ticket floor (mirrors repo_read_gate above): a cache ticket is
    // never a write credential — the private branch below only checks
    // the grant's write flag, so without this floor a ticket under a
    // granted writer would write on a private repo.
    if auth.role == Role::Ticket {
        return Err(ApiError::new(
            ApiCode::Forbidden,
            "cache tickets are not credentials on this route",
        ));
    }
    let repo_id = repo_id_or_404(state, repo).await?;
    if auth.role == Role::Admin {
        return Ok(());
    }
    let principal = auth.principal.clone();
    let role = auth.role;
    let admission = state
        .read(move |conn| {
            if !db::access_is_private(conn, repo_id).map_err(ApiError::log_internal)? {
                if role.at_least(Role::Writer) {
                    Ok(WriteAdmission::PublicAllowed)
                } else {
                    Ok(WriteAdmission::PublicDenied)
                }
            } else if db::access_row(conn, repo_id, &principal)
                .map_err(ApiError::log_internal)?
                .is_some_and(|row| row.write)
            {
                Ok(WriteAdmission::PrivateAllowed)
            } else {
                Ok(WriteAdmission::PrivateDenied)
            }
        })
        .await?;
    match admission {
        WriteAdmission::PublicAllowed | WriteAdmission::PrivateAllowed => Ok(()),
        WriteAdmission::PublicDenied => Err(ApiError::new(
            ApiCode::Forbidden,
            "authenticated principal lacks the required role",
        )),
        WriteAdmission::PrivateDenied => Err(invisible(repo)),
    }
}
