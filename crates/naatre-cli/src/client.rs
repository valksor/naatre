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

use std::env;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use serde_json::{Value, json};

use crate::verbs::Net;

pub(crate) const DEFAULT_URL: &str = "http://127.0.0.1:7400";

/// The highest conflict format this client materializes. Bump it when the
/// marker format gains a form.
pub const CONFLICT_FORMAT: u32 = 2;

/// The 503 envelope codes the CLI retries on idempotent verbs: the
/// single-writer slot, the concurrent-request shed, and the per-request
/// time budget — each `retryable: true` with a pinned `Retry-After` in the
/// closed registry. `blob_swept` joins them for the raw-PUT path: the GC's
/// sweep removed an old orphan's bytes mid-upload, the record step never
/// wrote the row, and the content-addressed re-PUT is idempotent — the
/// retry lands the bytes and the row fresh (the CLI's raw-PUT upload path
/// therefore re-uploads on the sweep race, the same recovery its batch
/// sibling gets from re-PUTting missing blobs). `derivation_pending` and
/// `blame_too_large` are 503s that must surface instead (a backlog is not
/// a 5 s event; a structural cap is deterministic).
const RETRY_CODES: [&str; 4] = [
    "writer_busy",
    "server_busy",
    "request_timeout",
    "blob_swept",
];
/// POST verbs that are NOT safe to replay after a request_timeout: the shed
/// drops the response but the blocking write may have committed (the server's
/// spawn_blocking task survives the future drop). Retrying these on
/// request_timeout can double-apply (a second snapshot) or surface a 409
/// repo_exists after a successful first attempt. Only the pre-execution
/// rejections (writer_busy/server_busy) are safe to retry for POSTs.
const POST_SAFE_RETRY_CODES: [&str; 2] = ["writer_busy", "server_busy"];
/// Attempts per retried request (the first plus two retries).
const RETRY_TRIES: u32 = 3;
/// Total added waiting across one request's retries.
const RETRY_BUDGET: Duration = Duration::from_secs(5);
/// Wait between attempts when a retryable 503 arrived without a parseable
/// `Retry-After` (the server always pins one; this is the fallback).
const RETRY_BACKOFF_DEFAULT: Duration = Duration::from_millis(100);

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
        /// The response's `Retry-After` header, in seconds, when present
        /// (the server pins `Retry-After: 1` on every retryable 503).
        retry_after: Option<u64>,
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

/// The platform root certificates (`rustls-native-certs`, the F1 root
/// store). Best-effort like the store's own platform load — a root set
/// that verifies nothing fails loudly at the first https handshake
/// (unknown issuer), never here.
fn platform_root_certs() -> Vec<ureq::tls::Certificate<'static>> {
    rustls_native_certs::load_native_certs()
        .certs
        .iter()
        .map(|c| ureq::tls::Certificate::from_der(c.as_ref()).to_owned())
        .collect()
}

/// Reads and parses `ca_file` as a PEM CA bundle: the file must exist, be
/// readable, parse as PEM, and carry at least ONE certificate. Only
/// certificates come back; private keys and other PEM sections are
/// skipped, not errors (a bundle may carry more than the roots). This is
/// the save-time validation `config set` and clone run for their stored
/// bundles, and the agent root builder shares it. The error names the
/// path.
pub(crate) fn read_ca_bundle(
    ca_file: &str,
) -> Result<Vec<ureq::tls::Certificate<'static>>, String> {
    let pem = std::fs::read(ca_file).map_err(|e| format!("ca_file {ca_file}: {e}"))?;
    let mut certs = Vec::new();
    for item in ureq::tls::parse_pem(&pem) {
        if let ureq::tls::PemItem::Certificate(cert) =
            item.map_err(|e| format!("ca_file {ca_file}: must be a PEM CA bundle: {e}"))?
        {
            certs.push(cert);
        }
    }
    if certs.is_empty() {
        return Err(format!(
            "ca_file {ca_file}: must be a PEM CA bundle (no certificates found)"
        ));
    }
    Ok(certs)
}

