#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

sdk/jvm/check.sh
sdk/jvm/check-server-adapter.sh
sdk/jvm/check-android.sh

printf '%s\n' '{"profile":"sdk.jvm.adapters-1","status":"passed"}'
