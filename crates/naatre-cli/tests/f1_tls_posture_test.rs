//! F1 TLS-posture pins, in-process (no external network):
//!
//! 13a — an https handshake through `ca_file` against a REAL local rustls
//!       server (rcgen CA + leaf): succeeds with the CA in `ca_file`,
//!       fails without it (the leaf is not publicly rooted).
//! 13b — `max_redirects(0)`: a local 302 to another host is not followed,
//!       the 3xx surfaces, and the redirect target NEVER receives an
//!       `Authorization` header.
//! 13c — the cache-tier plain-http rule: `cache_run`'s origin admission
//!       (exit 3 for a non-loopback plain-http origin and for the
//!       confusion shapes), and `fetch_blobs_verified`'s cache-route
//!       admission (loopback or the client's own opted-in origin only —
//!       hard usage error otherwise, no silent origin fallback).

use std::io::{Read, Write as _};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

use naatre_cli::client::{Client, HttpError};
use naatre_cli::host;
use naatre_cli::verbs::Net;
use naatre_cli::wc::Wc;
use naatre_cli::wc::fetch::{CacheRoute, FetchError, fetch_blobs_verified};
use naatre_core::ids::{Kind, object_id};

// ---- 13a: the ca_file handshake, against a real rustls server --------------

/// A throwaway CA (rcgen) plus a leaf signed by it, carrying SANs for
/// `localhost` and `127.0.0.1`.
struct TestCa {
    ca_pem: String,
    leaf_chain: Vec<rustls::pki_types::CertificateDer<'static>>,
    leaf_key_der: rustls::pki_types::PrivatePkcs8KeyDer<'static>,
}

fn test_ca() -> TestCa {
    use rcgen::{BasicConstraints, CertificateParams, DnType, IsCa, Issuer, KeyPair};
    let ca_key = KeyPair::generate().expect("CA keygen");
    let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("CA params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params
        .distinguished_name
        .push(DnType::CommonName, "naatre test ca");
    let ca = ca_params.self_signed(&ca_key).expect("self-signed CA");
    let issuer = Issuer::new(ca_params, ca_key);
    let leaf_key = KeyPair::generate().expect("leaf keygen");
    let leaf_params =
        CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).expect("leaf params");
    let leaf = leaf_params
        .signed_by(&leaf_key, &issuer)
        .expect("leaf signed");
    TestCa {
        ca_pem: ca.pem(),
        leaf_chain: vec![leaf.der().clone(), ca.der().clone()],
        leaf_key_der: rustls::pki_types::PrivatePkcs8KeyDer::from(leaf_key.serialize_der()),
    }
}

fn tls_server_config(ca: &TestCa) -> Arc<rustls::ServerConfig> {
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .expect("protocol versions")
    .with_no_client_auth()
    .with_single_cert(
        ca.leaf_chain.clone(),
        rustls::pki_types::PrivateKeyDer::Pkcs8(ca.leaf_key_der.clone_key()),
    )
    .expect("server cert");
    Arc::new(config)
}

/// Serves `{\"version\":\"0.1.0\"}` over TLS on a loopback port until the
/// listener is gone. Bounded: a handful of connections, a handful of
/// keep-alive requests each (ureq pools).
fn spawn_tls_server(config: Arc<rustls::ServerConfig>) -> u16 {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").expect("bind");
    let port = listener.local_addr().expect("local addr").port();
    std::thread::spawn(move || {
        for stream in listener.incoming().take(4).flatten() {
            let Ok(conn) = rustls::ServerConnection::new(Arc::clone(&config)) else {
                continue;
            };
            let mut tls = rustls::StreamOwned::new(conn, stream);
            let mut served = 0;
            while served < 8 {
                let mut buf = Vec::new();
                let mut chunk = [0u8; 4096];
                loop {
                    match tls.read(&mut chunk) {
                        Ok(0) => break,
                        Ok(n) => {
                            buf.extend_from_slice(&chunk[..n]);
                            if buf.windows(4).any(|w| w == b"\r\n\r\n") {
                                break;
                            }
                        }
                        Err(_) => break,
                    }
                }
                if buf.is_empty() {
                    break; // EOF or a failed handshake
                }
                let body = br#"{"version":"0.1.0"}"#;
                let head = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: \
                     {}\r\nConnection: keep-alive\r\n\r\n",
                    body.len()
                );
                if tls.write_all(head.as_bytes()).is_err()
                    || tls.write_all(body.as_slice()).is_err()
                    || tls.flush().is_err()
                {
                    break;
                }
                served += 1;
            }
        }
    });
    port
}