/// The root set for [`agent`]: platform roots PLUS the `ca_file` PEM
/// certificates — added on top, never replacing (the `sslrootcert` rule).
/// A missing file, an unparseable one, or one with no certificate in it is
/// a hard local error naming the path.
fn root_certs_with_ca(ca_file: &str) -> Result<Vec<ureq::tls::Certificate<'static>>, String> {
    let mut roots = platform_root_certs();
    roots.extend(read_ca_bundle(ca_file)?);
    Ok(roots)
}

/// The agent every CLI client rides (F1): rustls with the EXPLICIT ring
/// crypto provider (aws-lc-rs also sits in the graph, so a process default
/// would be ambiguous — the same rule the store's TLS follows) over the
/// root set above, and `max_redirects(0)` — a 3xx comes back as the
/// response, never followed, so no https→http downgrade and `Authorization`
/// can never cross hosts. There is deliberately no "skip verification"
/// option.
fn agent_with_roots(roots: Vec<ureq::tls::Certificate<'static>>) -> ureq::Agent {
    let tls = ureq::tls::TlsConfig::builder()
        .provider(ureq::tls::TlsProvider::Rustls)
        .unversioned_rustls_crypto_provider(Arc::new(rustls::crypto::ring::default_provider()))
        .root_certs(roots.into())
        .build();
    ureq::Agent::new_with_config(
        ureq::config::Config::builder()
            .http_status_as_error(false)
            .timeout_connect(Some(Duration::from_secs(5)))
            .timeout_global(Some(Duration::from_secs(120)))
            .max_redirects(0)
            .tls_config(tls)
            .build(),
    )
}

/// The no-`ca_file` agent — infallible ([`agent`] adds the bundle checks).
pub(crate) fn base_agent() -> ureq::Agent {
    agent_with_roots(platform_root_certs())
}

/// [`base_agent`] with an optional PEM CA bundle added to the platform
/// roots (the `--ca-file` / config `ca_file` private-CA story).
pub(crate) fn agent(ca_file: Option<&str>) -> Result<ureq::Agent, String> {
    match ca_file {
        None => Ok(base_agent()),
        Some(path) => Ok(agent_with_roots(root_certs_with_ca(path)?)),
    }
}

/// The blocking HTTP client shared by all server-talking verbs.
pub struct Client {
    agent: ureq::Agent,
    /// Server origin, no trailing slash (e.g. `http://127.0.0.1:7400`).
    pub base: String,
    token: Option<String>,
    version_checked: AtomicBool,
    /// The ca_file this client's root set was built from (`--ca-file` >
    /// `NAATRE_CA_FILE` > the working-copy config, resolved at build
    /// time); the cache route rebuilds the tier agent over the SAME root
    /// set (`wc::fetch`).
    pub(crate) ca_file: Option<String>,
    /// The working copy's PER-REMOTE plain-http opt-in origin key, when
    /// this client's remote is the opted-in origin; the cache-route
    /// admission compares the CACHE url's origin against it — the
    /// origin's opt-in never covers a different cache host (`wc::fetch`).
    pub(crate) insecure_origin: Option<crate::url::OriginKey>,
}

impl Client {
    /// Builds a client over `base` (origin URL) with an optional bearer.
    pub fn new(base: &str, token: Option<String>) -> Client {
        // The no-ca_file agent build cannot fail (see [`agent`]).
        Client {
            agent: base_agent(),
            base: base.trim_end_matches('/').to_string(),
            token,
            version_checked: AtomicBool::new(false),
            ca_file: None,
            insecure_origin: None,
        }
    }

    /// [`Client::new`] with an explicit-TLS CA bundle (`--ca-file` /
    /// `NAATRE_CA_FILE` / config `ca_file`): the bundle's certificates
    /// join the platform roots (never replace them). A bad bundle is a
    /// hard local error naming the path. `ca_file = None` cannot fail.
    pub fn new_with_ca(
        base: &str,
        token: Option<String>,
        ca_file: Option<&str>,
    ) -> Result<Client, String> {
        Ok(Client {
            agent: agent(ca_file)?,
            base: base.trim_end_matches('/').to_string(),
            token,
            version_checked: AtomicBool::new(false),
            ca_file: ca_file.map(str::to_string),
            insecure_origin: None,
        })
    }

