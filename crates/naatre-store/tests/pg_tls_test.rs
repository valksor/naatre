//! The Postgres TLS pins (F1), readable top to bottom: rcgen builds a
//! CA and server certs inside the test, and the verifier OBJECTS are
//! called directly — no network, no live Postgres, every verdict below
//! is the verifier's own decision.
//!
//! 1. `verify-full` refuses a wrong-hostname certificate (trusted CA).
//! 2. `verify-full` refuses an untrusted-CA certificate.
//! 3. `verify-ca` accepts a wrong-hostname certificate (trusted CA).
//! 4. `verify-ca` refuses an untrusted-CA certificate — UnknownIssuer,
//!    exactly.
//! 5. `verify-ca` refuses an EXPIRED leaf signed by the trusted CA —
//!    a validity error, not a swallowed name mismatch.
//! 6. `verify-ca` refuses a NOT-YET-VALID leaf signed by the trusted CA
//!    — a validity error, not a swallowed name mismatch.
//! 7. `require` accepts all of the above.
//! 8. `sslmode=disable` builds no connector — the named `TlsConfig`
//!    error, never a panic.
//! 9. The full connector builds at runtime for all three TLS modes, and
//!    with no usable roots anywhere the verify modes fail with the named
//!    "no trust roots" error — ring provider throughout, explicit,
//!    never a process default.
//! 10. The `sslrootcert` bundle ANCHORS the chain: roots loaded from
//!     the file verify a leaf the file's CA signed — the test fails if
//!     the bundle carries a leaf instead of the CA.
//! 11. The `sslrootcert` guards: a 0644 file is refused, a 0600 file
//!     with garbage bytes is refused — both `DbError::SslRootCert`.

use std::path::Path;
use std::sync::Arc;

use naatre_store::backend::PgTls;
use naatre_store::meta::DbError;
use naatre_store::pg_tls;
use rcgen::{BasicConstraints, CertificateParams, DnType, IsCa, Issuer, KeyPair, KeyUsagePurpose};
use rustls::client::danger::{ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, ServerName, UnixTime};
use rustls::{CertificateError, Error as TlsError, RootCertStore};

/// A throwaway CA: its [`Issuer`] signs the test leaves, and its DER is
/// the trust anchor the verifiers are built over. `name` must be UNIQUE
/// per CA — webpki picks candidate issuers by subject name first, so
/// same-named CAs fail signature checks instead of name matching.
struct TestCa {
    issuer: Issuer<'static, KeyPair>,
    cert_der: CertificateDer<'static>,
    /// The CA's own PEM — what an `sslrootcert` bundle must carry.
    pem: String,
}

fn trusted_ca(name: &str) -> TestCa {
    let key = KeyPair::generate().expect("CA keygen");
    let mut params = CertificateParams::new(Vec::<String>::new()).expect("CA params");
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params.key_usages = vec![KeyUsagePurpose::KeyCertSign];
    params.distinguished_name.push(DnType::CommonName, name);
    let cert = params.self_signed(&key).expect("self-signed CA");
    let cert_der = cert.der().clone();
    let pem = cert.pem();
    TestCa {
        issuer: Issuer::new(params, key),
        cert_der,
        pem,
    }
}

/// The leaf's identity: `sans` as its subject alternative names, with
/// rcgen's default validity window.
fn leaf_params(sans: &[&str]) -> CertificateParams {
    CertificateParams::new(sans.iter().map(|s| s.to_string()).collect::<Vec<_>>())
        .expect("leaf params")
}

fn sign_leaf(params: CertificateParams, ca: &TestCa) -> CertificateDer<'static> {
    let key = KeyPair::generate().expect("leaf keygen");
    params
        .signed_by(&key, &ca.issuer)
        .expect("leaf signed")
        .der()
        .clone()
}

/// A leaf certificate with `sans` as its subject alternative names.
fn leaf_cert(ca: &TestCa, sans: &[&str]) -> CertificateDer<'static> {
    sign_leaf(leaf_params(sans), ca)
}

/// A leaf with an explicit validity window, as UTC calendar dates (via
/// [`rcgen::date_time_ymd`] — keeps the `time` crate out of the
/// dev-deps). The windows are absolute dates on purpose: the expired
/// pin ends well in the past, the not-yet-valid pin starts well past
/// this century's midpoint.
fn leaf_cert_valid(
    ca: &TestCa,
    sans: &[&str],
    not_before: (i32, u8, u8),
    not_after: (i32, u8, u8),
) -> CertificateDer<'static> {
    let mut params = leaf_params(sans);
    params.not_before = rcgen::date_time_ymd(not_before.0, not_before.1, not_before.2);
    params.not_after = rcgen::date_time_ymd(not_after.0, not_after.1, not_after.2);
    sign_leaf(params, ca)
}

/// A leaf that expired in 2020, signed by the TRUSTED CA.
fn expired_leaf(ca: &TestCa, sans: &[&str]) -> CertificateDer<'static> {
    leaf_cert_valid(ca, sans, (1998, 1, 1), (2020, 1, 1))
}

