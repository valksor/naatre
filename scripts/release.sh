#!/usr/bin/env bash
# The M11 release step: build the release artifact set, emit a CycloneDX
# SBOM for the shipped binary's dependency closure, write SHA256SUMS over
# EVERY shipped file (binary, licenses, SBOM — the SBOM is written BEFORE
# the sums, so the minisign signature over SHA256SUMS covers it), and —
# when the release key is provided — sign the checksums with minisign.
# Verification is scripts/release-verify.sh; the commands are documented
# in SECURITY.md.
#
# The PRIVATE KEY IS NEVER in the repo: NAATRE_MINISIGN_KEY names the
# secret key file (keep it offline; a password-protected key is the
# recommended posture — minisign prompts at sign time). Without the
# variable the default mode stops with a clean error and NO artifacts;
# `--no-sign` produces the unsigned artifact set (CI's shape — CI never
# holds any release key; the repo carries only the CI TEST key).
# The verify round-trip checks against NAATRE_MINISIGN_PUB (default: the
# CI test key deploy/naatre-release-test.pub) — a PRODUCTION release sets
# it to the maintainer's published key, which arrives with the
# maintainer, never with the repo.
#
# Usage: scripts/release.sh [--no-sign] <dest-dir> [target-triple]
#   <target-triple> defaults to the host; CI passes
#   x86_64-unknown-linux-musl for the static build.

set -euo pipefail

NO_SIGN=0
if [[ "${1:-}" == "--no-sign" ]]; then
	NO_SIGN=1
	shift
fi
[[ $# -ge 1 ]] || {
	printf 'usage: %s [--no-sign] <dest-dir> [target-triple]\n' "$0" >&2
	exit 3
}
DEST=$1
TARGET=${2:-$(rustc -vV | sed -n 's/^host: //p')}
BIN="target/$TARGET/release/naatre"
SBOM_SRC="crates/naatre-cli/naatre_bin.cdx.json"

step() { printf '\n== %s\n' "$*"; }
fail() {
	printf 'release: %s\n' "$*" >&2
	exit 1
}
# sha256sum everywhere but macOS; shasum -a 256 is the same digest.
sum() {
	if command -v sha256sum >/dev/null 2>&1; then
		(cd "$DEST" && sha256sum "$@")
	else
		(cd "$DEST" && shasum -a 256 "$@")
	fi
}

step "build (--release --locked --target $TARGET)"
command -v rustup >/dev/null 2>&1 && rustup target add "$TARGET" >/dev/null
cargo build --release --locked --target "$TARGET"
[[ -f "$BIN" ]] || fail "the binary did not land at $BIN"

# The shipped binary carries the versioned name the checksums, the fixture,
# and the docs use: naatre-<ver>-<target>.
VER=$(sed -n 's/^version = "\(.*\)"$/\1/p' Cargo.toml | head -n 1)
[[ -n "$VER" ]] || fail "no workspace version found in Cargo.toml"
ARTIFACT="naatre-$VER-$TARGET"

step "stage artifacts in $DEST"
mkdir -p "$DEST"
cp "$BIN" "$DEST/$ARTIFACT"
cp THIRD_PARTY_LICENSES.md "$DEST/"

step "CycloneDX SBOM (cargo-cyclonedx: a documented tool dependency, not a runtime dep)"
command -v cargo-cyclonedx >/dev/null 2>&1 ||
	fail "cargo-cyclonedx is not installed: cargo install cargo-cyclonedx --locked --version <the version pinned in ci.yml>"
# The binary target's SBOM is the shipped artifact's dependency closure;
# the generated files land beside the crate sources and are cleaned up.
cargo cyclonedx --describe binaries --format json
[[ -f "$SBOM_SRC" ]] || fail "cargo-cyclonedx wrote no $SBOM_SRC"
cp "$SBOM_SRC" "$DEST/naatre.cdx.json"
command rm -f crates/naatre-cli/naatre_bin.cdx.json \
	crates/naatre-cli/naatre-cli.cdx.json \
	crates/naatre-core/naatre-core.cdx.json \
	crates/naatre-server/naatre-server.cdx.json \
	crates/naatre-store/naatre-store.cdx.json

# AFTER the SBOM: SHA256SUMS lists every shipped file, so the signature
# over it covers the SBOM too.
step "SHA256SUMS"
sum "$ARTIFACT" THIRD_PARTY_LICENSES.md naatre.cdx.json >"$DEST/SHA256SUMS"
cat "$DEST/SHA256SUMS"

if [[ "$NO_SIGN" == 1 ]]; then
	step "unsigned (--no-sign): CI's shape — CI never holds any release key"
	printf 'release: %s (SHA256SUMS, naatre.cdx.json, no signature)\n' "$DEST"
	exit 0
fi

step "minisign signature"
KEY=${NAATRE_MINISIGN_KEY:-}
if [[ -z "$KEY" ]]; then
	fail "NAATRE_MINISIGN_KEY is not set: point it at the release secret key \
(the private key is never in the repo); use --no-sign for the unsigned set"
fi
[[ -f "$KEY" ]] || fail "NAATRE_MINISIGN_KEY=$KEY is not a file"
command -v minisign >/dev/null 2>&1 ||
	fail "minisign is not installed (the documented signing tool)"
minisign -S -s "$KEY" -m "$DEST/SHA256SUMS" -x "$DEST/SHA256SUMS.minisig" \
	-t "naatre release SHA256SUMS"

step "verify round-trip (the gate release-verify.sh runs)"
PUB=${NAATRE_MINISIGN_PUB:-deploy/naatre-release-test.pub}
[[ -f "$PUB" ]] || fail "the public key $PUB is missing (set NAATRE_MINISIGN_PUB to the \
maintainer's published release key)"
if ! minisign -V -q -p "$PUB" -m "$DEST/SHA256SUMS" \
	-x "$DEST/SHA256SUMS.minisig"; then
	fail "the signature does not verify against $PUB (the signing key and the \
public key disagree — pass the matching NAATRE_MINISIGN_PUB)"
fi
printf 'release: signature verifies against %s\n' "$PUB"

step "artifacts"
ls -la "$DEST"