    /// Records the working copy's per-remote plain-http opt-in origin
    /// (the clone-time `--insecure-http` origin, or the working-copy
    /// config's stored origin); the cache-route reads compare against it.
    pub(crate) fn with_insecure_origin(mut self, origin: Option<crate::url::OriginKey>) -> Client {
        self.insecure_origin = origin;
        self
    }

    /// The admin-verb client: `--server` flag > `NAATRE_URL` (default
    /// `http://127.0.0.1:7400`), `NAATRE_TOKEN`, and `--ca-file` >
    /// `NAATRE_CA_FILE`. Admin verbs have no working copy, so their only
    /// plain-http opt-in is the invocation-level one (`--insecure-http` /
    /// `NAATRE_INSECURE_HTTP=1`); a non-loopback plain-http admin target
    /// without it is a usage refusal naming the opt-ins.
    pub fn from_env(net: &crate::verbs::Net) -> Result<Client, String> {
        let base = net
            .server
            .clone()
            .or_else(|| env::var("NAATRE_URL").ok().filter(|v| !v.is_empty()))
            .unwrap_or_else(|| DEFAULT_URL.to_string());
        let parsed =
            crate::url::parse_remote(&base).map_err(|e| format!("NAATRE_URL {base:?}: {e}"))?;
        crate::wc::admit_remote(
            &parsed,
            crate::verbs::invocation_optin(net.insecure_http),
            None,
        )?;
        let token = net.token.clone().or_else(|| env::var("NAATRE_TOKEN").ok());
        let ca_file = net
            .ca_file
            .clone()
            .or_else(|| env::var("NAATRE_CA_FILE").ok().filter(|v| !v.is_empty()));
        Ok(Client {
            agent: agent(ca_file.as_deref())?,
            base: parsed.origin(),
            token,
            version_checked: AtomicBool::new(false),
            ca_file,
            insecure_origin: None,
        })
    }

    /// Adds the bearer token and the client's conflict format
    /// ([`CONFLICT_FORMAT`]) to every API request.
    fn with_bearer<B>(&self, rb: ureq::RequestBuilder<B>) -> ureq::RequestBuilder<B> {
        let rb = rb.header("Naatre-Conflict-Format", CONFLICT_FORMAT.to_string());
        match &self.token {
            Some(t) => rb.header("Authorization", format!("Bearer {t}")),
            None => rb,
        }
    }

    /// GET /version once per process; warns on a major-version skew
    /// ("upgrade the server first"). Transport or shape problems are ignored
    /// here — the real call surfaces them. (Atomic: the client is shared
    /// across threads by the mount daemon.)
    fn check_version(&self) {
        if self
            .version_checked
            .compare_exchange(false, true, Ordering::Relaxed, Ordering::Relaxed)
            .is_err()
        {
            return;
        }
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
    /// A read — routed through the idempotent-verb retry contract.
    pub fn get_json(&self, path: &str, query: &[(&str, &str)]) -> Result<Value, HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        self.retrying(move || {
            let mut rb = self.with_bearer(self.agent.get(&url));
            for (k, v) in query {
                rb = rb.query(*k, *v);
            }
            let res = rb.call().map_err(|e| HttpError::transport(e, &url))?;
            self.finish_json(res).map(|(_, v)| v)
        })
    }