/// 13a: the handshake succeeds with the CA in `ca_file` and fails without
/// it — the leaf is signed by the test CA alone, so a pass proves the
/// bundle anchored it and a fail proves it is not publicly rooted.
#[test]
fn ca_file_anchors_a_real_rustls_handshake_and_platform_roots_do_not() {
    let ca = test_ca();
    let port = spawn_tls_server(tls_server_config(&ca));
    let base = format!("https://127.0.0.1:{port}");

    let ca_path = tempfile::tempdir().expect("tempdir");
    let ca_file = ca_path.path().join("ca.pem");
    std::fs::write(&ca_file, &ca.ca_pem).expect("write CA pem");

    let client = Client::new_with_ca(&base, None, Some(ca_file.to_str().unwrap()))
        .expect("a valid PEM bundle builds");
    let v = client
        .get_json("/version", &[])
        .expect("the ca_file anchors the handshake");
    assert_eq!(v["version"], "0.1.0", "{v}");

    // Without the bundle: platform roots only — the test CA's leaf refuses.
    let client = Client::new(&base, None);
    let err = client
        .get_json("/version", &[])
        .expect_err("the unanchored leaf must fail the handshake");
    assert!(
        matches!(err, HttpError::Transport(_)),
        "a TLS failure is a transport failure, not an envelope: {err:?}"
    );
}

// ---- 13b: 302s are surfaced, never followed, never carrying auth -----------

/// 13b: a local server answers 302 with a Location on a SECOND local
/// server; the client must surface the 3xx (never follow), and the second
/// server must never receive a request carrying `Authorization`.
#[test]
fn a_302_is_surfaced_never_followed_and_never_carries_authorization() {
    // The redirect target: records whether any request carried auth.
    let b = std::net::TcpListener::bind("127.0.0.1:0").expect("bind B");
    let b_port = b.local_addr().expect("B addr").port();
    let saw_auth = Arc::new(AtomicBool::new(false));
    let hits = Arc::new(AtomicUsize::new(0));
    {
        let saw_auth = Arc::clone(&saw_auth);
        let hits = Arc::clone(&hits);
        std::thread::spawn(move || {
            for stream in b.incoming().take(4).flatten() {
                let mut s = stream;
                hits.fetch_add(1, Ordering::SeqCst);
                let _ = s.set_read_timeout(Some(std::time::Duration::from_secs(2)));
                let head = read_head(&mut s);
                if head.to_ascii_lowercase().contains("authorization:") {
                    saw_auth.store(true, Ordering::SeqCst);
                }
                let _ = s.write_all(
                    b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi",
                );
            }
        });
    }
    // The 302 answerer: redirects everything to B.
    let a = std::net::TcpListener::bind("127.0.0.1:0").expect("bind A");
    let a_port = a.local_addr().expect("A addr").port();
    std::thread::spawn(move || {
        for stream in a.incoming().take(4).flatten() {
            let mut s = stream;
            let _ = s.set_read_timeout(Some(std::time::Duration::from_secs(2)));
            let _ = read_head(&mut s);
            let _ = s.write_all(
                format!(
                    "HTTP/1.1 302 Found\r\nLocation: \
                     http://127.0.0.1:{b_port}/v1/redirected\r\nContent-Length: \
                     0\r\nConnection: keep-alive\r\n\r\n"
                )
                .as_bytes(),
            );
        }
    });

    let client = Client::new(
        &format!("http://127.0.0.1:{a_port}"),
        Some("secret-token".into()),
    );
    let err = client
        .get_bytes("/v1/x")
        .expect_err("a 302 is not a 2xx body — it must surface, never follow");
    assert!(
        matches!(err, HttpError::Envelope { status: 302, .. }),
        "the 3xx surfaces as the envelope it is: {err:?}"
    );

    // Settle: any buggy follow would have connected within moments.
    for _ in 0..20 {
        if hits.load(Ordering::SeqCst) > 0 {
            break;
        }
        std::thread::sleep(std::time::Duration::from_millis(25));
    }
    assert_eq!(
        hits.load(Ordering::SeqCst),
        0,
        "max_redirects(0): the redirect target was never contacted"
    );
    assert!(
        !saw_auth.load(Ordering::SeqCst),
        "no Authorization header ever reaches a redirect target"
    );
}

/// Reads one request head (through `\r\n\r\n`, bounded).
fn read_head(s: &mut impl Read) -> String {
    let mut got = Vec::new();
    let mut chunk = [0u8; 4096];
    loop {
        match s.read(&mut chunk) {
            Ok(0) | Err(_) => break,
            Ok(n) => {
                got.extend_from_slice(&chunk[..n]);
                if got.windows(4).any(|w| w == b"\r\n\r\n") {
                    break;
                }
            }
        }
    }
    String::from_utf8_lossy(&got).into_owned()
}

