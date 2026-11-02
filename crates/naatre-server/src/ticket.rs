//! Origin-signed cache tickets (M13b): the Ed25519 credential pair the
//! origin mints and the `naatre cache` read tier verifies BEFORE any byte.
//!
//! Wire form: `<b64url(payload)>.<b64url(signature)>.<key-id>` presented as
//! the `NaatreTicket` authorization scheme. The payload is a versioned JSON
//! object (field order is the declaration order — serde_json is
//! deterministic for structs): protocol version, a 32-hex ticket nonce
//! (`tid`, CSPRNG), the bound principal, repo, and root-tree id, and the
//! issue/expiry stamps in ns since the epoch. The signature covers a
//! domain-separated prefix (`naatre-ticket-v1\n`) plus the payload bytes;
//! the trailing key-id names WHICH verify key checks the signature — key
//! rotation is the origin minting under a new key while retired keys keep
//! verifying their still-unexpired tickets (the ≤15-min staleness bound:
//! a ticket minted before a repo goes private cannot outlive it).
//!
//! Both directions of the lifetime cap are enforced: the mint refuses a
//! `ttl_secs` above [`TICKET_MAX_LIFETIME_SECS`], and verification refuses
//! a payload whose `exp - iat` exceeds the cap even under a valid origin
//! signature (the tier's defense against a misconfigured or compromised
//! origin minting long-lived tickets).
//!
//! Key material lives in the ORIGIN's data dir (the 0700 trust boundary,
//! the same boundary as the token store): `cache_signing.pkcs8` (the DER
//! PKCS#8 secret) beside `cache_keys.json` (the PUBLIC verify list —
//! current plus retired, each with its id). The verify list is the file
//! the cache operator copies to the tier (`naatre cache --key …`): the
//! cache never sees secret material, and its startup is fail-closed
//! without at least one verify key. Key ids are short BLAKE3 prefixes of
//! the public key — stable, self-describing on the wire, never secret.
//!
//! Crypto note: Ed25519 comes from `ring::signature` — ring is already in
//! the dependency graph (rustls/quinn-proto via naatre-store's S3
//! backend), so the M13b decision adds zero new crates to Cargo.lock.

use std::path::Path;

use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::signature::KeyPair as _;
use serde::{Deserialize, Serialize};

use naatre_core::ids::Id;

/// The ticket lifetime cap (the plan's "≤15 min"): enforced at mint AND at
/// verify. This cap is what bounds ACL-change staleness through the tier.
pub const TICKET_MAX_LIFETIME_SECS: u64 = 900;
/// The mint's default lifetime (a client that does not care gets the
/// shorter of the two useful values; the cap stays available explicitly).
pub const TICKET_DEFAULT_LIFETIME_SECS: u64 = 600;

/// The authorization scheme carrying a ticket (alongside the existing
/// `Bearer` scheme; the auth middleware parses both).
pub const TICKET_SCHEME: &str = "NaatreTicket";

/// Domain separator signed ahead of every payload: a ticket signature is
/// never valid for any other naatre artifact.
const SIGNING_DOMAIN: &[u8] = b"naatre-ticket-v1\n";

/// The verified (or freshly minted) ticket payload.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct TicketClaims {
    /// Payload protocol version (only 1 verifies).
    pub v: u32,
    /// Ticket nonce, 32 lowercase hex (CSPRNG at mint; replay tracking is
    /// the TTL's job, the nonce makes tickets distinguishable in logs).
    pub tid: String,
    /// The bound principal (who the ticket reads as).
    pub principal: String,
    /// The bound repo name.
    pub repo: String,
    /// The bound root-tree id, 64 lowercase hex — the membership walk's
    /// scope root.
    pub root_tree: String,
    /// Issue time, ns since the epoch.
    pub iat: u64,
    /// Expiry time, ns since the epoch (`iat + lifetime`, lifetime ≤ cap).
    pub exp: u64,
}