    /// POST a JSON body; succeeds on any 2xx, returning (status, body).
    /// Deliberately NOT retried: the POSTs that matter here are keyed
    /// writes (changeset create, land) whose duplicates are owned by their
    /// own key semantics. Idempotent POSTs (repo create, snapshot POST,
    /// the POST-shaped reads) go through [`Client::post_json_idempotent`].
    pub fn post_json(&self, path: &str, body: &Value) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let res = self
            .with_bearer(self.agent.post(&url))
            .send_json(body)
            .map_err(|e| HttpError::transport(e, &url))?;
        self.finish_json(res)
    }

    /// POST a JSON body under the idempotent-verb retry contract (see
    /// [`Client::retrying`]): repo create, snapshot POST, and the
    /// POST-shaped reads (`objects/present`, `objects/get`).
    pub fn post_json_idempotent(
        &self,
        path: &str,
        body: &Value,
    ) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let body = body.clone();
        self.retrying_codes(&POST_SAFE_RETRY_CODES, move || {
            let res = self
                .with_bearer(self.agent.post(&url))
                .send_json(&body)
                .map_err(|e| HttpError::transport(e, &url))?;
            self.finish_json(res)
        })
    }

    /// Raw PUT of bytes (the large-blob path); succeeds on any 2xx.
    /// Hash-addressed content at a content-addressed URL — idempotent, so
    /// routed through the retry contract.
    pub fn put_bytes(&self, path: &str, bytes: &[u8]) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        self.retrying(move || {
            let res = self
                .with_bearer(self.agent.put(&url))
                .send(bytes)
                .map_err(|e| HttpError::transport(e, &url))?;
            self.finish_json(res)
        })
    }

    /// PUT a JSON body; succeeds on any 2xx with the parsed JSON. Routed
    /// through the idempotent-verb retry contract: every PUT this client
    /// makes is a keyed upsert (`repo access`), so a replay cannot
    /// double-apply — the same uniform rule as the idempotent POSTs.
    pub fn put_json_idempotent(&self, path: &str, body: &Value) -> Result<(u16, Value), HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        let body = body.clone();
        self.retrying_codes(&POST_SAFE_RETRY_CODES, move || {
            let res = self
                .with_bearer(self.agent.put(&url))
                .send_json(&body)
                .map_err(|e| HttpError::transport(e, &url))?;
            self.finish_json(res)
        })
    }

    /// DELETE a path; succeeds on any 2xx with the parsed JSON. Routed
    /// through the idempotent-verb retry contract: every DELETE this
    /// client makes is keyed and idempotent (`token revoke`,
    /// `principal delete`), so a replay cannot double-apply — the same
    /// uniform rule as the idempotent POSTs above.
    pub fn delete_json_idempotent(&self, path: &str) -> Result<Value, HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        self.retrying_codes(&POST_SAFE_RETRY_CODES, move || {
            let res = self
                .with_bearer(self.agent.delete(&url))
                .call()
                .map_err(|e| HttpError::transport(e, &url))?;
            self.finish_json(res).map(|(_, v)| v)
        })
    }

    /// Raw GET of bytes (blob fetch). A non-2xx answer is parsed as the
    /// error envelope when possible. A read — routed through the
    /// idempotent-verb retry contract.
    pub fn get_bytes(&self, path: &str) -> Result<Vec<u8>, HttpError> {
        self.check_version();
        let url = format!("{}{}", self.base, path);
        self.retrying(move || {
            let res = self
                .with_bearer(self.agent.get(&url))
                .call()
                .map_err(|e| HttpError::transport(e, &url))?;
            let status = res.status().as_u16();
            let retry_after = retry_after_of(res.headers());
            let mut body = res.into_body();
            let bytes = body
                .read_to_vec()
                .map_err(|e| HttpError::Transport(format!("reading response body failed: {e}")))?;
            if !(200..300).contains(&status) {
                return Err(bytes_as_envelope(&bytes, status, retry_after));
            }
            Ok(bytes)
        })
    }

    /// The idempotent-verb retry contract: the client honors the server's
    /// own backpressure on verbs where a repeat is safe. A 503 envelope
    /// whose code is one of [`RETRY_CODES`] — the contention/transport
    /// sheds, each `retryable: true` + `Retry-After: 1` — is retried up to
    /// [`RETRY_TRIES`] attempts within [`RETRY_BUDGET`] of total waiting,
    /// each wait the server's `Retry-After` ([`RETRY_BACKOFF_DEFAULT`]
    /// when the header is missing, always capped at the remaining budget).
    /// Everything else returns as-is: `derivation_pending` (a derive
    /// backlog drains on the server's schedule, not inside 5 s) and the
    /// non-retryable `blame_too_large` are 503s that must surface
    /// immediately, and the keyed writes (changeset create, land) never
    /// route through here at all.
    fn retrying<T>(&self, f: impl FnMut() -> Result<T, HttpError>) -> Result<T, HttpError> {
        self.retrying_codes(&RETRY_CODES, f)
    }

    /// [`retrying`] restricted to `codes` — POST callers pass
    /// [`POST_SAFE_RETRY_CODES`].
    fn retrying_codes<T>(
        &self,
        codes: &[&str],
        mut f: impl FnMut() -> Result<T, HttpError>,
    ) -> Result<T, HttpError> {
        let mut remaining = RETRY_BUDGET;
        for attempt in 1..=RETRY_TRIES {
            let err = match f() {
                Ok(v) => return Ok(v),
                Err(e) => e,
            };
            let retry_after = match &err {
                HttpError::Envelope {
                    status: 503,
                    code,
                    retry_after,
                    ..
                } if codes.contains(&code.as_str()) => *retry_after,
                _ => return Err(err),
            };
            if attempt == RETRY_TRIES {
                return Err(err);
            }
            let wait = retry_after
                .map(Duration::from_secs)
                .unwrap_or(RETRY_BACKOFF_DEFAULT)
                .min(remaining);
            remaining -= wait;
            std::thread::sleep(wait);
        }
        unreachable!("every loop arm returns")
    }

    fn finish_json(
        &self,
        res: ureq::http::Response<ureq::Body>,
    ) -> Result<(u16, Value), HttpError> {
        let status = res.status().as_u16();
        let retry_after = retry_after_of(res.headers());
        let mut body = res.into_body();
        let value: Value = body
            .read_json()
            .map_err(|e| HttpError::Transport(format!("server sent a non-JSON response: {e}")))?;
        if !(200..300).contains(&status) {
            return Err(envelope_from(&value, status, retry_after));
        }
        Ok((status, value))
    }
}

