//! Bearer-token authentication: minting, the request middleware, and the
//! rate-limited auth-failure audit hook.
//!
//! Token model (plan "Auth"): `ntk_` + base64url-nopad of 24 CSPRNG bytes
//! (192 bits). Only the BLAKE3 hash of the full token string is stored
//! (`tokens.blake3`); the token itself is printed exactly once at mint time
//! and never logged. Lookup is by hash; failure codes are distinct
//! (`token_invalid` / `token_expired` / `token_revoked`).
//!
//! Logging rule: `Authorization` headers and token material never appear in
//! logs or audit rows. Auth-failure audit keys are short BLAKE3 prefixes of
//! the presented credential — stable per presented token, not reversible
//! into it, and never a peer address.

use axum::extract::{Request, State};
use axum::http::header;
use axum::middleware::Next;
use axum::response::{IntoResponse, Response};
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;

use crate::api_code::ApiCode;
use crate::api_error::ApiError;
use crate::db;
use crate::state::AppState;

/// Token wire prefix.
pub const TOKEN_PREFIX: &str = "ntk_";

/// Secret bytes behind the prefix: 24 → 192 bits of CSPRNG entropy.
const TOKEN_SECRET_BYTES: usize = 24;

/// Mints a new token: returns `(token string, blake3 hash of the string)`.
/// Panics only if the CSPRNG is unavailable — that is unrecoverable.
pub fn mint_token() -> (String, [u8; 32]) {
    let mut secret = [0u8; TOKEN_SECRET_BYTES];
    getrandom::fill(&mut secret).expect("CSPRNG available");
    let token = format!("{}{}", TOKEN_PREFIX, URL_SAFE_NO_PAD.encode(secret));
    let hash = hash_token(&token);
    (token, hash)
}

/// BLAKE3 hash of the full token string (prefix included) — the stored
/// lookup key.
pub fn hash_token(token: &str) -> [u8; 32] {
    *blake3::hash(token.as_bytes()).as_bytes()
}

/// A role carried by the principal row.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Role {
    Admin,
    Writer,
    Reader,
}

impl Role {
    /// Parses the `principals.role` wire/db string.
    pub fn parse(s: &str) -> Option<Role> {
        match s {
            "admin" => Some(Role::Admin),
            "writer" => Some(Role::Writer),
            "reader" => Some(Role::Reader),
            _ => None,
        }
    }

    /// The `principals.role` wire/db string.
    pub fn as_str(self) -> &'static str {
        match self {
            Role::Admin => "admin",
            Role::Writer => "writer",
            Role::Reader => "reader",
        }
    }

    /// True when `self` has at least `min`'s authority
    /// (admin > writer > reader).
    pub fn at_least(self, min: Role) -> bool {
        self.rank() >= min.rank()
    }

    fn rank(self) -> u8 {
        match self {
            Role::Admin => 2,
            Role::Writer => 1,
            Role::Reader => 0,
        }
    }
}

/// The authenticated principal, inserted into request extensions by
/// [`authenticate`].
#[derive(Clone, Debug)]
pub struct AuthedPrincipal {
    pub principal: String,
    pub role: Role,
    /// Segment-aligned path scope from the token row (`None` = whole repo).
    /// Not enforced on any M1b route; M2a/M4 enforce it on path routes.
    pub scope_path: Option<String>,
}

/// Gate for handlers: requires `auth.role` to be at least `min`.
pub(crate) fn require_role(auth: &AuthedPrincipal, min: Role) -> Result<(), ApiError> {
    if auth.role.at_least(min) {
        Ok(())
    } else {
        Err(ApiError::new(
            ApiCode::Forbidden,
            "authenticated principal lacks the required role",
        ))
    }
}

/// The stable audit/bucket key for a failed auth attempt: a short BLAKE3
/// prefix of the presented bearer token when one parses out of the header,
/// `"anon"` otherwise. Deliberately not the peer address and not raw token
/// material (audit rows are retained indefinitely).
fn failure_key(auth_header: Option<&str>) -> String {
    let presented = auth_header
        .and_then(|v| {
            let (scheme, token) = v.split_once(' ')?;
            scheme
                .eq_ignore_ascii_case("bearer")
                .then_some(token.trim())
        })
        .filter(|t| !t.is_empty());
    match presented {
        Some(token) => {
            let hash = hash_token(token);
            let hex: String = hash.iter().take(8).map(|b| format!("{b:02x}")).collect();
            format!("tk:{hex}")
        }
        None => "anon".to_string(),
    }
}

/// Records one failed auth attempt (rate-limited: within the per-key limit
/// AND the global per-window row cap an `audit_log` row is written, beyond
/// either the failure is only counted) and returns the 401 error. Best
/// effort — the row write is skipped if the single-writer slot is busy; the
/// failure remains counted.
async fn fail_auth(state: &AppState, key: &str, code: ApiCode, message: &'static str) -> ApiError {
    if state.tick_auth_failure(key) {
        let key = key.to_string();
        let now_ns = state.now_ns() as i64;
        let _ = state
            .write(move |store| {
                db::audit_append(
                    store.conn(),
                    now_ns,
                    "unknown",
                    "auth_failure",
                    &serde_json::json!({ "key": key }).to_string(),
                )
                .map_err(ApiError::log_internal)?;
                Ok(())
            })
            .await;
    }
    ApiError::new(code, message)
}

/// Authentication middleware for every `/v1` route: resolves the bearer
/// token, inserts [`AuthedPrincipal`], or answers 401 with the distinct
/// failure code and records the failure (rate-limited) in `audit_log`.
pub async fn authenticate(State(state): State<AppState>, mut req: Request, next: Next) -> Response {
    let header_value = req
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok());

    let token = header_value
        .and_then(|v| {
            let (scheme, token) = v.split_once(' ')?;
            scheme.eq_ignore_ascii_case("bearer").then(|| token.trim())
        })
        .filter(|t| t.starts_with(TOKEN_PREFIX));

    let Some(token) = token else {
        let key = failure_key(header_value);
        return fail_auth(
            &state,
            &key,
            ApiCode::TokenInvalid,
            "missing or malformed bearer token",
        )
        .await
        .into_response();
    };

    let lookup = {
        let hash = hash_token(token);
        state
            .read(move |conn| db::auth_lookup(conn, &hash).map_err(ApiError::log_internal))
            .await
    };
    let row = match lookup {
        Ok(row) => row,
        Err(e) => return e.into_response(),
    };

    let Some(row) = row else {
        let key = failure_key(header_value);
        return fail_auth(&state, &key, ApiCode::TokenInvalid, "unknown bearer token")
            .await
            .into_response();
    };
    if row.revoked {
        let key = failure_key(header_value);
        return fail_auth(
            &state,
            &key,
            ApiCode::TokenRevoked,
            "bearer token is revoked",
        )
        .await
        .into_response();
    }
    if let Some(expires_ns) = row.expires_ns
        && expires_ns <= state.now_ns() as i64
    {
        let key = failure_key(header_value);
        return fail_auth(
            &state,
            &key,
            ApiCode::TokenExpired,
            "bearer token is expired",
        )
        .await
        .into_response();
    }

    let role = match Role::parse(&row.role) {
        Some(role) => role,
        None => {
            return ApiError::log_internal(format!(
                "principal {} carries unknown role {:?}",
                row.principal, row.role
            ))
            .into_response();
        }
    };

    req.extensions_mut().insert(AuthedPrincipal {
        principal: row.principal,
        role,
        scope_path: row.scope_path,
    });
    next.run(req).await
}