/// Why a ticket failed verification. Callers map `Expired` to the 401
/// `token_expired` envelope and everything else to 401 `token_invalid`
/// (a ticket is a credential; the wire codes are the credential codes).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TicketError {
    /// Structurally malformed, badly signed, unknown key-id, wrong
    /// version, or a lifetime above the cap.
    Invalid,
    /// Valid signature, past its expiry.
    Expired,
}

/// Signs `claims` with the origin's PKCS#8 secret. Panics only on a
/// malformed configured key — that is a startup/config error surfaced at
/// load, never here (callers load via [`load_signing_key`]).
pub fn mint_ticket(claims: &TicketClaims, signing_key: &ring::signature::Ed25519KeyPair) -> String {
    let payload = serde_json::to_vec(claims).expect("ticket claims serialize");
    let mut signed = Vec::with_capacity(SIGNING_DOMAIN.len() + payload.len());
    signed.extend_from_slice(SIGNING_DOMAIN);
    signed.extend_from_slice(&payload);
    let sig = signing_key.sign(&signed);
    format!(
        "{}.{}.{}",
        URL_SAFE_NO_PAD.encode(&payload),
        URL_SAFE_NO_PAD.encode(sig.as_ref()),
        key_id(signing_key.public_key().as_ref())
    )
}

/// Verifies a ticket against the verify-key list at time `now_ns`:
/// signature (by key-id), payload shape, the lifetime cap, then expiry.
pub fn verify_ticket(
    ticket: &str,
    keys: &VerifyKeys,
    now_ns: u64,
) -> Result<TicketClaims, TicketError> {
    let (payload_b64, sig_b64, key_id) = ticket
        .split_once('.')
        .and_then(|(a, rest)| rest.split_once('.').map(|(b, c)| (a, b, c)))
        .ok_or(TicketError::Invalid)?;
    let key = keys
        .0
        .iter()
        .find(|k| k.id == key_id)
        .ok_or(TicketError::Invalid)?;
    let payload = URL_SAFE_NO_PAD
        .decode(payload_b64)
        .map_err(|_| TicketError::Invalid)?;
    let sig = URL_SAFE_NO_PAD
        .decode(sig_b64)
        .map_err(|_| TicketError::Invalid)?;
    let mut signed = Vec::with_capacity(SIGNING_DOMAIN.len() + payload.len());
    signed.extend_from_slice(SIGNING_DOMAIN);
    signed.extend_from_slice(&payload);
    ring::signature::UnparsedPublicKey::new(&ring::signature::ED25519, &key.public)
        .verify(&signed, &sig)
        .map_err(|_| TicketError::Invalid)?;
    let claims: TicketClaims =
        serde_json::from_slice(&payload).map_err(|_| TicketError::Invalid)?;
    if claims.v != 1 {
        return Err(TicketError::Invalid);
    }
    if Id::from_hex(&claims.root_tree).is_none() {
        return Err(TicketError::Invalid);
    }
    // The lifetime cap at verify: exp - iat over the cap is refused even
    // under a valid signature (saturating so a bogus exp below iat lands
    // here as a zero-length "lifetime", then the expiry check decides).
    if claims.exp.saturating_sub(claims.iat) > TICKET_MAX_LIFETIME_SECS * 1_000_000_000 {
        return Err(TicketError::Invalid);
    }
    // The future-iat refusal: a signed ticket minted "years from now"
    // would otherwise verify for YEARS (its exp is then far ahead of
    // now), defeating the verify-side cap's stated purpose — the defense
    // against a misconfigured or compromised origin. A small skew
    // tolerance accommodates clock drift between mint and verify hosts;
    // anything beyond it is refused as invalid.
    const IAT_SKEW_SECS: u64 = 30;
    if claims.iat > now_ns.saturating_add(IAT_SKEW_SECS * 1_000_000_000) {
        return Err(TicketError::Invalid);
    }
    // The cap's second arm at verify: even a WELL-STAMPED ticket (iat in
    // the past, exp - iat within the cap by the mint's own clock) cannot
    // buy more than the cap of validity from THIS verifier's now — an
    // origin with a skewed-forward mint clock cannot mint
    // longer-lived-than-cap tickets.
    if claims.exp > now_ns.saturating_add(TICKET_MAX_LIFETIME_SECS * 1_000_000_000) {
        return Err(TicketError::Invalid);
    }
    if now_ns >= claims.exp {
        return Err(TicketError::Expired);
    }
    Ok(claims)
}

