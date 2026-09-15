#!/usr/bin/env bash
set -euo pipefail

workflow_directory="${1:-.github/workflows}"
workflow_files=()
while IFS= read -r file; do
  workflow_files+=("$file")
done < <(find "$workflow_directory" -type f \( -name '*.yml' -o -name '*.yaml' \) -print | sort)

if ((${#workflow_files[@]} == 0)); then
  echo "no GitHub Actions workflows found" >&2
  exit 1
fi

runner_lines="$(grep -HnE '^[[:space:]]+runs-on:' "${workflow_files[@]}" || true)"
if [[ -z "$runner_lines" ]]; then
  echo "no GitHub Actions runner declarations found" >&2
  exit 1
fi

unexpected="$(printf '%s\n' "$runner_lines" | grep -vE 'runs-on:[[:space:]]+self-data[[:space:]]*$' || true)"
if [[ -n "$unexpected" ]]; then
  echo "all GitHub Actions jobs must run on self-data:" >&2
  printf '%s\n' "$unexpected" >&2
  exit 1
fi

untrusted_triggers="$(grep -HnE '(^|[^[:alnum:]_])pull_request(_target)?([^[:alnum:]_]|$)' "${workflow_files[@]}" || true)"
if [[ -n "$untrusted_triggers" ]]; then
  echo "pull request code must not execute on persistent self-data runners:" >&2
  printf '%s\n' "$untrusted_triggers" >&2
  exit 1
fi
