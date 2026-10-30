#!/usr/bin/env bash
# A/B benchmark: naatre vs git on the same machine (plan M4b: "A/B vs git in
# docs/bench.md — reported, not gated").
#
# Operations (3 runs each; every run times the WHOLE operation — the side's
# commands are summed into one duration — and the median of the run sums is
# reported):
#   sparse clone of one directory:
#     git:  clone --depth 1 --filter=blob:none --sparse  +  sparse-checkout set <dir>
#     naatre: clone --sparse <dir>   (the one-time `naatre import` is EXCLUDED)
#   one-line edit on a checkout:
#     git:  add -A + commit
#     naatre: commit + land
#
# Usage: scripts/bench_vs_git.sh [path-to-an-existing-git-repo]
# Without an argument a synthetic repo is built in a tempdir: 200 commits x
# 3 files x ~50 lines, via the git CLI. A provided repo is copied once into
# the workdir (never mutated) so the partial-clone serving flags can be set.
#
# Output: a markdown table plus an honest win/lose line per pair.

set -euo pipefail

COMMITS="${NAATRE_BENCH_COMMITS:-200}"
LINES="${NAATRE_BENCH_LINES:-50}"
RUNS=3
PORT="${NAATRE_BENCH_PORT:-17400}"

work="$(mktemp -d "${TMPDIR:-/tmp}/naatre-bench.XXXXXX")"
SERVER_PID=""
cleanup() {
    if [ -n "$SERVER_PID" ]; then kill "$SERVER_PID" 2>/dev/null || true; fi
    rm -rf "$work"
}
trap cleanup EXIT

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/target/debug/naatre"
if [ ! -x "$BIN" ]; then
    echo "building naatre (dev profile)…"
    (cd "$ROOT" && cargo build --locked -p naatre-cli >/dev/null)
fi

# ---- subject repo -----------------------------------------------------------

if [ $# -ge 1 ]; then
    echo "preparing a throwaway copy of $1 (source is never touched)…"
    GIT_SRC="$work/src"
    git clone -q --no-local "$1" "$GIT_SRC"
else
    GIT_SRC="$work/src"
    git init -q -b main "$GIT_SRC"
    python3 - "$GIT_SRC" "$COMMITS" "$LINES" <<'PY'
import os, subprocess, sys
src, commits, lines = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])

def run(*args):
    subprocess.run(["git", "-C", src, *args], check=True)

for k in range(1, 4):
    os.makedirs(f"{src}/dir{k}", exist_ok=True)
for c in range(commits):
    for k in range(1, 4):
        body = "".join(f"commit {c} line {j} of file {k}\n" for j in range(lines))
        with open(f"{src}/dir{k}/file{k}.txt", "w") as f:
            f.write(body)
    run("add", "-A")
    run("commit", "-q", "-m", f"commit {c}")
PY
fi

# Partial clone over file:// needs the serving side to allow filters.
git -C "$GIT_SRC" config uploadpack.allowFilter true
git -C "$GIT_SRC" config uploadpack.allowAnySHA1InWant true

# The sparse prefix both sides check out: dir1, or the first directory.
if [ -d "$GIT_SRC/dir1" ]; then
    SPARSE_DIR="dir1"
else
    SPARSE_DIR="$(git -C "$GIT_SRC" ls-tree -d --name-only HEAD | head -1)"
fi
[ -n "$SPARSE_DIR" ] || { echo "bench needs at least one directory in HEAD"; exit 1; }

# ---- timing helpers ----------------------------------------------------------