/// The stable key id: the first 8 hex chars of the public key's BLAKE3.
pub fn key_id(public_key: &[u8]) -> String {
    let hash = *blake3::hash(public_key).as_bytes();
    let hex: String = hash.iter().take(4).map(|b| format!("{b:02x}")).collect();
    hex
}

/// One public verify key: its id, raw 32-byte Ed25519 public key, and
/// whether it is the origin's CURRENT signing key (retired keys carry
/// `current = false` and keep verifying their unexpired tickets).
#[derive(Clone, Debug)]
pub struct VerifyKey {
    pub id: String,
    pub public: Vec<u8>,
    pub current: bool,
}

/// The verify-key list — the cache's whole trust anchor. Startup is
/// fail-closed without at least one key.
#[derive(Clone, Debug)]
pub struct VerifyKeys(Vec<VerifyKey>);

impl VerifyKeys {
    /// Parses the `cache_keys.json` shape: `{"keys":[{"id","public"
    /// (64 lowercase hex),"current"}]}`. An empty list, a bad key id, a
    /// non-hex public, or a duplicate id is a parse error — the cache
    /// refuses to start on any of them (fail-closed).
    pub fn parse(json: &str) -> Result<VerifyKeys, String> {
        #[derive(serde::Deserialize)]
        struct RawKey {
            id: String,
            public: String,
            #[serde(default)]
            current: bool,
        }
        #[derive(serde::Deserialize)]
        struct Raw {
            keys: Vec<RawKey>,
        }
        let raw: Raw = serde_json::from_str(json).map_err(|e| format!("bad key file: {e}"))?;
        let mut keys = Vec::new();
        for k in raw.keys {
            if k.id != key_id_from_hex_checked(&k.public)? {
                return Err(format!(
                    "key id {} does not match its public key's BLAKE3 prefix",
                    k.id
                ));
            }
            if keys.iter().any(|existing: &VerifyKey| existing.id == k.id) {
                return Err(format!(
                    "duplicate key id {} in the verify list (a key id names ONE key)",
                    k.id
                ));
            }
            keys.push(VerifyKey {
                id: k.id,
                public: raw_public(&k.public)?,
                current: k.current,
            });
        }
        if keys.is_empty() {
            return Err("no verify keys in the key file (fail-closed startup)".into());
        }
        Ok(VerifyKeys(keys))
    }

    pub fn keys(&self) -> &[VerifyKey] {
        &self.0
    }

    /// Renders the file shape (the origin's writer; the cache only parses).
    pub fn to_json(&self) -> String {
        #[derive(serde::Serialize)]
        struct RawKey<'a> {
            id: &'a str,
            public: String,
            current: bool,
        }
        #[derive(serde::Serialize)]
        struct Raw<'a> {
            keys: Vec<RawKey<'a>>,
        }
        let raw = Raw {
            keys: self
                .0
                .iter()
                .map(|k| RawKey {
                    id: &k.id,
                    public: hex_of(&k.public),
                    current: k.current,
                })
                .collect(),
        };
        serde_json::to_string_pretty(&raw).expect("verify keys serialize")
    }
}

fn raw_public(hex: &str) -> Result<Vec<u8>, String> {
    if hex.len() != 64
        || !hex
            .bytes()
            .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
    {
        return Err("public key must be 64 lowercase hex".into());
    }
    (0..32)
        .map(|i| u8::from_str_radix(&hex[i * 2..i * 2 + 2], 16).map_err(|e| e.to_string()))
        .collect()
}

