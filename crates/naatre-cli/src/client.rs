//! Thin HTTP layer for every client verb (the M3 pin): bearer auth, the
//! server's error envelope mapped to an [`HttpError`] carrying its stable
//! code, and a once-per-process version-skew warning.
//!
//! Exit codes (pinned): 0 success, 1 generic error, 2 conflict-class (the
//! 409 envelope codes: `ref_conflict`, `dirty_working_copy`, …), 3 usage.
//! [`report`] prints `error: <code>: <message>` and returns the mapped code.
//!
//! The agent is built with `http_status_as_error(false)` so non-2xx
//! responses keep their JSON envelope (ureq's default drops the body of
//! error statuses).

use std::cell::Cell;
use std::env;
use std::time::Duration;

use serde_json::{Value, json};

pub(crate) const DEFAULT_URL: &str = "http://127.0.0.1:7400";

/// A failed server exchange: either the standard error envelope (with its
/// stable code) or a transport failure (no response at all).
#[derive(Debug)]
pub enum HttpError {
    /// The server answered with the error envelope.
    Envelope {
        status: u16,
        code: String,
        message: String,
        /// The envelope's `details` object, when it carried one (e.g. a
        /// land 409 carries the conflict list + resolution recipe).
        details: Option<Value>,
    },
    /// The request never completed (connection refused, timeout, …).
    Transport(String),
}

impl std::fmt::Display for HttpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HttpError::Envelope { code, message, .. } => write!(f, "{code}: {message}"),
            HttpError::Transport(message) => f.write_str(message),
        }
    }
}

impl HttpError {
    /// Pinned exit code: the conflict class (409 envelopes) is 2, all other
    /// failures are 1.
    pub fn exit_code(&self) -> i32 {
        match self {
            HttpError::Envelope { status: 409, .. } => 2,
            _ => 1,
        }
    }

    fn transport(e: ureq::Error, url: &str) -> HttpError {
        HttpError::Transport(format!("request to {url} failed: {e}"))
    }
}

/// Prints `error: <code>: <message>` (envelope) or the transport failure,
/// and returns the pinned exit code.
pub fn report(e: &HttpError) -> i32 {
    eprintln!("error: {e}");
    e.exit_code()
}

/// The blocking HTTP client shared by all server-talking verbs.
pub struct Client {
    agent: ureq::Agent,
    /// Server origin, no trailing slash (e.g. `http://127.0.0.1:7400`).
    pub base: String,
    token: Option<String>,
    version_checked: Cell<bool>,
}

impl Client {
    /// Builds a client over `base` (origin URL) with an optional bearer.
    pub fn new(base: &str, token: Option<String>) -> Client {
        let agent = ureq::Agent::new_with_config(
            ureq::config::Config::builder()
                .http_status_as_error(false)
                .timeout_connect(Some(Duration::from_secs(5)))
                .timeout_global(Some(Duration::from_secs(120)))
                .build(),
        );
        Client {
            agent,
            base: base.trim_end_matches('/').to_string(),
            token,
            version_checked: Cell::new(false),
        }
    }

    /// The admin-verb client: `NAATRE_URL` (default `http://127.0.0.1:7400`)
    /// and `NAATRE_TOKEN`.
    pub fn from_env() -> Client {
        Client::new(
            &env::var("NAATRE_URL").unwrap_or_else(|_| DEFAULT_URL.to_string()),
            env::var("NAATRE_TOKEN").ok(),
        )
    }

    fn with_bearer<B>(&self, rb: ureq::RequestBuilder<B>) -> ureq::RequestBuilder<B> {
        match &self.token {
            Some(t) => rb.header("Authorization", format!("Bearer {t}")),
            None => rb,
        }
    }

    /// GET /version once per process; warns on a major-version skew
    /// ("upgrade the server first"). Transport or shape problems are ignored
    /// here — the real call surfaces them.
    fn check_version(&self) {
        if self.version_checked.get() {
            return;
        }
        self.version_checked.set(true);
        let url = format!("{}/version", self.base);
        let Ok(res) = self.agent.get(&url).call() else {
            return;
        };
        let Ok(v) = res.into_body().read_json::<Value>() else {
            return;
        };
        let Some(server) = v["version"].as_str() else {
            return;
        };
        let client_version = env!("CARGO_PKG_VERSION");
        if let (Some(s), Some(c)) = (major_of(server), major_of(client_version))
            && s != c
        {
            eprintln!(
                "warning: server version {server}, client version {client_version} — upgrade the server first"
            );
        }
    }

