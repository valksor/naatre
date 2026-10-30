//! Route handlers: the unversioned anon routes (`health`, `version`) and
//! the authenticated `/v1/…` API (`repos`, `refs`, `objects`, `tokens`,
//! `whoami`, `audit`, `openapi`). Every error is the standard envelope;
//! role gates map to 403 `forbidden`, auth to the 401 token codes.

pub mod audit;
pub mod changesets;
pub mod health;
pub mod history;
pub mod objects;
pub mod openapi;
pub mod refs;
pub mod repos;
pub mod tokens;
pub mod trees;
pub mod version;
pub mod whoami;

use serde::Deserialize;
use utoipa::IntoParams;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::db;
use crate::state::AppState;

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
        .read(move |store| {
            db::repo_id_by_name(store.conn(), &name_for_lookup).map_err(ApiError::log_internal)
        })
        .await?;
    found.ok_or_else(|| {
        ApiError::new(
            ApiCode::RepoNotFound,
            format!("no repo named {name:?} on this server"),
        )
    })
}
