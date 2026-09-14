#!/usr/bin/env bash
set -euo pipefail

: "${KOTLIN_HOME:?set KOTLIN_HOME to a Kotlin 2.2.20 compiler distribution}"
: "${KOTLIN_COROUTINES_JAR:?set KOTLIN_COROUTINES_JAR to kotlinx-coroutines-core-jvm 1.10.2}"

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
build_root="$(mktemp -d "${TMPDIR:-/tmp}/naatre-jvm.XXXXXX")"
trap 'rm -rf "$build_root"' EXIT

cd "$repo_root"
java_sources=()
while IFS= read -r source; do java_sources+=("$source"); done < <(find sdk/jvm/src/main/java sdk/jvm/generated/java -name '*.java' -type f | sort)
javac --release 17 -Xlint:all -Werror -parameters -d "$build_root/java" "${java_sources[@]}"

kotlin_sources=()
while IFS= read -r source; do kotlin_sources+=("$source"); done < <(find sdk/jvm/src/main/kotlin sdk/jvm/generated/kotlin sdk/jvm/src/test/kotlin -name '*.kt' -type f | sort)
"$KOTLIN_HOME/bin/kotlinc" -jvm-target 17 -Werror \
  -classpath "$build_root/java:$KOTLIN_COROUTINES_JAR" \
  -d "$build_root/kotlin.jar" "${kotlin_sources[@]}"

"$KOTLIN_HOME/bin/kotlin" \
  -classpath "$build_root/java:$build_root/kotlin.jar:$KOTLIN_COROUTINES_JAR" \
  io.naatre.sdk.JvmConformanceKt "$repo_root"

javap -v -classpath "$build_root/java" io.naatre.sdk.generated.java.GetAccount\$Account \
  | grep -q 'io.naatre.sdk.annotations.Nullable'
javap -v -classpath "$build_root/java" io.naatre.sdk.generated.java.GetAccount\$Variables \
  | grep -q 'io.naatre.sdk.annotations.NonNull'
