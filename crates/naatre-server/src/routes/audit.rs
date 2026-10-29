//! `/v1/audit`: the admin read surface over the append-only audit log.

use axum::Extension;
use axum::Json;
use axum::extract::{Query, State};
use serde::Serialize;
use utoipa::ToSchema;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, require_role};
use crate::db;
use crate::routes::{ListQuery, parse_limit};
use crate::state::AppState;

/// One audit row.
#[derive(Serialize, ToSchema)]
pub struct AuditEntry {
    pub id: i64,
    pub ts_ns: u64,
    pub principal: String,
    pub action: String,
    /// JSON string as written (actor/action/repo-shaped data only — never
    /// IP, user-agent, or token material).
    pub details_json: String,
}

/// A page of the audit log.
///
/// Pagination: request `?limit=<1..=200>` (default 50) and optionally
/// `?cursor=before:<id>` — the `next_cursor` value of the previous page.
/// Rows come newest-first (descending; id order matches ts order because
/// ids are assigned at insert). Follow `next_cursor` until `has_more` is
/// false.
#[derive(Serialize, ToSchema)]
pub struct AuditListResponse {
    pub audit: Vec<AuditEntry>,
    /// `before:<id>` of this page's oldest row, or null when exhausted.
    pub next_cursor: Option<String>,
    pub has_more: bool,
}

/// Lists audit rows, newest first (admin only).
#[utoipa::path(
    get,
    path = "/v1/audit",
    params(ListQuery),
    responses(
        (status = 200, description = "A page of audit rows", body = AuditListResponse),
        (status = 400, description = "Malformed cursor or limit"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 422, description = "limit out of the 1..=200 range")
    )
)]
pub async fn list(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Query(q): Query<ListQuery>,
) -> Result<Json<AuditListResponse>, ApiError> {
    require_role(&auth, Role::Admin)?;
    let limit = parse_limit(&q)?;
    let before = match q.cursor.as_deref() {
        None => i64::MAX,
        Some(cursor) => {
            let id = cursor
                .strip_prefix("before:")
                .ok_or_else(bad_cursor)?
                .parse::<i64>()
                .map_err(|_| bad_cursor())?;
            if id < 0 {
                return Err(bad_cursor());
            }
            id
        }
    };
    let rows = state
        .read(move |store| {
            db::audit_list(store.conn(), before, limit + 1).map_err(ApiError::log_internal)
        })
        .await?;
    let has_more = rows.len() as i64 > limit;
    let mut rows = rows;
    rows.truncate(limit as usize);
    let next_cursor = has_more.then(|| format!("before:{}", rows.last().map_or(0, |r| r.id)));
    Ok(Json(AuditListResponse {
        audit: rows
            .into_iter()
            .map(|r| AuditEntry {
                id: r.id,
                ts_ns: r.ts_ns as u64,
                principal: r.principal,
                action: r.action,
                details_json: r.details_json,
            })
            .collect(),
        next_cursor,
        has_more,
    }))
}

fn bad_cursor() -> ApiError {
    ApiError::new(ApiCode::Malformed, "cursor is not a valid page cursor")
}
