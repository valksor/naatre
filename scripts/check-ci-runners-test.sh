#!/usr/bin/env bash
set -euo pipefail

fixture_root="$(mktemp -d)"
trap 'rm -rf "$fixture_root"' EXIT

write_workflow() {
  printf '%b\n' "$1" >"$fixture_root/ci.yml"
}

expect_rejected() {
  local name="$1"
  local trigger="$2"
  write_workflow "name: policy-test\n$trigger\njobs:\n  test:\n    runs-on: self-data"
  if bash scripts/check-ci-runners.sh "$fixture_root" >/dev/null 2>&1; then
    printf 'runner policy accepted forbidden trigger form: %s\n' "$name" >&2
    exit 1
  fi
}

write_workflow 'name: policy-test\non:\n  push:\n    branches: [master]\njobs:\n  test:\n    runs-on: self-data'
bash scripts/check-ci-runners.sh "$fixture_root"

expect_rejected pull-request-mapping 'on:\n  pull_request:'
expect_rejected pull-request-target-mapping 'on:\n  pull_request_target:'
expect_rejected pull-request-scalar 'on: pull_request'
expect_rejected pull-request-target-scalar 'on: pull_request_target'
expect_rejected pull-request-flow 'on: [push, pull_request]'
expect_rejected pull-request-target-flow 'on: [push, pull_request_target]'
