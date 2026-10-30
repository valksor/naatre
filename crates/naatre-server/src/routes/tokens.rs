//! `/v1/tokens`: admin token management. The minted token string is
//! returned exactly once and is never stored (only its BLAKE3 hash is) or
//! logged.

use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::{Extension, Json};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::auth::{AuthedPrincipal, Role, mint_token, require_role};
use crate::db;
use crate::extract::AppJson;
use crate::state::AppState;

/// Request body of `POST /v1/tokens`.
#[derive(Deserialize, ToSchema)]
pub struct CreateTokenRequest {
    /// Principal name; created (or re-roled to `role`) if absent.
    pub principal: String,
    /// `admin`, `writer`, or `reader`.
    pub role: String,
    /// Token lifetime in days; null = never expires.
    pub ttl_days: Option<i64>,
    /// Segment-aligned path scope (`None`/null = whole repo). Stored
    /// without a trailing slash; a trailing slash is rejected (422).
    pub scope_path: Option<String>,
}

/// Created token body. `token` is printed exactly once — save it.
#[derive(Serialize, ToSchema)]
pub struct TokenCreated {
    pub token: String,
    pub id: i64,
    pub principal: String,
    pub role: String,
    /// Absolute expiry in ns since the epoch, or null (never).
    pub expires_ns: Option<u64>,
    pub scope_path: Option<String>,
}

/// A listed token (revoked tokens drop out of the list; the audit log
/// records revocations).
#[derive(Serialize, ToSchema)]
pub struct TokenSummary {
    pub id: i64,
    pub principal: String,
    pub role: String,
    pub expires_ns: Option<u64>,
    pub scope_path: Option<String>,
}

/// Token list body.
#[derive(Serialize, ToSchema)]
pub struct TokenListResponse {
    pub tokens: Vec<TokenSummary>,
}

/// Revoke result body.
#[derive(Serialize, ToSchema)]
pub struct RevokedResponse {
    pub revoked: bool,
}

/// Mints one token (admin, audited): upserts the principal with `role`,
/// stores the token hash, and returns the token once. 201.
#[utoipa::path(
    post,
    path = "/v1/tokens",
    request_body = CreateTokenRequest,
    responses(
        (status = 201, description = "Token minted (body carries the token, shown once)", body = TokenCreated),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 422, description = "Unknown role, empty principal, negative ttl_days, or invalid scope_path")
    )
)]
pub async fn create(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    body: AppJson<CreateTokenRequest>,
) -> Result<(StatusCode, Json<TokenCreated>), ApiError> {
    let AppJson(body) = body;
    require_role(&auth, Role::Admin)?;
    let role = Role::parse(&body.role).ok_or_else(|| {
        ApiError::new(ApiCode::Validation, "role must be admin, writer, or reader")
    })?;
    if body.principal.is_empty() {
        return Err(ApiError::new(
            ApiCode::Validation,
            "principal must not be empty",
        ));
    }
    let scope_path = match body.scope_path.as_deref() {
        None => None,
        Some("") => {
            return Err(ApiError::new(
                ApiCode::Validation,
                "scope_path must be a non-empty path or null",
            ));
        }
        Some(p) if p.ends_with('/') => {
            return Err(ApiError::new(
                ApiCode::Validation,
                "scope_path must not end with '/'",
            ));
        }
        Some(p) => Some(p.to_string()),
    };
    let expires_ns = match body.ttl_days {
        None => None,
        Some(days) if days < 0 => {
            return Err(ApiError::new(
                ApiCode::Validation,
                "ttl_days must not be negative",
            ));
        }
        Some(days) => {
            // now + days*86400e9 ns, saturated in i64 (ns since epoch fits
            // i64 until year 2262) — an absurd ttl_days lands at i64::MAX,
            // i.e. "effectively never", never a wrapped negative expiry.
            let ttl_ns = days.checked_mul(86_400_000_000_000).unwrap_or(i64::MAX);
            Some((state.now_ns() as i64).saturating_add(ttl_ns))
        }
    };

    let (token, hash) = mint_token();
    let now_ns = state.now_ns() as i64;
    let principal = body.principal.clone();
    let id = state
        .write(move |store| {
            db::token_create(
                store.conn(),
                &principal,
                role.as_str(),
                &hash,
                expires_ns,
                scope_path.as_deref(),
                now_ns,
            )
            .map_err(ApiError::log_internal)
        })
        .await?;
    Ok((
        StatusCode::CREATED,
        Json(TokenCreated {
            token,
            id,
            principal: body.principal,
            role: role.as_str().to_string(),
            expires_ns: expires_ns.map(|ns| ns as u64),
            scope_path: body.scope_path,
        }),
    ))
}

/// Lists non-revoked tokens (admin).
#[utoipa::path(
    get,
    path = "/v1/tokens",
    responses(
        (status = 200, description = "Active tokens", body = TokenListResponse),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role")
    )
)]
pub async fn list(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
) -> Result<Json<TokenListResponse>, ApiError> {
    require_role(&auth, Role::Admin)?;
    let rows = state
        .read(|conn| db::tokens_list(conn).map_err(ApiError::log_internal))
        .await?;
    Ok(Json(TokenListResponse {
        tokens: rows
            .into_iter()
            .filter(|t| !t.revoked)
            .map(|t| TokenSummary {
                id: t.id,
                principal: t.principal,
                role: t.role,
                expires_ns: t.expires_ns.map(|ns| ns as u64),
                scope_path: t.scope_path,
            })
            .collect(),
    }))
}

/// Revokes a token (admin, audited on the 0→1 transition). Idempotent:
/// an already-revoked id still answers 200.
#[utoipa::path(
    delete,
    path = "/v1/tokens/{id}",
    params(("id" = i64, Path, description = "Token id")),
    responses(
        (status = 200, description = "Revoked (or already revoked)", body = RevokedResponse),
        (status = 400, description = "Token id is not an integer"),
        (status = 401, description = "Missing, unknown, revoked, or expired token"),
        (status = 403, description = "Authenticated without the admin role"),
        (status = 404, description = "No token with that id")
    )
)]
pub async fn revoke(
    state: State<AppState>,
    Extension(auth): Extension<AuthedPrincipal>,
    Path(id): Path<String>,
) -> Result<Json<RevokedResponse>, ApiError> {
    require_role(&auth, Role::Admin)?;
    let id: i64 = id
        .parse()
        .map_err(|_| ApiError::new(ApiCode::Malformed, "token id must be an integer"))?;
    let now_ns = state.now_ns() as i64;
    let outcome = state
        .write(move |store| {
            db::token_revoke(store.conn(), id, now_ns).map_err(ApiError::log_internal)
        })
        .await?;
    match outcome {
        Some(_) => Ok(Json(RevokedResponse { revoked: true })),
        None => Err(ApiError::new(
            ApiCode::NotFound,
            format!("no token with id {id}"),
        )),
    }
}