fn hex_of(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// The key id implied by a public key's hex — the parse-side check that an
/// entry's id is honest (and the key verbs' writer: an id never ships
/// unless it is the public key's own BLAKE3 prefix).
pub fn key_id_from_hex_checked(hex: &str) -> Result<String, String> {
    Ok(key_id(&raw_public(hex)?))
}

/// The origin's file names inside the data dir.
pub const SIGNING_FILE: &str = "cache_signing.pkcs8";
pub const VERIFY_FILE: &str = "cache_keys.json";

/// The key-file load guard: a key file wider than 0600 (group- or
/// world-readable) is REFUSED with a named error, never loaded — the
/// same posture the data dir's 0700 boundary promises. Applies to the
/// secret AND to the verify list (public material, but a widened file
/// means the operator's copy story leaked permissions somewhere; fail
/// loud at load, not at audit time). The remedy NAMES the chmod.
fn check_private_file(path: &Path) -> Result<(), String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = std::fs::metadata(path)
            .map_err(|e| format!("reading {}: {e}", path.display()))?
            .permissions()
            .mode()
            & 0o777;
        if mode & 0o077 != 0 {
            return Err(format!(
                "{} is group/world-readable (mode {:04o}); key files must be 0600 — \
                 tighten the permissions first (chmod 600 {}) and retry",
                path.display(),
                mode,
                path.display()
            ));
        }
    }
    Ok(())
}

/// Loads the origin's CURRENT signing key pair. `Err` names the problem:
/// a missing file is `cache_disabled` territory (the caller decides), a
/// corrupt one is a loud failure.
pub fn load_signing_key(data: &Path) -> Result<ring::signature::Ed25519KeyPair, String> {
    let path = data.join(SIGNING_FILE);
    check_private_file(&path)?;
    let bytes = std::fs::read(path).map_err(|_| {
        format!(
            "no cache signing key at {} (run `naatre cache-key init`)",
            data.join(SIGNING_FILE).display()
        )
    })?;
    ring::signature::Ed25519KeyPair::from_pkcs8(&bytes).map_err(|_| {
        format!(
            "cache signing key at {} is not valid PKCS#8",
            data.join(SIGNING_FILE).display()
        )
    })
}

/// Loads the origin's verify list (current + retired). The origin serves
/// ticket verification from THIS list — the same list the cache operator
/// copies out.
pub fn load_verify_keys(data: &Path) -> Result<VerifyKeys, String> {
    load_verify_key_file(&data.join(VERIFY_FILE)).map_err(|e| {
        format!(
            "{e} (the origin's verify list: {} — run `naatre cache-key init`)",
            data.join(VERIFY_FILE).display()
        )
    })
}

/// Loads a verify list from an explicit PATH (the cache tier's `--key`
/// surface): the same 0600-at-load guard, then parse. Fail-closed on any
/// refusal — the tier never starts on a widened or malformed key file.
pub fn load_verify_key_file(path: &Path) -> Result<VerifyKeys, String> {
    check_private_file(path)?;
    let json =
        std::fs::read_to_string(path).map_err(|e| format!("reading {}: {e}", path.display()))?;
    VerifyKeys::parse(&json)
}

/// Generates a fresh key pair: returns the PKCS#8 secret DER and the
/// public key's hex (the caller builds the file rows).
pub fn generate_keypair() -> Result<(Vec<u8>, String), String> {
    let doc = ring::signature::Ed25519KeyPair::generate_pkcs8(&ring::rand::SystemRandom::new())
        .map_err(|_| "CSPRNG unavailable".to_string())?;
    let pair = ring::signature::Ed25519KeyPair::from_pkcs8(doc.as_ref())
        .map_err(|_| "generated key rejected by its own parser".to_string())?;
    Ok((doc.as_ref().to_vec(), hex_of(pair.public_key().as_ref())))
}

