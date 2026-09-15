#!/usr/bin/env bash
set -euo pipefail

: "${ANDROID_SDK_ROOT:?set ANDROID_SDK_ROOT to an Android SDK containing platforms/android-36 and build-tools/36.0.0}"
: "${KOTLIN_HOME:?set KOTLIN_HOME to a Kotlin 2.2.20 compiler distribution}"
: "${KOTLIN_COROUTINES_JAR:?set KOTLIN_COROUTINES_JAR to kotlinx-coroutines-core-jvm 1.10.2}"

android_api="${NAATRE_ANDROID_COMPILE_SDK:-36}"
build_tools="${NAATRE_ANDROID_BUILD_TOOLS:-36.0.0}"
minimum_api="${NAATRE_ANDROID_MIN_API:-26}"
android_jar="$ANDROID_SDK_ROOT/platforms/android-$android_api/android.jar"
d8="$ANDROID_SDK_ROOT/build-tools/$build_tools/d8"
test -f "$android_jar"
test -x "$d8"

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
build_root="$(mktemp -d "${TMPDIR:-/tmp}/naatre-jvm-android.XXXXXX")"
trap 'rm -rf "$build_root"' EXIT

cd "$repo_root"
java_sources=()
while IFS= read -r source; do java_sources+=("$source"); done < <(
  find sdk/jvm/src/main/java sdk/jvm/generated/java sdk/jvm/android/src/test/java \
    -name '*.java' -type f | sort
)
javac --release 17 -Xlint:all -Werror -parameters \
  -d "$build_root/java" "${java_sources[@]}"

kotlin_sources=()
while IFS= read -r source; do kotlin_sources+=("$source"); done < <(
  find sdk/jvm/src/main/kotlin sdk/jvm/generated/kotlin -name '*.kt' -type f | sort
)
"$KOTLIN_HOME/bin/kotlinc" -jvm-target 17 -Werror \
  -classpath "$build_root/java:$KOTLIN_COROUTINES_JAR" \
  -d "$build_root/kotlin" "${kotlin_sources[@]}"

java -cp "$build_root/java" io.naatre.sdk.android.AndroidCompatibilityConformance

jar --create --file "$build_root/android-profile.jar" \
  -C "$build_root/java" . \
  -C "$build_root/kotlin" .
if jar --list --file "$build_root/android-profile.jar" | grep -q '^io/naatre/sdk/http/'; then
  echo "server-only java.net.http adapter entered the Android artifact" >&2
  exit 1
fi

"$d8" --release --min-api "$minimum_api" \
  --lib "$android_jar" \
  --classpath "$KOTLIN_HOME/lib/kotlin-stdlib.jar" \
  --classpath "$KOTLIN_COROUTINES_JAR" \
  --output "$build_root/android-profile-dex.zip" \
  "$build_root/android-profile.jar"
unzip -Z1 "$build_root/android-profile-dex.zip" | grep -qx 'classes.dex'

javap -v -classpath "$build_root/java" io.naatre.sdk.generated.java.GetAccount\$Account \
  | grep -q 'io.naatre.sdk.annotations.Nullable'
javap -v -classpath "$build_root/java" io.naatre.sdk.generated.java.GetAccount\$Variables \
  | grep -q 'io.naatre.sdk.annotations.NonNull'

printf '%s\n' \
  "{\"compileSdk\":\"$android_api\",\"minApi\":\"$minimum_api\",\"profile\":\"sdk.jvm.android-compat-1\",\"status\":\"passed\"}"
