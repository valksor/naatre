#!/usr/bin/env bash
# Verify a release's SHA256SUMS: the minisign signature against a public
# key, then — when the checksummed files are staged — every checksum line.
# `--selftest` additionally proves the gate bites: a tampered SHA256SUMS
# line must FAIL verification (CI runs this mode on every push, against
# the fixture in scripts/release-fixture, bound to the CI TEST key).
#
# The repo carries ONLY the CI test key
# (deploy/naatre-release-test.pub): the PRODUCTION release key does not
# exist yet — the maintainer generates it offline (minisign -G) and
# publishes it before the first release, passing it via the second
# argument (or replacing the default). Until then, signatures verified
# against the test key prove MECHANICS, not release trust.
#
# Usage: scripts/release-verify.sh [--selftest] <dir-with-SHA256SUMS+minisig> [pubkey]

set -euo pipefail

PUB=deploy/naatre-release-test.pub
SELFTEST=0
if [[ "${1:-}" == "--selftest" ]]; then
	SELFTEST=1
	shift
fi
[[ $# -ge 1 ]] || {
	printf 'usage: %s [--selftest] <dir-with-SHA256SUMS+minisig> [pubkey]\n' "$0" >&2
	exit 3
}
DIR=$1
if [[ $# -ge 2 ]]; then
	PUB=$2
fi

step() { printf '== %s\n' "$*"; }
fail() {
	printf 'release-verify: %s\n' "$*" >&2
	exit 1
}
sum_check() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum --check --quiet "$1"
	else
		shasum -a 256 --check "$1"
	fi
}

command -v minisign >/dev/null 2>&1 ||
	fail "minisign is not installed (the documented verify tool)"
[[ -f "$PUB" ]] || fail "the public key $PUB is missing"
[[ -f "$DIR/SHA256SUMS" && -f "$DIR/SHA256SUMS.minisig" ]] ||
	fail "$DIR carries no SHA256SUMS + SHA256SUMS.minisig"

step "minisign signature vs $PUB"
minisign -V -q -p "$PUB" -m "$DIR/SHA256SUMS" -x "$DIR/SHA256SUMS.minisig" ||
	fail "the signature is INVALID for $DIR/SHA256SUMS"
printf 'release-verify: signature OK\n'

step "checksums (when the checksummed files are staged)"
MISSING=0
while IFS= read -r line; do
	f=$(printf '%s' "$line" | awk '{print $2}')
	[[ -f "$DIR/$f" ]] || MISSING=1
done <"$DIR/SHA256SUMS"
if [[ "$MISSING" == 0 ]]; then
	(cd "$DIR" && sum_check SHA256SUMS)
	printf 'release-verify: checksums OK\n'
else
	printf 'release-verify: checksummed files not staged here (fixture mode) — signature + tamper only\n'
fi

if [[ "$SELFTEST" == 1 ]]; then
	step "selftest: a tampered SHA256SUMS line must FAIL verification"
	TMP=$(mktemp -d "${TMPDIR:-/tmp}/naatre-release-verify.XXXXXX")
	trap 'rm -rf "$TMP"' EXIT
	cp "$DIR/SHA256SUMS" "$TMP/SHA256SUMS"
	printf '0000000000000000000000000000000000000000000000000000000000000000  tampered-extra-file\n' >>"$TMP/SHA256SUMS"
	if minisign -V -q -p "$PUB" -m "$TMP/SHA256SUMS" -x "$DIR/SHA256SUMS.minisig" 2>/dev/null; then
		fail "a tampered SHA256SUMS VERIFIED — the gate is broken"
	fi
	printf 'release-verify: tamper rejected (the gate bites)\n'
fi
