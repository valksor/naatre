//! The Postgres TLS stack (F1): one rustls setup per [`crate::backend::PgTls`]
//! mode for the sync `postgres` driver, via `tokio-postgres-rustls`'s
//! [`MakeRustlsConnect`] (a `MakeTlsConnect<Socket>`, which
//! `postgres::Config::connect` accepts). This replaces native-tls so the
//! whole graph runs on ONE TLS stack — rustls + ring — and OpenSSL leaves
//! `Cargo.lock` entirely (deny.toml `[bans]` keeps it out).
//!
//! Both `ring` and `aws-lc-rs` sit in the graph (aws-lc-rs rides in with
//! object_store's S3 feature), so a process-default crypto provider would
//! be ambiguous — rustls refuses to guess between them. Every config here
//! is therefore built with the ring provider EXPLICITLY; none of the
//! builder calls touches a process default.
//!
//! sslmode semantics, carried over verbatim from the native-tls builder
//! they replace:
//!
//! * `require`: encryption, server certificates NOT verified — but the
//!   handshake signatures still are, against the ring provider's
//!   signature algorithms (a verify-nothing verifier would let an active
//!   attacker hijack the key exchange itself).
//! * `verify-ca`: the chain is verified against the roots, the server
//!   NAME is not. Only the two name-mismatch errors are swallowed
//!   ([`CertificateError::NotValidForName`] and
//!   `NotValidForNameContext`); unknown issuer and every other webpki
//!   failure still fails — each pinned in `tests/pg_tls_test.rs`.
//! * `verify-full`: the plain webpki verifier the config builder builds
//!   itself — chain AND name, provider-bound via `builder_with_provider`.
//!
//! The root set for `verify-ca`/`verify-full` is the platform roots
//! (rustls-native-certs, loaded ONCE per process — [`platform_roots`])
//! PLUS the optional `sslrootcert` bundle — the file is added on top,
//! never replaces them. `require` loads NO roots at all: its verifier
//! accepts any certificate, so the platform loader is not even called
//! there. The platform load is best-effort (one odd keychain entry
//! never failed a connect under native-tls either); the `sslrootcert`
//! file is NOT best-effort — its 0600 (unix) and PEM-parse guards run
//! for every TLS mode, exactly as before, and fail as
//! [`DbError::SslRootCert`], never as driver errors. And a verify mode
//! left with an EMPTY combined root set fails at construction, as the
//! named [`DbError::TlsConfig`] ([`NO_TRUST_ROOTS`]) — never as the
//! redacted connect error.

use std::path::Path;
use std::sync::{Arc, OnceLock};

use rustls::client::WebPkiServerVerifier;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::crypto::{
    CryptoProvider, WebPkiSupportedAlgorithms, verify_tls12_signature, verify_tls13_signature,
};
use rustls::pki_types::{CertificateDer, ServerName, UnixTime, pem::PemObject};
use rustls::{
    CertificateError, ClientConfig, DigitallySignedStruct, Error as TlsError, RootCertStore,
    SignatureScheme,
};
use tokio_postgres_rustls::MakeRustlsConnect;

use crate::backend::PgTls;
use crate::meta::{DbError, PgErrKind};

/// The zero-roots error text, shared with the pin asserting it verbatim
/// in `tests/pg_tls_test.rs`.
pub const NO_TRUST_ROOTS: &str =
    "no trust roots: set sslrootcert or install system CA certificates";

