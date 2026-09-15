#!/usr/bin/env bash
set -euo pipefail

kernel="$(uname -s)"
architecture="$(uname -m)"
if [[ "$kernel" != Linux || ! "$architecture" =~ ^(aarch64|arm64)$ ]]; then
  printf 'self-data must be Linux ARM64, got %s/%s\n' "$kernel" "$architecture" >&2
  exit 1
fi

for tool in "$@"; do
  if ! command -v "$tool" >/dev/null; then
    printf 'self-data prerequisite is unavailable: %s\n' "$tool" >&2
    exit 1
  fi
done