// ---- 13c: the cache-tier plain-http rule -----------------------------------

/// 13c (cache_run): a non-loopback plain-http origin refuses (exit 3), the
/// confusion shapes refuse the same way, and a LOOPBACK origin passes the
/// origin rule (proven by reaching — and failing at — the key load: exit
/// 1, not 3).
#[test]
fn cache_run_origin_admission() {
    let dir = tempfile::tempdir().expect("tempdir");
    // The review's example: a LAN origin, no opt-in.
    let code = host::cache_run(
        "http://lan-origin:7400",
        "unused",
        "127.0.0.1:0",
        dir.path(),
        "info",
        false,
    );
    assert_eq!(code, 3, "a non-loopback plain-http origin refuses");
    // The confusion shapes are parse refusals (exit 3), never requests.
    for origin in [
        "http://127.0.0.1?@lan-origin:7400",
        "http://user@127.0.0.1:80@evil.example",
        "http://127.0.0.1:99999",
    ] {
        let code = host::cache_run(origin, "unused", "127.0.0.1:0", dir.path(), "info", false);
        assert_eq!(code, 3, "{origin}: a confusion-shape origin refuses");
    }
    // A loopback origin passes the admission (and, with no key file, the
    // run fails LATER — exit 1, not the origin refusal).
    let code = host::cache_run(
        "http://127.0.0.1:7400/",
        "this-key-file-does-not-exist",
        "127.0.0.1:0",
        dir.path(),
        "info",
        false,
    );
    assert_eq!(code, 1, "the loopback origin passed the origin rule");
}

/// 13c (fetch_blobs_verified): the cache route admits a LOOPBACK tier or a
/// tier whose origin equals the client's own per-remote opt-in — nothing
/// else — and a refusal is the hard usage class (no silent origin
/// fallback). Driven through a working copy whose config carries the
/// per-remote origin (the same resolution the verbs use).
#[test]
fn cache_route_admission_is_loopback_or_the_own_origin_only() {
    let root = object_id(Kind::Blob, b"root-tree");
    // No opt-in anywhere: a non-loopback plain-http tier refuses.
    let client = Client::new("http://127.0.0.1:7400", None);
    let mut cache = CacheRoute::from_base("http://naatre-cache.lan:7500");
    assert!(cache.is_some(), "the route exists; the refusal is at fetch");
    let err = fetch_blobs_verified(&client, "r", &root, &[], &mut cache)
        .expect_err("a non-admitted tier refuses");
    assert!(matches!(err, FetchError::Usage(_)), "{err:?}");
    // A tier URL that fails the one parse refuses the same way.
    let mut cache = CacheRoute::from_base("http://user@tier.lan/x");
    let err = fetch_blobs_verified(&client, "r", &root, &[], &mut cache)
        .expect_err("an unparseable tier URL refuses");
    assert!(matches!(err, FetchError::Usage(_)), "{err:?}");

    // With the per-remote opt-in: a working copy whose config names BOTH
    // the server and the opted-in origin (the verbs' own resolution).
    let dir = tempfile::tempdir().expect("tempdir");
    std::fs::create_dir_all(dir.path().join(".naatre")).expect(".naatre");
    let wc = Wc::open(dir.path()).expect("wc");
    wc.save_config(&naatre_cli::wc::Config {
        server_url: "http://naatre.lan:7400".into(),
        token: None,
        insecure_http: Some("http://naatre.lan:7400".into()),
        ca_file: None,
    })
    .expect("config");
    let net = Net::default();
    let client = wc.client(&net).expect("the stored origin admits itself");
    assert_eq!(client.base, "http://naatre.lan:7400");

    // The SAME origin as a tier: admitted (empty id list — no requests).
    let mut cache = CacheRoute::from_base("http://naatre.lan:7400");
    assert!(
        fetch_blobs_verified(&client, "r", &root, &[], &mut cache).is_ok(),
        "the opted-in origin's own tier is admitted"
    );
    // A DIFFERENT cache host: refused — the origin's opt-in never covers
    // another cache host.
    let mut cache = CacheRoute::from_base("http://other-cache.lan:7500");
    let err = fetch_blobs_verified(&client, "r", &root, &[], &mut cache)
        .expect_err("a different cache host refuses");
    assert!(matches!(err, FetchError::Usage(_)), "{err:?}");
    // A loopback tier needs no opt-in at all.
    let mut cache = CacheRoute::from_base("http://127.0.0.1:7500");
    assert!(fetch_blobs_verified(&client, "r", &root, &[], &mut cache).is_ok());
}
