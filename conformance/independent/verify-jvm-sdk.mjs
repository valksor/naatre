import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/jvm-sdk.json", root), "utf8"));
const runtimeVectors = [
  "request-hash-parity",
  "explicit-nullability",
  "unknown-variants",
  "lossless-scalars",
  "locale-time-zone-independence",
  "partial-errors",
  "malformed-response",
  "future-cancellation",
  "blocking-deadline",
  "blocking-interruption",
  "coroutine-cancellation",
  "flow-cancellation",
  "stream-terminal-truncation",
  "response-frame-limits",
  "redirect-credentials",
  "authentication-interceptor",
  "unsupported-capabilities",
];

assert.equal(fixture.profile, "sdk.jvm.core-1");
assert.equal(fixture.generatorVersion, "naatre.generator.jvm-sdk-1");
assert.deepEqual(fixture.runtimes.map(({ jdk, status }) => [jdk, status]), [["17", "passed"], ["21", "passed"], ["25", "passed"]]);
assert.ok(fixture.buildTools.every(({ status }) => status === "not-executed"));
assert.equal(fixture.android.status, "unsupported");

const evidence = [
  fixture.sources.model,
  fixture.sources.referenceOutput,
  fixture.generated.java,
  fixture.generated.kotlin,
  fixture.generated.manifest,
  ...fixture.evidence,
];
const evidencePaths = evidence.map(({ path }) => path);
assert.equal(new Set(evidencePaths).size, evidencePaths.length, "duplicate JVM evidence path");
for (const entry of evidence) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

const sourceSet = evidence
  .filter(({ path }) =>
    path === "sdk/jvm/check.sh" ||
    path.startsWith("sdk/jvm/generated/") ||
    path.startsWith("sdk/jvm/src/main/") ||
    path.startsWith("sdk/jvm/src/test/"))
  .sort(({ path: left }, { path: right }) => left.localeCompare(right));
assert.ok(sourceSet.length > 0, "JVM runtime source set is empty");
const sourceSetHash = createHash("sha256");
for (const entry of sourceSet) {
  sourceSetHash.update(entry.path);
  sourceSetHash.update("\0");
  sourceSetHash.update(readFileSync(new URL(entry.path, root)));
  sourceSetHash.update("\0");
}
const sourceSetSha256 = sourceSetHash.digest("hex");

const reportsRoot = new URL("conformance/reports/", root);
const reportPaths = new Set();
for (const runtime of fixture.runtimes) {
  assert.equal(typeof runtime.report, "string");
  const reportURL = new URL(runtime.report, root);
  assert.ok(reportURL.href.startsWith(reportsRoot.href), `runtime report escapes report directory: ${runtime.report}`);
  assert.equal(
    reportURL.href,
    new URL(`jvm-sdk-jdk${runtime.jdk}-macos-arm64.json`, reportsRoot).href,
    `unexpected runtime report path for JDK ${runtime.jdk}`,
  );
  assert.ok(!reportPaths.has(runtime.report), `duplicate runtime report: ${runtime.report}`);
  reportPaths.add(runtime.report);
  assert.equal(evidencePaths.filter((path) => path === runtime.report).length, 1, `runtime report must be hashed exactly once: ${runtime.report}`);

  const report = JSON.parse(readFileSync(reportURL, "utf8"));
  assert.equal(report.profile, fixture.profile, `${runtime.report}: profile`);
  assert.equal(report.status, runtime.status, `${runtime.report}: status`);
  assert.equal(report.jdk, runtime.runtime, `${runtime.report}: runtime`);
  assert.equal(report.kotlin, fixture.package.kotlin, `${runtime.report}: Kotlin version`);
  assert.equal(report.coroutines, fixture.package.coroutines, `${runtime.report}: coroutines version`);
  assert.equal(report.bytecodeTarget, fixture.package.bytecodeTarget, `${runtime.report}: bytecode target`);
  assert.match(report.platform, /^macOS [^ ]+ arm64$/, `${runtime.report}: platform`);
  assert.equal(report.command, "sdk/jvm/check.sh", `${runtime.report}: command`);
  assert.deepEqual(report.vectors, runtimeVectors, `${runtime.report}: vectors`);
  assert.equal(report.sourceSetSha256, sourceSetSha256, `${runtime.report}: source set`);
}

const execution = spawnSync(fileURLToPath(new URL("sdk/jvm/check.sh", root)), [], {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  timeout: 120_000,
  env: process.env,
});
if (execution.error || execution.status !== 0) {
  throw new Error(`JVM SDK conformance failed: ${execution.error?.message ?? execution.stderr}`);
}
const report = JSON.parse(execution.stdout.trim().split("\n").at(-1));
assert.deepEqual(report, { generatorVersion: fixture.generatorVersion, profile: fixture.profile, status: "passed" });
process.stdout.write(`${JSON.stringify(report)}\n`);
