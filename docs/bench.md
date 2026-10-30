# naatre vs git — sparse clone and commit/land (M4b, reported not gated)

Plan pin: the A/B comparison is *reported*, never a release gate. The goal is
an honest look at where naatre's thin client (fetch only the sparse prefixes,
hash-verified materialization, one-server-write commit+land) stands against
git's partial clone on the same machine.

## Methodology

`scripts/bench_vs_git.sh` builds a synthetic git repository — **200 commits ×
3 files × ~50 lines** (600 blobs, growing in-place rewrites), via the git CLI
in a tempdir — then measures, as the **median of 3 runs** each:

| Operation | git side | naatre side |
|---|---|---|
| Sparse clone of one directory | `git clone --depth 1 --filter=blob:none --sparse` + `git sparse-checkout set dir1` (fresh destination per run; both commands inside the timing) | `naatre clone --sparse dir1` (fresh destination per run; the one-time offline `naatre import` and the server process are **excluded** from the compare) |
| One-line edit on the checkout | `git add -A` + `git commit` (both inside the timing) | `naatre commit` + `naatre land` (both inside the timing) |

Both sides talk to their store over the same local disk; naatre's server runs
on loopback (`127.0.0.1`). The git side needs `uploadpack.allowFilter` on the
source to serve a real partial clone — the script sets it on its own synthetic
repo, and copies a provided repo into the workdir first so the source is never
mutated. Every timing runs a fresh command; nothing is cached across runs by
the script.

## Numbers

Apple Silicon (Apple M2 Max), macOS 26.6, git 2.56.0, rustc 1.99.1 —
**dev build** (`cargo build`, unoptimized), 2026-10-30:

| operation | git (s) | naatre (s) |
|---|---|---|
| sparse clone of one dir | 0.0876 | 0.0328 |
| one-line edit: commit (+land) | 0.2236 | 0.0374 |

→ naatre wins: 2.67x faster than git (sparse clone of one dir)
→ naatre wins: 5.98x faster than git (one-line edit: commit (+land))

## Reading it honestly

- naatre wins the sparse-clone comparison on this workload partly because the
  client fetches exactly one directory's blobs over a local HTTP loopback,
  while git's partial clone negotiates a pack over `file://` — different
  machinery, both doing the right thing. A cold page cache, larger history,
  or a network hop would change the picture; this table is a dev-build
  roundtrip on one machine, not a product claim.
- The commit row includes naatre's **land** (server rebase + CAS + a durable
  fsync'd transaction) against git's local commit only — naatre does strictly
  more work per edit (it publishes to the server atomically) and still pays
  less wall clock here, which says more about git's process fan-out (`add`
  + `commit` spawn several processes) than about either implementation.
- Re-running the script varies the numbers by a few percent run to run; the
  verdict direction has been stable across repeated runs on this machine.

Reproduce with `./scripts/bench_vs_git.sh [path-to-a-git-repo]` (env knobs:
`NAATRE_BENCH_COMMITS`, `NAATRE_BENCH_LINES`, `NAATRE_BENCH_PORT`).
