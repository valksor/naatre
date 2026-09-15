#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
build_root="$(mktemp -d "${TMPDIR:-/tmp}/naatre-jvm-http.XXXXXX")"
trap 'rm -rf "$build_root"' EXIT

cd "$repo_root"
sources=()
while IFS= read -r source; do sources+=("$source"); done < <(
  find \
    sdk/jvm/src/main/java \
    sdk/jvm/generated/java \
    sdk/jvm/java-http/src/main/java \
    sdk/jvm/java-http/src/test/java \
    -name '*.java' -type f | sort
)
javac --release 17 -Xlint:all -Werror -parameters -d "$build_root/classes" "${sources[@]}"
java -cp "$build_root/classes" io.naatre.sdk.http.JavaHttpTransportConformance