/// Extracts the envelope `error.code` / `error.message` (and `details`,
/// when present), with a fallback for responses that are not the standard
/// envelope.
fn envelope_from(value: &Value, status: u16, retry_after: Option<u64>) -> HttpError {
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
        retry_after,
    }
}

pub(crate) fn bytes_as_envelope(bytes: &[u8], status: u16, retry_after: Option<u64>) -> HttpError {
    match serde_json::from_slice::<Value>(bytes) {
        Ok(v) if v["error"]["code"].is_string() => envelope_from(&v, status, retry_after),
        _ => envelope_from(&Value::Null, status, retry_after),
    }
}

/// The response's `Retry-After` header, in whole seconds, when present and
/// parseable.
fn retry_after_of(headers: &ureq::http::HeaderMap) -> Option<u64> {
    headers
        .get("retry-after")?
        .to_str()
        .ok()?
        .trim()
        .parse()
        .ok()
}

/// The integer before the first `.`, if it parses.
fn major_of(version: &str) -> Option<u64> {
    version.split('.').next()?.parse().ok()
}

/// Epoch-ns as ISO-8601 UTC (`YYYY-MM-DDTHH:MM:SSZ`); sub-second truncated.
fn iso8601_utc(ns: u64) -> String {
    let secs = ns / 1_000_000_000;
    let rem = secs % 86_400;
    let (y, m, d) = civil_from_days((secs / 86_400) as i64);
    format!(
        "{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z",
        rem / 3600,
        (rem % 3600) / 60,
        rem % 60
    )
}

/// Days since 1970-01-01 → (year, month, day) in the proleptic Gregorian
/// calendar (Howard Hinnant's civil_from_days; no date dependency).
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 }; // [1, 12]
    (if m <= 2 { y + 1 } else { y }, m as u32, d as u32)
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

/// The admin-verb client build: [`Client::from_env`], with the plain-http
/// refusal (a usage class) printed and returned as exit 3.
fn admin_client(net: &Net) -> Result<Client, i32> {
    match Client::from_env(net) {
        Ok(c) => Ok(c),
        Err(e) => {
            eprintln!("error: {e}");
            Err(3)
        }
    }
}

