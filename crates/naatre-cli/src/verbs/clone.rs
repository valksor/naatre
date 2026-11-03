//! `naatre clone <url-or-name> [--sparse PREFIX]… [--into DIR]`.
//!
//! Resolves `main`, fetches the head changeset's root tree, walks only the
//! trees on the paths to the sparse prefixes (default: the whole tree),
//! fetches and hash-verifies every blob, and materializes into a staging
//! directory renamed atomically onto the destination. `.naatre/` (config,
//! state, index) is written inside staging so an interrupted clone leaves
//! no half-written working copy.

use std::path::Path;

use naatre_core::ids::Id;
use serde_json::{Value, json};

use super::{Net, VerbError};
use crate::client::{Client, DEFAULT_URL, uri_segment};
use crate::wc::fetch::{CacheRoute, fetch_blobs_verified, materialize, walk_tree};
use crate::wc::index::Index;
use crate::wc::{Config, StageDir, State};

/// sec-C2 origin equality over the one parser (`crate::url`): scheme,
/// host, and port — `http://host` and `https://host` are DIFFERENT
/// origins. Re-exported for the MCP clone tool's same-origin check.
pub(crate) use crate::url::origins_match;

/// Splits `https://host[:port]/<repo>` (or `http://` to a loopback host,
/// or any `http://` host when the invocation opted in with
/// `--insecure-http` / `NAATRE_INSECURE_HTTP=1`) into (origin, repo), or
/// treats a bare name as the repo on `NAATRE_URL` / `--server` / the
/// default URL. The URL (or the bare-name base) is parsed ONCE through
/// [`crate::url::parse_remote`] — userinfo, query/fragment, backslash,
/// percent-encoded host/port, and an unparseable port are USAGE errors,
/// never guesses — and the returned origin is that parse's canonical
/// origin string. Any other scheme is a usage error, and a non-loopback
/// plain `http://` URL is refused unless the invocation opted in — the
/// client speaks https (and plain http to loopback); it never downgrades
/// a URL behind the user's back. `pub(crate)`: the MCP clone tool reuses
/// this to compare a caller-named url's origin against the session's
/// configured server (sec-C2 — the clone tool is the one tool whose
/// target the CALLER names, so it is the exfiltration channel a prompt
/// injection reaches for).
pub(crate) fn split_url(url: &str, net: &Net) -> Result<(String, String), VerbError> {
    split_url_parsed(url, net).map(|(parsed, repo)| (parsed.origin(), repo))
}

/// [`split_url`] keeping the parse: the request base and the persisted
/// per-remote opt-in derive from [`ParsedRemote`], never from a re-slice.
pub(crate) fn split_url_parsed(
    url: &str,
    net: &Net,
) -> Result<(crate::url::ParsedRemote, String), VerbError> {
    // Trailing slashes carry no repo segment; the parse would keep them
    // in the path (and the scheme's `://` never strips).
    let mut url = url.trim();
    while url.ends_with('/') && !url.ends_with("://") {
        url = &url[..url.len() - 1];
    }
    if url.split_once("://").is_some() {
        let parsed = crate::url::parse_remote(url).map_err(VerbError::Usage)?;
        let repo = parsed.path.trim_start_matches('/').to_string();
        if repo.is_empty() {
            return Err(VerbError::Usage(format!(
                "URL {url:?} has no repo path (expected https://host[:port]/<repo>)"
            )));
        }
        crate::wc::admit_remote(&parsed, net.insecure_http, None).map_err(VerbError::Usage)?;
        return Ok((parsed, repo));
    }
    if url.contains('/') {
        return Err(VerbError::Usage(format!(
            "{url:?} is neither https://host[:port]/<repo> nor a bare repo name"
        )));
    }
    // A bare name: the server comes from --server / NAATRE_URL / the
    // default — and goes through the SAME one-parse admission (no client
    // path skips the plain-http refusal).
    let base = net
        .server
        .clone()
        .or_else(|| std::env::var("NAATRE_URL").ok().filter(|v| !v.is_empty()))
        .unwrap_or_else(|| DEFAULT_URL.to_string());
    let parsed = crate::url::parse_remote(&base).map_err(VerbError::Usage)?;
    crate::wc::admit_remote(&parsed, net.insecure_http, None).map_err(VerbError::Usage)?;
    Ok((parsed, url.to_string()))
}

