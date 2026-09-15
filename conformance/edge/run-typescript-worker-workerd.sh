#!/bin/sh
set -eu

runtime_log=$(mktemp /tmp/naatre-worker-workerd-log.XXXXXX)
result_file=$(mktemp /tmp/naatre-worker-workerd-result.XXXXXX)
runtime_pid=""
cleanup() {
  if [ -n "$runtime_pid" ]; then
    kill "$runtime_pid" 2>/dev/null || true
    wait "$runtime_pid" 2>/dev/null || true
  fi
  rm -f "$runtime_log" "$result_file"
}
trap cleanup EXIT HUP INT TERM

if [ -n "${WORKERD_BIN:-}" ]; then
  runtime_version=$("$WORKERD_BIN" --version)
  if [ "$runtime_version" != "workerd 2026-09-14" ]; then
    echo "unexpected workerd version: $runtime_version" >&2
    exit 1
  fi
  "$WORKERD_BIN" serve conformance/edge/typescript-worker-workerd.capnp >"$runtime_log" 2>&1 &
else
  npx --yes --userconfig=/dev/null workerd@1.20260914.1 serve \
    conformance/edge/typescript-worker-workerd.capnp >"$runtime_log" 2>&1 &
fi
runtime_pid=$!
runtime_port=""
attempt=0
while [ "$attempt" -lt 200 ]; do
  runtime_port=$(sed -n 's@.*[Ll]istening on http://[^: ]*:\([0-9][0-9]*\).*@\1@p' "$runtime_log" | head -n 1)
  if [ -n "$runtime_port" ]; then break; fi
  if ! kill -0 "$runtime_pid" 2>/dev/null; then
    cat "$runtime_log" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 0.05
done
if [ -z "$runtime_port" ]; then
  cat "$runtime_log" >&2
  exit 1
fi

curl --fail --silent --show-error "http://127.0.0.1:${runtime_port}/" >"$result_file"
node --input-type=module -e '
  import { readFileSync } from "node:fs";
  const result = JSON.parse(readFileSync(process.argv[1], "utf8"));
  if (result.profile !== "worker.javascript-typescript-1" || result.status !== "passed" || result.runtime !== "workerd-2026-09-14") process.exit(1);
  if (result.implementations.length !== 1 || result.implementations[0].language !== "plain-javascript") process.exit(1);
' "$result_file"
cat "$result_file"