/// Writes a DER file 0600 (the secret's on-disk posture; the data dir is
/// 0700 already, this is defense in depth).
pub fn write_signing_file(data: &Path, der: &[u8]) -> Result<(), String> {
    let path = data.join(SIGNING_FILE);
    #[cfg(unix)]
    {
        use std::io::Write as _;
        use std::os::unix::fs::OpenOptionsExt as _;
        use std::os::unix::fs::PermissionsExt as _;
        // mode(0600) applies only at CREATE — a rotation REWRITING a
        // pre-widened file must not inherit the wide mode, so the
        // permissions are SET explicitly after the write too (M13b-fix
        // security A4).
        std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&path)
            .and_then(|mut f| {
                f.write_all(der)?;
                std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
            })
            .map_err(|e| format!("writing {path:?}: {e}"))
    }
    #[cfg(not(unix))]
    std::fs::write(&path, der).map_err(|e| format!("writing {path:?}: {e}"))
}

/// Writes the verify list 0600 (public material; the mode matches its
/// neighbor so an operator copying the pair never widens one by accident).
pub fn write_verify_file(data: &Path, keys: &VerifyKeys) -> Result<(), String> {
    let path = data.join(VERIFY_FILE);
    #[cfg(unix)]
    {
        use std::io::Write as _;
        use std::os::unix::fs::OpenOptionsExt as _;
        use std::os::unix::fs::PermissionsExt as _;
        // set_permissions after the write (see write_signing_file): a
        // rewrite of a pre-widened file lands 0600, never inherits.
        std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .mode(0o600)
            .open(&path)
            .and_then(|mut f| {
                f.write_all(keys.to_json().as_bytes())?;
                std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
            })
            .map_err(|e| format!("writing {path:?}: {e}"))
    }
    #[cfg(not(unix))]
    std::fs::write(&path, keys.to_json()).map_err(|e| format!("writing {path:?}: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Fixed clock base: ns for 2023-11-14.
    const NOW: u64 = 1_700_000_000_000_000_000;

    struct Keys {
        _dir: tempfile::TempDir,
        pair: ring::signature::Ed25519KeyPair,
        verify: VerifyKeys,
        public_hex: String,
        key_id: String,
    }

    fn keys() -> Keys {
        let (der, public_hex) = generate_keypair().unwrap();
        let id = key_id_from_hex_checked(&public_hex).unwrap();
        let verify = VerifyKeys::parse(&format!(
            r#"{{"keys":[{{"id":"{id}","public":"{public_hex}","current":true}}]}}"#
        ))
        .unwrap();
        let pair =
            ring::signature::Ed25519KeyPair::from_pkcs8(&der).expect("generated key re-parses");
        Keys {
            _dir: tempfile::tempdir().unwrap(),
            pair,
            verify,
            public_hex,
            key_id: id,
        }
    }

    fn claims(lifetime_secs: u64) -> TicketClaims {
        TicketClaims {
            v: 1,
            tid: "ab".repeat(16),
            principal: "agent".into(),
            repo: "demo".into(),
            root_tree: "aa".repeat(32),
            iat: NOW,
            exp: NOW + lifetime_secs * 1_000_000_000,
        }
    }

    #[test]
    fn mint_verify_roundtrip() {
        let k = keys();
        let ticket = mint_ticket(&claims(600), &k.pair);
        assert_eq!(ticket.split('.').next_back(), Some(k.key_id.as_str()));
        let got = verify_ticket(&ticket, &k.verify, NOW).expect("fresh ticket verifies");
        assert_eq!(got, claims(600));
        // Still verifies near (but inside) expiry.
        verify_ticket(&ticket, &k.verify, NOW + 599 * 1_000_000_000).expect("inside TTL");
    }

    #[test]
    fn expired_ticket_refuses() {
        let k = keys();
        let ticket = mint_ticket(&claims(600), &k.pair);
        assert_eq!(
            verify_ticket(&ticket, &k.verify, NOW + 600 * 1_000_000_000),
            Err(TicketError::Expired)
        );
        assert_eq!(
            verify_ticket(&ticket, &k.verify, NOW + 601 * 1_000_000_000),
            Err(TicketError::Expired)
        );
    }

    #[test]
    fn future_iat_refuses_at_verify_even_when_signed() {
        let k = keys();
        // A validly-signed ticket whose iat sits in the FUTURE: without
        // the skew check it would verify for years (exp far ahead of
        // now) — the verify-side cap's stated purpose is exactly this
        // defense. 31 s beyond now: past the 30 s skew tolerance.
        let mut future = claims(600);
        future.iat = NOW + 31 * 1_000_000_000;
        future.exp = NOW + 31 * 1_000_000_000 + 600 * 1_000_000_000;
        let ticket = mint_ticket(&future, &k.pair);
        assert_eq!(
            verify_ticket(&ticket, &k.verify, NOW),
            Err(TicketError::Invalid),
            "a future-iat ticket refuses"
        );
        // Exactly at the skew boundary (30 s): allowed.
        let mut boundary = claims(600);
        boundary.iat = NOW + 30 * 1_000_000_000;
        boundary.exp = NOW + 30 * 1_000_000_000 + 600 * 1_000_000_000;
        let ticket = mint_ticket(&boundary, &k.pair);
        verify_ticket(&ticket, &k.verify, NOW).expect("skew-boundary iat verifies");
    }

    #[test]
    fn verify_side_cap_bounds_validity_from_the_verifiers_now() {
        let k = keys();
        // Minted 800 s ago with a 900 s lifetime (well-formed by the
        // mint's clock), but claiming 900 MORE seconds from the
        // verifier's now: the second cap arm refuses — a skewed-forward
        // origin cannot mint longer-lived-than-cap tickets.
        let mut skewed = claims(900);
        skewed.iat = NOW - 800 * 1_000_000_000;
        skewed.exp = NOW + 900 * 1_000_000_000;
        let ticket = mint_ticket(&skewed, &k.pair);
        assert_eq!(
            verify_ticket(&ticket, &k.verify, NOW),
            Err(TicketError::Invalid),
            "validity beyond the cap from the verifier's now refuses"
        );
        // The same shape inside the cap: exp within now + 900 verifies.
        let mut in_cap = claims(900);
        in_cap.iat = NOW - 800 * 1_000_000_000;
        in_cap.exp = NOW + 100 * 1_000_000_000;
        let ticket = mint_ticket(&in_cap, &k.pair);
        verify_ticket(&ticket, &k.verify, NOW).expect("in-cap ticket verifies");
    }

    #[test]
    fn lifetime_above_the_cap_refuses_at_verify_even_when_signed() {
        let k = keys();
        // Signed by the real origin key with a 16-minute lifetime: the
        // mint would refuse, and the tier's verify-side cap refuses it
        // too (the defense against a misconfigured origin).
        let ticket = mint_ticket(&claims(961), &k.pair);
        assert_eq!(
            verify_ticket(&ticket, &k.verify, NOW),
            Err(TicketError::Invalid)
        );
        // Exactly the cap is fine.
        let ticket = mint_ticket(&claims(900), &k.pair);
        verify_ticket(&ticket, &k.verify, NOW).expect("cap-length ticket verifies");
    }

    #[test]
    fn tampered_payload_and_wrong_key_refuse() {
        let k = keys();
        let ticket = mint_ticket(&claims(600), &k.pair);
        // Flip a payload byte (re-encode an altered claim).
        let mut alt = claims(600);
        alt.repo = "other".into();
        let forged = mint_ticket(&alt, &k.pair);
        let forged_payload = forged.split('.').next().unwrap().to_string();
        let rest = ticket.split_once('.').unwrap().1;
        let spliced = format!("{forged_payload}.{rest}");
        assert_eq!(
            verify_ticket(&spliced, &k.verify, NOW),
            Err(TicketError::Invalid),
            "a payload signed by the same key but not THIS payload must not verify"
        );
        // A foreign key's ticket: unknown key-id → invalid.
        let k2 = keys();
        let foreign = mint_ticket(&claims(600), &k2.pair);
        assert_eq!(
            verify_ticket(&foreign, &k.verify, NOW),
            Err(TicketError::Invalid)
        );
        // Garbage.
        assert_eq!(
            verify_ticket("garbage", &k.verify, NOW),
            Err(TicketError::Invalid)
        );
    }

    #[test]
    fn rotation_retired_key_verifies_until_expiry() {
        let k = keys();
        let old_ticket = mint_ticket(&claims(600), &k.pair);
        // The origin rotates: the new key joins, the old key stays listed
        // (retired). The OLD ticket must still verify.
        let (new_der, new_public) = generate_keypair().unwrap();
        let new_id = key_id_from_hex_checked(&new_public).unwrap();
        let rotated = VerifyKeys::parse(&format!(
            r#"{{"keys":[{{"id":"{}","public":"{}","current":true}},{{"id":"{}","public":"{}","current":false}}]}}"#,
            new_id, new_public, k.key_id, k.public_hex
        ))
        .unwrap();
        verify_ticket(&old_ticket, &rotated, NOW).expect("retired key's ticket verifies");
        // And the new pair's tickets verify under the rotated list.
        let new_pair = ring::signature::Ed25519KeyPair::from_pkcs8(&new_der).unwrap();
        let new_ticket = mint_ticket(&claims(600), &new_pair);
        verify_ticket(&new_ticket, &rotated, NOW).expect("current key's ticket verifies");
        // A key dropped from the list entirely: fail-closed.
        assert_eq!(
            verify_ticket(
                &old_ticket,
                &VerifyKeys::parse(&format!(
                    r#"{{"keys":[{{"id":"{new_id}","public":"{new_public}","current":true}}]}}"#
                ))
                .unwrap(),
                NOW
            ),
            Err(TicketError::Invalid)
        );
    }

    #[test]
    fn verify_keys_parse_is_fail_closed() {
        let k = keys();
        // Empty list: refuse.
        assert!(VerifyKeys::parse(r#"{"keys":[]}"#).is_err());
        // Id not matching the public key's hash prefix: refuse.
        assert!(
            VerifyKeys::parse(&format!(
                r#"{{"keys":[{{"id":"deadbeef","public":"{}","current":true}}]}}"#,
                k.public_hex
            ))
            .is_err()
        );
        // Uppercase hex: refuse (ids and publics are lowercase-hex canon).
        assert!(
            VerifyKeys::parse(&format!(
                r#"{{"keys":[{{"id":"{}","public":"{}","current":true}}]}}"#,
                k.key_id,
                k.public_hex.to_uppercase()
            ))
            .is_err()
        );
    }

    #[test]
    #[cfg(unix)]
    fn widened_key_files_refuse_to_load() {
        use std::os::unix::fs::PermissionsExt as _;
        let k = keys();
        let dir = k._dir.path();
        write_signing_file(dir, &generate_keypair().unwrap().0).unwrap();
        write_verify_file(dir, &k.verify).unwrap();
        // The 0600 files load (the roundtrip test pins the happy path);
        // widen each and the loader refuses with the named error — the
        // secret AND the public list, both directions.
        for name in [SIGNING_FILE, VERIFY_FILE] {
            let path = dir.join(name);
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
            let err = if name == SIGNING_FILE {
                load_signing_key(dir).unwrap_err()
            } else {
                load_verify_keys(dir).unwrap_err()
            };
            assert!(
                err.contains("group/world-readable") && err.contains("0600"),
                "the refusal names the problem: {err}"
            );
            // 0640 (group-readable) is equally refused; 0600 loads again.
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o640)).unwrap();
            let err = if name == SIGNING_FILE {
                load_signing_key(dir).unwrap_err()
            } else {
                load_verify_keys(dir).unwrap_err()
            };
            assert!(err.contains("group/world-readable"), "{err}");
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
            if name == SIGNING_FILE {
                load_signing_key(dir).expect("0600 loads");
            } else {
                load_verify_keys(dir).expect("0600 loads");
            }
        }
        // The cache tier's explicit-path loader carries the same guard.
        std::fs::set_permissions(
            dir.join(VERIFY_FILE),
            std::fs::Permissions::from_mode(0o604),
        )
        .unwrap();
        let err = load_verify_key_file(&dir.join(VERIFY_FILE)).unwrap_err();
        assert!(err.contains("group/world-readable"), "{err}");
    }

    #[test]
    #[cfg(unix)]
    fn duplicate_key_ids_refuse_at_parse() {
        let k = keys();
        // The same id twice — the M13b-fix parse guard refuses (a key id
        // names ONE key; a duplicated entry would make rotation bookkeeping
        // ambiguous).
        let json = format!(
            r#"{{"keys":[{{"id":"{}","public":"{}","current":true}},{{"id":"{}","public":"{}","current":false}}]}}"#,
            k.key_id, k.public_hex, k.key_id, k.public_hex
        );
        let err = VerifyKeys::parse(&json).unwrap_err();
        assert!(err.contains("duplicate key id"), "{err}");
    }

    #[test]
    #[cfg(unix)]
    fn rewrites_land_0600_even_over_widened_files() {
        use std::os::unix::fs::PermissionsExt as _;
        let (der, public_hex) = generate_keypair().unwrap();
        let id = key_id_from_hex_checked(&public_hex).unwrap();
        let verify = VerifyKeys::parse(&format!(
            r#"{{"keys":[{{"id":"{id}","public":"{public_hex}","current":true}}]}}"#
        ))
        .unwrap();
        let dir = tempfile::tempdir().unwrap();
        // Pre-widen BOTH files (an operator's leaked-copy shape): the
        // rewrite must land 0600, never inherit (security A4's rotate
        // half).
        std::fs::write(dir.path().join(SIGNING_FILE), b"").unwrap();
        std::fs::write(dir.path().join(VERIFY_FILE), b"").unwrap();
        std::fs::set_permissions(
            dir.path().join(SIGNING_FILE),
            std::fs::Permissions::from_mode(0o644),
        )
        .unwrap();
        std::fs::set_permissions(
            dir.path().join(VERIFY_FILE),
            std::fs::Permissions::from_mode(0o644),
        )
        .unwrap();
        write_signing_file(dir.path(), &der).unwrap();
        write_verify_file(dir.path(), &verify).unwrap();
        for name in [SIGNING_FILE, VERIFY_FILE] {
            let mode = std::fs::metadata(dir.path().join(name))
                .unwrap()
                .permissions()
                .mode()
                & 0o777;
            assert_eq!(mode, 0o600, "{name} lands 0600 after a rewrite");
        }
    }

    #[test]
    fn key_files_roundtrip_through_the_data_dir() {
        let k = keys();
        let dir = k._dir.path();
        write_signing_file(dir, &generate_keypair().unwrap().0).unwrap();
        write_verify_file(dir, &k.verify).unwrap();
        load_signing_key(dir).expect("signing key loads");
        let loaded = load_verify_keys(dir).expect("verify keys load");
        assert_eq!(loaded.keys().len(), 1);
        assert_eq!(loaded.keys()[0].id, k.key_id);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = std::fs::metadata(dir.join(VERIFY_FILE))
                .unwrap()
                .permissions()
                .mode()
                & 0o777;
            assert_eq!(mode, 0o600, "the verify file ships 0600");
        }
    }
}
