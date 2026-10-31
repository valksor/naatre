//! Test-only S3 provisioning (M9b): the S3 legs run against a REAL S3
//! server wherever one can be produced —
//!
//! 1. `NAATRE_TEST_S3_ENDPOINT` set → use that endpoint as-is (CI's service
//!    container or a LAN minio; bare `host:port`; credentials via
//!    `NAATRE_TEST_S3_ACCESS_KEY` / `NAATRE_TEST_S3_SECRET`, bucket via
//!    `NAATRE_TEST_S3_BUCKET`);
//! 2. unset → run the PINNED minio server binary (a REAL S3
//!    wire-protocol server — dl.min.io is archived, so the pin rides
//!    GitHub releases and its published `.sha256sum`): `NAATRE_TEST_MINIO_BIN`
//!    names a binary to use as-is; otherwise the pinned release is
//!    downloaded once into the user cache dir and sha256-verified on
//!    EVERY run;
//! 3. neither possible (no binary and the download fails) → the caller
//!    prints a skip notice (the only legitimate skip: no usable S3 server
//!    can be produced).
//!
//! A present but broken binary (bad sha256, refuses to start) is NOT a
//! skip — it panics, loudly. Trust: the server binds 127.0.0.1 only, the
//! bucket is created via a SigV4-signed CreateBucket against the
//! loopback endpoint, and the process dies with the test process. minio is
//! AGPL-3.0 — a test-only tool, never distributed with naatre.

use std::io::Read;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use sha2::Digest;

use naatre_store::backend::{BackendConfig, S3Connect};

/// The pinned server (minio is archived on dl.min.io; this is the newest
/// GitHub release still carrying binaries).
pub const MINIO_VERSION: &str = "RELEASE.2025-09-07T16-13-09Z";
/// sha256 of `minio.darwin-arm64.<VERSION>` (the release's own .sha256sum).
#[cfg(all(target_os = "macos", target_arch = "aarch64"))]
pub const MINIO_SHA256: &str = "7c3b3039b76e55a1b80935848ed83998d5e8d317374f87851f46a019ff5c0aa4";
/// sha256 of `minio.linux-amd64.<VERSION>` (the release's own .sha256sum).
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
pub const MINIO_SHA256: &str = "7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f";
#[cfg(not(any(
    all(target_os = "macos", target_arch = "aarch64"),
    all(target_os = "linux", target_arch = "x86_64")
)))]
pub const MINIO_SHA256: &str = "";

#[cfg(all(target_os = "macos", target_arch = "aarch64"))]
const MINIO_ASSET: &str = "minio.darwin-arm64";
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
const MINIO_ASSET: &str = "minio.linux-amd64";

/// Fixed loopback credentials (test-only; never a production secret).
const TEST_ACCESS_KEY: &str = "naatre";
const TEST_SECRET: &str = "naatre-test-secret";
pub const TEST_BUCKET: &str = "naatre-test";
const TEST_REGION: &str = "us-east-1";

/// A live S3 test server. `Drop` stops the provisioned minio (external
/// endpoints are left alone).
pub enum S3Target {
    External { endpoint: String },
    Provisioned(MinioServer),
}

pub struct MinioServer {
    endpoint: String,
    child: Option<Child>,
    dir: Option<tempfile::TempDir>,
}

impl S3Target {
    pub fn endpoint(&self) -> &str {
        match self {
            S3Target::External { endpoint } => endpoint,
            S3Target::Provisioned(m) => &m.endpoint,
        }
    }

    /// The BackendConfig for one test: blob = s3, path-style (LAN minio's
    /// shape), plaintext allowed (the loopback server has no TLS).
    pub fn config(&self) -> BackendConfig {
        let mut cfg = BackendConfig::s3(S3Connect {
            endpoint: Some(self.endpoint().to_string()),
            bucket: std::env::var("NAATRE_TEST_S3_BUCKET").unwrap_or_else(|_| TEST_BUCKET.into()),
            region: Some(TEST_REGION.into()),
            access_key_id: std::env::var("NAATRE_TEST_S3_ACCESS_KEY")
                .unwrap_or_else(|_| TEST_ACCESS_KEY.into()),
            secret: std::env::var("NAATRE_TEST_S3_SECRET").unwrap_or_else(|_| TEST_SECRET.into()),
            path_style: true,
        });
        cfg.allow_plaintext = true;
        cfg
    }
}

impl Drop for MinioServer {
    fn drop(&mut self) {
        if let Some(child) = self.child.take() {
            let mut child = child;
            let _ = child.kill();
            let _ = child.wait();
        }
        if let Some(dir) = self.dir.take() {
            let _ = dir.close(); // best-effort: /tmp cleans up anyway
        }
    }
}

