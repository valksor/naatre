#!/usr/bin/env bash
# Clean-dir cold boot (plan M5 check): init → serve → token+URL → repo
# create → clone from a second directory → commit → land → read back at
# main → snapshot → restore into a third directory.
#
# Two notes on the flow, both facts about the current binary:
# - A fresh repo has an unborn `main`: the refs route refuses `main`
#   (land-only) and land moves but never creates it, so nothing can be
#   cloned until the repo is seeded. `naatre import` is the product's
#   seeding path ("imports seed main"), and it takes the same data-dir
#   flock as serve — so the script stops serve, imports a tiny git repo,
#   and restarts serve.
# - There is no `log` client verb yet, so "land landed" is proven by
#   reading the file back at main (`naatre cat --at main`).
#
# Needs on PATH: git (for the import seed). Everything else is the binary.
#
# Usage: scripts/coldboot.sh path/to/naatre [port]

set -euo pipefail

[[ $# -ge 1 && -x "$1" ]] || {
	printf 'usage: %s path/to/naatre [port]\n' "$0" >&2
	exit 1
}
BIN=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
PORT=${2:-${NAATRE_COLDBOOT_PORT:-17400}}

ROOT=$(mktemp -d "${TMPDIR:-/tmp}/naatre-coldboot.XXXXXX")
DATA="$ROOT/data"
SERVE_PID=""
step() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'coldboot: %s\n' "$*" >&2
	exit 1
}
cleanup() {
	if [[ -n "$SERVE_PID" ]]; then
		kill "$SERVE_PID" 2>/dev/null || true
	fi
	rm -rf "$ROOT"
}
trap cleanup EXIT

wait_ready() {
	for _ in $(seq 1 100); do
		if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
			return 0
		fi
		sleep 0.1
	done
	fail "server never accepted connections on 127.0.0.1:$PORT (log: $ROOT/serve.log)"
}

start_serve() {
	"$BIN" serve --data "$DATA" --listen "127.0.0.1:$PORT" >"$ROOT/serve.log" 2>&1 &
	SERVE_PID=$!
	wait_ready
}

step "init (fresh data dir)"
OUT=$("$BIN" init --data "$DATA")
grep -q '^admin token: ' <<<"$OUT" || fail "init printed no admin token"
ADMIN=$(awk '/^admin token: /{print $3}' <<<"$OUT")
export NAATRE_URL="http://127.0.0.1:$PORT"
export NAATRE_TOKEN="$ADMIN"

step "serve"
start_serve

step "mint writer token"
WRITER=$("$BIN" token create agent --role writer | awk '/^token: /{print $2}')
[[ -n "$WRITER" ]] || fail "token create printed no token"

step "repo create demo"
"$BIN" repo create demo

step "seed main via import (stop serve: import holds the data-dir flock)"
kill "$SERVE_PID"
wait "$SERVE_PID" 2>/dev/null || true
SERVE_PID=""
G="$ROOT/seed-git"
git -c init.defaultBranch=main init -q "$G"
printf 'seed\n' >"$G/seed.txt"
git -C "$G" add seed.txt
git -C "$G" -c user.email=seed@example.com -c user.name=seed commit -q -m seed
"$BIN" import --data "$DATA" --into demo "$G"
start_serve

step "clone from a second directory"
mkdir "$ROOT/dir2"
(
	cd "$ROOT/dir2" &&
		NAATRE_TOKEN="$WRITER" "$BIN" clone "$NAATRE_URL/demo" --into demo
)
[[ -f "$ROOT/dir2/demo/seed.txt" ]] || fail "clone did not materialize seed.txt"

step "commit a file"
printf 'hello from the cold boot\n' >"$ROOT/dir2/demo/README.md"
(cd "$ROOT/dir2/demo" && "$BIN" commit -m "cold boot commit")

step "land"
(cd "$ROOT/dir2/demo" && "$BIN" land)

step "landed head is readable at main"
(cd "$ROOT/dir2/demo" && "$BIN" cat README.md --at main) |
	grep -qx 'hello from the cold boot' ||
	fail "README.md at main does not carry the committed content"

step "snapshot"
SNAP=$(
	(cd "$ROOT/dir2/demo" && "$BIN" snapshot) |
		sed -n 's/.*"id":"\([0-9a-f]*\)".*/\1/p'
)
[[ "$SNAP" =~ ^[0-9a-f]{64}$ ]] || fail "snapshot printed no 64-hex id"

step "restore into a third directory"
mkdir "$ROOT/dir3"
(
	cd "$ROOT/dir3" &&
		NAATRE_TOKEN="$WRITER" "$BIN" clone "$NAATRE_URL/demo" --into demo
)
rm "$ROOT/dir3/demo/README.md"
(cd "$ROOT/dir3/demo" && "$BIN" restore "$SNAP" --force)
grep -qx 'hello from the cold boot' "$ROOT/dir3/demo/README.md" ||
	fail "restore did not bring README.md back"

step "cold boot OK"
