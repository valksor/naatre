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