/// The test S3 server: the env endpoint, or a provisioned pinned minio, or
/// `None` (skip — no usable S3 server).
pub fn acquire() -> Option<S3Target> {
    if let Ok(endpoint) = std::env::var("NAATRE_TEST_S3_ENDPOINT") {
        for (var, needed) in [
            (
                "NAATRE_TEST_S3_ACCESS_KEY",
                std::env::var("NAATRE_TEST_S3_ACCESS_KEY").is_err(),
            ),
            (
                "NAATRE_TEST_S3_SECRET",
                std::env::var("NAATRE_TEST_S3_SECRET").is_err(),
            ),
        ] {
            if needed {
                panic!(
                    "NAATRE_TEST_S3_ENDPOINT is set but {var} is missing: an external \
                     test endpoint needs credentials"
                );
            }
        }
        ensure_bucket(&endpoint);
        return Some(S3Target::External { endpoint });
    }
    let bin = find_or_fetch_minio()?;
    Some(S3Target::Provisioned(start_minio(&bin)))
}

/// Locates or fetches the pinned minio binary; `None` (with a notice) when
/// neither a binary nor a usable download exists.
fn find_or_fetch_minio() -> Option<std::path::PathBuf> {
    if let Ok(bin) = std::env::var("NAATRE_TEST_MINIO_BIN") {
        let bin = std::path::PathBuf::from(bin);
        if sha256_of(&bin) != MINIO_SHA256 {
            panic!(
                "NAATRE_TEST_MINIO_BIN binary is not the pinned {MINIO_VERSION} build \
                 (sha256 mismatch) — the S3 legs must not run against an unpinned server"
            );
        }
        return Some(bin);
    }
    let cache = cache_dir().join(format!("minio-{MINIO_VERSION}"));
    if cache.is_file() {
        // Verify on every run: a silently corrupted/tampered cache is a
        // broken binary, not a skip.
        if sha256_of(&cache) != MINIO_SHA256 {
            panic!(
                "cached minio at {} is not the pinned {MINIO_VERSION} build (sha256 \
                 mismatch); delete it and re-run to re-download",
                cache.display()
            );
        }
        return Some(cache);
    }
    download_minio(&cache)
}

/// The per-user cache dir for the pinned test server binary.
fn cache_dir() -> std::path::PathBuf {
    if let Ok(xdg) = std::env::var("XDG_CACHE_HOME") {
        return std::path::PathBuf::from(xdg).join("naatre-test-minio");
    }
    if cfg!(target_os = "macos") {
        let home = std::env::var("HOME").expect("HOME for the test cache dir");
        std::path::PathBuf::from(home)
            .join("Library/Caches")
            .join("naatre-test-minio")
    } else {
        let home = std::env::var("HOME").expect("HOME for the test cache dir");
        std::path::PathBuf::from(home).join(".cache/naatre-test-minio")
    }
}

/// Downloads the pinned release, verifying the sha256 against the pin
/// BEFORE the binary is considered usable. A download failure is the one
/// legitimate skip (no usable S3 server can be produced).
fn download_minio(dest: &std::path::Path) -> Option<std::path::PathBuf> {
    // Each supported target pins exactly one release asset.
    let asset = match MINIO_ASSET {
        "" => {
            eprintln!(
                "skip: S3 legs have no pinned minio asset for this platform; set \
                 NAATRE_TEST_S3_ENDPOINT to run them against a real endpoint"
            );
            return None;
        }
        a => a,
    };
    let url = format!(
        "https://github.com/minio/minio/releases/download/{MINIO_VERSION}/{asset}.{MINIO_VERSION}"
    );
    eprintln!("fetching pinned minio {MINIO_VERSION} ({asset}) …");
    let dir = dest.parent().expect("cache dir has a parent");
    let _ = std::fs::create_dir_all(dir);
    // Unique per call: parallel test threads may fetch concurrently, and
    // each rename is atomic — last writer wins on identical (sha-verified)
    // bytes.
    let partial = dir.join(format!(
        ".partial-{}-{}-{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos(),
        dest.file_name().unwrap_or_default().to_string_lossy()
    ));
    let dl = match reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(600))
        .build()
    {
        Ok(c) => c,
        Err(e) => {
            eprintln!("skip: S3 legs could not start a download client: {e}");
            return None;
        }
    };
    let mut resp = match dl.get(&url).send() {
        Ok(r) if r.status().is_success() => r,
        Ok(r) => {
            eprintln!("skip: pinned minio download answered {}: {url}", r.status());
            return None;
        }
        Err(e) => {
            eprintln!(
                "skip: pinned minio download failed ({e}); set NAATRE_TEST_MINIO_BIN or NAATRE_TEST_S3_ENDPOINT to run the S3 legs without a download"
            );
            return None;
        }
    };
    let mut file = std::fs::File::create(&partial).expect("create partial download file");
    let mut hasher = sha2::Sha256::new();
    let mut buf = [0u8; 256 * 1024];
    loop {
        let n = match resp.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) => {
                eprintln!("skip: pinned minio download failed mid-stream: {e}");
                return None;
            }
        };
        use std::io::Write as _;
        file.write_all(&buf[..n]).expect("write partial download");
        hasher.update(&buf[..n]);
    }
    let got = format!("{:x}", hasher.finalize());
    if got != MINIO_SHA256 {
        panic!(
            "pinned minio download does not match its published .sha256sum \
             (expected {MINIO_SHA256}, got {got}) — refusing to run it"
        );
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&partial, std::fs::Permissions::from_mode(0o755))
            .expect("chmod the pinned minio binary");
    }
    std::fs::rename(&partial, dest).expect("activate the downloaded binary");
    eprintln!("fetched and verified {MINIO_VERSION}");
    Some(dest.to_path_buf())
}