/// Builds the TLS connector for one connect: the per-sslmode verifier,
/// the root set, and the ring provider — explicit, never a process
/// default. `sslrootcert` guards run for every TLS mode, exactly as they
/// did under native-tls.
pub fn make_rustls_connect(
    tls: PgTls,
    sslrootcert: Option<&Path>,
) -> Result<MakeRustlsConnect, DbError> {
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    // `with_safe_default_protocol_versions` = TLS 1.2 + 1.3; its error
    // (a provider without any default version) is connect-class like the
    // driver's own failures.
    let config = match tls {
        // The connect path uses `postgres::NoTls` for Disable; this arm
        // is defense for a future caller — a named local error, never a
        // panic.
        PgTls::Disable => {
            return Err(DbError::TlsConfig(
                "sslmode=disable makes no TLS connector; the connect \
                 path uses NoTls for Disable"
                    .into(),
            ));
        }
        // `require` needs NO roots: its verifier accepts any
        // certificate, so the platform loader is not even called. The
        // `sslrootcert` file's guards still run — they are per-config
        // errors in every mode.
        PgTls::Require => {
            sslrootcert_guards(sslrootcert)?;
            ClientConfig::builder_with_provider(provider.clone())
                .with_safe_default_protocol_versions()
                .map_err(|_| connect_error())?
                .dangerous()
                .with_custom_certificate_verifier(require_verifier(provider.clone()))
                .with_no_client_auth()
        }
        PgTls::VerifyCa => {
            let roots = root_store(sslrootcert)?;
            ClientConfig::builder_with_provider(provider.clone())
                .with_safe_default_protocol_versions()
                .map_err(|_| connect_error())?
                .dangerous()
                .with_custom_certificate_verifier(verify_ca_verifier(roots, provider)?)
                .with_no_client_auth()
        }
        // The one mode on the PLAIN builder path: chain + name via the
        // builder's own webpki verifier, bound to the provider above.
        PgTls::VerifyFull => {
            let roots = root_store(sslrootcert)?;
            ClientConfig::builder_with_provider(provider.clone())
                .with_safe_default_protocol_versions()
                .map_err(|_| connect_error())?
                .with_root_certificates(roots)
                .with_no_client_auth()
        }
    };
    Ok(MakeRustlsConnect::new(config))
}

/// `require`'s verifier: accepts ANY server certificate, while handshake
/// signatures still verify against the ring provider's signature
/// algorithms ([`AcceptAnyCert::algs`]).
pub fn require_verifier(provider: Arc<CryptoProvider>) -> Arc<dyn ServerCertVerifier> {
    Arc::new(AcceptAnyCert {
        algs: provider.signature_verification_algorithms,
    })
}

/// `verify-ca`'s verifier: the webpki verifier over `roots` with ONLY the
/// two server-name mismatch errors swallowed. `UnknownIssuer` and every
/// other [`CertificateError`] propagate unchanged (pinned in
/// `tests/pg_tls_test.rs`).
pub fn verify_ca_verifier(
    roots: RootCertStore,
    provider: Arc<CryptoProvider>,
) -> Result<Arc<dyn ServerCertVerifier>, DbError> {
    let webpki = WebPkiServerVerifier::builder_with_provider(Arc::new(roots), provider)
        .build()
        .map_err(|_| connect_error())?;
    Ok(Arc::new(IgnoreServerName(webpki)))
}

/// `verify-full`'s verifier: the plain webpki verifier over `roots` —
/// chain and hostname, nothing swallowed. The connect path uses the
/// config builder's identical internal one; this constructor exists so
/// the pins can call the verifier object directly.
pub fn verify_full_verifier(
    roots: RootCertStore,
    provider: Arc<CryptoProvider>,
) -> Result<Arc<dyn ServerCertVerifier>, DbError> {
    WebPkiServerVerifier::builder_with_provider(Arc::new(roots), provider)
        .build()
        .map_err(|_| connect_error())
        .map(|v| v as Arc<dyn ServerCertVerifier>)
}

/// The root set for `verify-ca`/`verify-full`: the platform roots (the
/// once-per-process cache of [`platform_roots`]) plus the `sslrootcert`
/// bundle — the file is added on top, never replaces them. The platform
/// load is best-effort, as the native-tls system store was; an EMPTY
/// combined set is refused HERE as the named [`DbError::TlsConfig`] — a
/// verifier over zero anchors could only fail at the first handshake,
/// and there as a redacted connect error nobody could act on.
fn root_store(sslrootcert: Option<&Path>) -> Result<RootCertStore, DbError> {
    let mut roots = platform_roots().clone();
    if let Some(path) = sslrootcert {
        for cert in sslrootcert_certs(path)? {
            roots
                .add(cert)
                .map_err(|e| DbError::SslRootCert(format!("{}: {e}", path.display())))?;
        }
    }
    if roots.is_empty() {
        return Err(DbError::TlsConfig(NO_TRUST_ROOTS.into()));
    }
    Ok(roots)
}

