import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/swift-sdk.json", root), "utf8"));
assert.equal(fixture.profile, "sdk.swift.core-1");
assert.equal(fixture.generatorVersion, "naatre.generator.swift-sdk-1");

for (const entry of [fixture.sources.model, fixture.sources.referenceOutput, fixture.generated.source, fixture.generated.manifest, ...fixture.evidence]) {
  const content = readFileSync(new URL(entry.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), entry.sha256, entry.path);
}

const execution = spawnSync("swift", [
  "run",
  "--disable-sandbox",
  "--package-path",
  fileURLToPath(new URL("sdk/swift", root)),
  "naatre-swift-conformance",
], { encoding: "utf8", timeout: 120_000 });
if (execution.error || execution.status !== 0) {
  throw new Error(`Swift SDK conformance failed: ${execution.error?.message ?? execution.stderr}`);
}
const report = JSON.parse(execution.stdout.trim().split("\n").at(-1));
assert.deepEqual(report, { generatorVersion: fixture.generatorVersion, profile: fixture.profile, status: "passed" });
process.stdout.write(`${JSON.stringify(report)}\n`);