# timed <cmd…>: run the command, print its duration in seconds.
timed() {
    python3 -c '
import subprocess, sys, time
t = time.monotonic()
r = subprocess.run(sys.argv[1:], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
d = time.monotonic() - t
if r.returncode != 0:
    sys.stderr.write(f"bench: command failed ({r.returncode}): {sys.argv[1:]}\n")
    sys.exit(1)
print(f"{d:.4f}")' "$@"
}

# append_sum <timesfile> <d1> <d2>: append one run's whole-operation duration
# (the two commands of the run, summed) to <timesfile>.
append_sum() {
    python3 -c '
import sys
with open(sys.argv[1], "a") as f:
    f.write(f"{float(sys.argv[2]) + float(sys.argv[3]):.4f}\n")' "$@"
}

# median <timesfile>
median() {
    python3 -c '
import sys
xs = sorted(float(x) for x in open(sys.argv[1]) if x.strip())
print(f"{xs[len(xs)//2]:.4f}")' "$1"
}

# timed_in <dir> <cmd…>: run in <dir> (the cd itself is outside the timed
# window), printing the duration in seconds.
timed_in() {
    local dir="$1"
    shift
    (cd "$dir" && timed "$@")
}

# verdict <git-seconds> <naatre-seconds> — printed honestly, no cherry-picking.
verdict() {
    python3 - "$1" "$2" <<'PY'
import sys
g, n = float(sys.argv[1]), float(sys.argv[2])
if n < g:
    print(f"→ naatre wins: {g/n:.2f}x faster than git")
elif n > g:
    print(f"→ naatre LOSES: {n/g:.2f}x slower than git")
else:
    print("→ tie")
PY
}

# ---- (a) git: sparse clone ---------------------------------------------------

GIT_T="$work/git-times"
: > "$GIT_T"
for _ in $(seq 1 "$RUNS"); do
    rm -rf "$work/git-clone"
    t1="$(timed git clone -q --depth 1 --filter=blob:none --sparse \
        "file://$GIT_SRC" "$work/git-clone")"
    t2="$(timed git -C "$work/git-clone" sparse-checkout set "$SPARSE_DIR")"
    append_sum "$GIT_T" "$t1" "$t2"
done

# ---- (b) naatre: import (excluded) + serve + sparse clone --------------------

DATA="$work/data"
"$BIN" init --data "$DATA" > "$work/init.out"
TOKEN="$(sed -n 's/^admin token: //p' "$work/init.out")"
[ -n "$TOKEN" ] || { echo "could not read the admin token from init output"; exit 1; }

# The import runs offline BEFORE serve (the data-dir flock excludes both).
"$BIN" import --data "$DATA" --into bench "$GIT_SRC" > "$work/import.out"

"$BIN" serve --data "$DATA" --listen "127.0.0.1:$PORT" \
    > "$work/serve.out" 2> "$work/serve.err" &
SERVER_PID=$!
up=0
for _ in $(seq 1 100); do
    if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then up=1; break; fi
    sleep 0.1
done
[ "$up" = 1 ] || { echo "naatre serve did not come up"; cat "$work/serve.err"; exit 1; }

NAATRE_T="$work/naatre-times"
: > "$NAATRE_T"
for _ in $(seq 1 "$RUNS"); do
    rm -rf "$work/naatre-clone"
    # One command per run: the run duration IS the operation duration.
    timed env \
        NAATRE_URL="http://127.0.0.1:$PORT" NAATRE_TOKEN="$TOKEN" \
        "$BIN" clone "http://127.0.0.1:$PORT/bench" --sparse "$SPARSE_DIR" \
        --into "$work/naatre-clone" >> "$NAATRE_T"
done

# ---- (c) one-line edit: commit (+ land) --------------------------------------

# An existing file under the sparse prefix, same on both sides.
EDIT_FILE="$(cd "$work/git-clone" && find "$SPARSE_DIR" -type f | head -1)"
[ -n "$EDIT_FILE" ] || { echo "no file under $SPARSE_DIR to edit"; exit 1; }

GIT_C="$work/git-commit-times"
: > "$GIT_C"
for i in $(seq 1 "$RUNS"); do
    echo "bench edit $i" >> "$work/git-clone/$EDIT_FILE"
    t1="$(timed git -C "$work/git-clone" add -A)"
    t2="$(timed git -C "$work/git-clone" \
        -c user.name="naatre bench" -c user.email=bench@naatre.local \
        commit -q -m "bench $i")"
    append_sum "$GIT_C" "$t1" "$t2"
done

NAATRE_C="$work/naatre-commit-times"
: > "$NAATRE_C"
for i in $(seq 1 "$RUNS"); do
    echo "bench edit $i" >> "$work/naatre-clone/$EDIT_FILE"
    t1="$(timed_in "$work/naatre-clone" env \
        NAATRE_URL="http://127.0.0.1:$PORT" NAATRE_TOKEN="$TOKEN" \
        "$BIN" commit -m "bench $i")"
    t2="$(timed_in "$work/naatre-clone" env \
        NAATRE_URL="http://127.0.0.1:$PORT" NAATRE_TOKEN="$TOKEN" \
        "$BIN" land --key "bench-$i")"
    append_sum "$NAATRE_C" "$t1" "$t2"
done

# ---- report ------------------------------------------------------------------

CLONE_G="$(median "$GIT_T")"
CLONE_N="$(median "$NAATRE_T")"
COMMIT_G="$(median "$GIT_C")"
COMMIT_N="$(median "$NAATRE_C")"

echo
echo "- subject: ${1:-synthetic ($COMMITS commits x 3 files x ~$LINES lines)}; sparse prefix: $SPARSE_DIR"
echo "- timings: median of $RUNS runs, each run timing the whole operation, dev build, seconds"
echo
echo "| operation | git (s) | naatre (s) |"
echo "|---|---|---|"
echo "| sparse clone of one dir | $CLONE_G | $CLONE_N |"
echo "| one-line edit: commit (+land) | $COMMIT_G | $COMMIT_N |"
echo
verdict "$CLONE_G" "$CLONE_N"
verdict "$COMMIT_G" "$COMMIT_N"
