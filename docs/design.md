# naatre design notes

The durability invariants the server enforces, the limits registry (every
limit naming the milestone that lifts it, or saying plainly that none does),
and the honest-scope boundaries — what the security and consistency model
does and does not claim. Facts here are as built at M5b; each entry names
the code that carries it.

## Durability invariants

(These hold per backend. On Postgres (M9) the same statements are true
with COMMIT semantics replacing WAL: land is one transaction, blob CAS
writes (fs) still land before the referencing rows commit, and the
derive-queue lease/`seq`-order machinery is schema-identical modulo the
documented `seq` identity columns — the M9a conformance suite runs the
same store-level tests against both.)

Enforced by the store API (`naatre-store`, `naatre-server/src/land.rs`,
`db.rs`) and pinned by tests. SQLite runs in WAL with `synchronous=FULL`;
every connection sits behind `spawn_blocking` with `busy_timeout`.

1. **A blob is durable before any row referencing it commits.** Object and
   snapshot writes go tmp → rename → fsync into the CAS before the insert
   in the same transaction. The failure direction is an orphan blob (harm-
   less, swept later), never a dangling reference. This is what makes the
   README's backup ordering safe: DB snapshot first, then `objects/`.
2. **Land is one transaction.** `BEGIN IMMEDIATE` → pending `lands` row →
   rebase → CAS `main` → done → COMMIT. A crash before COMMIT leaves
   nothing: the key stays unsettled, a retry replays from scratch; a crash
   after it means replay-by-key returns the settled state
   (`m4b_crash_test.rs` walks the deterministic crash-point enum).
3. **Ephemeral GC is whole-directory and never touches history.** A pass
   deletes expired snapshots' whole `ephemeral/<snap-id>/` directories and
   their rows (dirs first, rows after: a crash mid-pass leaves an expired
   row whose fetch answers 410, never a live row whose blobs are gone), and
   sweeps rowless dirs older than the default TTL (7 days) — the convergence
   rule for an upload that crashed between blob PUTs and the snapshot POST:
   an upload older than the default TTL can no longer be POSTed (presence
   answers 422) even though a snapshot row, once created, may live up to the
   366-day ceiling. Restore re-checks expiry at fetch time, so expiry is
   enforced at every read, not only at GC time.

**Schema migration authority**: `serve` auto-applies migrations on boot; a
binary older than the database's schema version refuses to start with a
clear error. The schema, not any binary, is the authority.

## Limits registry

Every deliberate limit, with the milestone that lifts it — or an explicit
"no milestone", where the limit is structural rather than temporary.

