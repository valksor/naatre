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
minisign -Vm naatre-<ver>-x86_64-unknown-linux-musl \
    -p <the published release public key>   # reads SHA256SUMS.minisig beside it
sha256sum -c SHA256SUMS                     # after unpacking the artifacts
```

`scripts/release-verify.sh [--selftest] <artifact-dir> [pubkey]` runs the
same gate: signature against the given key (default: the CI test key —
mechanics only), the checksum lines, and (with `--selftest`) proof that a
tampered `SHA256SUMS` line FAILS verification. To rotate the production
key, generate a new pair, replace the published `.pub`, and announce the
change on the release channel.

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
