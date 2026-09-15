import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/jvm-adapters.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.jvm.adapters-1");
assert.equal(fixture.ownerIssue, 82);
assert.equal(fixture.coreAuthority, "sdk.jvm.core-1");
assert.deepEqual(fixture.dependencies, [{
  issue: 40,
  revision: "f2455310695238e11b81e36d121243191a7aee1c",
  profile: "sdk.jvm.core-1",
  manifest: fixture.dependencies[0].manifest,
}]);
assert.equal(fixture.dependencies[0].manifest.path, "conformance/v1/jvm-sdk.json");

const allEvidence = [
  fixture.dependencies[0].manifest,
  ...fixture.sources.server,
  ...fixture.sources.android,
  ...fixture.evidence,
];
for (const entry of allEvidence) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

const serverSourceSet = sourceSetSha256(fixture.sources.server);
const androidSourceSet = sourceSetSha256(fixture.sources.android);
const serverReport = report(fixture.profiles.server.report);
const androidReport = report(fixture.profiles.android.report);

assert.equal(serverReport.profile, fixture.profiles.server.profile);
assert.equal(serverReport.status, "passed");
assert.equal(serverReport.jdk, fixture.toolchains.java.runtime);
assert.equal(serverReport.bytecodeTarget, fixture.toolchains.java.bytecodeTarget);
assert.equal(serverReport.sourceSetSha256, serverSourceSet);
assert.equal(serverReport.command, "sdk/jvm/check-server-adapter.sh");
assert.deepEqual(fixture.profiles.server.capabilities, [
  "unary-http",
  "manual-307-308-redirects",
  "active-cancellation",
  "transport-close",
]);

assert.equal(androidReport.profile, fixture.profiles.android.profile);
assert.equal(androidReport.status, "passed");
assert.equal(androidReport.runtimeStatus, "not-executed");
assert.equal(androidReport.minimumApi, fixture.toolchains.android.minimumApi);
assert.equal(androidReport.compileSdk, fixture.toolchains.android.compileSdk);
assert.equal(androidReport.compileSdkRevision, fixture.toolchains.android.compileSdkRevision);
assert.equal(androidReport.buildTools, fixture.toolchains.android.buildTools);
assert.equal(androidReport.kotlin, fixture.toolchains.kotlin.version);
assert.equal(androidReport.coroutines, fixture.toolchains.coroutines.version);
assert.equal(androidReport.sourceSetSha256, androidSourceSet);
assert.equal(androidReport.command, "sdk/jvm/check-android.sh");
assert.ok(androidReport.unsupported.includes("device-or-emulator-runtime-certification"));
assert.ok(androidReport.unsupported.includes("android-java-net-http"));

const vectorPolarities = new Set(fixture.vectors.map(({ polarity }) => polarity));
assert.deepEqual(vectorPolarities, new Set(["positive", "negative", "boundary", "cancellation"]));
for (const concern of ["parity", "precision", "nullability", "unknown-variants", "failure-safety", "resource-limit"]) {
  assert.ok(fixture.vectors.some((vector) => vector.concern === concern), `missing ${concern} vector`);
}
for (const capability of [
  "authenticated-post-sse",
  "websocket",
  "compressed-responses",
  "automatic-authentication-refresh",
  "automatic-retry-or-non-idempotent-replay",
  "pagination-orchestration",
  "android-concrete-http-engine",
  "android-framework-lifecycle-binding",
  "android-device-or-emulator-certification",
  "gradle-maven-publication-and-reproducibility",
  "complete-official-sdk-matrix-owned-by-69",
]) {
  assert.ok(fixture.unsupported.includes(capability), `missing unsupported capability ${capability}`);
}

const coroutines = process.env.KOTLIN_COROUTINES_JAR;
const kotlinHome = process.env.KOTLIN_HOME;
const androidHome = process.env.ANDROID_SDK_ROOT;
assert.ok(coroutines && kotlinHome && androidHome, "KOTLIN_HOME, KOTLIN_COROUTINES_JAR, and ANDROID_SDK_ROOT are required");
assert.equal(
  createHash("sha256").update(readFileSync(coroutines)).digest("hex"),
  fixture.toolchains.coroutines.jarSha256,
  "coroutines artifact",
);
assert.equal(
  createHash("sha256")
    .update(readFileSync(`${androidHome}/platforms/android-${fixture.toolchains.android.compileSdk}/source.properties`))
    .digest("hex"),
  fixture.toolchains.android.platformSourcePropertiesSha256,
  "Android platform revision",
);
assert.equal(
  createHash("sha256")
    .update(readFileSync(`${androidHome}/build-tools/${fixture.toolchains.android.buildTools}/source.properties`))
    .digest("hex"),
  fixture.toolchains.android.buildToolsSourcePropertiesSha256,
  "Android build tools revision",
);

for (const tool of [
  { command: `${kotlinHome}/bin/kotlinc`, arguments: ["-version"], expected: /kotlinc-jvm 2\.2\.20(?:\s|$)/ },
  { command: "java", arguments: ["-version"], expected: /version "21\.0\.12\.1"/ },
  {
    command: `${androidHome}/build-tools/${fixture.toolchains.android.buildTools}/d8`,
    arguments: ["--version"],
    expected: /D8 8\.10\.9-dev .*a7ad18a70460b799d0482e497c109a75bf7f91de/,
  },
]) {
  const checked = spawnSync(tool.command, tool.arguments, {
    cwd: fileURLToPath(root),
    encoding: "utf8",
    env: process.env,
    timeout: 30_000,
  });
  assert.equal(checked.error, undefined, `${tool.command}: ${checked.error?.message}`);
  assert.equal(checked.status, 0, `${tool.command}: ${checked.stderr}`);
  assert.match(`${checked.stdout}\n${checked.stderr}`, tool.expected);
}

const execution = spawnSync(fileURLToPath(new URL("sdk/jvm/check-adapters.sh", root)), [], {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  env: process.env,
  timeout: 240_000,
});
assert.equal(execution.error, undefined, execution.error?.message);
assert.equal(execution.status, 0, execution.stderr);
const finalLine = execution.stdout.trim().split("\n").at(-1);
assert.deepEqual(JSON.parse(finalLine), { profile: fixture.profile, status: "passed" });
process.stdout.write(`${finalLine}\n`);

function report(path) {
  return JSON.parse(readFileSync(new URL(path, root), "utf8"));
}

function sourceSetSha256(entries) {
  const hash = createHash("sha256");
  for (const entry of [...entries].sort(({ path: left }, { path: right }) => left.localeCompare(right))) {
    hash.update(entry.path);
    hash.update("\0");
    hash.update(readFileSync(new URL(entry.path, root)));
    hash.update("\0");
  }
  return hash.digest("hex");
}