/// A leaf that is not valid until 2099, signed by the TRUSTED CA.
fn not_yet_valid_leaf(ca: &TestCa, sans: &[&str]) -> CertificateDer<'static> {
    leaf_cert_valid(ca, sans, (2099, 1, 1), (2120, 1, 1))
}

fn roots_with(ca: &TestCa) -> RootCertStore {
    let mut roots = RootCertStore::empty();
    roots.add(ca.cert_der.clone()).expect("CA anchors");
    roots
}

fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// The verifier's verdict for `cert` presented for `name`.
fn verify(
    verifier: &dyn ServerCertVerifier,
    cert: &CertificateDer<'static>,
    name: &str,
) -> Result<ServerCertVerified, TlsError> {
    let name = ServerName::try_from(name.to_owned()).expect("test name parses");
    verifier.verify_server_cert(cert, &[], &name, &[], UnixTime::now())
}

/// The two name-mismatch shapes (rustls 0.23 surfaces the webpki
/// context variant in practice; the plain one exists in the enum).
fn is_name_mismatch(err: &TlsError) -> bool {
    matches!(
        err,
        TlsError::InvalidCertificate(CertificateError::NotValidForName)
            | TlsError::InvalidCertificate(CertificateError::NotValidForNameContext { .. })
    )
}

#[test]
fn verify_full_refuses_wrong_hostname() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let cert = leaf_cert(&ca, &["wrong.example.com"]);
    let verifier = pg_tls::verify_full_verifier(roots_with(&ca), provider())
        .expect("verify-full verifier builds");
    match verify(verifier.as_ref(), &cert, "localhost") {
        Err(err) if is_name_mismatch(&err) => {}
        other => panic!("expected a name mismatch, got {other:?}"),
    }
}

#[test]
fn verify_full_refuses_untrusted_ca() {
    let trusted = trusted_ca("naatre pg_tls trusted CA");
    let untrusted = trusted_ca("naatre pg_tls untrusted CA");
    let cert = leaf_cert(&untrusted, &["localhost"]);
    let verifier = pg_tls::verify_full_verifier(roots_with(&trusted), provider())
        .expect("verify-full verifier builds");
    match verify(verifier.as_ref(), &cert, "localhost") {
        Err(TlsError::InvalidCertificate(CertificateError::UnknownIssuer)) => {}
        other => panic!("expected UnknownIssuer, got {other:?}"),
    }
}

#[test]
fn verify_ca_accepts_wrong_hostname() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let cert = leaf_cert(&ca, &["wrong.example.com"]);
    let verifier =
        pg_tls::verify_ca_verifier(roots_with(&ca), provider()).expect("verify-ca verifier builds");
    verify(verifier.as_ref(), &cert, "localhost")
        .expect("verify-ca checks the chain, not the name");
}

#[test]
fn verify_ca_refuses_untrusted_ca() {
    let trusted = trusted_ca("naatre pg_tls trusted CA");
    let untrusted = trusted_ca("naatre pg_tls untrusted CA");
    let cert = leaf_cert(&untrusted, &["localhost"]);
    let verifier = pg_tls::verify_ca_verifier(roots_with(&trusted), provider())
        .expect("verify-ca verifier builds");
    match verify(verifier.as_ref(), &cert, "localhost") {
        Err(TlsError::InvalidCertificate(CertificateError::UnknownIssuer)) => {}
        other => {
            panic!("expected UnknownIssuer (name skipping must not swallow it), got {other:?}")
        }
    }
}

#[test]
fn verify_ca_refuses_expired_leaf() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let cert = expired_leaf(&ca, &["localhost"]);
    let verifier =
        pg_tls::verify_ca_verifier(roots_with(&ca), provider()).expect("verify-ca verifier builds");
    match verify(verifier.as_ref(), &cert, "localhost") {
        Err(TlsError::InvalidCertificate(
            CertificateError::Expired | CertificateError::ExpiredContext { .. },
        )) => {}
        Err(err) if is_name_mismatch(&err) => panic!(
            "an expired leaf must fail as a validity error, not a swallowed name mismatch: {err:?}"
        ),
        other => panic!("expected Expired for the expired leaf, got {other:?}"),
    }
}

#[test]
fn verify_ca_refuses_not_yet_valid_leaf() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let cert = not_yet_valid_leaf(&ca, &["localhost"]);
    let verifier =
        pg_tls::verify_ca_verifier(roots_with(&ca), provider()).expect("verify-ca verifier builds");
    match verify(verifier.as_ref(), &cert, "localhost") {
        Err(TlsError::InvalidCertificate(
            CertificateError::NotValidYet | CertificateError::NotValidYetContext { .. },
        )) => {}
        Err(err) if is_name_mismatch(&err) => panic!(
            "a not-yet-valid leaf must fail as a validity error, not a swallowed name \
             mismatch: {err:?}"
        ),
        other => panic!("expected NotValidYet for the not-yet-valid leaf, got {other:?}"),
    }
}

