# naatre

Single-binary, self-hosted source control for agent-heavy repos: a
server-authoritative VCS with changesets as a write-ahead log, content-
addressed blobs, server-side rebase landing on one `main`, thin always-
online clients, and ephemeral snapshots. One static binary, zero required
external services. License: GPL-3.0-only.

- Object formats: [`docs/format.md`](docs/format.md)
- Design notes, limits registry, honest-scope boundaries:
  [`docs/design.md`](docs/design.md)
- API contract: `GET /v1/openapi.json` on a running server (any
  authenticated role); the same document is checked in as
  [`docs/openapi.json`](docs/openapi.json) and CI regenerates + diffs it
- Deploy: [`deploy/`](deploy/) (Dockerfile, systemd unit),
  [`SECURITY.md`](SECURITY.md), [`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md)

## Quickstart

Everything runs from one binary — server and client verbs. CI extracts the
fenced blocks below and runs them end-to-end against a throwaway data dir,
so the quickstart cannot silently drift from the tested path.

```sh
# 1. Create a data dir; prints the admin token exactly once.
naatre init --data ./naatre-data

# 2. Save the token WITHOUT pasting it on the command line: a literal
#    `export NAATRE_TOKEN=naatre_…` lands in your shell history, and
#    `--token` values are visible in `ps`. `read -s` prompts silently.
read -s NAATRE_TOKEN && export NAATRE_TOKEN

export NAATRE_URL=http://127.0.0.1:7400

# 3. Serve in the background (steps 5–6 stop it again via SERVE_PID).
#    Binds 127.0.0.1:7400 by default.
naatre serve --data ./naatre-data &
SERVE_PID=$!
until curl -fsS "$NAATRE_URL/healthz" >/dev/null 2>&1; do sleep 0.2; done

# 4. Create a repo (admin).
naatre repo create demo
```

Prefer watching the server in the foreground? Run
`naatre serve --data ./naatre-data` in its own terminal and stop it with
Ctrl-C (it drains gracefully); skip the `&` and `SERVE_PID` lines above,
and the `kill "$SERVE_PID"` lines below don't apply.

A freshly created repo has an unborn `main` — the refs route refuses to
create `main` (it moves only via land) and land moves but never creates it,
so a new repo is seeded through `naatre import` (offline; stops the server
for the duration — import and serve hold the same data-dir flock):

```sh
# 5. Seed the repo from a local git repo (one-way; see "Git interop").
#    ./my-git-repo stands for any local git checkout with a commit on its
#    default branch.
kill "$SERVE_PID" && wait "$SERVE_PID"   # release the flock
naatre import --data ./naatre-data --into demo ./my-git-repo
naatre serve --data ./naatre-data &      # back up
SERVE_PID=$!
until curl -fsS "$NAATRE_URL/healthz" >/dev/null 2>&1; do sleep 0.2; done

# 6. Clone from a second directory, commit, land.
mkdir ../quickstart-clone && cd ../quickstart-clone
naatre clone "$NAATRE_URL/demo"
cd demo && echo hi > README.md && naatre commit -m "hello" && naatre land

# Done — stop the server (graceful drain).
kill "$SERVE_PID" && wait "$SERVE_PID"
```

Prefer the working copy's 0600 config over the env var? From inside a
clone: `naatre login --server http://127.0.0.1:7400 --token "$NAATRE_TOKEN"`
writes it there (the config is 0600; the `--token` argv is momentarily
visible in `ps`); later verbs in that copy read the config and need no env.

Exposing the server beyond loopback (`--listen 0.0.0.0:7400`) prints a
warning: bearer tokens would cross the network in cleartext. Put a
TLS-terminating reverse proxy in front (see below).

## How naatre differs from git

- **A commit uploads a changeset, not a snapshot.** Changesets are delta
  objects in a server-side write-ahead log; the server derives trees.
- **Nothing reaches `main` until land.** A commit is private to your head.
  `naatre land` sends the head chain to the server, which rebases it onto
  `main` server-side and moves the ref in one transaction. Conflict? A
  structured 409 with a resolution recipe; re-land with the same key after
  resolving.
- **Land rewrites ids.** Rebased changesets get new ids (single-parent,
  generation-bumped). Pre-rebase originals stay fetchable by id and never
  appear in `main`'s log — that is not corruption; GC collects them after a
  grace window (default 30d).
- **Snapshots are ephemeral working state** (default TTL 7d,
  `--ttl`): server-side captures of a working copy for handoff between
  agents or machines. They are not history and never become changesets;
  GC deletes them wholesale past their TTL.
- **`main` moves only via land** (monotonic, server-side rebase); other
  refs move via admin compare-and-swap; force-moves validate
  fast-forward-only.

## TLS and reverse proxies

naatre speaks plain HTTP and assumes a TLS-terminating reverse proxy for
anything non-loopback (the client refuses `https://` URLs — it never
downgrades silently; terminate TLS at the proxy and forward http to
loopback). Two proxy-relevant facts, both deliberate:

- **No peer-address surfaces.** Access logs record method, path, status,
  elapsed, request_id — never a peer address — and are **off by default**
  (they are `debug`-level; enable with `--log-level debug` or
  `NAATRE_LOG=debug`). Auth-failure rate-limiting keys on a hash of the
  *presented credential*, not on addresses, so naatre needs no proxy
  headers to attribute failures and has no `--trust-proxy` flag; there is
  nothing to spoof. The known trade-off (an anonymous flood shares one
  `anon` bucket) is in the limits registry, `docs/design.md`.
- **Rate limiting is your proxy's job** — no milestone adds per-principal
  quotas, so front naatre with a rate-limiting proxy today. Transport caps
  (30 s request budget, 256 concurrent requests, size caps) shed overload
  with retryable 503s; per-client admission control is not built in.

## Backup

The data dir is `naatre.db` (SQLite, WAL) + `objects/` (immutable
content-addressed blobs) + `ephemeral/` (snapshot blobs, TTL'd) + `tmp/`.

- **Stop-and-copy** (simplest): stop the server, copy the whole data dir.
- **Live**: order matters, because a plain file copy of a live WAL database
  (`naatre.db` alone) can be torn. Take a **consistent DB snapshot first**,
  then the objects:
  ```sh
  sqlite3 naatre.db ".backup '/backups/naatre-db.db'"    # or: VACUUM INTO
  rsync -a objects/ /backups/objects/                    # immutable → incremental
  ```
  Blob fsync precedes every referencing row commit (see design notes), so
  a DB snapshot that is older than the `objects/` copy is always safe:
  orphans are harmless, dangling references never happen.
- `ephemeral/` need not be backed up — snapshots expire by design.
- Dedicated `naatre backup`/`restore` verbs land in M11.
Backup copies and the TLS proxy's own access logs are outside the data
dir's erasure guarantee — they are the operator's to handle.

## Git interop

One-way, offline: `naatre import --data <dir> [--into <repo>] <git-path>`.
After an import **naatre is the authoritative repo and git is a read-only
source**. Incremental re-import only ever **fast-forwards** `main` (import
must be the sole writer); a diverged or force-pushed source, or a repo
with native lands, refuses with `git_history_diverged` — no merge, no
rewrite.

Import discloses its two content mutations and its skip behavior:

- `\r\n` → `\n` normalization of commit messages is the **one** content
  mutation.
- Author/message text violating the closed character rule (Unicode Cc/Cf —
  see the path grammar table's sibling rule in `docs/format.md` §3) is
  never a skip: each offending character becomes U+FFFD and the commit is
  listed in the report's `sanitized` array.
- Unsupported content (octopus parents, symlinks, gitlinks, …) aborts the
  import by default with a per-object report; `--skip-unsupported` skips
  the commit **and all its descendants** (the `cascaded` list) — one
  offending early commit skips everything after it.

## Error codes

Every error is one envelope: `{"error":{"code","message","retryable",
"details","request_id"}}`. The registry is closed; the machine-readable
form is the `ErrorCode` enum in `docs/openapi.json`. Recoverable codes
carry recovery data in `details` (e.g. `ref_conflict` carries the current
ref version; land conflicts carry the resolution recipe). 5xx bodies carry
a generic message + `request_id` only.

| Code | HTTP | Meaning |
|---|---|---|
| `token_invalid` | 401 | bearer token missing, malformed, or unknown |
| `token_expired` | 401 | token past its expiry |
| `token_revoked` | 401 | token revoked |
| `forbidden` | 403 | authenticated, not allowed here |
| `not_found` | 404 | resource or route not found |
| `repo_not_found` | 404 | no repo with that name |
| `repo_exists` | 409 | repo name taken |
| `object_not_found` | 404 | no such object in this repo's membership |
| `ref_conflict` | 409 | ref CAS lost (stale version) |
| `idempotency_key_settled` | 409 | land key already settled with a different payload |
| `dirty_working_copy` | 409 | sync/restore refused: local edits or colliding untracked files |
| `git_history_diverged` | 409 | import graft refused: histories diverged |
| `conflict_markers_present` | 409 | commit content contains conflict markers (M12) |
| `malformed` | 400 | structurally malformed payload |
| `validation` | 422 | semantic validation failure |
| `path_not_normalized` | 422 | path is not NFC |
| `empty_changeset` | 422 | create with an empty file map |
| `not_fast_forward` | 422 | ref move is not a fast-forward |
| `unknown_parent` | 422 | parent changeset not in this repo |
| `duality` | 422 | path collides with a file/dir in the parent tree |
| `unsorted_changes` | 422 | changeset paths not strictly sorted |
| `duplicate_path` | 422 | same path twice in one changeset |
| `too_large` | 413 | body or blob over the size cap |
| `snapshot_expired` | 410 | snapshot past its TTL |
| `object_collected` | 410 | object collected by GC after the grace window (M10) |
| `object_redacted` | 410 | object redacted (M10 tombstones) |
| `writer_busy` | 503 | single-writer slot busy; retry with backoff |
| `derivation_pending` | 503 | derived data still computing (M6) |
| `request_timeout` | 503 | request shed at the 30 s budget; retry |
| `server_busy` | 503 | at the concurrent-request cap; retry |
| `internal` | 500 | internal error; specifics stay in server logs |

Retryable 503s carry `Retry-After: 1`; back off and retry.

## Path grammar

Enforced server-side at changeset create and import; client-side at
materialization. First failure wins. Full specification with the character
rules: `docs/format.md` §2–3.

| # | Rule |
|---|---|
| 1 | not empty |
| 2 | relative — no leading `/` |
| 3 | no backslash `\` |
| 4 | no NUL byte |
| 5 | no character of Unicode category Cc (control) or Cf (format), including `\n`/`\t` |
| 6 | exactly NFC-normalized (`path == nfc(path)`) |
| 7 | no component `.` or `..` |
| 8 | each component ≤ 255 UTF-8 bytes |
| 9 | no empty component (no `//`) |
| 10 | total ≤ 4096 UTF-8 bytes |
| 11 | first component not `.naatre` or `.git` |

Additionally rejected at create: case-folded duplicates of an existing
sibling in the same directory (macOS/Windows safety), and file/dir duality
violations (add under a file, modify a directory, delete an absent path).
Repo names: `^[a-z0-9][a-z0-9-]{0,62}$`. Ids: 64 lowercase hex at every
boundary.

## Erasure: what carries personal data

Destroying the data dir erases everything in it. Backup copies and the TLS
proxy's own access logs are **not** covered — the operator owns those.
History is append-only until M10's redaction. Personal-data carriers,
explicitly:

- **`changesets.author`** — the client-supplied `Name <email>` string, and
  the same names/emails as imported from git.
- **`changesets.canonical`** — the immutable canonical bytes embed the same
  author string, so out-of-band surgery that rewrites only the columns
  above leaves the author in the blob; the surgery must rewrite `canonical`
  alongside the `author` column (M10's tombstone + purge handles the
  canonical bytes instead).
- **Principal-attributed columns** — `changesets.principal`,
  `refs.last_principal`, `lands.principal` + `lands.result_json`,
  `snapshots.principal` (TTL-bounded by the snapshot TTL, but with no
  enforced maximum TTL).
- **The `principals` table** and **revoked-but-retained `tokens` rows**.
- **`audit_log`** — `principal` plus the actor fields in `details_json`.
  The log is append-only and retained **indefinitely on a
  security-accountability basis**; principal deletion (M10) pseudonymizes
  the actor rather than deleting rows.
- **Snapshot blob content** expires at the TTL (rows go with it).

Until M10 (GC + redaction + principal deletion), per-subject erasure on a
shared instance is out-of-band database surgery — same framing as backward
ref moves. On a single-person instance, deleting the data dir is complete
erasure.

## Distribution note (conditional)

naatre ships as FOSS under GPL-3.0-only; this project's own distribution
practice is non-commercial (the license itself permits commercial
conveyance). If *you* distribute it commercially in the EU, the Cyber
Resilience Act's Annex I manufacturer obligations may attach to that
distribution — security-update support for the expected product lifetime,
reporting actively exploited vulnerabilities, and conformity assessment —
assess for your own products.

## License

GPL-3.0-only. Dependency license coverage: `cargo deny check licenses` in
CI; the third-party table is [`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md).
