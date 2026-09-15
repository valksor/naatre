#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const report = JSON.parse(readFileSync(new URL("conformance/v1/rust-worker.json", root), "utf8"));

assert.equal(report.profile, "worker.remote-1.rust");
assert.equal(report.baseProfile, "worker.remote-1");
assert.equal(report.package.minimumRust, "1.85.0");
assert.equal(report.package.feature, "server");
assert.equal(report.package.defaultEnabled, false);
assert.deepEqual(report.package.externalRuntimeDependencies, []);
assert.equal(report.claims.remoteWorker, true);
assert.equal(report.claims.goGatewayPath, true);
assert.equal(report.claims.independentNativeRuntime, false);
assert.equal(report.claims.tokioAdapter, false);
assert.equal(report.claims.axumAdapter, false);
assert.equal(report.claims.productionHttpTransport, false);
assert.equal(report.claims.panicAbortRecovery, false);
assert.equal(report.panicProfiles.unwind.includes("contained"), true);
assert.equal(report.panicProfiles.abort.includes("process-failure"), true);
assert.equal(report.featureMatrix.some((row) => row.toolchain === "1.85.0" && row.features === "server"), true);
assert.equal(report.featureMatrix.some((row) => row.profile === "release-abort"), true);
assert.equal(report.vectors.includes("concurrent-request-scope-isolation"), true);

for (const evidence of report.evidence) {
  const content = readFileSync(new URL(evidence.path, root));
  assert.equal(createHash("sha256").update(content).digest("hex"), evidence.sha256, evidence.path);
}

const cargoManifest = fileURLToPath(new URL("sdk/rust/Cargo.toml", root));
const tests = spawnSync("cargo", [
  "test",
  "--manifest-path",
  cargoManifest,
  "--profile",
  "unwind",
  "--features",
  "server",
  "--test",
  "server",
  "--locked",
  "--offline",
], { encoding: "utf8", timeout: 60_000 });
if (tests.error || tests.status !== 0) {
  process.stderr.write(tests.stderr || "Rust worker conformance profile failed\n");
  process.exit(1);
}

process.stdout.write(`${JSON.stringify({
  profile: report.profile,
  status: "passed",
  nativeRuntime: report.claims.independentNativeRuntime,
})}\n`);