| Limit (as built) | Value / behavior | Lifted by |
|---|---|---|
| Single-writer per database | One write transaction at a time per data dir; a contended write slot answers 503 `writer_busy` + `Retry-After` instead of queueing. The SQLite-level `busy_timeout` is pinned at **5000 ms** (`naatre-store/src/store.rs`, `BUSY_TIMEOUT_MS`) — a write that cannot take the DB inside it surfaces as an error, never an unbounded wait. Reads run on a separate read-only pool and never contend with the writer (WAL). | **M13** (DB-per-repo turns this into per-repo; global until then) |
| Whole-directory tree rewrite | Derivation is path-copy: a change re-canonicalizes every directory node on the changed paths' ancestor chain, O(change × depth) rows per changeset. Structural to the derived-tree design (Mononoke/Bonsai precedent); per-changeset caps bound the cost. | **No milestone** — accepted, not a temporary limit |
| Single-process working copy | Since M7, concurrent writers in one working copy are DETECTED and REFUSED: `naatre mcp` holds `.naatre/wc.lock` (advisory flock, released on exit or process death) for its session; every CLI working-copy verb takes the same lock for its duration; a second writer's refusal names the lock. Mounts (M8) are not working-copy clients: each daemon owns a per-export state dir instead (below). | **Shipped** |
| No per-client request rate limiting | The server rate-limits nothing by client: transport caps (below) shed overload, auth-failure auditing has its own caps, and ordinary requests are admitted unconditionally. Behind a proxy, load-shedding per client is the proxy's job. | **No milestone** — front naatre with a rate-limiting proxy; per-principal quotas are unscheduled |
| Transport caps | Blob ≤ **64 MiB** pre-compression (413 over); request body ≤ **96 MiB**; canonical changeset object ≤ **16 MiB** with ≤ **100 000** entries and ≤ **2** parents; path ≤ 4096 bytes, ≤ 256 components, components ≤ 255 bytes; request time budget **30 s**; concurrent requests ≤ **256**; pagination `limit` 1–**200** (default 50); batch PUT is effectively one-max-blob-per-request because the base64 payload of a max blob dominates the body cap — a raw `PUT /v1/repos/{r}/objects/{id}` exists for large blobs. | **No milestone** — these are the design's shape, not a pending relaxation |
| Repo-name visibility | `GET /v1/repos` lists every repo name to every authenticated principal; names are visible, contents are not (per-repo reads still gate on auth and membership). | **M13** (repo read ACLs) |
| Cursor-during-land duplicates | Keyset pagination is anchored on changeset ids; a concurrent land can make a page boundary re-serve a changeset the next window also contains. Duplicates are possible; **skips are not** — the pinned contract, tested in M4. | **No milestone** — inherent to pagination over an append-only log; documented, not fixed |
| Proxy audit degradation | naatre logs no peer address anywhere (access logs are method/path/status/elapsed/request-id only) and keys auth-failure auditing by presented credential. Behind a TLS proxy, every request shares the proxy's address — but since nothing keys on addresses, the practical degradation is narrow: an anonymous flood presents no credential, keys to the single `anon` bucket, and is visible only through the global counters (see auth-failure caps below). Attribution is by principal, never by network origin. | **No milestone** — the design keys attribution on identity, not origin |
| Snapshot POST closure caps | `MAX_SNAPSHOT_TREES` **10 000** trees per POST and `MAX_SNAPSHOT_BLOBS` **8 192** distinct blobs per POST (422 beyond, `details.limit`); the missing-blob response list is capped at 256 ids with a `missing_blobs_truncated` flag. A sparse cone with >10k directories cannot snapshot whole — raise the constants when that bites; no config knob yet. | **No milestone** — raise the constants in code when sparse cones bite |
| Snapshot TTL bounds | 60-second floor (plan-pinned) and a 366-day ceiling (server-side safety bound beyond the plan; supervisor judgment, flagged in the route contract). | **No milestone** — the ceiling is deliberate, not a pending relaxation |
| Snapshot restore closure walks | The closure-membership check on `GET .../snapshots/{id}/trees/{tid}` re-walks the root closure per request (`db::tree_in_closure`, one entries-load per visited tree): an n-tree restore costs O(n²) server-side row loads — near the 10k-tree cap, minutes. Acceptable at today's snapshot sizes. | **No milestone** — a POST-time membership structure (tree → mounted paths) is the mitigation when large snapshots become real |
| `snapshot_gc` audit rows | Written per collecting pass (never idle passes), actor `system`, details `{expired, rowless, rows_purged, trees_purged}`. | **No milestone** — recorded behavior, not a limit |
| Derive walk budget (per kind, COOPERATIVE) | One derivation call may visit at most `walk_budget` rows / tree nodes / committed outputs through its `DeriveCtx` — a PER-KIND value (`Deriver::walk_budget`; bootstrap default 200,000 ≈ 2× the 100k-entry changeset cap): blame 1,000,000 (a parent-tree walk per touched path of a max changeset), path_history 400,000 (one visit per row), commit_graph 1,000 (two charges per skip entry, O(log chain) entries). The budget binds derivers that charge their visits; a deriver issuing raw SQL past the ctx is not capped by it. BUDGET exhaustion quarantines the node immediately (structural — a retry walks the same graph), with the reason in `derive_reports` and children proceeding on the empty marker. The wall-clock DEADLINE behind it (~2 s, `DERIVE_WALK_DEADLINE_NS`, injectable per state) is deliberately NOT structural: an overrun releases the lease back to pending as a FAILED ATTEMPT (bounded by the 3-attempt cap; the pass ends so the next tick retries), rows a DISTINCT `derive_reports` reason carrying the measured units/sec, and quarantines only at the cap. The budget bounds a deriver's per-CALL walk, never history length: long history derives batch-by-batch through the queue, and a 1,100-commit linear chain is the regression pin (`m6a_test.rs::linear_chain_derives_to_the_end`). | **No milestone** — raise a kind's value in code if a legitimate node shape trips it |
| Reachability GC pass cost | The mark reads PRIMARY TABLES ONLY (refs, changesets + parents + file_changes, unexpired snapshots, lands, `git_map`) — O(primary rows) per pass, repo-by-repo with every per-repo statement's index leading on `repo_id`; the collectible selection re-verifies each candidate inside its deleting transaction through migration 9's probe indexes (`idx_refs_target`, `idx_lands_head/base`, `idx_git_map_cs`, `idx_cs_parents_parent_id`). Collection runs in generation/age-ordered batches (10 000 rows / 256 blobs per write-slot hold, `BEGIN IMMEDIATE` — on Postgres the fixed advisory lock, the same destructive-writer section land takes), so ticks only ever 503 writes for one batch. Blob deletion is TWO-PHASE (condemn in one pass, delete in a later one; any PUT's row touch clears the mark first), and the grace window (default 30 days, `gc_grace_secs`) is measured from the FIRST-observed-unreachable timestamp. A collecting pass writes ONE `gc` audit row — committed inside its final collecting batch's transaction, actor `system`; idle passes write none. | **No milestone** — the plan's mark clause names this cost class |
| Derive-queue quiet passes | The background pass never occupies the single-writer slot on a quiet server: the lease sweep read-probes before taking a write transaction (same pattern as the snapshot-GC junction probe), and the drain writes only when a node is actually eligible. Writes a quiet server makes per tick: none. | **No milestone** — recorded behavior, not a limit |
| Blame / path-history read cost | `GET /blame` walks the first-parent chain from `at`: two indexed point queries per visited node (queue state + diff-state), plus one CAS fetch per changed-blob version on the chain, plus the in-memory line diff of each touched pair — O(history of the file), never O(trees). `GET /log?path=` is two indexed queries per page off the `path_history` inverted index. | **No milestone** — the index shape, not a pending relaxation |
| Derived-surface storage growth | `blame_state` grows with (changeset × touched paths) rows; `path_history` with (changeset × touched paths) index rows; `derive_state.output` carries ≤ ~49 bytes × O(log chain) skip entries per node for commit_graph. All bounded by the changeset caps (≤ 100k entries/changeset). The reachability GC collects every M6 row keyed by a collected changeset (queue, reports, blame, path-history) alongside it — and the redaction purge does the same for a tombstoned one (M10b). | **No milestone** — bounded by the same caps |
| Redaction is one-way, whole-object, repo-scoped | A tombstone (`redactions`, migration 10) marks ONE object (a blob or a changeset) in ONE repo; there is no un-redact verb (lifting a tombstone is out-of-band surgery on the row), no path-level or author-only redaction (whole-changeset granularity — an author-only rewrite would rewrite every descendant id), and no cross-repo propagation (the other repo referencing the same content keeps serving it; the purge is refcount-aware across repos). The mark gates serving IMMEDIATELY at every path; the GC's redaction phase reclaims later, only when nothing references the object, and needs no grace window of its own (in-txn re-verification + the upload touch grace; a record step racing the delete answers the retryable `blob_swept`). | **No milestone** — the shape, not a pending relaxation |
| On-demand derive compute | A derived read finding a pending region answers 503 `derivation_pending` + `Retry-After` and does NOT compute nodes inline. The plan's contract permits on-demand compute within the request timeout; the background queue (5 s tick) makes the 503 window short, so the inline path is unimplemented. | **No milestone** — revisit only if interactive freshness after bulk writes becomes a reported need |
| Mount hydrate cache | Per-mount state under `$XDG_CACHE_HOME|~/.cache`/`naatre/mounts/<export>/` (0700 dirs, 0600 files): `blobs/` is **LRU-evicted at 1024 blobs per mount** (count-bound; the oldest-touched file is deleted, not rewritten), `shadow/` holds the writable mode's staged writes, `session.json` carries the daemon pid. A SIGKILLed daemon's dir is evicted by the next mount's startup prune (dead pid / unreadable session); a graceful exit removes its own dir. Blob bytes in the cache are content-addressed repo content (no attribution data); destroying the cache dir is always safe — every cached blob re-hydrates hash-verified. | **No milestone** — the bound is per-mount by design; a shared cross-mount cache is unscheduled |
| Mount writable v1 scope (UNSTABLE) | File create + write only, on a ref pinned at mount time: grammar + NFC enforced at create (rejected, naming why; never silently normalized), case-fold twins refused, no mkdir/unlink/rename/symlink/setattr (front-ends answer EPERM). Flush **stages** one changeset on top of the pin and prints its id — nothing is landed, no ref moves; re-flush re-stages. `--flush-land` (UNSTABLE, requires `--writable`) then lands the staged head through the SAME land path `naatre land` drives (`verbs::land::land_changeset`; key printed before the request, main advances, exit code mirrors land's conflict contract 2/1). Read-only is the default; every write op answers EROFS. | **No milestone** — the fuller writable surface is unscheduled until demand exists |
| Backend selection is file-pinned | `backend.toml` (0600) in the data dir selects `meta`/`blob`; absent file = SQLite+fs. A data dir whose existing metadata store disagrees with the file is refused at open (naming both sides); backends never switch in place. The strict parser refuses unknown keys, unknown values, non-0600 permissions, and inline-credential DSNs. | **No milestone** — the shape, not a pending relaxation |
| One server per Postgres database | The single-writer slot is in-process. IMMEDIATE transactions — every DESTRUCTIVE write section: land, the reachability GC's collection batches, and the blob-upload record step — additionally take a fixed transaction-scoped ADVISORY LOCK (`pg_advisory_xact_lock`, `naatre_store::meta::PG_WRITER_ADVISORY_KEY`) with a 5000 ms `lock_timeout` — the `BEGIN IMMEDIATE` + `busy_timeout` semantics across PROCESSES: a second destructive writer blocks until the holder commits, then fails as retryable contention (55P03 → the writer-busy 503 shape) if the holder exceeds the timeout. MVCC contention (40001/40P01/55P03) maps to the same retryable shape. The data-dir flock still serializes same-data-dir processes end to end; what the advisory lock adds is destructive-section safety for processes on DIFFERENT data dirs sharing one DSN — one live INSTANCE per DSN/bucket remains the deployment rule (the README's backend section names the cross-instance silent-loss shape; the lock serializes transactions, not the mark-then-sweep reasoning). | **M13** (DB-per-repo catalog reconsiders multi-writer) |
| Postgres driver-error redaction | Postgres error TEXT can embed host, user, and DSN fragments; naatre reduces every driver error to a classified kind + SQLSTATE (`naatre_store::meta::DbError::Postgres`) — the banner, logs, and error strings never carry the driver's message, the DSN, or the endpoint. Cost: connect failures are diagnosed from the classification + the operator's own DSN knowledge, not from the log line. | **No milestone** — the redaction is the M9 contract |
| S3 blob backend (M9b) | `blob = "s3"` puts history blobs at `objects/ab/cd/<62hex>` and snapshot blobs at `ephemeral/<snap>/objects/…` in one bucket — the fs CAS's layout and zstd encoding byte-for-byte, so a migration is a straight copy (an fs data dir is never switched in place; the mismatch guard refuses). The sync store drives async `object_store` through a per-store current-thread runtime under a mutex: **S3 operations serialize per server instance** (fs CAS has no such limit; lifting it is a facade change, not a seam change). S3 client errors embed request URLs, so they reduce to an op-named redacted error (`StoreError::S3`), same contract as the Postgres redaction above. Bucket-side lifecycle rules / versioning can break the never-dangling invariant and weaken the GC's physical purge; `fsck` and the repo stats scan bucket presence and report drift without repairing it — the operator duties live in the README's erasure enumeration. | **No milestone** — the M9b shape, not a pending relaxation |
| Mount front-ends and real-mount e2e | Two front-ends over one pinned `NaatreVfs`: **NFSv3** (nfsserve 0.11, loopback-only ephemeral port + unguessable `/naatre/<32-hex>` export by default; non-loopback `--listen` prints `serve`'s cleartext warning; **AUTH_SYS authenticates nothing** — network-reachable mounts are readable (and writable, with `--writable`) by anyone on the path) and **FUSE** (Linux-only `--fuse <mountpoint>`, fuser 0.18, `SessionACL::Owner`: only the mounting user can access it, no network surface, attribute TTL 0 because the overlay changes sizes under stable paths). Both front-ends share the mount plan (cache dir, session, stale prune) and the flush/land exit; view files belong to the mounting user. **Handle layer**: the export path gates only MOUNT — GETATTR/READ need just a structurally valid handle, and nfsserve's default encoding (`[epoch-ms-of-start ‖ fileid]`) is brute-forceable — so the daemon overrides `id_to_fh`/`fh_to_id` to embed a per-mount 128-bit CSPRNG secret (`[secret ‖ fileid]`); wrong length or wrong secret is indistinguishable BADHANDLE (no oracle; raw-RPC test pins real-handle-serves + forged/default-layout rejected). Honest remaining limits: same-user processes can read an established mount; the client's mount command line (with the export path) is visible in `ps`. The REAL kernel-mount e2e (byte parity vs materialized clone, cold cargo build against the mount, writable flush-land through the kernel client) runs where the OS allows and otherwise SKIPS with a printed notice; macOS note: `mount_nfs` needs `noresvport` for non-root mounts (the printed line carries it), and some macOS builds accept the unprivileged mount but suppress data ops until root mounts it (verified against a bare upstream-shape nfsserve adapter — not a naatre defect). | **Shipped** (M8b) |

## Derived-data reads (M6b)

The three production kinds — `blame`, `path_history`, `commit_graph` — are
deferred: a write only enqueues, the background queue derives, and `naatre
serve`'s boot-time backfill enqueues everything a registry-less process
wrote (offline import) or a pre-derive database carried (in-place upgrade,
same queue). The read contracts on top:

- **Read-during-backlog**: `GET /log?path=` and `GET /blame` answer 503
  `derivation_pending` + `Retry-After: 1` while any needed region is
  queued or deriving — never an empty-as-if-no-history answer. After the
  queue drains the same reads are exact.
- **Poison degrades, never lies**: taint is stored on the queue rows — a
  node that derived on a quarantined ancestor's empty marker records done
  WITH `tainted = 1` and `poison_cs` = the nearest poisoned ancestor,
  inherited by every descendant. A blame read raises `degraded: true`
  with that `poisoned_cs` straight off the walked rows (O(1) per row); a
  walk that reaches the quarantined node itself stops there and
  attributes every line it could not resolve past the boundary to that
  changeset (its blob provably contains those lines — attribution bounded
  below by the quarantine, flagged, not silent). `?path=` flags a page
  PRECISELY: only when a quarantined node's primary file changes touch
  that path (exactly the touches the index may be missing) — an untouched
  branch is never flagged. A pending node anywhere outranks a degraded
  one: 503 wins.
- **Merge-base never 503s**: the route runs the skip-list walk when both
  endpoints' `commit_graph` outputs are done, and the plain
  generation-bounded walk otherwise (kind unregistered, pending,
  quarantined, undecodable) — pending derivation costs the acceleration,
  never the read. Skip entries hop 1, 3, 7, 15, … first-parent levels;
  only PURE spans (every spanned node single-parent) may be jumped, so no
  spanned node's non-first parents are lost.
- **Blame pins**: human format `<8-hex cs> <author> | <line>` (the CLI's);
  `--json` shape `{"lines":[{cs,author,text}]}` with full 64-hex ids; a
  binary file at `at` is a structured empty result — HTTP 200, empty
  lines, `binary: true`, stable code `blame_binary`; an empty file blames
  as empty lines. First-parent semantics throughout.
- **`GET /log?path=`** lists every changeset touching the path (all
  branches, newest first: generation DESC, id ASC) off the derived
  inverted index, with `degraded`/`poisoned_cs` added to the log-page
  shape. With the kind unregistered the same rows are scanned out of
  `file_changes` — the fallback, and the parity oracle's brute force.

## Request-timeout shedding and in-flight writes

The 30 s per-request budget sheds by dropping the handler's future
(`middleware::request_timeout`, 503 `request_timeout`, retryable). The drop
does not roll back work already handed to `spawn_blocking`: a write closure
past its DB commit point **may still commit after the client saw 503**.
Healing is by class:

- **Keyed writes** (land, keyed changeset creates): the client retries with
  the same key; the settled-row replay returns the identical outcome. No
  duplication by construction.
- **Unkeyed writes** (e.g. token create, repo create): a retry after a shed
  request may produce a second row. Tokens are inert until used; an
  unreferenced duplicate is harmless. Idempotency keys are the tool for
  anything where that is not acceptable.

## Trees: repo-membership-gated reads

Tree ids are content-addressed (BLAKE3 over canonical bytes), so two repos
committing an identical subtree derive the **same** id and share one
`trees` row. Which repos may read a tree is tracked in the
**`repo_trees(repo_id, tree_id)` junction** (schema migration 3), written
by the HISTORY tree paths only — changeset create, land, and import all
funnel through `db::trees_write`. `GET /v1/repos/{r}/trees/{id}` answers
200 iff a membership row exists for (this repo, id):

- **A tree is readable by every repo that wrote it** — including the
  shared-subtree case where two repos independently derive one id.
- **A repo that never wrote the tree gets 404 even though the id exists**
  on the server: trees carry path names and blob ids, so reads stay
  repo-membership-gated (the plan's cross-repo posture; the same rule the
  by-id objects boundary applies through `repo_blobs`).
- An unknown id is 404 everywhere (`m2a_test.rs::trees_are_repo_membership_gated`
  pins all three cases).

**Snapshot trees are outside the junction.** The snapshot POST writes its
trees junction-less (`db::trees_write_snapshot`): a snapshot's tree is
own-principal data with a TTL, and a membership row would expose its path
names and blob ids to every repo reader through the bare-id tree route.
Snapshot trees are served only through the owner-gated snapshot routes
(`GET /v1/repos/{r}/snapshots/{id}`, another principal's id is 404), and
principals other than the owner get 404 on the tree route too
(`m5a_test.rs::snapshot_trees_have_no_membership_and_gc_purges_them`).

**GC of snapshot-only trees is mark-and-sweep, candidates = zero-junction
rows, protection TRANSITIVE through candidates.** A candidate (tree row
with no `repo_trees` junction row — zero-junction means snapshot-only by
definition, because every history-tree write path inserts junction rows
and migration 3's backfill covered pre-migration rows; backfill
completeness is the trust condition, and no provenance marker is needed or
wanted) is PROTECTED if any of: (a) it sits in the closure of an UNEXPIRED
snapshot's tree (content-addressing shares rows between snapshots, so the
mark is what keeps a live sibling's shared subtree alive when another
expires); (b) it is a changeset root; (c) it has a RETAINED parent — a
tree with a junction row or a changeset root pointing at it as a child
(reverse lookup via `idx_tree_entries_object`); or (d) it is a candidate
child of an already-protected candidate, recursively — so a junction-less
CHAIN under a retained parent survives in full; depth-one protection alone
would orphan the chain's children. (b)–(d) are the safety net for junction
completeness: a future write path that forgets junction rows for a whole
subtree cannot get its live history silently swept
(`m5a_test.rs::gc_spares_a_deeper_junctionless_chain`), while a chain whose
only reference is an expired snapshot sweeps fully, children included
(`gc_sweeps_an_expired_snapshot_chain_fully`). The walk stays inside the
candidate set: cost is one indexed junction probe per tree row
(`idx_repo_trees_tree`) plus indexed lookups per candidate
(`idx_changesets_root`, `idx_tree_entries_object`) — it grows with total
tree rows, not only with snapshots. The sweep deletes unprotected
candidates in ONE write-slot transaction together with the expired-row
purge — a snapshot POST reusing a shared subtree cannot land between mark
and delete, because both paths run through `state.write` (the
single-writer slot; the snapshot POST's whole row commit already does).
The pass probes on a read connection first (are there junction-less tree
rows at all, or expired rows to purge?) so a quiet server's periodic pass
never occupies the write slot. Rule as implemented: `db::unprotected_trees`
+ `db::delete_tree_rows`, driven by `snapshots_gc::gc_snapshots`
(`m5a_test.rs::shared_snapshot_subtree_survives_gc_for_live_sibling`,
`history_tree_identical_to_snapshot_subtree_survives_purge`,
`gc_spares_history_trees_whose_junction_row_was_lost`).

`trees.repo_id` is informational (the first writer; rows are `INSERT OR
IGNORE`) and gates nothing. **Backfill**: migration 3 derived membership
for existing history trees inside the migration itself — a recursive CTE
seeded from every changeset's `root_tree_id`, walking the `tree_entries`
closure (dir entries point at child trees); re-running is impossible by
construction (each migration applies once, atomically with its version
bump).

## First-class conflicts (M12)

A conflict is data inside the changeset, not marker text in a blob
(jj-style). The representation (see docs/format.md §8: sides' blob ids,
optional base, path — >= 2 sides, generator covers >= 3) lives in the
changeset payload; such changesets are format v2 (`naatre\0v2\0` id domain,
`\x02` magic) and their `root_tree_id` is the markers-materialized tree —
the v2 encoding itself is the flag that says "materialized", so blame and
the client slice can rely on it without a separate marker column.

**Land contract preserved.** The default land keeps the M4 shape: any path
conflict rolls the transaction back, the key stays unsettled, and the 409
carries every conflicting path plus the resolution recipe. The recipe now
ALSO names the explicit opt-in — `--record-conflict` (API request field
`record_conflict`, default false) — so an agent can choose it from the 409
alone. Opting in does NOT relax the settlement: the land still settles
exactly once, and the conflicted changeset IS the settlement. Its land row
records `lands.state = 'conflicted'` (a free-form string column — no
schema change, no migration; the v1 values `pending`/`done` are
unchanged), and both settled states replay idempotently.

**Rebase semantics.** Conflicted nodes rebase like any node: their markers
are ordinary tree content to the 3-way, and their representations travel —
paths the rebase did not re-conflict keep their reps verbatim, and a
path that re-conflicts under `--record-conflict` takes the new sides (the
conflict composes: the previous materialization is itself a side of the
new conflict). `lands.state` is `conflicted` exactly when the settlement
node (main's new tip) is a conflicted node; a land that merely carries a
conflicted ancestor through settles `done`. Conflicts with fewer than two
content sides (a deletion side has no blob id to carry) are not
representable in v2 and refuse under the opt-in too — loudly, never
silently "resolved" by recording.

**Sequencing (honest scope).** This slice owns the format, the land
semantics, and the pins (golden fixtures, commute/rebase proptest). The
GC walker's traversal of conflict representations (losing-side blobs stay
reachable) and blame's conflicted-region reporting arrive with their
owners in M12b, as does the client surface (`--record-conflict` flag in
`land --help`, marker materialization on sync, `status` conflict marks,
`naatre resolve`, the version-skew refusal on conflicted nodes).
**HOLD-BEFORE-DEPLOY: no release between M12a and M12b.** Two standing
hazards close together in M12b: (1) until the GC walker traverses
conflict representations, a losing-side blob of a landed conflicted node
stays reachable only while some reachable node's delta references it —
recorded here as a decision, not an oversight; (2) migration 012's
version bump is a ONE-WAY, store-wide lockout — the first boot of a
format-v2 binary upgrades the data dir to schema_version 12 and no older
binary can ever open it again, even holding only v1 data, so the only
rollback is a pre-upgrade backup (`naatre backup`). The flag itself is
not gated: `--record-conflict` is an explicit opt-in per land, and its
opt-in nature is unchanged by the hold.

## Honest-scope boundaries

- **Path scopes gate writes and filter listings; by-id fetches are
  repo-level.** Within a repo, a token's segment-aligned path scope gates
  writes and filters the listing endpoints (`log`, `trees`, `diff`
  listings); every by-id fetch endpoint (objects, `changesets/{id}`,
  `file`, `merge-base`) is repo-level — ids and path metadata are
  repo-visible, not path-private. The authz matrix asserts both halves.
- **Tree reads are repo-membership-gated** (see above): every repo that
  wrote a tree can read it; no other repo can, id existing or not.
- **Repo listing is visible to every authenticated principal** until M13's
  read ACLs (limits registry).
- **Symlink defense is check-then-write, with the TOCTOU window accepted.**
  Every path component under a checkout root is checked with
  `symlink_metadata` (no dereference); a planted link is refused before the
  write. The check fails open on a stat error (reads as "not a symlink") by
  design: the subsequent write fails loudly on the same underlying error —
  an EACCES is never mistaken for a planted link — so the net is the write,
  not the stat. A symlink planted *between* check and write is inside the
  accepted TOCTOU window; this is defense in depth on a single-user working
  copy, not a sandbox boundary.
- **Sync/restore dirty gates refuse, never merge.** `sync` and `restore`
  with local edits to incoming paths, colliding untracked files, or
  symlinks on any affected path component refuse with 409
  `dirty_working_copy`, whose `details` enumerate the dirty **and
  colliding-untracked** paths (plus the symlink colliders) — exactly the
  list `--force` discards. `--force` deletes those named paths and nothing
  else; symlinks on them are unlinked, never followed. Without the flag
  nothing on disk is touched.
- **Auth-failure auditing is capped twice and the overflow is the signal.**
  Per key (a BLAKE3 prefix of the presented credential, never the peer
  address): 10 rows per 60 s sliding window. Globally: 200 rows per 60 s.
  Beyond either limit a failure is **counted, not rowed**, under any
  keying — an anonymous flood cannot exhaust the audit budget under fresh
  fake keys faster than the global cap drains. The alert signal is the
  aggregate delta `(global_count − rows_written)` from
  `auth_fail_stats()`; a non-zero sustained delta means failures are being
  suppressed by design and someone should be looking at the auth logs.

## Import mutations and sanitization

One documented content mutation on import: `\r\n` → `\n` in commit
messages. Author and message strings go through the same closed character
rule as changeset create (Unicode Cc + Cf), but on import a violation is
**never a skip**: by default every offending character is replaced with
U+FFFD and the commit is listed in the import report's `sanitized` array;
with `--sanitize` (M11) the text is instead escaped losslessly (`\u{hex}`
per offending character, literal backslashes doubled — the inverse codec
decodes to the original bytes), with per-object `escaped` report entries
naming each escaped field and code point. Decode only objects the
`escaped` report names (an un-escaped string keeps its literal
backslashes). The `\r\n` normalization applies before either mode.
`--skip-unsupported` skips a commit **and all its descendants** (the
`cascaded` list reports the cascade) — one offending early commit skips
everything after it.

## Known deviations and costs (honest record)

- **`--trust-proxy` not implemented (plan deviation, unscheduled).** The approved plan
  pins auth-failure keying as "token bucket keyed by presented principal, else peer
  address; `--trust-proxy` opt-in, keyed by the *last* `X-Forwarded-For` hop". The built
  code keys on a hash of the presented credential only — no address keying, no flag
  (the M1b2 brief compressed the clause away). Effect: a credential-less flood shares
  one `anon` bucket; there is no per-origin attribution. Honest absence, documented here
  and in the README; no plan milestone contains this work — scheduling it needs an
  explicit plan decision.
- **GC sweep cost:** the zero-junction candidate probe is an anti-join over all `trees`
  rows — O(total trees) inside the write slot on each pass (60s tick + each POST) once
  any snapshot exists. Cheaper than history-closure marking; a candidate index or a
  dirty flag is an M6+ optimization if it ever shows.
- **Verification anchors:** `git stash create` anchors cover tracked content only;
  untracked files are outside the guard's content compare.
