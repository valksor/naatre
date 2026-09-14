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
trap 'rm -f "$dom_file" "$error_file"' EXIT

if ! "$CHROME_HEADLESS_SHELL" \
  --headless \
  --disable-gpu \
  --no-sandbox \
  --virtual-time-budget=5000 \
  --dump-dom \
  http://127.0.0.1:18775/conformance/browser/typescript-adapter-runtime.html \
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