/// Runs clone; prints the summary and returns the exit code.
pub fn run(url: &str, sparse: &[String], into: Option<&str>, net: &Net) -> i32 {
    match value(url, sparse, into, net) {
        Ok(v) => {
            let sparse_list: Vec<&str> = v["sparse"]
                .as_array()
                .map(|a| a.iter().filter_map(|s| s.as_str()).collect())
                .unwrap_or_default();
            let sparse_note = if sparse_list.is_empty() {
                String::new()
            } else {
                format!(" (sparse: {})", sparse_list.join(", "))
            };
            println!(
                "cloned {} at {} into {} ({} file(s)){sparse_note}",
                v["repo"].as_str().unwrap_or("?"),
                v["head"].as_str().unwrap_or("?"),
                v["dest"].as_str().unwrap_or("?"),
                v["files"].as_u64().unwrap_or(0),
            );
            for p in v["conflicted"].as_array().map_or(&[][..], Vec::as_slice) {
                println!("C {}", p.as_str().unwrap_or("?"));
            }
            0
        }
        Err(e) => e.report(),
    }
}

/// The clone result in `--json`-style shape: `{repo, head, dest, files,
/// sparse}` (no HTTP analog; the MCP `clone` tool returns the same value).
pub fn value(
    url: &str,
    sparse: &[String],
    into: Option<&str>,
    net: &Net,
) -> Result<Value, VerbError> {
    let (parsed_base, repo) = split_url_parsed(url, net)?;
    let base = parsed_base.origin();
    let dest = Path::new(into.unwrap_or(&repo));
    // Token precedence: --token flag > NAATRE_TOKEN env (no config yet —
    // clone writes it).
    let token = net
        .token
        .clone()
        .or_else(|| std::env::var("NAATRE_TOKEN").ok().filter(|v| !v.is_empty()));
    // split_url already refused a non-opted-in plain-http URL. The CA
    // bundle (`--ca-file` flag > `NAATRE_CA_FILE` env) joins the platform
    // roots — a bad bundle is a hard local error naming the path, already
    // validated here by the client build.
    let ca_file = net.ca_file.clone().or_else(|| {
        std::env::var("NAATRE_CA_FILE")
            .ok()
            .filter(|v| !v.is_empty())
    });
    // The per-remote opt-in this clone runs with: a plain-http clone that
    // opted in records ITS origin — the cache-route reads admit exactly
    // that origin, and the working copy's later verbs (via the persisted
    // config below) admit nothing else.
    let opted_origin = if parsed_base.scheme == "http" && net.insecure_http {
        Some(parsed_base.clone())
    } else {
        None
    };
    let client = Client::new_with_ca(&base, token.clone(), ca_file.as_deref())
        .map_err(VerbError::Local)?
        .with_insecure_origin(opted_origin.as_ref().map(crate::url::ParsedRemote::key));

    // main → head changeset → root tree id.
    let refv = client.get_json(&format!("/v1/repos/{}/refs/main", uri_segment(&repo)), &[])?;
    let head = refv["target"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| {
            VerbError::Local("repo has no main ref (unborn); nothing to clone".into())
        })?;
    let rootv = client.get_json(
        &format!(
            "/v1/repos/{}/changesets/{}/root",
            uri_segment(&repo),
            head.to_hex()
        ),
        &[],
    )?;
    let root_tree = rootv["root_tree_id"]
        .as_str()
        .and_then(Id::from_hex)
        .ok_or_else(|| VerbError::Local("server returned a malformed root tree id".into()))?;

    // Walk (sparse-filtered), then fetch and verify every blob. Masked
    // entries (redacted server-side) are unavailable: they never reach the
    // fetch and are reported.
    let walk = walk_tree(&client, &repo, &root_tree, sparse)?;
    let files = walk.files;
    let unavailable = walk.redacted;
    let blob_ids: Vec<Id> = files.iter().map(|f| f.blob).collect();
    // M13b: NAATRE_CACHE_URL routes the by-id fetches through the tier.
    let mut cache = CacheRoute::from_env();
    let blobs = fetch_blobs_verified(&client, &repo, &root_tree, &blob_ids, &mut cache)?;

    // Materialize into staging; atomic rename onto the destination.
    let stage = StageDir::beside(dest)
        .map_err(|e| VerbError::Local(format!("cannot stage beside {}: {e}", dest.display())))?;
    let mut index = Index::default();
    // M12b: the conflicted paths of the cloned tree — the blobs whose
    // bytes are a rendered marker file (a conflicted node's
    // markers-materialized tree carries them as ordinary file entries).
    let mut conflicted: Vec<String> = Vec::new();
    for file in &files {
        let bytes = &blobs[&file.blob];
        let row = materialize(stage.path(), file, bytes)?;
        index.rows.insert(file.path.clone(), row);
        if naatre_core::conflict::marker_shaped(bytes) {
            conflicted.push(file.path.clone());
        }
    }
    let mut listed: Vec<String> = Vec::new();
    for p in sparse {
        if !listed.contains(p) {
            listed.push(p.clone());
        }
    }
    let state = State {
        head: head.to_hex(),
        base: None,
        sparse: listed,
        server_repo: repo.clone(),
        conflicted,
    };
    {
        // The CA bundle persists canonicalized (absolute); the client build
        // above already validated it as a readable PEM bundle with at
        // least one certificate.
        let saved_ca_file = match ca_file.as_deref() {
            None => None,
            Some(path) => Some(
                std::fs::canonicalize(path)
                    .map_err(|e| {
                        VerbError::Usage(format!(
                            "ca_file {path:?}: cannot resolve to an absolute path: {e}"
                        ))
                    })?
                    .to_string_lossy()
                    .into_owned(),
            ),
        };
        let wc = crate::wc::Wc::open(stage.path()).map_err(VerbError::Local)?;
        wc.save_config(&Config {
            server_url: client.base.clone(),
            token,
            // The opt-in persists PER REMOTE (the opted-in origin, never a
            // bool): a non-loopback plain-http remote records its origin so
            // the working copy's later verbs admit exactly that origin, and
            // `--ca-file` records the bundle its clients need.
            insecure_http: opted_origin.as_ref().map(crate::url::ParsedRemote::origin),
            ca_file: saved_ca_file,
        })?;
        wc.save_state(&state)?;
        wc.save_index(&index)?;
    }
    stage
        .commit(dest)
        .map_err(|e| VerbError::Local(format!("cannot move the clone into place: {e}")))?;

    Ok(json!({
        "repo": repo,
        "head": head.to_hex(),
        "dest": dest.display().to_string(),
        "files": files.len(),
        "unavailable": unavailable,
        "sparse": sparse,
        "conflicted": state.conflicted,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn net() -> Net {
        Net::default()
    }

    fn net_opted_in() -> Net {
        Net {
            insecure_http: true,
            ..Net::default()
        }
    }

    #[test]
    fn https_urls_are_accepted() {
        let (origin, repo) = split_url("https://naatre.example.com:7443/team/lib", &net())
            .expect("https is accepted");
        assert_eq!(origin, "https://naatre.example.com:7443");
        assert_eq!(repo, "team/lib");
        // The scheme is normalized (the origin comparison keycases it).
        let (origin, _) =
            split_url("HTTPS://naatre.example.com/lib", &net()).expect("scheme case is normalized");
        assert_eq!(origin, "https://naatre.example.com");
    }

    #[test]
    fn plain_http_loopback_hosts_are_accepted() {
        for url in [
            "http://localhost:7400/lib",
            "http://LOCALHOST/lib",
            "http://127.0.0.1:7400/lib",
            "http://127.9.9.9/lib",
            "http://[::1]:7400/lib",
        ] {
            let (origin, repo) = split_url(url, &net()).unwrap_or_else(|e| panic!("{url}: {e}"));
            assert_eq!(repo, "lib", "{url}");
            assert!(origin.starts_with("http://"), "{url}");
        }
    }

    #[test]
    fn plain_http_non_loopback_is_refused_unless_opted_in() {
        let err = split_url("http://naatre.lan:7400/lib", &net()).expect_err("refused");
        let text = err.to_string();
        assert!(text.contains("--insecure-http"), "{text:?}");
        assert!(text.contains("https://"), "{text:?}");
        // The opt-in admits it.
        let (origin, repo) =
            split_url("http://naatre.lan:7400/lib", &net_opted_in()).expect("opted in");
        assert_eq!(origin, "http://naatre.lan:7400");
        assert_eq!(repo, "lib");
    }

    #[test]
    fn other_schemes_are_refused_naming_the_scheme() {
        for url in ["ftp://host/lib", "git://host/lib", "ws://host/lib"] {
            let err = split_url(url, &net()).expect_err(url);
            let VerbError::Usage(text) = err else {
                panic!("{url}: not a usage error")
            };
            let scheme = url.split(':').next().expect("scheme");
            assert!(text.contains(scheme), "{text:?}");
        }
    }

    #[test]
    fn bare_names_resolve_through_the_net_overrides() {
        let net = Net {
            server: Some("https://naatre.example.com:7443".into()),
            ..Net::default()
        };
        let (origin, repo) = split_url("lib", &net).expect("bare name");
        assert_eq!(origin, "https://naatre.example.com:7443");
        assert_eq!(repo, "lib");
    }

    #[test]
    fn loopback_hosts_after_the_one_parse() {
        // The parsed host is what the loopback rule sees (no userinfo, no
        // port, no brackets — the one parser stripped them).
        for url in [
            "http://localhost:7400/lib",
            "http://LocalHost/lib",
            "http://127.0.0.1:7400/lib",
            "http://127.255.0.3/lib",
            "http://[::1]:7400/lib",
        ] {
            let (parsed, _) =
                split_url_parsed(url, &net()).unwrap_or_else(|e| panic!("{url}: {e}"));
            assert!(parsed.is_loopback(), "{url}");
        }
        for url in [
            "http://naatre.lan:7400/lib",
            "http://10.0.0.1/lib",
            "http://127.0.0.1.example.com/lib",
            "http://12.0.0.1/lib",
            "http://127.0.0.1.2/lib",
            "http://[::2]/lib",
            "http://example.com/lib",
        ] {
            let (parsed, _) =
                split_url_parsed(url, &net_opted_in()).unwrap_or_else(|e| panic!("{url}: {e}"));
            assert!(!parsed.is_loopback(), "{url}");
        }
    }

    /// sec-C2 regression pins (the review's four examples): every URL
    /// whose authority the connecting parser could read differently than a
    /// string comparison is a USAGE error in split_url — never a request.
    #[test]
    fn sec_c2_confusion_urls_are_usage_errors() {
        for url in [
            "https://evil.example?@naatre.example.com/lib",
            "https://a@naatre.example.com:443@evil.example/r",
            "http://lan-host?@127.0.0.1/lib",
            "http://x@127.0.0.1:80@evil.com/r",
            // A port that does not parse as u16 is an error, never the
            // default.
            "https://naatre.example.com:99999/lib",
            // Percent-encoding in the host never reaches a comparison.
            "https://naatre.example%2ecom/lib",
        ] {
            let err = split_url(url, &net_opted_in()).expect_err(url);
            let VerbError::Usage(text) = err else {
                panic!("{url}: not a usage error");
            };
            assert!(!text.is_empty(), "{url}");
        }
    }
}
