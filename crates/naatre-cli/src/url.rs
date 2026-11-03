//! The ONE remote-URL parser (the F1 correction round's sec-C2 fix).
//!
//! Every remote — a clone URL, the MCP clone tool's caller-named url, the
//! resolved `--server`/`NAATRE_URL` value, the `NAATRE_CACHE_URL` tier, and
//! the `naatre cache --origin` tier origin — is parsed exactly ONCE through
//! [`parse_remote`], and everything downstream (origin keys, loopback
//! checks, scheme admission, the request base) derives from that single
//! parse. The parser is built on `ureq::http::Uri` — the same parser the
//! HTTP layer connects with — so what the security checks compare is what
//! the client dials.
//!
//! The regression this closes: the old string-slicing split the authority
//! at the first `@` and treated an unparseable port as the scheme default,
//! while the connecting parser ends the authority at the first `/`, `?` or
//! `#` and takes the host after the LAST `@` — so
//! `https://evil.example?@naatre.example.com/lib` and
//! `https://a@naatre.example.com:443@evil.example/r` compared as the
//! trusted origin but connected to the attacker's. [`parse_remote`]
//! refuses instead of guessing: userinfo is never allowed, `?`/`#` are
//! never allowed (the API takes no query or fragment), `\` is never a URL
//! character, `%` never appears in a scheme/host/port, and a port that
//! does not parse as u16 is an error — never silently the default.

use std::str::FromStr as _;

use ureq::http::Uri;

/// What origin equality compares: (scheme, host, port) — scheme and host
/// lowercased, port with the scheme default applied (80 http, 443 https).
pub(crate) type OriginKey = (String, String, u16);

/// The scheme's default port.
pub(crate) fn default_port(scheme: &str) -> Option<u16> {
    match scheme {
        "http" => Some(80),
        "https" => Some(443),
        _ => None,
    }
}

/// One successfully parsed remote. `host` is lowercased and exactly what
/// `Uri` yields (an IPv6 literal loses its brackets); `port` is the
/// explicit port or the scheme default; `path` is the URI path (`/` when
/// the URL carried none).
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct ParsedRemote {
    pub scheme: String,
    pub host: String,
    pub port: u16,
    pub path: String,
}

impl ParsedRemote {
    /// The origin comparison key (scheme, host, port-with-default).
    pub fn key(&self) -> OriginKey {
        (self.scheme.clone(), self.host.clone(), self.port)
    }

    /// The origin as a URL string: scheme and host lowercased, an absent
    /// or default port omitted, an IPv6 host re-bracketed. This is the
    /// request base and the per-remote `insecure_http` origin's stored
    /// shape.
    pub fn origin(&self) -> String {
        let host = if self.host.contains(':') {
            format!("[{}]", self.host)
        } else {
            self.host.clone()
        };
        match default_port(&self.scheme) {
            Some(p) if p == self.port => format!("{}://{}", self.scheme, host),
            _ => format!("{}://{}:{}", self.scheme, host, self.port),
        }
    }

    /// The loopback rule over the parsed host (no port, no userinfo — the
    /// parse stripped them).
    pub fn is_loopback(&self) -> bool {
        is_loopback_host(&self.host)
    }
}

/// True for the loopback hosts the plain-http rule admits: `localhost`
/// (case-insensitive), any IPv4 address in `127.0.0.0/8`, or `::1`.
/// Takes the PARSED host — never a raw authority (userinfo and ports are
/// the parser's job, and string-slicing them is the sec-C2 regression
/// this module exists to close).
pub(crate) fn is_loopback_host(host: &str) -> bool {
    if host.eq_ignore_ascii_case("localhost") || host == "::1" {
        return true;
    }
    let octets: Vec<&str> = host.split('.').collect();
    octets.len() == 4
        && octets[0].parse::<u8>() == Ok(127)
        && octets[1..].iter().all(|o| o.parse::<u8>().is_ok())
}