/// Hex sha256 of a file, streamed.
fn sha256_of(path: &std::path::Path) -> String {
    let mut f = std::fs::File::open(path).unwrap_or_else(|e| {
        panic!("open {} for sha256: {e}", path.display());
    });
    let mut hasher = sha2::Sha256::new();
    let mut buf = [0u8; 256 * 1024];
    loop {
        let n = f.read(&mut buf).expect("read for sha256");
        if n == 0 {
            break;
        }
        hasher.update(&buf[..n]);
    }
    format!("{:x}", hasher.finalize())
}

/// Bootstraps and starts the throwaway server. Binaries present but broken
/// is a panic, not a skip.
fn start_minio(bin: &std::path::Path) -> MinioServer {
    let dir = tempfile::tempdir().expect("temp dir for the throwaway minio");
    let data = dir.path().join("data");
    std::fs::create_dir_all(&data).expect("create minio data dir");

    // Ephemeral port: bind :0, read the port, release (a tiny race window,
    // acceptable for a throwaway test server — same as pg_provision).
    let port = std::net::TcpListener::bind("127.0.0.1:0")
        .expect("ephemeral port bind")
        .local_addr()
        .unwrap()
        .port();
    let endpoint = format!("127.0.0.1:{port}");

    let child = Command::new(bin)
        .arg("server")
        .arg(&data)
        .arg("--address")
        .arg(&endpoint)
        .arg("--quiet")
        .env("MINIO_ROOT_USER", TEST_ACCESS_KEY)
        .env("MINIO_ROOT_PASSWORD", TEST_SECRET)
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap_or_else(|e| panic!("spawn {}: {e}", bin.display()));

    let server = MinioServer {
        endpoint,
        child: Some(child),
        dir: Some(dir),
    };
    wait_ready(server)
}

/// Polls /minio/health/ready until 200 (or panics: a binary present but
/// broken is not a skip).
fn wait_ready(mut server: MinioServer) -> MinioServer {
    let deadline = Instant::now() + Duration::from_secs(30);
    let url = format!("http://{}/minio/health/ready", server.endpoint);
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(5))
        .build()
        .expect("health client");
    loop {
        if let Ok(resp) = client.get(&url).send()
            && resp.status().as_u16() == 200
        {
            ensure_bucket(&server.endpoint);
            eprintln!("provisioned throwaway minio at {}", server.endpoint);
            return server;
        }
        // A crashed server must not burn the whole deadline.
        if let Some(child) = server.child.as_mut()
            && let Ok(Some(_)) = child.try_wait()
        {
            panic!(
                "pinned minio exited during startup — the S3 legs must RUN when a server binary exists"
            );
        }
        if Instant::now() > deadline {
            panic!("pinned minio never answered {url} within 30s");
        }
        std::thread::sleep(Duration::from_millis(250));
    }
}

/// Creates the test bucket if absent (CreateBucket; 409
/// BucketAlreadyOwnedByYou counts as present). SigV4-signed by hand: the
/// harness avoids a second client dependency for one bootstrap call, and
/// the canonical form (path-style, `/{bucket}/`, empty-payload hash) is
/// the same one real S3 accepts.
fn ensure_bucket(endpoint: &str) {
    let bucket = std::env::var("NAATRE_TEST_S3_BUCKET").unwrap_or_else(|_| TEST_BUCKET.into());
    let access =
        std::env::var("NAATRE_TEST_S3_ACCESS_KEY").unwrap_or_else(|_| TEST_ACCESS_KEY.into());
    let secret = std::env::var("NAATRE_TEST_S3_SECRET").unwrap_or_else(|_| TEST_SECRET.into());
    let (status, body) = sigv4_request(
        "PUT",
        endpoint,
        &format!("/{bucket}/"),
        b"",
        &access,
        &secret,
    );
    match status {
        200 => {}
        409 if body.contains("BucketAlreadyOwnedByYou") => {}
        other => panic!(
            "CreateBucket for the test bucket failed: HTTP {other} (details withheld; \
             check the endpoint, credentials, and bucket permissions)"
        ),
    }
}

