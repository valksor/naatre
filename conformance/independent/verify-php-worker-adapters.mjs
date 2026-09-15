import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/php-worker-adapters.json", root), "utf8"));

assert.equal(fixture.profile, "sdk.php.worker-adapters-1");
assert.equal(fixture.ownerIssue, 97);
assert.equal(fixture.coreAuthority, "sdk.php.server-1");
assert.deepEqual(fixture.dependencies.map(({ issue, revision, profile }) => ({ issue, revision, profile })), [{
  issue: 58,
  revision: "10c560933509dacbc7ad987b864f86f2ee0994de",
  profile: "sdk.php.server-1",
}]);

for (const entry of [fixture.dependencies[0].manifest, fixture.dependencies[0].lock, ...fixture.evidence]) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

const expectedProfiles = [
  "php.symfony-worker-bridge-1",
  "php.laravel-worker-bridge-1",
  "php.fpm-host-1",
  "php.roadrunner-worker-1",
  "php.swoole-worker-1",
];
assert.deepEqual(Object.keys(fixture.profiles), expectedProfiles);

const evidenceDimensions = [
  "lifecycle",
  "cancellation",
  "pool",
  "transaction",
  "streaming",
  "requestIsolation",
  "failureSafety",
  "resourceLimit",
];
const vectorNames = new Set(fixture.vectors.map(({ name }) => name));
for (const [id, profile] of Object.entries(fixture.profiles)) {
  assert.equal(profile.status, "passed");
  assert.equal(profile.executedRuntime, "php-cli-lifecycle-model");
  assert.ok(profile.supportedPlatforms.length > 0, `${id} omits supported platforms`);
  assert.ok(profile.supportedRuntimes.length > 0, `${id} omits supported runtimes`);
  assert.ok(profile.unsupported.length > 0, `${id} omits unsupported capabilities`);
  assert.deepEqual(Object.keys(profile.evidence), evidenceDimensions);
  for (const vector of Object.values(profile.evidence)) {
    assert.ok(vectorNames.has(vector), `${id} references unknown vector ${vector}`);
  }
}

assert.deepEqual(new Set(fixture.vectors.map(({ polarity }) => polarity)), new Set([
  "positive",
  "negative",
  "boundary",
  "cancellation",
  "resource-limit",
]));
assert.deepEqual(fixture.publicFailureCodes, [
  "REMOTE_CAPABILITY_MISMATCH",
  "REMOTE_CANCELLATION_INVALID",
  "REMOTE_REGISTRATION_INVALID",
  "REMOTE_WORKER_MALFORMED",
]);
assert.equal(fixture.claims.nativeRuntimeCertification, false);
assert.equal(fixture.claims.frameworkReleaseCertification, false);
assert.equal(fixture.claims.productionTransportCertification, false);
assert.equal(fixture.claims.hardCancellation, false);
assert.equal(fixture.claims.processIsolation, false);
assert.equal(fixture.claims.exactlyOnceEffects, false);

const execution = spawnSync("php", ["sdk/php/tests/worker-adapters.php"], {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  timeout: 60_000,
});
assert.equal(execution.error, undefined, execution.error?.message);
assert.equal(execution.status, 0, execution.stderr);
const report = JSON.parse(execution.stdout.trim());
assert.equal(report.profile, fixture.profile);
assert.equal(report.status, "passed");
assert.equal(report.dependencyRevision, fixture.dependencies[0].revision);
assert.deepEqual(Object.keys(report.profiles), expectedProfiles);
for (const id of expectedProfiles) {
  assert.deepEqual(report.profiles[id], fixture.profiles[id], id);
}

process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime: report.runtime,
  dependencyRevision: report.dependencyRevision,
  profiles: expectedProfiles,
})}\n`);
