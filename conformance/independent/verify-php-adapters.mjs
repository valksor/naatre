import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/php-adapters.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.php.adapters-1");

function sha256(path) {
  return createHash("sha256").update(readFileSync(new URL(path, root))).digest("hex");
}

assert.equal(sha256(fixture.dependencyRevisions.core.fixture.path), fixture.dependencyRevisions.core.fixture.sha256);
for (const evidence of fixture.evidence) {
  assert.equal(sha256(evidence.path), evidence.sha256, evidence.path);
}

const executed = spawnSync("php", ["sdk/php/tests/adapters.php"], {
  cwd: fileURLToPath(root),
  encoding: "utf8",
  timeout: 60_000,
});
if (executed.error || executed.status !== 0) {
  throw new Error("PHP adapter conformance failed");
}
const report = JSON.parse(executed.stdout.trim());
assert.equal(report.profile, fixture.profile);
assert.equal(report.status, "passed");
assert.deepEqual(report.dependencyRevisions, fixture.dependencyRevisions);
assert.deepEqual(report.vectors, fixture.fixtures);

process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime: report.runtime,
  dependencyRevisions: fixture.dependencyRevisions,
})}\n`);