/// Parses one remote URL, refusing everything the connecting parser could
/// read differently than a string comparison would. The refusals, in
/// order: no scheme, a scheme other than http/https, any `\`, any `?` or
/// `#` (query/fragment on a remote origin at all — the API takes none),
/// an empty authority, userinfo (`@`) in the authority, `%` in the
/// scheme/host/port part, and an explicit port that does not parse as
/// u16 (an ERROR — never the default). Anything left must survive
/// `Uri::from_str` unchanged, or the parse fails.
pub(crate) fn parse_remote(url: &str) -> Result<ParsedRemote, String> {
    let url = url.trim();
    let Some((scheme_raw, rest)) = url.split_once("://") else {
        return Err(format!(
            "{url:?} has no scheme (expected https://host[:port]/<repo>)"
        ));
    };
    let scheme = scheme_raw.to_ascii_lowercase();
    if scheme != "http" && scheme != "https" {
        return Err(format!(
            "URL scheme {scheme:?} is not supported: the naatre client speaks https \
             (and plain http to loopback hosts)"
        ));
    }
    if url.contains('\\') {
        return Err(format!(
            "{url:?} carries a backslash: `\\` is not a URL character and some \
             clients read it as `/`"
        ));
    }
    if url.contains('?') || url.contains('#') {
        return Err(format!(
            "{url:?} carries a query or fragment: a remote URL takes none — the \
             authority ends at the first `?`/`#` while the host is taken after \
             the last `@`, so such URLs do not name the origin they appear to"
        ));
    }
    // The authority is everything before the path's first `/`; the path
    // itself may carry `%` (it is never decoded into a comparison key).
    let authority = rest.split('/').next().unwrap_or(rest);
    if authority.is_empty() {
        return Err(format!("{url:?} has no host"));
    }
    if authority.contains('@') {
        return Err(format!(
            "userinfo (@) is not allowed in a remote URL authority ({authority:?}): \
             the connecting parser takes the host after the LAST `@`, so anything \
             before it can make the URL name a different origin than it shows"
        ));
    }
    if authority.contains('%') {
        return Err(format!(
            "percent-encoding is not allowed in the scheme/host/port of a remote \
             URL ({authority:?}): hosts and ports are compared literally"
        ));
    }
    // A port that does not parse as u16 is an ERROR — never the default
    // (the old default-fallback was the sec-C2 port hole). Checked against
    // the RAW authority text: the connecting parser accepts an
    // out-of-range port and would silently answer `port_u16() == None`,
    // which must never read as "use the default".
    let raw_port = if let Some(after_open) = authority.strip_prefix('[') {
        // An IPv6 literal: the port follows the closing bracket.
        after_open
            .split_once(']')
            .and_then(|(_, p)| p.strip_prefix(':'))
    } else {
        authority.rsplit_once(':').map(|(_, p)| p)
    };
    if let Some(raw) = raw_port
        && (raw.is_empty() || raw.parse::<u16>().is_err())
    {
        return Err(format!(
            "port {raw:?} in {url:?} is not a valid port (0-65535)"
        ));
    }
    let uri = Uri::from_str(url)
        .map_err(|e| format!("{url:?} is not a URL the client can connect with: {e}"))?;
    let Some(host) = uri.host() else {
        return Err(format!("{url:?} has no host"));
    };
    // `Uri::host()` keeps an IPv6 literal's brackets; the host is stored
    // bare (the origin string re-brackets it).
    let host = host
        .strip_prefix('[')
        .and_then(|h| h.strip_suffix(']'))
        .unwrap_or(host);
    let port = uri
        .port_u16()
        .unwrap_or_else(|| default_port(&scheme).expect("http/https have default ports"));
    Ok(ParsedRemote {
        scheme,
        host: host.to_ascii_lowercase(),
        port,
        path: uri.path().to_string(),
    })
}

