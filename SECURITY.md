# Security policy

## Reporting a vulnerability

Do **not** open a public issue for a security report.

Report vulnerabilities privately to the repository owner through **GitHub
security advisories** (the repository's "Security" tab → "Report a
vulnerability"). If you cannot use GitHub, contact the repository owner
directly and ask for a private channel.

Please include: the affected commit or release, a reproduction (script or
request sequence), and your assessment of impact. You will get an
acknowledgment and a fix timeline in the advisory thread.

## Known gaps (being fixed on master)

These are open on master now and are scheduled fixes, not accepted
limits:

- Path-scoped tokens restrict writes, but the `log`, `diff` and `trees`
  listings are not yet filtered by scope.
- Path scopes are not bound to a repo; use private repos to contain a
  token to the repos it should see.
- Tokens never expire unless created with a TTL.
- Requests are not rate-limited per client; front the server with a
  rate-limiting proxy.

## Supported versions

Only the latest `master` is supported: naatre is developed without release
branches or maintenance forks at this stage, so security fixes land on
master and users deploy master (or a build of it). Pinned-by-digest
containers built from a given master commit are supported for that
commit's lifetime.

## Verifying release artifacts

Release artifacts carry `SHA256SUMS` (written by `scripts/release.sh`,
the M0 behavior) plus a **minisign** signature of those checksums and a
CycloneDX SBOM. minisign was chosen over cosign deliberately: offline
verification, ed25519, and no registry or keyless infrastructure — the
right shape for a single-binary self-hosted product.

**No production release key exists yet.** The only key in the repository
is [`deploy/naatre-release-test.pub`](deploy/naatre-release-test.pub) —
the **CI TEST key**, used exclusively by the signature selftest
(`scripts/release-verify.sh --selftest` against
`scripts/release-fixture`), which pins the verify mechanics on every
push. It is **NOT a release anchor**: a signature that verifies against
it proves nothing about release trust. Before the first release, the
maintainer generates the production key OFFLINE (`minisign -G`; the
secret key never touches this repository or CI), publishes the public
half in this file, and announces it on the release channel. **Until then,
signatures are untrusted-by-default.**

Verify a download once the production key exists (the artifacts sit in
one directory):

```sh
minisign -Vm SHA256SUMS -p <the published release public key>   # reads SHA256SUMS.minisig beside it
sha256sum -c SHA256SUMS                                         # the signed list covers the binary, the licenses, and the SBOM
```

`scripts/release-verify.sh [--selftest] <artifact-dir> [pubkey]` runs the
same gate: signature against the given key (default: the CI test key —
mechanics only), the checksum lines, and (with `--selftest`) proof that a
tampered `SHA256SUMS` line FAILS verification and that a modified artifact
under a valid signature FAILS the checksum check. To rotate the production
key, generate a new pair, replace the published `.pub`, and announce the
change on the release channel.

## The cache-tier ticket signing key (M13b)

The origin's cache-ticket signing material lives in the data dir:
`cache_signing.pkcs8` (the Ed25519 secret, 0600) beside
`cache_keys.json` (the PUBLIC verify list — current + retired keys, each
with its id). The data dir is the trust boundary (the same boundary as
the token store): anything that can read the data dir can mint tickets.
**Compromised-key containment (M13b-fix)**: a believed-compromised
signing key is contained by (1) `naatre cache-key rotate --data <dir>` —
a rotation retires the suspect key and rows its replacement; the suspect
key's still-unexpired tickets verify until their ≤15-minute TTL — bounded
staleness; (2) `naatre cache-key drop <retired-id> --data <dir>` — removes
the RETIRED key from the verify list entirely (the CURRENT key refuses —
rotate first), so tickets under the suspect key fail at their NEXT verify
instead of living out the TTL; copy the updated list to every tier and
restart them. Both verbs are audited (`cache_key_rotate`,
`cache_key_drop`). If the list itself leaked, rotate once more (mints
move to the fresh key) and re-copy.

**TLS posture, stated plainly (F1 correction round)**: ticket-bearing
requests (`Authorization: NaatreTicket` headers) and tier↔origin object
fetches ride rustls with the platform root stores — both the tier's
origin client and the CLI's cache client speak `https://`, verified
(there is no "skip verification" option). The tier's origin client has
**no `ca_file` flag**: a private CA must chain to the platform roots, so
front an otherwise-private origin with a certificate from a
publicly-rooted CA or a proxy you trust. (The CLI client DOES take
`--ca-file` / `NAATRE_CA_FILE` / a working-copy `ca_file` — the bundle
joins, never replaces, the platform roots — and the cache route rides
the same root set as the client that built it.)

Plain `http://` follows the loopback rule, enforced on EVERY client path
(working-copy verbs included; each refusal is a usage error, exit 3):
loopback hosts are always allowed; a non-loopback host is allowed ONLY
with an explicit opt-in — a working copy's per-remote
`insecure_http = "http://host[:port]"` (the stored ORIGIN, applied to
that origin alone: a `--server`/`NAATRE_URL` override to another host
does not inherit it), set at clone time by `clone --insecure-http` or
afterwards with `naatre config set insecure_http "<origin>"` — or, for
the admin verbs (`token`, `repo`, `principal`: they have no working
copy), the invocation-level `--insecure-http` flag or
`NAATRE_INSECURE_HTTP=1`. The `NAATRE_CACHE_URL` cache route is admitted
for its OWN origin only (loopback, or equal to the stored per-remote
opt-in) — an origin server's opt-in never covers a different cache host.
Every remote URL is parsed once with the same parser that connects, so
userinfo (`@`), query/fragment, backslash, percent-encoded host/port,
and a non-u16 port are refused outright — they cannot smuggle a
different origin past the checks. A ticket in transit stays a
bearer-grade credential for by-id reads within its TTL, so an `https://`
origin is the recommended shape — treat the LAN accordingly.

The verify list carries no secrets and is the file operators copy to
`naatre cache` tiers; a tier holds NO secret material and NO origin
token — its whole authority is verifying ticket signatures. Rotation
(`naatre cache-key rotate`, host-local and audited) retires the old key
but keeps its public half verifying until the ≤15-minute ticket TTL
drains outstanding tickets; restart tiers after copying the rotated
list. Ticket-presenting requests travel as `Authorization: NaatreTicket`
headers — bearer tokens do NOT flow to the tier — so a TLS-fronted
deployment protects ticket traffic the same way it protects token
traffic (README "TLS and reverse proxies").

## Scope notes

- The **data dir is the trust boundary** (deployed 0700): anything that
  can read it can mint admin tokens (`naatre token --data … --new-admin`
  is host-local by design).
- The server speaks plain HTTP; TLS terminates at the reverse proxy you
  put in front (see README "TLS and reverse proxies"). Non-loopback binds
  print a cleartext warning.
- Bearer tokens are stored only as BLAKE3 hashes; `Authorization` values
  and token material are never logged.
- The working-copy symlink defense is check-then-write defense in depth,
  with an accepted TOCTOU window — `docs/design.md` records the posture.
- To report a vulnerability **in a dependency**, open the report anyway;
  the cargo-deny advisories gate in CI is the standing tripwire.