    /// GET with query params; succeeds on any 2xx with the parsed JSON.
    pub fn get_json(&self, path: &str, query: &[(&str, &str)]) -> Result<Value, HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let mut rb = self.with_bearer(self.agent.get(&url));
        for (k, v) in query {
            rb = rb.query(*k, *v);
        }
        let res = rb.call().map_err(|e| HttpError::transport(e, &url))?;
        self.finish_json(res).map(|(_, v)| v)
    }

    /// POST a JSON body; succeeds on any 2xx, returning (status, body).
    pub fn post_json(&self, path: &str, body: &Value) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let res = self
            .with_bearer(self.agent.post(&url))
            .send_json(body)
            .map_err(|e| HttpError::transport(e, &url))?;
        self.finish_json(res)
    }

    /// Raw PUT of bytes (the large-blob path); succeeds on any 2xx.
    pub fn put_bytes(&self, path: &str, bytes: &[u8]) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let res = self
            .with_bearer(self.agent.put(&url))
            .send(bytes)
            .map_err(|e| HttpError::transport(e, &url))?;
        self.finish_json(res)
    }

    /// Raw GET of bytes (blob fetch). A non-2xx answer is parsed as the
    /// error envelope when possible.
    pub fn get_bytes(&self, path: &str) -> Result<Vec<u8>, HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let res = self
            .with_bearer(self.agent.get(&url))
            .call()
            .map_err(|e| HttpError::transport(e, &url))?;
        let status = res.status().as_u16();
        let mut body = res.into_body();
        let bytes = body
            .read_to_vec()
            .map_err(|e| HttpError::Transport(format!("reading response body failed: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(bytes_as_envelope(&bytes, status));
        }
        Ok(bytes)
    }

    fn finish_json(
        &self,
        res: ureq::http::Response<ureq::Body>,
    ) -> Result<(u16, Value), HttpError> {
        let status = res.status().as_u16();
        let mut body = res.into_body();
        let value: Value = body
            .read_json()
            .map_err(|e| HttpError::Transport(format!("server sent a non-JSON response: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(envelope_from(&value, status));
        }
        Ok((status, value))
    }
}

/// Extracts the envelope `error.code` / `error.message` (and `details`,
/// when present), with a fallback for responses that are not the standard
/// envelope.
fn envelope_from(value: &Value, status: u16) -> HttpError {
    let code = value["error"]["code"].as_str().unwrap_or("internal");
    let message = value["error"]["message"].as_str().unwrap_or_else(|| {
        if value.is_null() {
            "server returned no body"
        } else {
            "server returned an unexpected body"
        }
    });
    let details = value["error"]["details"]
        .as_object()
        .map(|_| value["error"]["details"].clone());
    HttpError::Envelope {
        status,
        code: code.to_string(),
        message: message.to_string(),
        details,
    }
}

fn bytes_as_envelope(bytes: &[u8], status: u16) -> HttpError {
    match serde_json::from_slice::<Value>(bytes) {
        Ok(v) if v["error"]["code"].is_string() => envelope_from(&v, status),
        _ => envelope_from(&Value::Null, status),
    }
}

/// The integer before the first `.`, if it parses.
fn major_of(version: &str) -> Option<u64> {
    version.split('.').next()?.parse().ok()
}

/// Percent-encodes a single URL path segment (repo names, ref names).
pub fn uri_segment(segment: &str) -> String {
    let mut out = String::new();
    for b in segment.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'.' | b'_' | b'~' => {
                out.push(b as char);
            }
            other => out.push_str(&format!("%{other:02X}")),
        }
    }
    out
}

/// `naatre repo create <name>` (admin).
pub fn repo_create(name: &str) -> i32 {
    let client = Client::from_env();
    let body = json!({ "name": name });
    match client.post_json("/v1/repos", &body) {
        Ok((_, value)) => {
            println!("created repo {} (id {})", value["name"], value["id"]);
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre repo stats <name>` (admin).
pub fn repo_stats(name: &str) -> i32 {
    let client = Client::from_env();
    match client.get_json(&format!("/v1/repos/{name}/stats"), &[]) {
        Ok(value) => {
            println!("changesets: {}", value["changesets"]);
            println!("objects: {}", value["objects"]);
            println!("bytes: {}", value["bytes"]);
            println!("cbytes: {}", value["cbytes"]);
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre token list` (HTTP, admin).
pub fn token_list() -> i32 {
    let client = Client::from_env();
    match client.get_json("/v1/tokens", &[]) {
        Ok(value) => {
            println!("{:<6} {:<16} {:<8} expires", "id", "principal", "role");
            for t in value["tokens"].as_array().map_or(&[][..], Vec::as_slice) {
                let expires = t["expires_ns"]
                    .as_u64()
                    .map_or("never".to_string(), |ns| ns.to_string());
                println!(
                    "{:<6} {:<16} {:<8} {}",
                    t["id"],
                    t["principal"].as_str().unwrap_or("?"),
                    t["role"].as_str().unwrap_or("?"),
                    expires,
                );
            }
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre token create <principal>` (admin). Prints the token once.
pub fn token_create(
    principal: &str,
    role: &str,
    ttl_days: Option<u64>,
    scope: Option<&str>,
) -> i32 {
    let client = Client::from_env();
    let mut body = json!({ "principal": principal, "role": role });
    if let Some(days) = ttl_days {
        body["ttl_days"] = json!(days);
    }
    if let Some(scope) = scope {
        body["scope_path"] = json!(scope);
    }
    match client.post_json("/v1/tokens", &body) {
        Ok((_, value)) => {
            // Printed exactly once; the server stores only the hash.
            println!("token: {}", value["token"].as_str().unwrap_or("?"));
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre token revoke <id>` (admin; idempotent).
pub fn token_revoke(id: u64) -> i32 {
    let client = Client::from_env();
    let path = format!("/v1/tokens/{id}");
    let run = || -> Result<(), HttpError> {
        let url = format!("{}{}", client.base, path);
        let res = client
            .with_bearer(client.agent.delete(&url))
            .call()
            .map_err(|e| HttpError::transport(e, &url))?;
        client.finish_json(res).map(|_| ())
    };
    match run() {
        Ok(()) => {
            println!("revoked {id}");
            0
        }
        Err(e) => report(&e),
    }
}