/// Origin equality over the one parser: scheme, host, and port (an
/// http→https swap is a different origin; an absent port equals the
/// scheme default). Anything either side fails to parse never matches —
/// an unparseable origin is never "same".
pub(crate) fn origins_match(a: &str, b: &str) -> bool {
    match (parse_remote(a), parse_remote(b)) {
        (Ok(x), Ok(y)) => x.key() == y.key(),
        _ => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_scheme_host_port_path() {
        let p = parse_remote("https://naatre.example.com:7443/team/lib").expect("parses");
        assert_eq!(p.scheme, "https");
        assert_eq!(p.host, "naatre.example.com");
        assert_eq!(p.port, 7443);
        assert_eq!(p.path, "/team/lib");
        assert_eq!(p.origin(), "https://naatre.example.com:7443");
        // Default ports are filled in (and omitted from the origin string).
        let p = parse_remote("https://HOST.example.com/lib").expect("parses");
        assert_eq!(p.host, "host.example.com");
        assert_eq!(p.port, 443);
        assert_eq!(p.origin(), "https://host.example.com");
        let p = parse_remote("http://localhost").expect("parses");
        assert_eq!(p.port, 80);
        assert_eq!(p.path, "/");
        // IPv6 literals lose their brackets in the host and regain them in
        // the origin string.
        let p = parse_remote("http://[::1]:7400/lib").expect("parses");
        assert_eq!(p.host, "::1");
        assert_eq!(p.port, 7400);
        assert_eq!(p.origin(), "http://[::1]:7400");
        assert!(p.is_loopback());
        // Scheme case is normalized by the parse (and by the http crate).
        let p = parse_remote("HTTPS://naatre.example.com/lib").expect("parses");
        assert_eq!(p.scheme, "https");
        assert_eq!(p.origin(), "https://naatre.example.com");
    }

    /// The four sec-C2 regression examples from the review, plus the
    /// port and encoding shapes — each REFUSED, never guessed.
    #[test]
    fn refuses_every_authority_confusion_shape() {
        for url in [
            "https://evil.example?@naatre.example.com/lib",
            "https://a@naatre.example.com:443@evil.example/r",
            "http://lan-host?@127.0.0.1/lib",
            "http://x@127.0.0.1:80@evil.com/r",
            "https://host:99999/lib",
            "https://host:7443x/lib",
            "https://host%2f.evil.example/lib",
            "https://host/lib\\..%2f",
            "https://host/lib#frag",
            "https://user@host/lib",
            "http://[::1%25en0]:7400/lib",
            "ftp://host/lib",
            "host.example.com/lib",
            "https://",
        ] {
            let err = parse_remote(url).expect_err(url);
            assert!(!err.is_empty(), "{url}");
        }
    }

    #[test]
    fn loopback_hosts_exactly() {
        for host in [
            "localhost",
            "LocalHost",
            "127.0.0.1",
            "127.9.9.9",
            "127.255.0.3",
            "::1",
        ] {
            assert!(is_loopback_host(host), "{host}");
        }
        for host in [
            "naatre.lan",
            "10.0.0.1",
            "12.0.0.1",
            "127.0.0.1.2",
            "127.0.0.1.example.com",
            "127.0.0.1.",
            "[::1]",
            "::2",
            "localhost.evil.example",
            "",
        ] {
            assert!(!is_loopback_host(host), "{host}");
        }
    }

    #[test]
    fn origin_keys_scheme_host_port() {
        assert!(origins_match(
            "https://Host.example.com/repo",
            "https://host.example.com:443"
        ));
        assert!(origins_match("http://localhost", "http://LOCALHOST:80"));
        assert!(!origins_match(
            "https://host.example.com",
            "http://host.example.com"
        ));
        assert!(!origins_match("http://host:7400", "http://host:7500"));
        assert!(!origins_match("http://host", "http://host:7400"));
        assert!(!origins_match(
            "https://a.example.com",
            "https://b.example.com"
        ));
        // Unparseable origins never match anything.
        assert!(!origins_match("host.example.com", "host.example.com"));
        assert!(!origins_match("ftp://host", "ftp://host"));
        // The confusion shapes never match the origin they impersonate.
        assert!(!origins_match(
            "https://evil.example?@naatre.example.com/lib",
            "https://naatre.example.com"
        ));
        assert!(!origins_match(
            "https://a@naatre.example.com:443@evil.example/r",
            "https://naatre.example.com:443"
        ));
    }
}
