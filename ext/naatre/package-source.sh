#!/bin/sh
set -eu

revision=${1:-HEAD}
output=${2:-naatre-0.1.0.tgz}
prefix=naatre-0.1.0/

git archive --format=tar --prefix="$prefix" "$revision":ext/naatre | gzip -n > "$output"
shasum -a 256 "$output" > "$output.sha256"
