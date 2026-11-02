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
  GC deletes them wholesale past their TTL. A restore re-walks the
  snapshot's closure on every tree fetch — O(n²) server-side row loads for
  an n-tree snapshot, fine at the current caps (limits registry,
  `docs/design.md`).
- **`main` moves only via land** (monotonic, server-side rebase); other
  refs move via admin compare-and-swap; force-moves validate
  fast-forward-only. A ref write onto a tombstoned target answers 410
  `object_redacted` — a ref would stall that changeset's purge forever.
- **Conflicts can be recorded, not only refused (M12).** Default land is
  unchanged: a rebase conflict still answers the structured 409 with the
  resolution recipe (which leads with the opt-in). Explicitly setting
  `record_conflict` in the land request body — the CLI's
  `naatre land --record-conflict` — records the conflict on `main` as a
  conflicted changeset — a logical conflict stored inside the changeset
  payload (format v2), whose tree holds conflict markers; the land still
  settles exactly once, with land status `conflicted`. Conflicted nodes
  rebase like any node and the conflict travels; a conflict with a
  deletion side is not recordable and still refuses. **The client side
  (M12b):** syncing/cloning a conflicted main materializes the markers
  (the conflicted node's root tree IS the markers-materialized tree);
  `naatre status` marks the conflicted paths (`C <path>`); a commit of
  marker-shaped content refuses with `conflict_markers_present`; the
  resolution is `naatre resolve <path>` — the resolved content commits as
  an explicit resolution changeset referencing the conflicted head, and
  the following `naatre land` advances main past the conflict (linear
  history). **Mixed-fleet honesty:** the documented skew refusal
  (`version_skew_conflicted`) is emitted by NO shipped binary — it exists
  only in v2-aware-lineage client builds that lack the v2 capability
  (unit-pinned as a contract); an UNCHANGED pre-M12 client binary has
  neither the detection code nor the marker gate, so it syncs a conflicted
  main as plain marker text and can commit those markers as ordinary
  content. Operators should upgrade clients before enabling
  `record_conflict` flows (a server-side marker-shape check at land is
  the recorded follow-up). Formats: `docs/format.md` §8; semantics:
  `docs/design.md` ("First-class conflicts").
- **Deploy note for format v2 (M12a): the schema upgrade is a ONE-WAY,
  store-wide lockout.** The first boot of a format-v2 binary upgrades the
  data dir to `schema_version` 12; no older binary can ever open that
  data dir again — even one holding only v1 data. Rollback after the
  first upgrade means restoring a pre-upgrade backup (`naatre backup`
  first). **Take a backup before upgrading.** The GC mark traverses
  conflict representations (M12b): a conflicted changeset's rep side/base
  blobs stay reachable through collection, so a conflicted land is safe
  to run GC against; tombstoned content purges exactly as before
  (erasure is never held).

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

## Backends: Postgres metadata, S3 blobs (M9)

The zero-config default needs nothing: SQLite metadata + filesystem blob
CAS, all inside the data dir. Larger deployments select backends with a
`backend.toml` file in the data dir (**0600** — it carries credentials;
naatre refuses to read a looser one):

```toml
[backend]
meta = "postgres"        # or "sqlite" (default when the file is absent)
blob = "s3"              # or "fs" (default when the file is absent)
dsn = "postgres://db.internal:5432/naatre?sslmode=verify-full"
user = "naatre"          # or NAATRE_PG_USER
password = "…"           # or NAATRE_PG_PASSWORD — never in the URL
sslrootcert = "…"        # optional: PEM CA bundle (0600) for verify-* with a private-CA server
endpoint = "minio.lan:9000"  # S3: bare host[:port]; absent = AWS defaults
bucket = "naatre-history"    # S3: required
region = "us-east-1"         # S3: optional (driver default us-east-1)
access_key_id = "…"          # S3: or NAATRE_S3_ACCESS_KEY_ID
secret = "…"                 # S3: or NAATRE_S3_SECRET — never in a URL
path_style = true            # S3: path-style URLs (minio); default virtual-host
allow_plaintext = false  # the explicit plaintext opt-in (see below)
gc_interval_secs = 3600   # reachability GC background-pass interval; 0 disables
gc_grace_secs = 2592000   # collection grace window (the 30-day default)
```

## Layout: one DB or one per repo (M13a)

The metadata layout is also config-selected, defaulting to `single-db`
(today's shape: every table in `naatre.db`, still **single-writer** —
documented profile limit). The alternative splits the metadata across
SQLite files: a **catalog** (`catalog.db`) holding the global tables —
principals, tokens, the audit log, the repo registry (each repo's db
path), the global `objects/` CAS ledger and the GC's `gc_pending` ledger —
plus **one SQLite file per repo** (`repos/<id>.db`) holding that repo's
changesets, trees, refs, lands, snapshots, derived rows, and its
redactions. The `objects/` CAS itself is untouched by the split: bytes
stay global, refcounts stay cross-repo, and the GC mark walks every repo
file through the registry and unions reachability before sweeping the
shared CAS once (the never-dangling invariant holds across files).

```toml
[backend]
meta = "sqlite"
[layout]
mode = "db-per-repo"     # or "single-db" (the default)
```

**When to choose it**: many repos on one server whose write bursts contend
for the single writer — with `db-per-repo` each repo lands on its own
write slot, so two repos commit concurrently (same-repo writes stay
serialized, exactly the single-DB behavior per repo). SQLite-only: a
Postgres metadata backend never splits (the config parser refuses the
combination). Per-repo connections carry WAL + the same durability
pragmas; each repo file holds a small read pool, the catalog a larger one
(the cost rows live in `docs/design.md`). Repo create/delete serialize on
the catalog — under live lands that is the documented contention point.

**Adopting it on an existing single-DB store** — the offline migration:

```
naatre migrate-db-per-repo --data /var/lib/naatre
```

Run with the server STOPPED (the flock refuses a live data dir). The tool
copies each repo's rows into fresh per-repo files, builds the catalog,
verifies row-count parity per table per repo, and runs a full `fsck`
against the new layout (refusing to leave anything but a clean verdict),
then prints the cutover steps:

1. add `[layout] mode = "db-per-repo"` to `backend.toml`;
2. delete the old `naatre.db` (keep a `naatre backup` copy until the
   post-cutover fsck passes);
3. start naatre — the startup banner names `layout db-per-repo`.

Until the config flip the server keeps serving from the single DB: a
crashed or abandoned migration leaves a consistent, servable store, and
the original `naatre.db` is never touched by the tool. Backups cover the
split: `naatre backup` snapshots the catalog and every repo file (the
coordinated snapshot under the same flock) plus the objects, and
`naatre restore` refuses a layout-combo mismatch (a single-DB backup into
a db-per-repo target and vice versa). Where erasure data lives per
layout: `redactions` rows follow their repo's file; `audit_log` and the
`objects` ledger stay in the catalog — the erasure enumeration below
applies unchanged, with tombstones deleted with their repo's file.

- **Postgres 16+** uses the same schema via its own migration set (applied
  at boot, same version guard). Migration parity is a tested property.
- **TLS by default.** The DSN's `sslmode` defaults to `require`;
  `verify-ca` and `verify-full` are supported; `sslmode=disable` is
  refused unless `allow_plaintext = true` is set (the LAN-dev opt-in).
  `allow`/`prefer` are not supported — pick one posture. `verify-*`
  verify the server certificate against the **platform root store**
  (public CAs) plus the optional `sslrootcert` file key — a PEM CA
  bundle, 0600, added to the roots. That key is how a self-hosted
  Postgres with a private CA reaches `verify-full`; without it,
  `verify-*` can only verify publicly-trusted certificates. Production
  wants `verify-full` (+ `sslrootcert` where the CA is private);
  `require` encrypts but authenticates nothing.
- **Inline credentials are rejected**: `postgres://user:password@host/db`
  fails at parse. The password goes in the `password` key or
  `NAATRE_PG_PASSWORD` (the file wins over the environment; env vars:
  `NAATRE_PG_DSN`, `NAATRE_PG_USER`, `NAATRE_PG_PASSWORD`).
- **Credentials never surface**: the startup banner, request logs, and
  error strings carry the backend *kind* name only ("metadata backend:
  postgres"). Driver errors are redacted by construction — a Postgres
  connect/query failure prints a classified, parameter-free message, so
  no DSN/endpoint/user can leak through the driver's error text.
- **A data dir never switches backends in place**: an existing
  `naatre.db` under `meta = "postgres"` (or a missing one under
  `meta = "sqlite"`) is refused, naming both sides.
- **Single-writer scope**: the in-process slot serializes writers per
  server, and every DESTRUCTIVE write section — land's transaction, the
  reachability GC's collection batches, and the blob-upload record step —
  takes the same fixed Postgres advisory lock (5 s timeout) via `BEGIN
  IMMEDIATE` semantics across processes: a second destructive writer
  blocks, then answers the same retryable `writer_busy`-shaped 503 as
  SQLite lock contention. The data-dir flock keeps same-dir processes
  fully exclusive. What the advisory lock does NOT make safe is running
  two live server instances against the SAME DSN or the SAME bucket:
  the lock serializes individual write transactions, but the GC's
  mark-then-sweep and grace-ledger reasoning assume one writer process —
  two instances can silently lose data each other is still referencing
  (cross-instance silent loss). One naatre instance per Postgres
  database, and one per bucket (or bucket key prefix): exclusive, by
  deployment, not by locking.
- **S3-compatible blob CAS** (`blob = "s3"`, M9b): history blobs live at
  `objects/ab/cd/<62hex>` in the bucket and snapshot blobs at
  `ephemeral/<snap>/objects/…` — the same layout and zstd encoding as the
  fs CAS, so a backend migration is a byte-for-byte copy of `objects/`
  into the bucket (a data dir holding fs objects is never switched in
  place: the mismatch refusal names both sides; start a fresh data dir or
  copy out-of-band). **TLS by default**: an `endpoint` is reached over
  HTTPS unless `allow_plaintext = true` (the LAN-minio opt-in); without an
  `endpoint`, AWS defaults are used (always TLS). `path_style` selects
  path-style URLs for minio-shaped endpoints. GC of the ephemeral
  namespace deletes by key prefix with the same both-gates semantics as
  the fs dirs.
- **Two TLS stacks ship** (stated honestly): Postgres rides
  `native-tls` (OpenSSL/Security.framework — every rustls connector in the
  ecosystem targets tokio-postgres, none the sync client naatre uses), S3
  rides `object_store`'s rustls. Static-musl builds therefore need an
  OpenSSL for the target — relevant to release/Docker images.
- `naatre init`/`token`/`import` honor the same file, so admin verbs work
  unchanged against a Postgres-backed dir (there is no `naatre.db` in it).

**Erasure on external-metadata deployments**: with `meta = "postgres"`,
the metadata — including every personal-data carrier enumerated below —
lives in the Postgres database, OUTSIDE the data dir. Destroying the data
dir erases only the blobs and config; the metadata erasure story is the
Postgres deployment's (drop the database, and its backups). The S3 CAS
(M9b) extends this to blob contents.

## Garbage collection and fsck

`serve` runs a reachability GC pass hourly (off-switch/interval:
`backend.toml` `gc_interval_secs`; `0` leaves collection to the manual
verb). The mark reads the primary tables only — ref targets, the
changeset graph, unexpired snapshots, lands, `git_map` — never the
advisory indexes. Data that no seed reaches becomes a ledger candidate
and collects only after the **grace window** (`gc_grace_secs`, default
30 days), so in-flight client work is never overtaken. Blob deletion is
two-phase (condemn in one pass, delete in a later one) and an upload
that races the sweep answers a retryable 503 `blob_swept` — the client
re-PUTs, idempotent at a content-addressed URL. Pre-rebase originals
(the pre-rewrite heads a conflict leaves behind) are garbage from the
moment no seed reaches them and collect after the window; GC writes one
`gc` audit row per collecting pass, actor `system`.

Two host-local verbs operate on a stopped data dir (same flock as
`serve`/`import`):

```sh
naatre gc --data ./naatre-data [--json]     # one full pass, to convergence
naatre fsck --data ./naatre-data [--json]   # consistency check; exit 0 clean, 1 findings
```

**Redaction** (M10b) rides the same pass: `naatre redact blob|changeset
<id> --reason R --repo <name> --data ./naatre-data` tombstones one
object in one repo — every serving path then refuses it
immediately (by-id fetches answer 410 `object_redacted`, `/log` and
path-history drop the node, diff/file degrade structurally, blame
degrades at the tombstone with the author withheld, a snapshot holding
the blob serves its tree listing masked and restores only its available
files — the redacted blob's own snapshot fetch answers 410
`object_redacted` immediately, TTL irrelevant, and the content never
returns — tree listings mask the redacted entry's pointer, size, and
exec bit (the name serves by design; the entry's own size, exec bit,
and hash do not, and a directory whose aggregate would embed a redacted
file's byte length serves size-null — subtracting children from a
parent listing recovers nothing), and a land whose base chain crosses
it answers 409 `redacted_base`) — while the GC's
redaction phase reclaims the bytes/rows later, and only when NO repo
still references the content (two repos sharing a subtree redact
independently; the other repo keeps serving).

**The tombstone outlives the purge, and the gate never lapses**: content
once redacted in a repo never serves there again. Because content is
addressed by hash, a same-content re-upload recreates the same id — so
every write boundary refuses a tombstoned id (active or purged): blob
PUTs (raw and batch), changeset creates naming the id or referencing the
blob, snapshot blob uploads and snapshot POSTs carrying it in their tree
closure, ref writes onto the id, a land whose rebase body reproduces the
id or references the blob, and git imports. The refusal is PER-REPO, like
the gate: a
tombstone binds only the repo that marked it, so a repo that never
redacted the content keeps serving it and may re-upload it after another
repo's purge reclaimed the bytes (changeset ids cannot collide this way —
they hash the repo id). `POST /objects/present` reports a tombstoned id
as `redacted`, never `missing` — sync clients are never invited to
re-upload. A re-mark on a purged tombstone RE-ARMS it (a fresh
`marked_ns` and a new `redact` audit row — never a silent no-op), and
the purge state clears only while something remains to purge: a blob
re-arm re-enters the purge work list, while a fully purged CHANGESET's
re-arm keeps the tombstone purged (re-stamped — nothing remains to
purge, the write gates stay armed); a re-mark naming the other kind is
refused. The
tombstone row itself — path + date only — is retained indefinitely as
the redaction record; the purge stamps `purged_ns` but does not delete
it.

Remote administration
goes through `POST/GET /v1/repos/{r}/redactions` (admin, audited):
the tombstone ROW carries path + date only — never an author, no
reason, no actor (free text can carry personal data; the actor and the
reason live ONLY in the audited `redact` action, which is the
accountability record). Admins: do not put data-subject names in the
free-text reason — the reason is retained indefinitely with the audit
row, so name the content (path or id), not the person.

`fsck` is read-only: dangling-reference anti-joins (a nonzero count
anywhere is dirt), reachability counts (total vs marked), CAS presence
vs the DB inventory (missing bytes for a DB-known blob dirty the store),
the pending-candidate ledger, the cumulative GC audit totals, and the
redaction counts (tombstoned-but-unpurged is normal; a tombstone stamped
purged whose object still exists from before the stamp is DRIFT and dirties
the store — an object row touched at or after the purge stamp is a
never-redacted repo's legitimate re-upload, not drift). On the
S3 backend it attaches the external-drift note: bucket-side lifecycle
rules or versioning can delete or resurrect objects outside naatre's
control — that is drift to investigate, never silently repaired.

## Backup and restore (M11)

The data dir is `naatre.db` (SQLite, WAL) + `objects/` (immutable
content-addressed blobs) + `ephemeral/` (snapshot blobs, TTL'd) + `tmp/`.

**The verbs** — `naatre backup --data <dir> <dest>` and
`naatre restore --data <target> <backup-dir>` — snapshot the whole data
dir for every backend combo into ONE backup directory: `manifest.json`
(format, naatre version, backend combo as enum strings, schema version,
timestamp, counts, every history-CAS object id, and the SHA256 of every
artifact) plus the artifacts below. **The manifest carries no credentials
and no connection config** — the combo is `sqlite|postgres` × `fs|s3`
strings only; `pg_dump`/`psql` get their password via the `PGPASSWORD`
environment (or the operator's connection service file), never on a
command line. The backup directory is created **0700** and every artifact
is **0600** — it holds the whole instance's personal data. Both verbs take
the data-dir flock, so they are stop-and-copy by construction — a running
`naatre serve` holds the same lock; stop it first. `dest` must not exist
(a backup is never overwritten), and a torn backup (no manifest — it is
written last) is refused at restore time.

**The safe order: consistent DB snapshot FIRST, then the immutable
objects.** The verb enforces it; the argument is the same on every combo:
blob fsync precedes every referencing row commit (durability invariant 1),
so a DB snapshot older than the object copy leaves at most harmless
orphans, never dangling references.

- **sqlite + fs**: a consistent single-file snapshot via `VACUUM INTO`,
  then `objects/` copied per the manifest. **Never an in-place file copy
  of a WAL database** — a plain `cp` of a live `naatre.db` can be torn;
  the snapshot is a fresh, self-contained file.
- **postgres + fs**: one `pg_dump` run — a single MVCC-consistent
  snapshot — then `objects/`. `pg_dump` (and restore's `psql`) are
  **documented external dependencies of Postgres deployments** (the same
  convention as git for import); a missing binary is a clean operator
  error naming the tool. A missing/mismatched `pg_dump` is refused before
  anything is written; on a dump failure the tool's stderr lands in
  `tool-error.log` (0600) inside the new backup dir and naatre's own error
  carries the exit status only (connection errors can name the host and
  user). **Backup of an uninitialized Postgres target is refused**,
  naming `naatre init` — the probe connects without creating anything, so
  no empty schema is migrated into existence by the check. Any OTHER
  failed backup removes its manifest-less partial dir; one that kept a
  `tool-error.log` stays, and the next run's error names it as the
  leftover of a failed backup. **Role hygiene**: `pg_dump` runs with
  `--no-owner --no-privileges` (psql, the replay tool, takes no such flags,
  so a role-clean dump is what makes the replay role-independent): the
  restored objects belong to the **restoring role**, and a restore under a
  different role than the one that dumped succeeds. naatre assumes a
  **dedicated database** per instance (production stores use the DSN
  database's default schema, never a named one): `pg_dump` dumps that whole
  database, and the replay recreates it in the target database.
- **sqlite + s3 / postgres + s3**: the DB snapshot first; `manifest.json`
  then lists the object ids and **the bucket copy is the operator's duty**
  — naatre does not duplicate a bucket into the backup dir (out-of-band S3
  tooling, e.g. `aws s3 sync`/`mc mirror`). **Sync the WHOLE bucket**, not
  the manifest's ids one by one: objects are immutable, so incremental
  whole-bucket syncs between backups are cheap, and a whole-bucket copy
  also carries any object written between the DB snapshot and the sync.
  Stragglers are caught after the fact: restore's presence gate refuses
  before it writes anything, and `naatre fsck` reports a referenced object
  missing from the bucket as `bytes_missing`. The same caveat as the
  erasure section below applies: external bucket lifecycle/versioning
  policies can violate invariants naatre cannot see — that note lives in
  "Erasure".
- `ephemeral/` is never backed up — snapshots expire by design (TTL),
  and fsck does not scan that namespace.
- The **DB-per-repo coordinated snapshot** (one backup spanning several
  repos' databases atomically) lands with M13a; these verbs back up one
  data dir's store.

**Restore** validates the manifest, refuses a backend-combo mismatch with
the target's config (naming both sides), and refuses a non-empty target —
naming what it found — unless `--force` is given. The manifest's artifact
paths are checked at parse time: only `naatre.db`, `postgres.sql`, and
`objects/<2 hex>/<2 hex>/<60 hex>` (a 64-hex id split 2/2/60 across the
shard dirs and the file name) are accepted — absolute paths, `..`/`.`
components, and every other shape are refused with the target untouched.
**Combo mismatch after a wipe**: the combo comes from the target's
`backend.toml`. After a full wipe of a **non-default** data dir, recreate
`backend.toml` (or do not wipe it) before restoring — the error says so.

*What the restore claims, and what pins it*:

- **Trust boundary**: the manifest's checksums catch a corrupted or
  tampered ARTIFACT; the manifest itself is unsigned, so someone who can
  rewrite the whole backup directory (manifest included) is outside what
  the check proves — keep backup dirs 0700 and treat them as trusted
  input (signing manifests is a recorded followup).
- **A corrupt, tampered, or too-new backup leaves the target untouched.**
  Every artifact is copied into a staging directory (streamed; the
  checksum covers the STAGED copy, so no window exists between hashing and
  copying) and SHA256-verified against the manifest BEFORE any write to
  the target. A backup whose schema version is newer than this binary
  supports is refused from the staged snapshot (SQLite) or the staged dump
  (Postgres; a dump whose version cannot be read is refused too — it is
  never assumed old) before anything is written.
- **A failed Postgres replay leaves the uninitialized target it started
  on.** `psql` runs the dump in ONE transaction (`-1`), so a failure
  anywhere in the replay rolls back every statement, schema included, and
  an immediate re-run works. **Postgres targets** are refused while the
  database already holds the naatre schema, and that refusal holds **even
  with `--force`**: dropping the schema IS the wipe. `psql`'s stderr lands
  in `tool-error.log` (0600) beside the target (its parent directory) —
  restore never writes into the backup dir; if that log cannot be
  written, the error says so after the real `psql` status.
- **SQLite: leftover WAL/SHM sidecars never replay.** A target holding
  `naatre.db-wal`/`-shm` — even without a `naatre.db` — counts as
  non-empty (refused without `--force`, the refusal names them), and a
  forced restore unlinks them before the snapshot lands, so the restored
  store carries exactly the snapshot's content.
- **Restore answers only when the target holds every manifest object.**
  On S3 the presence gate runs BEFORE any target write: an incomplete
  bucket copy is refused with the target still uninitialized, naming the
  missing ids. An S3-configured target whose local `objects/` holds data
  is the backend mismatch, refused regardless of `--force`. fs objects are
  fsynced as they land, like the DB.

Then the verified DB snapshot is moved into place and the objects follow.
Restore checks every manifest id is PRESENT in the target CAS (not
reachability), writes one `restore` audit row into the restored store
(manifest format, backup timestamp, combo, the waived ids if any, and the
restored object count — nothing else), prints the pre-redaction duty
reminder keyed to the backup's age (below), and points the operator at
`naatre fsck` — restore never silently skips verification.

**S3 purge race — the two-step waiver.** If a redaction purge already ate
bucket objects the manifest lists, no bucket copy can ever satisfy the
presence gate. The refusal lists the missing ids AND a waiver argument (a
short digest of that exact set):
`naatre restore --data <target> <backup-dir> --allow-missing <digest-or-ids>`
accepts it — the argument is either the digest or the comma-separated id
list. The waiver covers EXACTLY the set it was issued for: if the missing
set differs on the second run (a purge landed in between), restore refuses
again and prints the NEW set and its digest; a purge is never silently
waived. Waived ids appear in the restore output and the `restore` audit
row, and the redaction duty below applies to them.

**GC, purges, and the copy phase**: plain GC cannot race a fresh backup
within `gc_grace_secs` (bytes reachable at snapshot time survive the
grace window), and the flock keeps host-local GC/fsck/import out
entirely. A **redaction purge** between the DB snapshot and the object
copy CAN delete bytes immediately — per backend: on fs the copy phase
records the vanished id in the report (the backup does not fail; the
manifest lists only what it carries, and restore of it succeeds with the
store dirty until the duty below runs); on S3 the bucket copy is the
operator's and restore's presence gate names any gap. The report flags a
copy phase that started past `gc_grace_secs`.

**Operator duty — restore of a pre-redaction backup**: a backup taken
before a redaction carries the content and NOT the tombstones, so
restoring it brings redacted content back. Re-apply the redactions
(`naatre redact …`, the audit trail lists them): the tombstones gate again
immediately (every serving path and the import boundary refuse the
content), and the next GC pass re-purges the bytes. Backups taken AFTER a
redaction carry the tombstones and re-purge (or stay gated) on their own.
Every restore prints this duty keyed to the backup's age (redactions
recorded only in the destroyed store must be re-applied; `fsck` cannot
detect them). Backup copies — and the TLS proxy's own access logs — are outside the
data dir's erasure guarantee; they are the operator's to handle.

## Git interop

One-way, offline: `naatre import --data <dir> [--into <repo>] <git-path>`.
After an import **naatre is the authoritative repo and git is a read-only
source**. Incremental re-import only ever **fast-forwards** `main` (import
must be the sole writer); a diverged or force-pushed source, or a repo
with native lands, refuses with `git_history_diverged` — no merge, no
rewrite. An import into a repo that has redacted content the source still
carries refuses too: a tombstoned blob or changeset id (active or purged)
fails the import naming the id — redacted content never re-enters through
the import door. Per-commit durability means the commits before the
refusing one stay imported. Whether a re-run helps depends on the cause:
transient failures (a lost race, a busy server) are resumable — re-run
and the fast-forward continues from where the run stopped; a redaction
refusal is PERMANENT — no un-mark exists, so the content must be removed
from the source history itself before that commit can ever import.

Import discloses its two content mutations and its skip behavior:

- `\r\n` → `\n` normalization of commit messages is the **one** content
  mutation.
- Author/message text violating the closed character rule (Unicode Cc/Cf —
  see the path grammar table's sibling rule in `docs/format.md` §3) is
  never a skip: by default each offending character becomes U+FFFD and the
  commit is listed in the report's `sanitized` array. With `--sanitize`
  the escape is **lossless** instead: every offending character becomes a
  `\u{hex}` escape (literal backslashes doubled), the report's `escaped`
  entries name each escaped field and code point, and decoding the stored
  text (the same codec, invertible) returns the original bytes — modulo
  the `\r\n` normalization above, which applies before sanitization in
  both modes. Decode only objects the report's `escaped` list names: a
  string with no offending character is stored unchanged, and decoding it
  would corrupt a literal backslash.
- Unsupported content (octopus parents, symlinks, gitlinks, …) aborts the
  import by default with a per-object report; `--skip-unsupported` skips
  the commit **and all its descendants** (the `cascaded` list) — one
  offending early commit skips everything after it.

**Legacy imported stores**: imports older than the M11 record step left
referenced blobs without their `objects` row — fsck reports them as
dangling (`membership_row_object_missing`, `file_change_blob_row_missing`,
`tree_blob_row_missing`). The one-time repair is
`naatre fsck --data <dir> --repair`: for each blob the store REFERENCES
whose CAS bytes are present, it backfills the row (sizes verified through
the blob store's read path) and writes ONE audited `fsck_repair` row
naming the count (the count only — no ids, no paths, no content). Two
exclusions, pinned: an id with a redactions row in ANY repo (active or
purged) NEVER gains a row — the objects table is global, and backfilling a
tombstoned id would resurrect purge drift; an id whose CAS bytes are
absent gains no row (the size comes from the bytes) and stays a finding.
The repair is scoped to exactly that dirt class — every other fsck finding
stays a finding — and a second run is a no-op.

## MCP server

`naatre mcp` (run inside a working copy) serves the client verbs as MCP
tools over stdio: one JSON-RPC 2.0 message per line on stdout; diagnostics
go to stderr (`MCP_LOG=1` enables per-request lines). The session is bound
to one checkout — the working copy it starts in, whose `.naatre/config.toml`
supplies the server and token (no server override; no tool accepts one).
Tools: `clone`, `sync`, `sparse`, `status`, `log`, `diff`, `cat`, `ls`,
`commit`, `land`, `snapshot`, `restore`, `blame` — the read tools' inputs
mirror the HTTP API's query parameters (`docs/openapi.json`).

Auth: the session uses the working copy's stored token, or `--token` on the
command line (**argv is visible in `ps` — prefer the token store**).
**Use a least-privilege scoped token**: `naatre token create agent \
--role writer --scope src` mints a token that can only write under
`src/` of that repo — an agent leaking or misusing it cannot touch other
repos or paths. (A single trailing slash on `--scope` is trimmed by the
server; a doubled one is a 422.) `--read-only` runs the allowlist mode:
only the pure reads
(`status`, `log`, `diff`, `cat`, `ls`, `blame`) answer; every other tool
returns an error naming the flag.

One writer per working copy: the session holds `.naatre/wc.lock` (an
advisory flock) for its whole lifetime, and every CLI working-copy verb
takes the same lock while it runs — a second writer refuses with an error
naming the lock instead of racing on `.naatre/state.json`. A killed
process releases the lock; nothing to clean up.

## Virtual checkout: `naatre mount`

`naatre mount <repo> [--at REF]` serves a ref as a lazily-hydrated
filesystem over **NFSv3**: directories load on first list, files on first
read, and every hydrated blob is hash-verified against its id. Sizes and
exec bits come from tree entries, so stat never fetches content. The ref
is **pinned at mount time** — the daemon resolves it once at startup and
serves that tree for its whole life; advancing `main` afterwards is
invisible to a running mount (a new mount picks up the new ref). Ctrl-C or
SIGTERM stops the daemon. The same VFS layer runs in-process in the test
suite (no kernel mount needed — that is how CI exercises it, including a
byte-exact mount-vs-clone parity walk, a cold-mount cargo build of
naatre's own sources, and corrupt-cache recovery).

**Platform front-ends.** NFSv3 ships everywhere; Linux also has the FUSE
front-end: `naatre mount <repo> --fuse /mnt/point` (UNSTABLE) mounts
through `fuser` (pure Rust, no libfuse; needs `/dev/fuse` and the
`fusermount3` helper). A FUSE mount has **no network surface at all** and
only the mounting user can access it — the AUTH_SYS caveat below does not
apply. Stop it with SIGINT/SIGTERM (unmounts first) or an external
`umount /mnt/point`; the writable flush runs either way.

**Real-mount e2e.** Where the platform allows it, the test suite mounts
the daemon's export with the REAL kernel client — byte parity vs a
materialized clone (readdir + read + stat sizes + exec bits), a cold
`cargo build` with sources served off the mount, and the writable
`--flush-land` flow. When the environment refuses the mount or suppresses
data operations, the tests SKIP WITH A PRINTED NOTICE (never silently);
under root a mount failure is treated as a real defect. Note for macOS:
`mount_nfs` defaults to a reserved source port (`resvport`), which needs
root — the printed macOS line carries `noresvport` so a normal user can
mount; some macOS builds accept that unprivileged mount but then suppress
data operations (metadata-only) until the mount is made by root.

Security/bind: the daemon binds **127.0.0.1 on an ephemeral port** with an
unguessable per-mount export path (`/naatre/<32 random hex>`), and prints
ready-to-paste `mount_nfs` (macOS) and `mount` (Linux) command lines. A
non-loopback `--listen` is explicit opt-in and prints the same cleartext
warning `serve` does. **AUTH_SYS authenticates nothing**: NFS grants by
network identity alone, so anyone who can reach the port can read the
export (and write to a writable one) — keep mounts on loopback or behind
a network you control. Files in the view belong to the mounting user.
What the secret path protects, precisely: **mounting** requires it, and
NFS file handles embed a per-mount 128-bit random secret (a forged handle
— e.g. nfsserve's predictable default layout — is rejected as a bad
handle; a raw-RPC test pins this). Honest limits that remain: a
**same-user process can read an established mount**, and the NFS **mount
command line, including the export path, is visible in `ps`** to same-user
processes while the client mount exists.

Hydration failures name the object: a missing object or a hash mismatch
prints the id on the daemon's stderr and surfaces a distinct errno
(`EIO` for missing, `ESTALE` for mismatch — a client re-lookup re-hydrates).

The hydrate cache lives under
`$XDG_CACHE_HOME/naatre/mounts/<export>/` (default `~/.cache/…`), 0700,
one 0600 file per blob, **LRU-evicted at 1024 blobs per mount**. A daemon
killed with SIGKILL leaves its cache dir behind; the next mount evicts
dirs whose owner is gone.

`--writable` (UNSTABLE) enables file create + write: the default mount is
read-only and every write op answers EROFS. With the flag, file-create
enforces the path grammar and NFC at create (rejections name why) and
writes buffer in a shadow dir; on exit the daemon **stages** them as a
changeset on top of the pin and prints its id — **flush never lands
anything** (no ref moves; re-flush re-stages). With `--flush-land`
(UNSTABLE, requires `--writable`), the staged changeset then goes through
the **same land path `naatre land` uses**: the idempotency key is printed
before the request, main advances onto the landed head, and the exit code
mirrors land's conflict contract (2 for a 409 conflict or a settled key).
No mkdir, delete, or rename in this first cut. **Known hole (named, not
papered over): the writable mount has no client marker gate** — a
marker-shaped file written through the mount flush-lands as ordinary
content on a current binary, because the client-side
`conflict_markers_present` check runs in the commit/resolve verbs, which
the mount's flush path never passes through. Interim workaround: avoid
conflicted paths on writable mounts until the server-side marker-shape
check at land (the recorded follow-up) exists.

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
| `redacted_base` | 409 | the land's base chain crosses a redacted changeset (details name it) |
| `conflict_markers_present` | 409 | the CLI's commit gate refuses marker-shaped content (M12; CLI-emitted only — the server never emits it) |
| `version_skew_conflicted` | 409 | conflicted-node sync refusal (M12) — emitted by NO shipped binary; lives in the registry for v2-aware-lineage builds with the v2 capability absent (unit-pinned as a contract; the server never emits it) |
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
| `object_collected` | 410 | object collected by GC after the grace window |
| `object_redacted` | 410 | object redacted (M10 tombstones) |
| `writer_busy` | 503 | single-writer slot busy; retry with backoff |
| `blob_swept` | 503 | GC removed the blob's bytes mid-upload; re-PUT (idempotent at a content-addressed URL) |
| `derivation_pending` | 503 | derived data still computing (M6) |
| `blame_too_large` | 503 | blame exceeded a structural read cap; not retryable |
| `request_timeout` | 503 | request shed at the 30 s budget; retry |
| `server_busy` | 503 | at the concurrent-request cap; retry |
| `internal` | 500 | internal error; specifics stay in server logs |

Retryable 503s carry `Retry-After: 1`; back off and retry. The bundled CLI
does exactly that on idempotent verbs (reads, object PUT): it retries
`writer_busy`/`server_busy`/`request_timeout`/`blob_swept` honoring
`Retry-After`, bounded at 3 tries / 5 s of total waiting. The idempotent POSTs (`repo
create`, snapshot POST, the POST-shaped batch reads) retry only the
**pre-execution** rejections `writer_busy`/`server_busy` — a
`request_timeout` on a POST surfaces instead of replaying, because the
shed can drop the response after the write already committed and a replay
could double-apply. `derivation_pending` and `blame_too_large` surface
immediately (a derive backlog is not a 5 s event; a structural cap is
deterministic), and keyed writes (`land`, changeset create) are never
retried — their key semantics own duplicates.

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
| 10 | at most 256 components |
| 11 | total ≤ 4096 UTF-8 bytes |
| 12 | first component not `.naatre` or `.git` |

Additionally rejected at create: case-folded duplicates of an existing
sibling in the same directory (macOS/Windows safety), and file/dir duality
violations (add under a file, modify a directory, delete an absent path).
Repo names: `^[a-z0-9][a-z0-9-]{0,62}$`. Ids: 64 lowercase hex at every
boundary.

## Erasure: what carries personal data

Destroying the data dir erases everything in it. Backup copies and the TLS
proxy's own access logs are **not** covered — the operator owns those.
History is append-only; redaction (M10) is the sanctioned exception.
Personal-data carriers, explicitly:

- **`changesets.author`** — the client-supplied `Name <email>` string, and
  the same names/emails as imported from git. Redaction is the erasure
  tool: a tombstoned changeset stops serving (author, message, attribution
  — everywhere) at mark time, and the GC's redaction phase later purges
  its rows — the canonical bytes that embed the author included — once
  nothing references the node anymore. Whole-changeset granularity: a
  changeset is redacted or it is not; there is no author-only rewrite.
- **Blob content** — redactable the same way: the tombstone gates every
  fetch (history, batch, snapshot restore, blame layers) immediately, and
  the purge deletes the CAS bytes once no repo references remain.
- **Principal-attributed columns** — `changesets.principal`,
  `refs.last_principal`, `lands.principal` + `lands.result_json`,
  `snapshots.principal` (TTL-bounded by the snapshot TTL, capped at
  366 days).
- **The `principals` table** and **`tokens` rows** — deletable:
  `naatre principal delete <name>` (admin) deletes the principal and all
  its token rows and pseudonymizes the audit actor to
  `deleted:<random-id>` — an opaque 16-hex-char token generated per
  deletion event, stable across every row rewritten by that deletion
  (the actor history stays linkable without keeping the name).
- **`audit_log`** — `principal` plus the actor fields in `details_json`.
  The log is append-only and retained **indefinitely on a
  security-accountability basis**, with sanctioned in-place rewrites at
  principal deletion only: the deleted principal's actor rows are
  pseudonymized to the `deleted:<random-id>` token above, and that same
  token replaces the NAME field in the principal's own `token_create`
  rows' `details_json` (parsed and rewritten by key — the role and every
  other key stay) — these are the only UPDATEs the log ever takes.
  `details_json` payloads are otherwise history and stay, with exactly
  what survives stated: the ROLE in the token_create rows, the per-event
  pseudonym token in the `principal_delete` row's own details (the
  erasure receipt — the record that erasure happened, linkable to the
  rewritten actor rows without naming the principal), and admin-supplied
  free text (redaction reasons), which is never scrubbed.
- **Where the rows live per layout (M13a)**: in the `db-per-repo` layout
  the carriers split with the metadata — `changesets.*`, `lands.*`,
  `snapshots.*`, and the `redactions` tombstones live in the REPO's file
  (`repos/<id>.db`; deleting a repo's file is the repo's erasure), while
  `principals`, `tokens`, and `audit_log` (with the pseudonymization
  rules above) stay in the catalog and outlive any single repo. The
  `objects` ledger row is catalog-side too; the CAS bytes are global —
  a purged blob's bytes leave the shared CAS only when NO repo references
  remain (the cross-repo refcount rule, unchanged by the layout).
- **Snapshot blob content** expires at the TTL (rows go with it). A
  redacted blob id is gated ahead of the TTL: the snapshot's tree
  listing serves the entry masked and a restore brings every available
  file (reporting masked paths unavailable) — but the redacted blob's
  own snapshot FETCH answers 410 `object_redacted` immediately, TTL
  irrelevant; the content never returns.

**The honest limit**: history attribution survives principal deletion —
`changesets.principal`, `refs.last_principal`, and the lands rows keep the
name unless the changesets are themselves tombstoned. Whole-changeset
redaction is the erasure primitive; there is no name-only rewrite of
history, because the canonical bytes are content-addressed (rewriting a
name rewrites every descendant id — that is a history migration, not an
erasure, and naatre does not have one).

**Client-side copies sit outside every server-side purge**: the mount
hydrate cache (`~/.cache/naatre/mounts/…` — persistent, no TTL, only
LRU-capped) and every clone's working copy hold plaintext blob content
that is user/operator-owned. Server-side erasure — redaction purge, GC,
data-dir destruction — never reaches those; erasing them is the client
machine's own story.

**External metadata changes the erasure surface**: with
`meta = "postgres"` the rows above (and the append-only `audit_log`) live
in the Postgres database, not the data dir — destroying the data dir
erases only blobs and config, and the metadata's erasure is the Postgres
deployment's own story (its backups included).

**S3 blobs outlive the data dir too** (`blob = "s3"`, M9b): destroying
the data dir erases nothing in the bucket — history and snapshot keys
remain, and the bucket's erasure is the S3 deployment's own story (its
versioning and lifecycle configuration included). Two operator duties
follow, and they are the price of the backend, not bugs:

- **Lifecycle policies and versioned buckets can violate the
  never-dangling invariant**: a bucket-side lifecycle rule (or a manual
  expiry) can delete object versions naatre's rows still reference, and
  object versioning can retain bytes after naatre deletes them — the
  first breaks reads (dangling references), the second weakens the
  GC's physical purge (bytes survive that the DB believes erased).
  Configure the bucket with NO lifecycle rules and NO versioning; naatre
  owns its own expiry (the snapshot TTL) and its own GC.
- **External drift is visible, not corrected**: `fsck` and the repo
  stats walk every referenced id and report which are missing from the
  bucket (or unreferenced in it) — a presence scan over external state.
  Repairing drift caused outside naatre is the operator's action;
  naatre never guesses.

The same framing as the Postgres case: destroying a data dir is complete
erasure only on the zero-config SQLite+fs deployment.

## Release artifacts

Releases ship as static binaries with `SHA256SUMS`, a minisign signature
of those checksums, and a CycloneDX SBOM (`scripts/release.sh`; the SBOM
generator `cargo-cyclonedx` is a documented tool dependency, not a
runtime dep). **No production release key exists yet** — the repo carries
only the CI TEST key for the signature selftest, so signatures are
untrusted-by-default until the maintainer publishes the production
public key (see [`SECURITY.md`](SECURITY.md), "Verifying release
artifacts"). Once it exists, verify a download with it:

```sh
minisign -Vm SHA256SUMS -p <the published release public key>   # reads SHA256SUMS.minisig beside it
sha256sum -c SHA256SUMS
```

The signature covers `SHA256SUMS`, and `SHA256SUMS` lists every shipped
file — the binary (`naatre-<ver>-<target>`), `THIRD_PARTY_LICENSES.md`, and
the SBOM (`naatre.cdx.json`) — so the signature extends to the SBOM too.
The SBOM generator is pinned in CI (`cargo-cyclonedx` 0.5.9).

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
