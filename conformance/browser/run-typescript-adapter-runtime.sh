#!/bin/sh
set -eu

: "${CHROME_HEADLESS_SHELL:?set CHROME_HEADLESS_SHELL to Chrome headless shell 153.0.8010.36}"

browser_version=$("$CHROME_HEADLESS_SHELL" --version)
if [ "$browser_version" != "Google Chrome for Testing 153.0.8010.36" ]; then
  echo "unexpected Chrome headless shell version: $browser_version" >&2
  exit 1
fi

dom_file=$(mktemp /tmp/naatre-adapter-browser-dom.XXXXXX)
error_file=$(mktemp /tmp/naatre-adapter-browser-errors.XXXXXX)
server_log=$(mktemp /tmp/naatre-adapter-browser-server.XXXXXX)
server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -f "$dom_file" "$error_file" "$server_log"
}
trap cleanup EXIT HUP INT TERM

python3 -u -m http.server 0 --bind 127.0.0.1 >"$server_log" 2>&1 &
server_pid=$!
server_port=""
attempt=0
while [ "$attempt" -lt 100 ]; do
  server_port=$(sed -n 's/.* port \([0-9][0-9]*\) .*/\1/p' "$server_log" | head -n 1)
  if [ -n "$server_port" ]; then
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$server_log" >&2
    exit 1
  fi
  attempt=$((attempt + 1))
  sleep 0.05
done
if [ -z "$server_port" ]; then
  cat "$server_log" >&2
  exit 1
fi

if ! "$CHROME_HEADLESS_SHELL" \
  --headless \
  --disable-gpu \
  --no-sandbox \
  --virtual-time-budget=5000 \
  --dump-dom \
  "http://127.0.0.1:${server_port}/conformance/browser/typescript-adapter-runtime.html" \
  >"$dom_file" 2>"$error_file"; then
  cat "$error_file" >&2
  exit 1
fi

if ! grep -q '<html lang="en" data-status="passed">' "$dom_file"; then
  cat "$dom_file"
  cat "$error_file" >&2
  exit 1
fi

sed -n '/<pre id="result">/p' "$dom_file"