/// The platform roots, loaded ONCE per process. The cache holds whatever
/// the platform load returned — an empty store stays an empty store (an
/// empty-string `SSL_CERT_FILE` makes `rustls-native-certs` yield zero
/// roots), nothing here assumes success. The load itself is best-effort,
/// as the native-tls system store always was.
fn platform_roots() -> &'static RootCertStore {
    static PLATFORM: OnceLock<RootCertStore> = OnceLock::new();
    PLATFORM.get_or_init(|| {
        let mut roots = RootCertStore::empty();
        let native = rustls_native_certs::load_native_certs();
        roots.add_parsable_certificates(native.certs);
        roots
    })
}

/// The `require` path's root handling: it needs NO roots — the platform
/// loader is not even called — but the `sslrootcert` file's guards still
/// run, exactly as in the verifying modes (per-config errors in both).
fn sslrootcert_guards(sslrootcert: Option<&Path>) -> Result<(), DbError> {
    if let Some(path) = sslrootcert {
        sslrootcert_certs(path)?;
    }
    Ok(())
}

/// The `sslrootcert` file, exactly as the connect path has always
/// treated it: 0600 (unix), readable, PEM-parseable — anything else is
/// the named [`DbError::SslRootCert`] ("sslrootcert: …"), never a
/// driver error. Every certificate in the file becomes a root.
fn sslrootcert_certs(path: &Path) -> Result<Vec<CertificateDer<'static>>, DbError> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(path)
            .map_err(|e| DbError::SslRootCert(format!("{}: {e}", path.display())))?
            .permissions()
            .mode();
        if mode & 0o177 != 0 {
            return Err(DbError::SslRootCert(format!(
                "{} must be 0600 (found mode {:o}); chmod 600 it",
                path.display(),
                mode & 0o777
            )));
        }
    }
    let pem = std::fs::read(path)
        .map_err(|e| DbError::SslRootCert(format!("{}: {e}", path.display())))?;
    let certs = CertificateDer::pem_slice_iter(&pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| DbError::SslRootCert(format!("{}: {e}", path.display())))?;
    // native-tls rejected a file with NO certificates in it; a PEM
    // scanner over garbage just finds no sections, so pin the refusal.
    if certs.is_empty() {
        return Err(DbError::SslRootCert(format!(
            "{}: no certificates found",
            path.display()
        )));
    }
    Ok(certs)
}

/// TLS setup that is not tied to a local file — driver-class, redacted
/// like every other connect failure.
fn connect_error() -> DbError {
    DbError::Postgres {
        kind: PgErrKind::Connect,
        state: None,
    }
}

/// `require`'s verifier: any certificate, authenticated handshake.
#[derive(Debug)]
struct AcceptAnyCert {
    /// The ring provider's signature algorithms (Copy — all static
    /// refs); the sig checks below delegate to them.
    algs: WebPkiSupportedAlgorithms,
}

impl ServerCertVerifier for AcceptAnyCert {
    fn verify_server_cert(
        &self,
        _end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, TlsError> {
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        verify_tls12_signature(message, cert, dss, &self.algs)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        verify_tls13_signature(message, cert, dss, &self.algs)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.algs.supported_schemes()
    }
}

/// `verify-ca`'s verifier: webpki, minus server-name checking.
#[derive(Debug)]
struct IgnoreServerName(Arc<WebPkiServerVerifier>);

impl ServerCertVerifier for IgnoreServerName {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, TlsError> {
        match self
            .0
            .verify_server_cert(end_entity, intermediates, server_name, ocsp_response, now)
        {
            // The ONLY two errors `verify-ca` ignores: the name does not
            // match, plain or with the (expected vs presented) context
            // rustls 0.23 always carries. Nothing else — not
            // UnknownIssuer, not any other InvalidCertificate variant.
            Err(TlsError::InvalidCertificate(CertificateError::NotValidForName))
            | Err(TlsError::InvalidCertificate(CertificateError::NotValidForNameContext {
                ..
            })) => Ok(ServerCertVerified::assertion()),
            other => other,
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        self.0.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        self.0.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.0.supported_verify_schemes()
    }
}
