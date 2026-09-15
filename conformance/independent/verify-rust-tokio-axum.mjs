#!/usr/bin/env node

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = new URL("../../", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("conformance/v1/rust-tokio-axum.json", root), "utf8"));
const cargoManifest = fileURLToPath(new URL("sdk/rust/Cargo.toml", root));

assert.equal(fixture.profile, "worker.remote-1.rust-tokio-axum-1");
assert.equal(fixture.ownerIssue, 100);
assert.equal(fixture.normativeOwnerIssue, 61);
assert.equal(fixture.protocol, "naatre.remote-worker.v1");
assert.match(fixture.dependencies.issue61Commit, /^[0-9a-f]{40}$/u);
assertDigest(fixture.dependencies.workerFixture);
assertDigest(fixture.dependencies.protocolFixture);
assertDigest(fixture.dependencies.cargoLock);
for (const evidence of fixture.evidence) assertDigest(evidence);

assert.deepEqual(
  fixture.dependencies.crates.map(({ name, version }) => `${name}@${version}`),
  ["axum@0.8.9", "tokio@1.53.1", "tower@0.5.3"],
);
const cargoLock = readFileSync(new URL("sdk/rust/Cargo.lock", root), "utf8");
for (const dependency of fixture.dependencies.crates) {
  const packageBlock = cargoLock
    .split("[[package]]")
    .find((block) => block.includes(`name = "${dependency.name}"`) && block.includes(`version = "${dependency.version}"`));
  assert.ok(packageBlock, `missing locked crate ${dependency.name}`);
  assert.ok(packageBlock.includes(`checksum = "${dependency.checksum}"`), `checksum mismatch for ${dependency.name}`);
}

const classes = new Set(fixture.cases.map((testCase) => testCase.class));
assert.deepEqual([...classes].sort(), ["boundary", "cancellation", "negative", "positive", "resource-limit"]);
assert.equal(fixture.cases.every((testCase) => testCase.status === "passed"), true);
assert.equal(fixture.runtime.detachedTasks, 0);
assert.equal(fixture.runtime.internalUnboundedQueues, 0);
assert.equal(fixture.transport.streaming, false);
assert.equal(fixture.panicProfiles.unwind.includes("contained"), true);
assert.equal(fixture.panicProfiles.abort.includes("process-failure"), true);

const metadata = JSON.parse(run("cargo", [
  "metadata",
  "--manifest-path",
  cargoManifest,
  "--format-version",
  "1",
  "--no-deps",
  "--locked",
  "--offline",
]).stdout).packages.find((candidate) => candidate.name === fixture.package.name);
assert.equal(metadata.rust_version, "1.85");
assert.equal(metadata.edition, fixture.package.edition);
assert.deepEqual(metadata.features.default, fixture.package.defaultFeatures);
assert.deepEqual(metadata.features["tokio-runtime"], fixture.package.features["tokio-runtime"]);
assert.deepEqual(metadata.features.axum, fixture.package.features.axum);
for (const dependency of fixture.dependencies.crates) {
  const metadataDependency = metadata.dependencies.find((candidate) => candidate.name === dependency.name);
  assert.equal(metadataDependency.req, `=${dependency.version}`);
}

run("cargo", [
  "test",
  "--manifest-path",
  cargoManifest,
  "--profile",
  "unwind",
  "--features",
  "axum",
  "--test",
  "axum_worker",
  "--locked",
  "--offline",
]);
run("cargo", [
  "build",
  "--manifest-path",
  cargoManifest,
  "--release",
  "--features",
  "axum",
  "--locked",
  "--offline",
]);

const rustc = run("rustc", ["--version"]);
process.stdout.write(`${JSON.stringify({
  profile: fixture.profile,
  status: "passed",
  runtime: rustc.stdout.trim(),
  capabilities: fixture.supported,
  cases: fixture.cases.map(({ id, class: fixtureClass, status }) => ({ id, class: fixtureClass, status })),
  dependencies: fixture.dependencies.crates,
})}\n`);

function assertDigest(entry) {
  assert.match(entry.sha256, /^[0-9a-f]{64}$/u, entry.path);
  const location = new URL(entry.path, root);
  const actual = createHash("sha256").update(readFileSync(location)).digest("hex");
  if (actual !== entry.sha256) assert.fail(`digest mismatch for ${entry.path}: ${actual}`);
}

function run(command, arguments_) {
  const result = spawnSync(command, arguments_, {
    encoding: "utf8",
    timeout: 120_000,
  });
  if (result.error || result.status !== 0) {
    process.stderr.write(result.stderr || result.stdout || `${command} failed\n`);
    process.exit(1);
  }
  return result;
}