/// One SigV4-signed request against `endpoint` (bare host:port, plain
/// HTTP). Returns (status, body-up-to-4KiB).
fn sigv4_request(
    method: &str,
    endpoint: &str,
    path: &str,
    body: &[u8],
    access: &str,
    secret: &str,
) -> (u16, String) {
    use hmac::Mac;
    use sha2::Digest;

    let host = endpoint.to_string();
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .expect("clock")
        .as_secs();
    let (datestamp, amz_date) = http_dates(now);

    let payload_hash = format!("{:x}", sha2::Sha256::digest(body));
    let mut names = ["host", "x-amz-content-sha256", "x-amz-date"];
    names.sort_unstable();
    let signed_headers = names.join(";");
    let canonical_headers =
        format!("host:{host}\nx-amz-content-sha256:{payload_hash}\nx-amz-date:{amz_date}\n");
    let canonical_request =
        format!("{method}\n{path}\n\n{canonical_headers}\n{signed_headers}\n{payload_hash}");
    let scope = format!("{datestamp}/{TEST_REGION}/s3/aws4_request");
    let string_to_sign = format!(
        "AWS4-HMAC-SHA256\n{amz_date}\n{scope}\n{:x}",
        sha2::Sha256::digest(canonical_request.as_bytes())
    );

    type HmacSha256 = hmac::Hmac<sha2::Sha256>;
    let mut mac = HmacSha256::new_from_slice(format!("AWS4{secret}").as_bytes())
        .expect("hmac accepts any key length");
    mac.update(datestamp.as_bytes());
    let mut key = mac.finalize().into_bytes();
    let mut step = |data: &[u8]| {
        let mut mac = HmacSha256::new_from_slice(&key).expect("hmac key");
        mac.update(data);
        key = mac.finalize().into_bytes();
    };
    step(TEST_REGION.as_bytes());
    step(b"s3");
    step(b"aws4_request");
    let mut mac = HmacSha256::new_from_slice(&key).expect("hmac key");
    mac.update(string_to_sign.as_bytes());
    let signature = format!("{:x}", mac.finalize().into_bytes());

    let url = format!("http://{host}{path}");
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .expect("sigv4 client");
    let resp = client
        .request(reqwest::Method::from_bytes(method.as_bytes()).expect("HTTP method"), &url)
        .header("Authorization", format!("AWS4-HMAC-SHA256 Credential={access}/{scope}, SignedHeaders={signed_headers}, Signature={signature}"))
        .header("x-amz-content-sha256", &payload_hash)
        .header("x-amz-date", &amz_date)
        .body(body.to_vec())
        .send()
        .unwrap_or_else(|e| panic!("request {method} {path} against {host}: {e}"));
    let status = resp.status().as_u16();
    let body = resp.text().unwrap_or_default();
    (status, body.chars().take(4096).collect())
}

/// (YYYYMMDD, YYYYMMDDThhmmssZ) for the SigV4 scope/date headers.
fn http_dates(unix_secs: u64) -> (String, String) {
    // Days → Y/M/D via the civil-from-days algorithm (no chrono dep in the
    // harness: date math only, UTC).
    let days = unix_secs / 86_400;
    let secs_of_day = unix_secs % 86_400;
    let z = days as i64 + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = if m <= 2 { y + 1 } else { y };
    let (h, min, s) = (
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60,
    );
    (
        format!("{y:04}{m:02}{d:02}"),
        format!("{y:04}{m:02}{d:02}T{h:02}{min:02}{s:02}Z"),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn http_dates_match_known_instants() {
        // 2026-10-31T00:00:00Z = 1793404800
        let (d, dt) = http_dates(1_793_404_800);
        assert_eq!(d, "20261031");
        assert_eq!(dt, "20261031T000000Z");
        // Epoch itself.
        let (d, dt) = http_dates(0);
        assert_eq!(d, "19700101");
        assert_eq!(dt, "19700101T000000Z");
        // Intra-day time: 2026-10-31T12:34:56Z = 1793404800 + 45296
        let (d, dt) = http_dates(1_793_404_800 + 45_296);
        assert_eq!(d, "20261031");
        assert_eq!(dt, "20261031T123456Z");
    }
}