#[test]
fn require_accepts_anything() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let wrong_host = leaf_cert(&ca, &["wrong.example.com"]);
    let untrusted = trusted_ca("naatre pg_tls untrusted CA");
    let rogue = leaf_cert(&untrusted, &["localhost"]);
    let verifier = pg_tls::require_verifier(provider());
    verify(verifier.as_ref(), &wrong_host, "localhost").expect("require does not check the name");
    verify(verifier.as_ref(), &rogue, "localhost")
        .expect("require does not check the chain either");
}

#[test]
fn disable_builds_no_connector() {
    match pg_tls::make_rustls_connect(PgTls::Disable, None).map(|_| ()) {
        Err(DbError::TlsConfig(why)) => {
            assert!(why.contains("sslmode=disable"), "names the misuse: {why}");
        }
        other => panic!("expected DbError::TlsConfig for Disable, got {other:?}"),
    }
}

#[test]
fn connector_builds_for_every_tls_mode() {
    // Without `sslrootcert` the root store is the platform's. The verify
    // modes refuse an EMPTY combined root set — the named "no trust
    // roots" error, before any handshake — so with no sslrootcert their
    // outcome depends on whether the platform store yielded anchors at
    // all; `require` needs no roots and always constructs.
    let platform_has_roots = !rustls_native_certs::load_native_certs().certs.is_empty();
    pg_tls::make_rustls_connect(PgTls::Require, None)
        .unwrap_or_else(|e| panic!("connector builds for Require: {e:?}"));
    for mode in [PgTls::VerifyCa, PgTls::VerifyFull] {
        match pg_tls::make_rustls_connect(mode, None).map(|_| ()) {
            Ok(()) if platform_has_roots => {}
            Err(DbError::TlsConfig(why)) if !platform_has_roots => {
                assert_eq!(why, pg_tls::NO_TRUST_ROOTS);
            }
            other => panic!(
                "{mode:?} connector outcome does not match the platform store \
                 (has roots: {platform_has_roots}): {other:?}"
            ),
        }
    }
}

#[test]
fn sslrootcert_bundle_anchors_the_chain() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let dir = tempfile::tempdir().expect("temp dir");
    let path = dir.path().join("ca.pem");
    std::fs::write(&path, ca_pem(&ca)).expect("write CA PEM");
    chmod_0600(&path);
    for mode in [PgTls::Require, PgTls::VerifyCa, PgTls::VerifyFull] {
        pg_tls::make_rustls_connect(mode, Some(&path))
            .unwrap_or_else(|e| panic!("connector builds for {mode:?} with sslrootcert: {e:?}"));
    }
    // The bundle ANCHORS the chain: roots loaded from the file verify a
    // leaf the file's CA signed, name included. A leaf in the bundle
    // instead would fail HERE — a leaf is no issuer, so the chain would
    // not build; this is what keeps the helper honest.
    let pem = std::fs::read(&path).expect("read the bundle");
    let mut roots = RootCertStore::empty();
    for cert in CertificateDer::pem_slice_iter(&pem) {
        roots
            .add(cert.expect("bundle PEM parses"))
            .expect("bundle cert anchors");
    }
    let verifier = pg_tls::verify_full_verifier(roots, provider()).expect("verifier builds");
    let leaf = leaf_cert(&ca, &["localhost"]);
    verify(verifier.as_ref(), &leaf, "localhost").expect("the bundle's CA anchors the leaf");
}

#[test]
fn sslrootcert_guards() {
    let ca = trusted_ca("naatre pg_tls trusted CA");
    let dir = tempfile::tempdir().expect("temp dir");

    // 0644, valid PEM: refused — the file must be 0600.
    let loose = dir.path().join("loose.pem");
    std::fs::write(&loose, ca_pem(&ca)).expect("write CA PEM");
    chmod_0644(&loose);
    match pg_tls::make_rustls_connect(PgTls::VerifyFull, Some(&loose)).map(|_| ()) {
        Err(DbError::SslRootCert(_)) => {}
        other => panic!("expected DbError::SslRootCert for a 0644 file, got {other:?}"),
    }

    // 0600, garbage bytes: refused — not PEM at all.
    let garbage = dir.path().join("garbage.pem");
    std::fs::write(&garbage, b"definitely not a PEM file").expect("write garbage");
    chmod_0600(&garbage);
    match pg_tls::make_rustls_connect(PgTls::VerifyFull, Some(&garbage)).map(|_| ()) {
        Err(DbError::SslRootCert(_)) => {}
        other => panic!("expected DbError::SslRootCert for garbage bytes, got {other:?}"),
    }
}

/// The CA's own PEM — what an `sslrootcert` bundle must carry: the trust
/// anchor, not a leaf. (The old `rcgen_pem` minted a fresh leaf, so the
/// bundle test proved nothing about the file's contents.)
fn ca_pem(ca: &TestCa) -> String {
    ca.pem.clone()
}

#[cfg(unix)]
fn chmod_0600(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600)).expect("chmod 600");
}

#[cfg(unix)]
fn chmod_0644(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o644)).expect("chmod 644");
}

#[cfg(not(unix))]
fn chmod_0600(_path: &Path) {}
#[cfg(not(unix))]
fn chmod_0644(_path: &Path) {}