/// `naatre repo create <name>` (admin). Idempotent POST: routed through
/// the retry contract (the README quickstart's first write races the boot
/// backfill for the single-writer slot).
pub fn repo_create(name: &str, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let body = json!({ "name": name });
    match client.post_json_idempotent("/v1/repos", &body) {
        Ok((_, value)) => {
            println!("created repo {} (id {})", value["name"], value["id"]);
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre repo stats <name>` (admin).
pub fn repo_stats(name: &str, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match client.get_json(&format!("/v1/repos/{}/stats", uri_segment(name)), &[]) {
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

/// `naatre token list` (HTTP, admin). Expiry renders as ISO-8601 UTC (or
/// "never"), not raw epoch ns.
pub fn token_list(net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match client.get_json("/v1/tokens", &[]) {
        Ok(value) => {
            println!("{:<6} {:<16} {:<8} expires", "id", "principal", "role");
            for t in value["tokens"].as_array().map_or(&[][..], Vec::as_slice) {
                let expires = t["expires_ns"]
                    .as_u64()
                    .map_or("never".to_string(), iso8601_utc);
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
/// Routed through the idempotent-POST retry contract: `writer_busy` /
/// `server_busy` are PRE-execution rejections (the single-writer slot was
/// busy, or the shed happened before any work started), so a replay cannot
/// double-apply — and the boot-time derive backfill makes a slot collision
/// a realistic first-try event, not a pathological one.
pub fn token_create(
    principal: &str,
    role: &str,
    ttl_days: Option<u64>,
    scope: Option<&str>,
    net: &Net,
) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let mut body = json!({ "principal": principal, "role": role });
    if let Some(days) = ttl_days {
        body["ttl_days"] = json!(days);
    }
    if let Some(scope) = scope {
        body["scope_path"] = json!(scope);
    }
    match client.post_json_idempotent("/v1/tokens", &body) {
        Ok((_, value)) => {
            // Printed exactly once; the server stores only the hash. The
            // numeric id is what `token revoke` takes.
            println!("token: {}", value["token"].as_str().unwrap_or("?"));
            println!("token id: {}", value["id"]);
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre token revoke <id>` (admin; idempotent) — through the
/// idempotent-DELETE retry contract.
pub fn token_revoke(id: u64, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match client.delete_json_idempotent(&format!("/v1/tokens/{id}")) {
        Ok(_) => {
            println!("revoked {id}");
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre principal delete <name>` (admin). The server deletes the
/// principal + token rows and pseudonymizes the audit actor to
/// `deleted:<name>`; history attribution survives unless the changesets
/// are themselves tombstoned. Through the idempotent-DELETE retry
/// contract.
pub fn principal_delete(name: &str, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let path = format!("/v1/principals/{}", uri_segment(name));
    match client.delete_json_idempotent(&path) {
        Ok(value) => {
            println!(
                "deleted {} ({} token(s) went with it)",
                value["principal"].as_str().unwrap_or(name),
                value["tokens_deleted"]
            );
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre repo access grant <repo> <principal> --read|--write` (admin,
/// M13b). `--write` implies read server-side (the invariant is enforced
/// at the route: a write-only grant is a 422). Idempotent PUT.
pub fn repo_access_grant(repo: &str, principal: &str, read: bool, write: bool, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    let body = json!({ "principal": principal, "read": read, "write": write });
    match client.put_json_idempotent(&format!("/v1/repos/{}/access", uri_segment(repo)), &body) {
        Ok((_, value)) => {
            println!(
                "granted {} on {}: read={} write={}",
                value["principal"], value["repo"], value["read"], value["write"]
            );
            0
        }
        Err(e) => report(&e),
    }
}

/// `naatre repo access revoke <repo> <principal> [--read|--write]`
/// (admin). `--write` downgrades to read-only (a PUT with write = false);
/// `--read` or no flag removes the grant entirely (DELETE — write cannot
/// stand without read). Removing the repo's LAST grant returns it to
/// public (the documented flip). Idempotent either way.
pub fn repo_access_revoke(repo: &str, principal: &str, read: bool, write: bool, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    // A bare `--write` downgrades to read-only (a PUT with write = false);
    // `--read` (with or without --write) or NO flag removes the grant
    // entirely (write cannot stand without read).
    if write && !read {
        // The write-only revoke: downgrade the row to read-only.
        let body = json!({ "principal": principal, "read": true, "write": false });
        match client.put_json_idempotent(&format!("/v1/repos/{}/access", uri_segment(repo)), &body)
        {
            Ok((_, value)) => {
                println!(
                    "revoked write for {} on {}: read={} write={}",
                    value["principal"], value["repo"], value["read"], value["write"]
                );
                0
            }
            Err(e) => report(&e),
        }
    } else {
        match client.delete_json_idempotent(&format!(
            "/v1/repos/{}/access/{}",
            uri_segment(repo),
            uri_segment(principal)
        )) {
            Ok(_) => {
                println!("revoked all access for {principal} on {repo}");
                0
            }
            Err(e) => report(&e),
        }
    }
}

/// `naatre repo access list <repo>` (admin).
pub fn repo_access_list(repo: &str, net: &Net) -> i32 {
    let client = match admin_client(net) {
        Ok(c) => c,
        Err(code) => return code,
    };
    match client.get_json(&format!("/v1/repos/{}/access", uri_segment(repo)), &[]) {
        Ok(value) => {
            let rows = value["access"].as_array().map_or(&[][..], Vec::as_slice);
            if rows.is_empty() {
                println!("(no grants — {repo} is public)");
            } else {
                println!("{:<20} {:<6} {:<6}", "principal", "read", "write");
                for row in rows {
                    println!(
                        "{:<20} {:<6} {:<6}",
                        row["principal"].as_str().unwrap_or("?"),
                        row["read"].as_bool().unwrap_or(false),
                        row["write"].as_bool().unwrap_or(false),
                    );
                }
            }
            0
        }
        Err(e) => report(&e),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ca_file_errors_name_the_path_and_say_pem_bundle() {
        // A missing file is a hard error naming the path.
        let err =
            root_certs_with_ca("/nonexistent/naatre-ca.pem").expect_err("missing file refused");
        assert!(err.contains("/nonexistent/naatre-ca.pem"), "{err:?}");
        // A file with no certificates in it is refused as a PEM-bundle
        // problem, also naming the path.
        let dir = tempfile::tempdir().expect("tempdir");
        let path = dir.path().join("ca.pem");
        std::fs::write(&path, "not a pem at all").expect("write");
        let err = root_certs_with_ca(path.to_str().unwrap()).expect_err("garbage refused");
        assert!(err.contains("must be a PEM CA bundle"), "{err:?}");
        assert!(err.contains("ca.pem"), "{err:?}");
    }

    #[test]
    fn client_construction_keeps_the_two_argument_form_infallible() {
        let client = Client::new("http://127.0.0.1:7400/", Some("t".into()));
        assert_eq!(client.base, "http://127.0.0.1:7400");
        assert_eq!(client.insecure_origin, None);
        assert_eq!(client.ca_file, None);
        // The ca_file=None form is the same client (and cannot fail).
        let client = Client::new_with_ca("https://naatre.example.com", None, None)
            .expect("no-ca build is infallible");
        assert_eq!(client.base, "https://naatre.example.com");
    }

    #[test]
    fn from_env_admin_refusal_is_the_plain_http_usage_class() {
        // The default and any loopback NAATRE_URL need no opt-in.
        assert!(Client::from_env(&Net::default()).is_ok());
        let loopback = Net {
            server: Some("http://127.0.0.1:7499".into()),
            ..Net::default()
        };
        assert!(Client::from_env(&loopback).is_ok());
        // A non-loopback plain-http admin target without the opt-in is a
        // usage refusal naming the opt-ins (exit 3 at the verb).
        let lan = Net {
            server: Some("http://naatre.lan:7400".into()),
            ..Net::default()
        };
        let Err(err) = Client::from_env(&lan) else {
            panic!("a non-loopback plain-http admin target must refuse");
        };
        assert!(err.contains("--insecure-http"), "{err:?}");
        assert!(err.contains("NAATRE_INSECURE_HTTP"), "{err:?}");
        // The invocation-level opt-in admits it (the admin-verb path).
        let opted = Net {
            insecure_http: true,
            ..lan
        };
        let client = Client::from_env(&opted).expect("opted in");
        assert_eq!(client.base, "http://naatre.lan:7400");
    }
}
